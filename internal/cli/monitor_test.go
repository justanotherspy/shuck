package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justanotherspy/shuck/internal/monitor"
)

// monitorHome points shuck's whole on-disk state at a temp directory, so a
// test never touches the developer's real monitor.
func monitorHome(t *testing.T) {
	t.Helper()
	t.Setenv("SHUCK_HOME", t.TempDir())
}

// noDaemonClient returns a client that will never find or start a daemon,
// which is the state every one of these tests runs against: the CLI's job here
// is to report that clearly, not to conjure one.
func noDaemonClient(t *testing.T) {
	t.Helper()
	original := newMonitorClient
	newMonitorClient = func() (*monitor.Client, error) {
		c, err := monitor.NewClient()
		if err != nil {
			return nil, err
		}
		c.AutoStart = false
		return c, nil
	}
	t.Cleanup(func() { newMonitorClient = original })
}

// fakeDaemon stands in for the monitor daemon. A client dials whatever
// endpoint.json names and speaks one JSON line each way, so a listener plus
// that file is a real daemon as far as the CLI is concerned — which is the only
// way to assert what these commands actually put on the wire.
type fakeDaemon struct {
	mu       sync.Mutex
	requests []monitor.Request
	handle   func(monitor.Request) monitor.Response
}

// fakeDaemonToken guards the loopback endpoint. The real protocol requires a
// bearer token on TCP, so the fake rejects a client that omits it rather than
// letting one pass here and fail against the daemon it ships with.
const fakeDaemonToken = "monitor-test-token"

// startFakeDaemon points shuck's state at a temp directory, serves handle
// there, and makes sure nothing in the test can spawn a real daemon.
func startFakeDaemon(t *testing.T, handle func(monitor.Request) monitor.Response) *fakeDaemon {
	t.Helper()
	monitorHome(t)
	noDaemonClient(t)

	dir, err := monitor.Dir()
	if err != nil {
		t.Fatalf("monitor.Dir: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	raw, err := json.Marshal(map[string]any{
		"network": "tcp",
		"address": ln.Addr().String(),
		"token":   fakeDaemonToken,
		"pid":     os.Getpid(),
	})
	if err != nil {
		t.Fatalf("encode endpoint: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "endpoint.json"), append(raw, '\n'), 0o600); err != nil {
		t.Fatalf("write endpoint: %v", err)
	}

	d := &fakeDaemon{handle: handle}
	go d.serve(ln)
	return d
}

func (d *fakeDaemon) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go d.answer(conn)
	}
}

func (d *fakeDaemon) answer(conn net.Conn) {
	defer conn.Close()
	line, err := bufio.NewReaderSize(conn, 64<<10).ReadBytes('\n')
	if err != nil {
		return
	}
	var req monitor.Request
	if err := json.Unmarshal(line, &req); err != nil {
		return
	}

	// Recording and answering under the lock keeps a scripted handler's own
	// state race-free without every test having to guard it.
	d.mu.Lock()
	d.requests = append(d.requests, req)
	resp := monitor.Response{OK: false, Error: "monitor token mismatch"}
	if req.Auth == fakeDaemonToken {
		resp = d.handle(req)
	}
	d.mu.Unlock()

	out, err := json.Marshal(resp)
	if err != nil {
		return
	}
	_, _ = conn.Write(append(out, '\n'))
}

// seen returns the requests the daemon has been sent so far.
func (d *fakeDaemon) seen() []monitor.Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]monitor.Request(nil), d.requests...)
}

func runCLI(args ...string) (code int, stdoutText, stderrText string) {
	var stdout, stderr bytes.Buffer
	code = Run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestMonitorUsage(t *testing.T) {
	code, _, stderr := runCLI("monitor", "nonsense")
	if code != 2 {
		t.Errorf("exit = %d, want 2 for an unknown subcommand", code)
	}
	for _, want := range []string{"unknown subcommand", "shuck monitor watch", "shuck monitor events", "shuck monitor stream"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr is missing %q:\n%s", want, stderr)
		}
	}
}

func TestMonitorAliasIsRegistered(t *testing.T) {
	// `shuck m` must reach the monitor, not be mistaken for a target.
	_, _, stderr := runCLI("m", "nonsense")
	if !strings.Contains(stderr, "shuck monitor") {
		t.Errorf("`shuck m` did not dispatch to the monitor:\n%s", stderr)
	}
}

func TestMonitorStatusWithNoDaemon(t *testing.T) {
	monitorHome(t)
	noDaemonClient(t)

	// --no-start is the honest read: report that nothing is running rather
	// than starting one just to say it is empty.
	code, stdout, _ := runCLI("monitor", "status", "--no-start")
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "not running") {
		t.Errorf("stdout = %q, want it to say the monitor is not running", stdout)
	}
}

func TestMonitorReadCommandsDoNotStartADaemon(t *testing.T) {
	monitorHome(t)
	noDaemonClient(t)

	for _, args := range [][]string{
		{"monitor", "events"},
		{"monitor", "poke"},
		{"monitor", "unwatch"},
	} {
		code, _, stderr := runCLI(args...)
		if code != 2 {
			t.Errorf("%v exited %d, want 2 with no daemon", args, code)
		}
		if !strings.Contains(stderr, "no shuck monitor is running") {
			t.Errorf("%v stderr = %q, want it to name the problem", args, stderr)
		}
	}
}

func TestMonitorStopWithNoDaemonIsNotAnError(t *testing.T) {
	monitorHome(t)
	noDaemonClient(t)

	// Stopping something already stopped is what the caller wanted.
	code, stdout, _ := runCLI("monitor", "stop")
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "not running") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestMonitorLogWithNoLog(t *testing.T) {
	monitorHome(t)
	code, stdout, _ := runCLI("monitor", "log")
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "no log yet") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestMonitorWatchRejectsABadTarget(t *testing.T) {
	monitorHome(t)
	noDaemonClient(t)

	code, _, stderr := runCLI("monitor", "watch", "not-a-target")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if stderr == "" {
		t.Error("a bad target should be explained")
	}
}

func TestMonitorWatchNeedsAToken(t *testing.T) {
	monitorHome(t)
	noDaemonClient(t)
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")

	// With nothing listening, this command has to start the daemon, and the
	// daemon it starts inherits this environment — so a missing token is
	// reported here, in front of the person who can fix it, rather than
	// silently inside a background process. It is also reported in place of
	// the connection failure, because it is the half that can be acted on.
	code, _, stderr := runCLI("monitor", "watch", "o/r#1")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "no GitHub token") {
		t.Errorf("stderr = %q, want the missing token named", stderr)
	}
}

// TestMonitorWatchNeedsNoTokenWhenADaemonIsRunning is the other half of that
// rule, and a bug that was observed rather than imagined: a running daemon
// polls with the token it was started with, so a client that only wants to
// register a watch needs no credential of its own. Demanding one refused the
// registration while a perfectly healthy monitor was polling GitHub.
func TestMonitorWatchNeedsNoTokenWhenADaemonIsRunning(t *testing.T) {
	d := startFakeDaemon(t, func(req monitor.Request) monitor.Response {
		stored := *req.Watch
		stored.ID = "pr:o/r#1"
		return monitor.Response{OK: true, Watch: &stored}
	})
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")

	code, stdout, stderr := runCLI("monitor", "watch", "o/r#1")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "watching") {
		t.Errorf("stdout = %q, want the registration confirmed", stdout)
	}
	if n := len(d.seen()); n != 1 {
		t.Errorf("made %d calls, want the one watch", n)
	}
	// Nothing about the client's own credentials reaches a daemon that is
	// already running; asserting it stayed empty is what keeps a future change
	// from quietly making the token load-bearing here again.
	if got := d.seen()[0].Op; got != monitor.OpWatch {
		t.Errorf("op = %q, want %q", got, monitor.OpWatch)
	}
}

func TestMonitorHookNeedsAnEvent(t *testing.T) {
	code, _, stderr := runCLI("monitor", "hook")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "which hook") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestMonitorHookExitsZeroWithNoDaemon(t *testing.T) {
	monitorHome(t)
	// A hook must never be the reason a session stalls.
	code, stdout, _ := runCLI("monitor", "hook", "user-prompt-submit")
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want silence", stdout)
	}
}

func TestMonitorRunRejectsBadFlags(t *testing.T) {
	monitorHome(t)
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")

	code, _, stderr := runCLI("monitor", "run")
	if code != 2 {
		t.Errorf("exit = %d, want 2 with no token", code)
	}
	if !strings.Contains(stderr, "no GitHub token") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestRenderStatus(t *testing.T) {
	var b bytes.Buffer
	renderStatus(&b, &monitor.Status{
		PID: 42, Version: "v1.2.3", Uptime: 90 * time.Second,
		RateRemaining: 4200, RateLimit: 5000,
		Watches: []monitor.Watch{
			{ID: "tree:/w", Kind: monitor.WatchTree, Path: "/w", Owner: "o", Repo: "r", Number: 7, Branch: "feature"},
		},
		Targets: []monitor.TargetStatus{
			{Target: "o/r#7", Verdict: "failed", NextPoll: time.Now().Add(30 * time.Second)},
		},
		Events: 12, Pending: 3,
	})

	got := b.String()
	for _, want := range []string{"v1.2.3", "pid 42", "4200/5000", "/w", "o/r#7", "CI FAILING", "next check in", "12 event", "3 waiting"} {
		if !strings.Contains(got, want) {
			t.Errorf("status is missing %q:\n%s", want, got)
		}
	}
}

func TestRenderStatusEmpty(t *testing.T) {
	var b bytes.Buffer
	renderStatus(&b, &monitor.Status{Version: "dev"})
	got := b.String()
	if !strings.Contains(got, "shuck monitor watch") {
		t.Errorf("an empty monitor should say how to point it at something:\n%s", got)
	}
}

func TestTargetLine(t *testing.T) {
	tests := []struct {
		name   string
		target monitor.TargetStatus
		want   []string
	}{
		{"green", monitor.TargetStatus{Verdict: "passed"}, []string{"CI green"}},
		{"red", monitor.TargetStatus{Verdict: "failed"}, []string{"CI FAILING"}},
		{"running", monitor.TargetStatus{}, []string{"running or not started"}},
		{"merged", monitor.TargetStatus{Verdict: "passed", Lifecycle: "merged"}, []string{"merged"}},
		{"due now", monitor.TargetStatus{NextPoll: time.Now().Add(-time.Minute)}, []string{"checking now"}},
		{"failing", monitor.TargetStatus{LastError: "401"}, []string{"error: 401"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := targetLine(tt.target)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("targetLine = %q, want it to contain %q", got, want)
				}
			}
		})
	}
}

func TestEmitEvents(t *testing.T) {
	events := []monitor.Event{
		{ID: 1, Kind: monitor.KindCIFailed, Target: "o/r#7", Title: "test failed", Body: "boom"},
	}

	t.Run("text", func(t *testing.T) {
		var out bytes.Buffer
		if err := emitEvents(&out, events, false, true); err != nil {
			t.Fatalf("emitEvents: %v", err)
		}
		if !strings.Contains(out.String(), "test failed") || !strings.Contains(out.String(), "boom") {
			t.Errorf("stdout = %q", out.String())
		}
	})

	t.Run("json is one object per line", func(t *testing.T) {
		var out bytes.Buffer
		if err := emitEvents(&out, events, true, true); err != nil {
			t.Fatalf("emitEvents: %v", err)
		}
		if lines := strings.Count(strings.TrimSpace(out.String()), "\n"); lines != 0 {
			t.Errorf("one event should be one line, got %d newlines", lines+1)
		}
		if !strings.Contains(out.String(), `"kind":"ci.failed"`) {
			t.Errorf("stdout = %q", out.String())
		}
	})

	t.Run("nothing new", func(t *testing.T) {
		var out bytes.Buffer
		if err := emitEvents(&out, nil, false, true); err != nil {
			t.Fatalf("emitEvents: %v", err)
		}
		if !strings.Contains(out.String(), "nothing new") {
			t.Errorf("stdout = %q", out.String())
		}

		out.Reset()
		if err := emitEvents(&out, nil, false, false); err != nil {
			t.Fatalf("emitEvents: %v", err)
		}
		if out.String() != "" {
			t.Errorf("while following, an empty batch should print nothing, got %q", out.String())
		}
	})
}

func TestEmitJSON(t *testing.T) {
	t.Run("a value survives the round trip", func(t *testing.T) {
		st := &monitor.Status{
			PID: 42, Version: "v1.2.3", Uptime: 90 * time.Second, Events: 12,
			Watches: []monitor.Watch{{ID: "tree:/w", Kind: monitor.WatchTree, Path: "/w", Owner: "o", Repo: "r", Number: 7}},
		}

		var out, errBuf bytes.Buffer
		if code := emitJSON(&out, &errBuf, st); code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		if errBuf.Len() != 0 {
			t.Errorf("stderr = %q, want silence on success", errBuf.String())
		}

		var got monitor.Status
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("stdout is not a JSON document: %v\n%s", err, out.String())
		}
		if got.PID != st.PID || got.Version != st.Version || got.Events != st.Events {
			t.Errorf("round trip lost the scalars: %+v", got)
		}
		if len(got.Watches) != 1 || got.Watches[0].ID != "tree:/w" || got.Watches[0].Number != 7 {
			t.Errorf("round trip lost the watches: %+v", got.Watches)
		}

		// --json output is read by people about as often as by programs, so the
		// indent is part of the contract; the trailing newline is what makes a
		// sequence of documents line-delimited for whatever consumes it.
		if !strings.Contains(out.String(), "\n  \"pid\": 42") {
			t.Errorf("stdout is not indented:\n%s", out.String())
		}
		if !strings.HasSuffix(out.String(), "}\n") {
			t.Errorf("stdout should end in a newline: %q", out.String())
		}
	})

	// An encode failure has to reach the exit code and stderr carrying the
	// encoder's own reason: "shuck: could not encode" leaves nobody able to
	// tell an unmarshalable type from a NaN, which are different bugs in
	// different places. (Stdout staying empty is encoding/json's own
	// marshal-then-write guarantee, not shuck's — what is shuck's is not
	// echoing the diagnostic there, where a consumer's parser would meet it
	// where a document should be.)
	unencodable := []struct {
		name    string
		v       any
		wantErr string
	}{
		{"a channel it cannot marshal", map[string]any{"watches": 1, "notify": make(chan int)}, "unsupported type: chan int"},
		{"a NaN with no JSON spelling", map[string]any{"rate_remaining": math.NaN()}, "unsupported value: NaN"},
	}
	for _, tt := range unencodable {
		t.Run(tt.name, func(t *testing.T) {
			var out, errBuf bytes.Buffer
			if code := emitJSON(&out, &errBuf, tt.v); code != 2 {
				t.Errorf("exit = %d, want 2 — an unencodable value is an operational error", code)
			}
			if out.Len() != 0 {
				t.Errorf("stdout = %q, want the failure reported on stderr alone", out.String())
			}
			if !strings.Contains(errBuf.String(), "shuck: ") || !strings.Contains(errBuf.String(), tt.wantErr) {
				t.Errorf("stderr = %q, want it to name the encode failure (%q)", errBuf.String(), tt.wantErr)
			}
		})
	}
}

func TestMonitorStatusJSON(t *testing.T) {
	d := startFakeDaemon(t, func(req monitor.Request) monitor.Response {
		if req.Op != monitor.OpStatus {
			return monitor.Response{Error: "unexpected op " + string(req.Op)}
		}
		return monitor.Response{OK: true, Status: &monitor.Status{
			PID: 4242, Version: "v9.9.9", Uptime: time.Minute,
			Watches: []monitor.Watch{{ID: "tree:/w", Kind: monitor.WatchTree, Path: "/w", Owner: "o", Repo: "r", Number: 7, Branch: "feature"}},
			Targets: []monitor.TargetStatus{{Target: "o/r#7", Verdict: "failed"}},
			Events:  12, Pending: 3,
		}}
	})

	code, stdout, stderr := runCLI("monitor", "status", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}

	var got monitor.Status
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("--json did not emit a JSON document: %v\n%s", err, stdout)
	}
	if got.PID != 4242 || got.Version != "v9.9.9" || got.Pending != 3 {
		t.Errorf("the daemon's status did not reach the consumer: %+v", got)
	}
	if len(got.Targets) != 1 || got.Targets[0].Target != "o/r#7" || got.Targets[0].Verdict != "failed" {
		t.Errorf("targets = %+v, want the daemon's one failing target", got.Targets)
	}
	// --json replaces the text view rather than joining it.
	if strings.Contains(stdout, "CI FAILING") || strings.Contains(stdout, "Watching (") {
		t.Errorf("--json emitted the text rendering too:\n%s", stdout)
	}
	if reqs := d.seen(); len(reqs) != 1 || reqs[0].Op != monitor.OpStatus {
		t.Errorf("requests = %+v, want exactly one status call", reqs)
	}
}

func TestMonitorEventsJSON(t *testing.T) {
	d := startFakeDaemon(t, func(monitor.Request) monitor.Response {
		return monitor.Response{OK: true, Cursor: 1, Events: []monitor.Event{
			{ID: 1, Kind: monitor.KindCIFailed, Target: "o/r#7", Title: "build failed", Body: "boom"},
		}}
	})

	code, stdout, stderr := runCLI("monitor", "events", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	// The event feed is line-delimited JSON, not the indented document
	// `status --json` emits: consumers read it an event at a time.
	line := strings.TrimSpace(stdout)
	if strings.Contains(line, "\n") {
		t.Errorf("one event should be one line:\n%s", stdout)
	}
	var got monitor.Event
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if got.ID != 1 || got.Kind != monitor.KindCIFailed || got.Body != "boom" {
		t.Errorf("event = %+v, want the daemon's failure with its body intact", got)
	}

	// A bare read is one non-blocking, uncapped drain of what is pending for
	// the "cli" consumer. None of that is visible in the output above, so the
	// request the daemon was handed is the only place it can be checked.
	reqs := d.seen()
	if len(reqs) != 1 {
		t.Fatalf("made %d calls, want exactly 1", len(reqs))
	}
	want := monitor.Request{Op: monitor.OpEvents, Auth: fakeDaemonToken, Consumer: "cli"}
	if reqs[0] != want {
		t.Errorf("request  = %+v\nwant       %+v", reqs[0], want)
	}
}

// TestMonitorEventsFlagsReachTheDaemon pins the whole flag → Request plumbing.
// --all, --limit, --wait and --consumer leave no trace in what the CLI prints:
// the daemon decides what to hand back, so dropping any one of them from the
// Request silently changes which events a caller gets, and only the request log
// can tell. --consumer in particular decides *whose* cursor advances, so
// hardcoding it would quietly hand one session's failures to another.
func TestMonitorEventsFlagsReachTheDaemon(t *testing.T) {
	d := startFakeDaemon(t, func(monitor.Request) monitor.Response {
		return monitor.Response{OK: true, Cursor: 9}
	})

	code, stdout, stderr := runCLI("monitor", "events", "--all", "--limit", "3", "--wait", "2s", "--consumer", "session-7")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "nothing new") {
		t.Errorf("stdout = %q, want a one-shot read to say the feed was empty", stdout)
	}

	reqs := d.seen()
	if len(reqs) != 1 {
		t.Fatalf("made %d calls, want exactly 1", len(reqs))
	}
	want := monitor.Request{
		Op:       monitor.OpEvents,
		Auth:     fakeDaemonToken,
		Consumer: "session-7",
		Limit:    3,
		Wait:     2 * time.Second,
		All:      true,
	}
	if reqs[0] != want {
		t.Errorf("request  = %+v\nwant       %+v", reqs[0], want)
	}
}

// TestMonitorEventsFollowThroughTheCLI drives --follow the way a person does.
// TestMonitorFollow below calls monitorFollow directly, which leaves the flag
// itself unpinned: with the dispatch gone, `--follow` degrades to a single
// one-shot read that prints the first batch and exits 0, and every assertion
// about the loop still passes because the loop is being called by hand. The
// second read is what proves the CLI entered it.
func TestMonitorEventsFollowThroughTheCLI(t *testing.T) {
	round := 0
	d := startFakeDaemon(t, func(monitor.Request) monitor.Response {
		round++
		if round == 1 {
			return monitor.Response{OK: true, Cursor: 1, Events: []monitor.Event{
				{ID: 1, Kind: monitor.KindCIFailed, Target: "o/r#7", Title: "build failed", Body: "boom"},
			}}
		}
		// A daemon that cannot answer is how this follow ends: the test has no
		// safe way to deliver the Ctrl-C the loop otherwise waits for.
		return monitor.Response{Error: "journal is unreadable"}
	})

	code, stdout, stderr := runCLI("monitor", "events", "--follow", "--all", "--limit", "5", "--consumer", "session-9")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 — the follow ended on a daemon error; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "build failed") {
		t.Errorf("stdout = %q, want the first batch printed before the error", stdout)
	}
	if !strings.Contains(stderr, "journal is unreadable") {
		t.Errorf("stderr = %q, want the daemon's reason", stderr)
	}

	reqs := d.seen()
	if len(reqs) != 2 {
		t.Fatalf("made %d reads, want 2 — one read means --follow never reached monitorFollow", len(reqs))
	}
	for i, r := range reqs {
		if r.Op != monitor.OpEvents {
			t.Errorf("read %d asked for %q, want %q", i+1, r.Op, monitor.OpEvents)
		}
		// Following overrides --all, which would otherwise re-deliver the whole
		// journal on every pass. (--wait is rejected outright rather than
		// overridden — see TestMonitorEventsFollowRejectsWait.)
		if r.All {
			t.Errorf("read %d asked for the whole journal", i+1)
		}
		if r.Wait < 5*time.Second {
			t.Errorf("read %d waited %s; a follow has to block for a meaningful stretch", i+1, r.Wait)
		}
		if r.Consumer != "session-9" || r.Limit != 5 {
			t.Errorf("read %d = consumer %q limit %d, want the caller's own", i+1, r.Consumer, r.Limit)
		}
	}
}

// followDaemon scripts a feed: each batch in turn answers one events call, and
// the call after the last one cancels the context — which is how these tests
// spell "the operator hit Ctrl-C".
func followDaemon(t *testing.T, cancel context.CancelFunc, batches ...[]monitor.Event) *fakeDaemon {
	t.Helper()
	round := 0
	return startFakeDaemon(t, func(monitor.Request) monitor.Response {
		if round < len(batches) {
			round++
			return monitor.Response{OK: true, Events: batches[round-1], Cursor: uint64(round)}
		}
		cancel()
		return monitor.Response{OK: true, Cursor: uint64(round)}
	})
}

// interruptedCtx reports that it is finished while never closing Done. A real
// cancelled context proves nothing about the loop's own exit check: net.Dialer
// refuses to dial one, so a follow that ignored the context entirely would
// still stop — on the resulting dial error rather than on the interruption.
// Holding Done open leaves the daemon reachable and answering, so the loop ends
// only if it consults the context after delivering a batch.
type interruptedCtx struct{ context.Context }

func (interruptedCtx) Err() error { return context.Canceled }

// runFollow drives monitorFollow with a deadline, so a follow that never gives
// up fails the test instead of hanging the package.
func runFollow(t *testing.T, ctx context.Context, req monitor.Request, jsonOut bool) (code int, stdoutText, stderrText string) {
	t.Helper()
	client, err := newMonitorClient()
	if err != nil {
		t.Fatalf("newMonitorClient: %v", err)
	}

	var out, errBuf bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- monitorFollow(ctx, client, req, jsonOut, &out, &errBuf) }()
	select {
	case code := <-done:
		return code, out.String(), errBuf.String()
	case <-time.After(30 * time.Second):
		t.Fatal("monitorFollow never returned")
		return 0, "", ""
	}
}

func TestMonitorFollow(t *testing.T) {
	t.Run("streams batch after batch until interrupted", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		d := followDaemon(t, cancel,
			[]monitor.Event{{ID: 1, Kind: monitor.KindCIFailed, Target: "o/r#7", Title: "build failed", Body: "boom"}},
			[]monitor.Event{{ID: 2, Kind: monitor.KindCIPassed, Target: "o/r#7", Title: "checks are green"}},
		)

		// --all and a short --wait are what the caller's flags may have set;
		// following overrides both, and the request log is where that shows.
		req := monitor.Request{Consumer: "session-1", Limit: 5, All: true, Wait: time.Millisecond}
		code, stdout, stderr := runFollow(t, ctx, req, false)
		if code != 0 {
			t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
		}
		if stderr != "" {
			t.Errorf("stderr = %q, want silence", stderr)
		}

		first, second := strings.Index(stdout, "build failed"), strings.Index(stdout, "checks are green")
		if first < 0 || second < 0 {
			t.Fatalf("both batches should have been printed:\n%s", stdout)
		}
		if first > second {
			t.Errorf("events printed out of order:\n%s", stdout)
		}

		reqs := d.seen()
		if len(reqs) != 3 {
			t.Fatalf("made %d reads, want 3 (two batches then the interrupted one)", len(reqs))
		}
		for i, r := range reqs {
			if r.Op != monitor.OpEvents {
				t.Errorf("read %d asked for %q, want %q", i+1, r.Op, monitor.OpEvents)
			}
			if r.All {
				t.Errorf("read %d asked for the whole journal; a follow may only ever ask for what is new", i+1)
			}
			// Comparing against followInterval would only prove the constant
			// equals itself. The contract is that a follow *blocks* — the
			// caller's 1ms must be overridden by something long enough that a
			// quiet feed is one held connection rather than a hot loop of
			// dials — so the bound is written out here, independent of what the
			// production constant happens to be.
			if r.Wait < 5*time.Second {
				t.Errorf("read %d waited %s; a follow that does not block for a meaningful stretch is a spin loop", i+1, r.Wait)
			}
			if r.Consumer != "session-1" || r.Limit != 5 {
				t.Errorf("read %d = consumer %q limit %d, want the caller's own", i+1, r.Consumer, r.Limit)
			}
		}
	})

	t.Run("a quiet feed prints nothing", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		followDaemon(t, cancel, nil)

		// Every 30 seconds a blocking read comes back empty. Saying "nothing
		// new" each time would bury the events the follow exists to show.
		code, stdout, stderr := runFollow(t, ctx, monitor.Request{Consumer: "cli"}, false)
		if code != 0 {
			t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want nothing for an empty batch", stdout)
		}
	})

	t.Run("--json follows through to the stream", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		followDaemon(t, cancel, []monitor.Event{{ID: 7, Kind: monitor.KindReviewComment, Target: "o/r#7", Title: "please rename this"}})

		code, stdout, stderr := runFollow(t, ctx, monitor.Request{Consumer: "cli"}, true)
		if code != 0 {
			t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
		}
		var got monitor.Event
		if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &got); err != nil {
			t.Fatalf("--follow --json is not JSON: %v\n%s", err, stdout)
		}
		if got.ID != 7 || got.Kind != monitor.KindReviewComment {
			t.Errorf("event = %+v, want the daemon's review comment", got)
		}
	})

	t.Run("a daemon error stops the follow", func(t *testing.T) {
		d := startFakeDaemon(t, func(monitor.Request) monitor.Response {
			return monitor.Response{Error: "journal is unreadable"}
		})

		// The context is alive, so this is a real fault: report it and stop
		// rather than hammering a daemon that cannot answer.
		code, stdout, stderr := runFollow(t, context.Background(), monitor.Request{Consumer: "cli"}, false)
		if code != 2 {
			t.Fatalf("exit = %d, want 2", code)
		}
		if !strings.Contains(stderr, "journal is unreadable") {
			t.Errorf("stderr = %q, want the daemon's reason", stderr)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want nothing", stdout)
		}
		if n := len(d.seen()); n != 1 {
			t.Errorf("made %d reads, want 1 — a failing daemon must not be retried in a loop", n)
		}
	})

	t.Run("an interruption is not a failure", func(t *testing.T) {
		monitorHome(t)
		noDaemonClient(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		// Ctrl-C during a blocking read surfaces as a connection error. It is
		// how a follow ends, so it must not be reported as one.
		code, stdout, stderr := runFollow(t, ctx, monitor.Request{Consumer: "cli"}, false)
		if code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
		if stderr != "" {
			t.Errorf("stderr = %q, want an interrupted follow to say nothing", stderr)
		}
		if stdout != "" {
			t.Errorf("stdout = %q", stdout)
		}
	})

	t.Run("an interruption ends the loop, not the next failed read", func(t *testing.T) {
		reads := 0
		d := startFakeDaemon(t, func(monitor.Request) monitor.Response {
			reads++
			if reads == 1 {
				return monitor.Response{OK: true, Cursor: 1, Events: []monitor.Event{
					{ID: 1, Kind: monitor.KindCIPassed, Target: "o/r#7", Title: "checks are green"},
				}}
			}
			// Nothing should ask a second time. Answering with an error rather
			// than another batch bounds a loop that does, instead of letting it
			// spin against a healthy daemon until the test timeout.
			return monitor.Response{Error: "the follow should already have stopped"}
		})

		// The daemon here stays up and willing, so only the loop's own check on
		// the context can end this — which is the point: termination is the
		// follow's decision, not a side effect of a dial that happens to fail.
		code, stdout, stderr := runFollow(t, interruptedCtx{context.Background()}, monitor.Request{Consumer: "cli"}, false)
		if code != 0 {
			t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
		}
		if !strings.Contains(stdout, "checks are green") {
			t.Errorf("stdout = %q, want the batch already in hand delivered before stopping", stdout)
		}
		if stderr != "" {
			t.Errorf("stderr = %q, want an interrupted follow to say nothing", stderr)
		}
		if n := len(d.seen()); n != 1 {
			t.Errorf("made %d reads, want 1 — an interrupted follow must stop after the batch it delivered", n)
		}
	})
}

// streamDaemon scripts the daemon a `monitor stream` talks to: it registers the
// watch, hands over each batch in turn, and then refuses to answer — which is
// how these tests end a command that otherwise runs until the session does.
// There is no safe way to deliver the SIGTERM Claude Code would send.
func streamDaemon(t *testing.T, batches ...[]monitor.Event) *fakeDaemon {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "test-token")
	round := 0
	return startFakeDaemon(t, func(req monitor.Request) monitor.Response {
		if req.Op == monitor.OpWatch {
			stored := *req.Watch
			return monitor.Response{OK: true, Watch: &stored}
		}
		if req.Op == monitor.OpSeek {
			return monitor.Response{OK: true, Cursor: 1}
		}
		if round < len(batches) {
			round++
			return monitor.Response{OK: true, Events: batches[round-1], Cursor: uint64(round)}
		}
		return monitor.Response{Error: "the monitor went away"}
	})
}

// streamMarkers lists the marker files a stream has left behind.
func streamMarkers(t *testing.T) []string {
	t.Helper()
	dir, err := monitor.Dir()
	if err != nil {
		t.Fatalf("monitor.Dir: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "streams"))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestMonitorStreamDeliversTheAgentFacingBlock is the whole command in one
// test: it registers this working tree, reads under a cursor of its own, and
// writes each batch as the block a session is meant to receive — wrapper,
// provenance line and all. A stream that emitted the terminal rendering instead
// would hand an agent a wall of CI output with nothing saying where it came
// from.
func TestMonitorStreamDeliversTheAgentFacingBlock(t *testing.T) {
	d := streamDaemon(t, []monitor.Event{
		{ID: 1, Kind: monitor.KindCIFailed, Target: "o/r#7", Title: "build failed", Body: "boom"},
	})

	code, stdout, stderr := runCLI("monitor", "stream")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — a monitor that stops is not a failed command; stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q; only stdout becomes a notification, so nothing may go here", stderr)
	}
	for _, want := range []string{"<shuck-monitor>", "not a message from the user", "build failed", "boom"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the notification is missing %q:\n%s", want, stdout)
		}
	}
	// The daemon going away ends the stream in prose, not in a stack trace.
	if !strings.Contains(stdout, "not streaming into this session") || !strings.Contains(stdout, "the monitor went away") {
		t.Errorf("stdout should end by saying the stream stopped and why:\n%s", stdout)
	}

	reqs := d.seen()
	if len(reqs) < 2 || reqs[0].Op != monitor.OpWatch {
		t.Fatalf("requests = %+v, want the working tree registered first", reqs)
	}
	if reqs[0].Watch == nil || reqs[0].Watch.Kind != monitor.WatchTree {
		t.Errorf("registered %+v, want a watch on this working tree", reqs[0].Watch)
	}
	for _, r := range reqs[1:] {
		if r.Op != monitor.OpEvents && r.Op != monitor.OpSeek {
			t.Errorf("op = %q, want %q", r.Op, monitor.OpEvents)
		}
		if r.Op == monitor.OpSeek {
			continue
		}
		// The prefix is what keeps the stream's cursor out of the session's: a
		// stream sharing one would consume the events the Stop hook gates on.
		if !strings.HasPrefix(r.Consumer, "stream:") {
			t.Errorf("consumer = %q, want an identity that cannot collide with a session's", r.Consumer)
		}
		if r.Limit <= 0 {
			t.Error("an uncapped read hands a first-ever stream the whole journal as one notification")
		}
		if r.All {
			t.Error("a stream may only ever ask for what is new")
		}
		if r.Wait < 5*time.Second {
			t.Errorf("read waited %s; a stream that does not block is a dial loop", r.Wait)
		}
	}

	// The marker is the hooks' only way to know the stream has gone, so it has
	// to go with it.
	if names := streamMarkers(t); len(names) != 0 {
		t.Errorf("the stream left %v behind; the hooks will stay silent until it ages out", names)
	}
}

// TestMonitorStreamJSON keeps the machine-readable half symmetrical with
// `monitor events --json`: one object per line, each carrying schema_version.
func TestMonitorStreamJSON(t *testing.T) {
	streamDaemon(t, []monitor.Event{
		{ID: 7, Kind: monitor.KindReviewComment, Target: "o/r#7", Title: "please rename this"},
	})

	code, stdout, _ := runCLI("monitor", "stream", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	var got struct {
		SchemaVersion int          `json:"schema_version"`
		ID            uint64       `json:"id"`
		Kind          monitor.Kind `json:"kind"`
	}
	line := strings.SplitN(stdout, "\n", 2)[0]
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("--json did not emit one object per line: %v\n%s", err, stdout)
	}
	if got.SchemaVersion != monitorSchemaVersion || got.ID != 7 || got.Kind != monitor.KindReviewComment {
		t.Errorf("event = %+v, want the daemon's review comment with the schema handle", got)
	}
}

// TestMonitorStreamStartsANewCursorAtThePresent is the difference between a
// notification and a history lesson. A stream's identity is derived from its
// working tree, so the first stream in a fresh checkout — a new repo, a new git
// worktree — is a consumer the daemon has never seen, and a cursorless read is
// served from the head of the retained journal: yesterday's failures on another
// branch, delivered as this session's opening notification.
func TestMonitorStreamStartsANewCursorAtThePresent(t *testing.T) {
	d := streamDaemon(t, []monitor.Event{
		{ID: 9, Kind: monitor.KindCIFailed, Target: "o/r#7", Title: "build failed"},
	})

	if code, _, stderr := runCLI("monitor", "stream"); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
	}

	var seek *monitor.Request
	for i, r := range d.seen() {
		if r.Op == monitor.OpEvents {
			if seek == nil {
				t.Fatalf("request %d read events before seeding the cursor; a first-ever stream would be handed the retained journal", i)
			}
			break
		}
		if r.Op == monitor.OpSeek {
			seek = &d.seen()[i]
		}
	}
	if seek == nil {
		t.Fatal("the stream never seeded its cursor")
	}
	if !seek.IfNew {
		t.Error("the stream seeked unconditionally; a restarted one would skip everything published while it was down")
	}
	if !strings.HasPrefix(seek.Consumer, "stream:") {
		t.Errorf("seek consumer = %q, want the stream's own identity", seek.Consumer)
	}
	for _, r := range d.seen() {
		if r.Op == monitor.OpEvents && r.Consumer != seek.Consumer {
			t.Errorf("read under %q but seeded %q; the seed did not move the cursor it reads from", r.Consumer, seek.Consumer)
		}
	}
}

// TestMonitorStreamJSONStandsDownInJSON keeps the --json feed parseable to its
// last line. The stand-down is the one line a consumer most needs to branch on —
// the feed has ended, and why — so emitting the prose form there hands a decoder
// a parse error instead.
func TestMonitorStreamJSONStandsDownInJSON(t *testing.T) {
	t.Run("mid-stream, after the daemon goes away", func(t *testing.T) {
		streamDaemon(t, []monitor.Event{
			{ID: 7, Kind: monitor.KindCIFailed, Target: "o/r#7", Title: "build failed"},
		})

		code, stdout, _ := runCLI("monitor", "stream", "--json")
		if code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		lines := strings.Split(strings.TrimSpace(stdout), "\n")
		for i, line := range lines {
			var doc map[string]any
			if err := json.Unmarshal([]byte(line), &doc); err != nil {
				t.Fatalf("line %d of a --json feed is not JSON: %v\n%s", i+1, err, line)
			}
		}
		var last struct {
			SchemaVersion int    `json:"schema_version"`
			Streaming     bool   `json:"streaming"`
			Reason        string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
			t.Fatal(err)
		}
		if last.Streaming || last.SchemaVersion != monitorSchemaVersion || !strings.Contains(last.Reason, "the monitor went away") {
			t.Errorf("the terminal line = %+v, want a schema-stamped stand-down naming the reason", last)
		}
	})

	t.Run("before it ever streams", func(t *testing.T) {
		monitorHome(t)
		noDaemonClient(t)
		t.Setenv("GITHUB_TOKEN", "")
		t.Setenv("GH_TOKEN", "")

		code, stdout, stderr := runCLI("monitor", "stream", "--json")
		if code != 0 {
			t.Fatalf("exit = %d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
		}
		var doc struct {
			Streaming bool   `json:"streaming"`
			Reason    string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &doc); err != nil {
			t.Fatalf("a start-up failure under --json is not JSON: %v\n%s", err, stdout)
		}
		if doc.Streaming || !strings.Contains(doc.Reason, "no GitHub token") {
			t.Errorf("document = %+v, want it to say the stream is not running and why", doc)
		}
	})
}

// TestMonitorStreamHonorsACallerSuppliedConsumer covers the escape hatch for a
// host that would rather name its own cursor.
func TestMonitorStreamHonorsACallerSuppliedConsumer(t *testing.T) {
	d := streamDaemon(t)
	t.Setenv("SHUCK_MONITOR_CONSUMER", "notifier-1")

	if code, _, _ := runCLI("monitor", "stream"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, r := range d.seen() {
		if r.Op == monitor.OpEvents && r.Consumer != "notifier-1" {
			t.Errorf("consumer = %q, want the caller's own", r.Consumer)
		}
	}
}

// TestMonitorStreamRespectsTheGlobalOptOut is the same opt-out the hooks honor,
// reaching the loudest half of the integration. "No monitor in this session"
// has to mean no notifications either — and it has to cost nothing, so the
// daemon is never dialed at all.
func TestMonitorStreamRespectsTheGlobalOptOut(t *testing.T) {
	d := streamDaemon(t, []monitor.Event{{ID: 1, Kind: monitor.KindCIFailed, Title: "build failed"}})
	t.Setenv("SHUCK_MONITOR_DISABLE", "1")

	code, stdout, stderr := runCLI("monitor", "stream")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if stdout != "" || stderr != "" {
		t.Errorf("stdout = %q stderr = %q, want silence", stdout, stderr)
	}
	if n := len(d.seen()); n != 0 {
		t.Errorf("made %d calls, want none — an opted-out stream registers nothing", n)
	}
	if names := streamMarkers(t); len(names) != 0 {
		t.Errorf("an opted-out stream claimed %v, which would silence the hooks too", names)
	}
}

// TestMonitorStreamNeedsNoTokenWhenADaemonIsRunning is the failure this change
// was written for: a session's plugin monitor stood down saying "no GitHub
// token found" while the daemon it was about to connect to was polling GitHub
// quite happily. The daemon holds the credential; the stream only reads from
// it, and a stand-down costs the session every notification it would have had.
func TestMonitorStreamNeedsNoTokenWhenADaemonIsRunning(t *testing.T) {
	streamDaemon(t, []monitor.Event{
		{ID: 1, Kind: monitor.KindCIFailed, Target: "o/r#7", Title: "build failed", Body: "boom"},
	})
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")

	code, stdout, stderr := runCLI("monitor", "stream")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if strings.Contains(stdout, "no GitHub token") {
		t.Errorf("the stream stood down over a token the daemon already has:\n%s", stdout)
	}
	if !strings.Contains(stdout, "build failed") {
		t.Errorf("the event never reached the session:\n%s", stdout)
	}
}

// TestMonitorStreamStandsDownOnAStartupFailure covers every way the command can
// fail before it has anything to stream. Each one is a sentence and exit 0: a
// notification is no place for a stack trace, and a session whose stream never
// started still has its hooks.
func TestMonitorStreamStandsDownOnAStartupFailure(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T)
		want  string
	}{
		{
			name: "no GitHub token to poll with",
			setup: func(t *testing.T) {
				monitorHome(t)
				noDaemonClient(t)
				t.Setenv("GITHUB_TOKEN", "")
				t.Setenv("GH_TOKEN", "")
			},
			want: "no GitHub token",
		},
		{
			name: "no daemon, and none could be started",
			setup: func(t *testing.T) {
				monitorHome(t)
				noDaemonClient(t)
				t.Setenv("GITHUB_TOKEN", "test-token")
			},
			want: "no shuck monitor is running",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setup(t)

			code, stdout, stderr := runCLI("monitor", "stream")
			if code != 0 {
				t.Fatalf("exit = %d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want the reason on stdout where it becomes one notification", stderr)
			}
			if lines := strings.Count(strings.TrimSpace(stdout), "\n"); lines != 0 {
				t.Errorf("stood down over %d lines, want one:\n%s", lines+1, stdout)
			}
			if !strings.Contains(stdout, tt.want) {
				t.Errorf("stdout = %q, want it to name the problem (%q)", stdout, tt.want)
			}
			if !strings.Contains(stdout, "shuck logs") {
				t.Errorf("stdout = %q, want it to say what to do instead", stdout)
			}
			if names := streamMarkers(t); len(names) != 0 {
				t.Errorf("a stream that never started claimed %v, which would silence the hooks as well", names)
			}
		})
	}
}

// TestMonitorStatusJSONWhenNotRunning covers the one answer a --json consumer
// most needs to branch on. `--no-start` with no daemon is a legitimate reply,
// not a failure, so it exits 0 — but it used to print a bare line of prose on
// stdout regardless of --json, which lands in a parser as a syntax error at the
// exact moment the caller is trying to find out whether the monitor is up.
func TestMonitorStatusJSONWhenNotRunning(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	t.Setenv("GITHUB_TOKEN", "test-token")

	code, stdout, stderr := runCLI("monitor", "status", "--json", "--no-start")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — 'not running' is an answer; stderr=%q", code, stderr)
	}

	var got struct {
		Running *bool `json:"running"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("--json emitted something unparseable: %v\n%s", err, stdout)
	}
	if got.Running == nil {
		t.Fatalf("the JSON answer must carry `running` so a consumer can branch on it:\n%s", stdout)
	}
	if *got.Running {
		t.Errorf("running = true with no daemon:\n%s", stdout)
	}
}

// TestMonitorStatusPlainWhenNotRunning keeps the human-facing half of the same
// path: without --json it stays a readable line, not a JSON document.
func TestMonitorStatusPlainWhenNotRunning(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	t.Setenv("GITHUB_TOKEN", "test-token")

	code, stdout, _ := runCLI("monitor", "status", "--no-start")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "not running") {
		t.Errorf("stdout = %q, want a plain-language answer", stdout)
	}
	if strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Errorf("the default view must stay prose, got JSON:\n%s", stdout)
	}
}

// TestMonitorEventsWaitOutlivesTheDaemonCap is the whole no-polling story in
// one test. The daemon caps a single blocking read at monitor.MaxWait, so the
// `--wait 30m` every doc tells an agent to run used to come back "nothing new"
// after ten minutes — exit 0, same output as a genuinely quiet half hour, so
// the caller cannot tell the two apart and walks off with CI still red.
//
// The fake daemon here answers the first read empty (standing in for the cap
// expiring) and the second with the failure that landed in between. One read,
// or a read asking for more than the daemon will honor, is the bug.
func TestMonitorEventsWaitOutlivesTheDaemonCap(t *testing.T) {
	round := 0
	d := startFakeDaemon(t, func(monitor.Request) monitor.Response {
		round++
		if round == 1 {
			return monitor.Response{OK: true, Cursor: 0}
		}
		return monitor.Response{OK: true, Cursor: 1, Events: []monitor.Event{
			{ID: 1, Kind: monitor.KindCIFailed, Target: "o/r#7", Title: "build failed", Body: "boom"},
		}}
	})

	code, stdout, stderr := runCLI("monitor", "events", "--wait", "25m")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "build failed") {
		t.Errorf("stdout = %q, want the failure that arrived after the daemon's cap expired", stdout)
	}
	if strings.Contains(stdout, "nothing new") {
		t.Errorf("stdout = %q, want the wait to outlive one capped read", stdout)
	}

	reqs := d.seen()
	if len(reqs) != 2 {
		t.Fatalf("made %d reads, want 2 — one read means --wait stops at the daemon's cap, not the caller's deadline", len(reqs))
	}
	if reqs[0].Wait > monitor.MaxWait {
		t.Errorf("first read asked for %s; the daemon silently truncates anything over %s, so asking for more is asking to be lied to",
			reqs[0].Wait, monitor.MaxWait)
	}
	if reqs[0].Wait != monitor.MaxWait {
		t.Errorf("first read asked for %s, want the daemon's full %s — a shorter one turns a wait into a poll loop",
			reqs[0].Wait, monitor.MaxWait)
	}
}

// TestMonitorEventsWaitStopsAtTheDeadline pins the other half of the loop: a
// wait that expires with nothing to show has to end, and end quietly. Re-asking
// is only safe because the caller's own deadline bounds it — without that check
// the command never returns, which is a far worse failure than the capped read
// it replaced.
func TestMonitorEventsWaitStopsAtTheDeadline(t *testing.T) {
	// The real daemon blocks for the wait it is handed, so a round trip costs
	// what was asked for. This fake answers instantly, so it paces itself
	// instead — otherwise the loop's own dial rate, not the deadline, would be
	// what the test measured.
	d := startFakeDaemon(t, func(monitor.Request) monitor.Response {
		time.Sleep(5 * time.Millisecond)
		return monitor.Response{OK: true, Cursor: 0}
	})

	done := make(chan int, 1)
	var stdout string
	go func() {
		code, out, _ := runCLI("monitor", "events", "--wait", "80ms")
		stdout = out
		done <- code
	}()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit = %d, want 0 — a quiet wait is an answer, not a failure", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("`monitor events --wait` never returned; the deadline does not bound the loop")
	}
	if !strings.Contains(stdout, "nothing new") {
		t.Errorf("stdout = %q, want an expired wait to say the feed was empty", stdout)
	}
	if n := len(d.seen()); n < 2 {
		t.Errorf("made %d reads, want more than one — the wait outlasted a single empty read", n)
	}
}

// TestMonitorEventsFollowRejectsWait covers the combination that used to be
// accepted and ignored: monitorFollow overwrites Wait with its own interval, so
// `--follow --wait 30m` ran forever while reading as if it would stop.
func TestMonitorEventsFollowRejectsWait(t *testing.T) {
	// The daemon answers with a fault so that a build which still accepts the
	// combination ends the follow instead of hanging the test out to its
	// timeout — the failure should read as "the flags were accepted", not as a
	// dead package.
	d := startFakeDaemon(t, func(monitor.Request) monitor.Response {
		return monitor.Response{Error: "journal is unreadable"}
	})

	code, stdout, stderr := runCLI("monitor", "events", "--follow", "--wait", "30m")
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stdout=%q", code, stdout)
	}
	if !strings.Contains(stderr, "--wait") || !strings.Contains(stderr, "--follow") {
		t.Errorf("stderr = %q, want both flags named so the caller knows which to drop", stderr)
	}
	if n := len(d.seen()); n != 0 {
		t.Errorf("made %d reads, want 0 — a rejected combination must not reach the daemon", n)
	}
}

// TestMonitorJSONIsRegisteredOnlyWhereItIsRead is the flag-surface contract.
// unwatch, poke and stop each print one sentence and have no document to emit,
// so accepting --json there means `shuck monitor unwatch --json | jq` gets
// prose and exit 0 — a silent lie that no amount of downstream parsing can
// detect. Rejecting the flag is the only honest answer.
func TestMonitorJSONIsRegisteredOnlyWhereItIsRead(t *testing.T) {
	for _, sub := range []string{"unwatch", "poke", "stop"} {
		t.Run("rejected on "+sub, func(t *testing.T) {
			monitorHome(t)
			noDaemonClient(t)

			code, stdout, stderr := runCLI("monitor", sub, "--json")
			if code != 2 {
				t.Fatalf("exit = %d, want 2 — %s has no JSON shape to emit; stdout=%q", code, sub, stdout)
			}
			if !strings.Contains(stderr, "not defined: -json") {
				t.Errorf("stderr = %q, want the flag named as unknown", stderr)
			}
			if strings.Contains(stdout, "stopped") || strings.Contains(stdout, "not running") {
				t.Errorf("stdout = %q, want nothing done for a rejected flag", stdout)
			}
		})
	}

	for _, sub := range []string{"status", "watch", "events"} {
		t.Run("still offered on "+sub, func(t *testing.T) {
			monitorHome(t)
			noDaemonClient(t)

			_, _, stderr := runCLI("monitor", sub, "--json")
			if strings.Contains(stderr, "not defined: -json") {
				t.Errorf("%s dropped --json, which it does honor: %q", sub, stderr)
			}
		})
	}
}

// TestMonitorStatusJSONCarriesSchemaVersion pins the version handle every other
// shuck --json document has. It lives in the CLI's envelope, not in
// monitor.Status: that type is the daemon's IPC payload, and versioning it
// would version the wire format between two ends that ship together.
func TestMonitorStatusJSONCarriesSchemaVersion(t *testing.T) {
	startFakeDaemon(t, func(monitor.Request) monitor.Response {
		return monitor.Response{OK: true, Status: &monitor.Status{Running: true, PID: 4242, Version: "v9.9.9"}}
	})

	code, stdout, stderr := runCLI("monitor", "status", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}

	var got struct {
		SchemaVersion int    `json:"schema_version"`
		PID           int    `json:"pid"`
		Version       string `json:"version"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if got.SchemaVersion != monitorSchemaVersion {
		t.Errorf("schema_version = %d, want %d", got.SchemaVersion, monitorSchemaVersion)
	}
	// The envelope wraps without nesting: a consumer already reading .pid must
	// keep reading it there.
	if got.PID != 4242 || got.Version != "v9.9.9" {
		t.Errorf("the status fields moved: %+v\n%s", got, stdout)
	}
}

// TestMonitorStatusJSONNotRunningCarriesSchemaVersion covers the answer a
// consumer most needs to branch on, which is built by the CLI rather than the
// daemon and so has its own path to the envelope.
func TestMonitorStatusJSONNotRunningCarriesSchemaVersion(t *testing.T) {
	monitorHome(t)
	noDaemonClient(t)

	code, stdout, _ := runCLI("monitor", "status", "--json", "--no-start")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	var got struct {
		SchemaVersion int   `json:"schema_version"`
		Running       *bool `json:"running"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if got.SchemaVersion != monitorSchemaVersion {
		t.Errorf("schema_version = %d, want %d", got.SchemaVersion, monitorSchemaVersion)
	}
	if got.Running == nil || *got.Running {
		t.Errorf("running = %v, want false:\n%s", got.Running, stdout)
	}
}

// TestMonitorEventsJSONCarriesSchemaVersionPerLine pins where the version goes
// in a line-delimited feed: on every line. A consumer that tails the feed, or
// reads a single line out of a pipe, never sees a header.
func TestMonitorEventsJSONCarriesSchemaVersionPerLine(t *testing.T) {
	startFakeDaemon(t, func(monitor.Request) monitor.Response {
		return monitor.Response{OK: true, Cursor: 2, Events: []monitor.Event{
			{ID: 1, Kind: monitor.KindCIFailed, Target: "o/r#7", Title: "build failed", Body: "boom"},
			{ID: 2, Kind: monitor.KindCIPassed, Target: "o/r#7", Title: "checks are green"},
		}}
	})

	code, stdout, stderr := runCLI("monitor", "events", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}

	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want one per event:\n%s", len(lines), stdout)
	}
	for i, line := range lines {
		var got struct {
			SchemaVersion int          `json:"schema_version"`
			ID            uint64       `json:"id"`
			Kind          monitor.Kind `json:"kind"`
			Body          string       `json:"body"`
		}
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d is not JSON: %v\n%s", i+1, err, line)
		}
		if got.SchemaVersion != monitorSchemaVersion {
			t.Errorf("line %d schema_version = %d, want %d", i+1, got.SchemaVersion, monitorSchemaVersion)
		}
		if got.ID != uint64(i+1) {
			t.Errorf("line %d id = %d, want %d — the envelope must not reorder or nest the events", i+1, got.ID, i+1)
		}
	}
	// The event's own fields stay at the top level, where a consumer reads them.
	if !strings.Contains(lines[0], `"kind":"ci.failed"`) || !strings.Contains(lines[0], `"body":"boom"`) {
		t.Errorf("the event fields were nested away:\n%s", lines[0])
	}
}

// TestMonitorWatchJSONCarriesSchemaVersion pins the last monitor document that
// went out bare. status and events both carry the handle, so a consumer that
// branches on schema_version used to get nothing from watch alone — the worst
// of both choices.
func TestMonitorWatchJSONCarriesSchemaVersion(t *testing.T) {
	startFakeDaemon(t, func(req monitor.Request) monitor.Response {
		if req.Op != monitor.OpWatch || req.Watch == nil {
			return monitor.Response{Error: "unexpected request"}
		}
		stored := *req.Watch
		return monitor.Response{OK: true, Watch: &stored}
	})
	t.Setenv("GITHUB_TOKEN", "test-token")

	code, stdout, stderr := runCLI("monitor", "watch", "--json", "o/r#7")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}

	var got struct {
		SchemaVersion int               `json:"schema_version"`
		ID            string            `json:"id"`
		Kind          monitor.WatchKind `json:"kind"`
		Number        int               `json:"number"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if got.SchemaVersion != monitorSchemaVersion {
		t.Errorf("schema_version = %d, want %d", got.SchemaVersion, monitorSchemaVersion)
	}
	// The envelope wraps without nesting: `.id` is the identity the daemon keys
	// the watch on, so it has to stay where a consumer already reads it.
	if got.ID != monitor.PRWatchID("o", "r", 7) || got.Kind != monitor.WatchPR || got.Number != 7 {
		t.Errorf("the watch fields moved or changed: %+v\n%s", got, stdout)
	}
	// --json replaces the text view rather than joining it.
	if strings.Contains(stdout, "watching ") {
		t.Errorf("--json emitted the text line too:\n%s", stdout)
	}
}

// TestMonitorWatchWithNoRecordReturned covers a daemon that answers OK without
// the watch it stored — nothing this binary's daemon does, but an older one on
// the socket might. The registration went through either way, so the text line
// still reports success; --json cannot, because the only honest document is one
// it does not have. Emitting a zero-valued Watch instead would hand a consumer
// an empty id and a year-1 timestamp that read as a real record.
func TestMonitorWatchWithNoRecordReturned(t *testing.T) {
	for _, tt := range []struct {
		name     string
		args     []string
		wantCode int
	}{
		{"text says it is watching", []string{"monitor", "watch", "o/r#7"}, 0},
		{"json refuses to invent one", []string{"monitor", "watch", "--json", "o/r#7"}, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			startFakeDaemon(t, func(monitor.Request) monitor.Response {
				return monitor.Response{OK: true}
			})
			t.Setenv("GITHUB_TOKEN", "test-token")

			code, stdout, stderr := runCLI(tt.args...)
			if code != tt.wantCode {
				t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", code, tt.wantCode, stdout, stderr)
			}
			if tt.wantCode == 0 {
				if !strings.Contains(stdout, "watching") {
					t.Errorf("stdout = %q, want the registration reported", stdout)
				}
				return
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want nothing where a document should be", stdout)
			}
			if !strings.Contains(stderr, "no record") {
				t.Errorf("stderr = %q, want the missing record named", stderr)
			}
		})
	}
}

// TestMonitorRunDetachedReportsAMissingTokenToTheLog covers the failure a user
// actually hits: no token, so `shuck monitor` spawns a daemon that dies. The
// client points them at the monitor's log, and the daemon used to resolve the
// token before opening it — so the reason went to a stderr the spawn discarded
// and the log the message named was never created.
func TestMonitorRunDetachedReportsAMissingTokenToTheLog(t *testing.T) {
	monitorHome(t)
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")

	code, _, _ := runCLI("monitor", "run", "--detached")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 with no token", code)
	}

	dir, err := monitor.Dir()
	if err != nil {
		t.Fatalf("monitor.Dir: %v", err)
	}
	logged, err := os.ReadFile(filepath.Join(dir, "daemon.log"))
	if err != nil {
		t.Fatalf("the daemon's log is where a user is sent to read this, and nothing created it: %v", err)
	}
	if !strings.Contains(string(logged), "no GitHub token") {
		t.Errorf("daemon.log = %q, want the reason the monitor would not start", logged)
	}
}

// TestEventsWithinStopsOnInterrupt keeps Ctrl-C responsive through the re-ask
// loop. A `--wait 30m` that ignored the interruption would hold the terminal
// for up to another ten minutes per round, which is the loop turning a fix into
// a worse bug than the cap it removed. interruptedCtx leaves the daemon
// reachable and answering, so only the loop's own check can end this.
func TestEventsWithinStopsOnInterrupt(t *testing.T) {
	d := startFakeDaemon(t, func(monitor.Request) monitor.Response {
		return monitor.Response{OK: true, Cursor: 0}
	})
	client, err := newMonitorClient()
	if err != nil {
		t.Fatalf("newMonitorClient: %v", err)
	}

	done := make(chan []monitor.Event, 1)
	go func() {
		events, err := eventsWithin(interruptedCtx{context.Background()}, client, monitor.Request{Consumer: "cli", Wait: 30 * time.Minute})
		if err != nil {
			t.Errorf("eventsWithin: %v", err)
		}
		done <- events
	}()
	select {
	case events := <-done:
		if len(events) != 0 {
			t.Errorf("events = %+v, want none — the feed was empty when the wait was interrupted", events)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("eventsWithin ignored the interruption and kept waiting")
	}
	if n := len(d.seen()); n != 1 {
		t.Errorf("made %d reads, want 1 — an interrupted wait must not re-ask", n)
	}
}

// TestMonitorStreamRecordsWhereTheSessionBegan is the stream's half of the
// contract the Stop hook depends on.
//
// No hook runs at session start any more, so the first one to see a session id
// can be an hour into the conversation — and a cursor seeded *then* steps over
// whatever landed during the first turn, which is precisely what the Stop gate
// exists to catch. The stream is started and killed with the session, so where
// it began reading is where the session began; recording that on its marker is
// the only durable trace of the moment, and the hooks read it back through
// monitor.StreamOrigin.
//
// The marker is read while the stream is still live, since it retires it on the
// way out.
func TestMonitorStreamRecordsWhereTheSessionBegan(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu     sync.Mutex
		origin uint64
		known  bool
	)
	startFakeDaemon(t, func(req monitor.Request) monitor.Response {
		switch req.Op {
		case monitor.OpWatch:
			stored := *req.Watch
			return monitor.Response{OK: true, Watch: &stored}
		case monitor.OpSeek:
			// The journal was at 41 when this session opened.
			return monitor.Response{OK: true, Cursor: 41}
		default:
			mu.Lock()
			origin, known = monitor.StreamOrigin(cwd)
			mu.Unlock()
			return monitor.Response{Error: "the monitor went away"}
		}
	})

	if code, _, stderr := runCLI("monitor", "stream"); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}

	mu.Lock()
	defer mu.Unlock()
	if !known {
		t.Fatal("the stream recorded no origin; a hook seeding this session later would seed it to the present and lose the turn's own failure")
	}
	if origin != 41 {
		t.Errorf("origin = %d, want the cursor the stream started from (41)", origin)
	}
}

// TestMonitorStreamScopesItsReadsToThisTree pins the client half of per-session
// scoping. The daemon filters by Request.Scope and a stream that sends none is
// handed every watch's events — which is exactly the reported defect: a session
// in one worktree receiving three other worktrees' CI and pin findings. Nothing
// the stream prints reveals whether the field was sent, so only the request log
// can tell.
func TestMonitorStreamScopesItsReadsToThisTree(t *testing.T) {
	d := streamDaemon(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	if code, _, stderr := runCLI("monitor", "stream"); code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}

	reads := 0
	for _, r := range d.seen() {
		if r.Op != monitor.OpEvents {
			continue
		}
		reads++
		if r.Scope != cwd {
			t.Errorf("scope = %q, want this working tree %q", r.Scope, cwd)
		}
	}
	if reads == 0 {
		t.Fatal("the stream made no reads to check")
	}
	// The watch has to carry the same tree, or the daemon has nothing to match
	// the scope against.
	if w := d.seen()[0].Watch; w == nil || !slices.Contains(w.Scopes, cwd) {
		t.Errorf("registered %+v, want it scoped to %q", w, cwd)
	}
}

// TestMonitorEventsSendsNoScope is the other half of the same contract, and it
// is deliberate rather than an omission: `shuck monitor events` is how somebody
// debugging the monitor sees the whole feed, so it must stay unfiltered.
func TestMonitorEventsSendsNoScope(t *testing.T) {
	d := startFakeDaemon(t, func(monitor.Request) monitor.Response {
		return monitor.Response{OK: true, Cursor: 1}
	})

	if code, _, stderr := runCLI("monitor", "events"); code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	reqs := d.seen()
	if len(reqs) != 1 {
		t.Fatalf("made %d calls, want exactly 1", len(reqs))
	}
	if reqs[0].Scope != "" {
		t.Errorf("scope = %q, want the CLI read left unfiltered", reqs[0].Scope)
	}
}
