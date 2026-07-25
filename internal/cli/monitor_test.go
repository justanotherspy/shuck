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
	for _, want := range []string{"unknown subcommand", "shuck monitor watch", "shuck monitor events"} {
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

	// The daemon a client starts inherits this environment, so a missing token
	// is reported here — in front of the person who can fix it — rather than
	// silently inside a background process.
	code, _, stderr := runCLI("monitor", "watch", "o/r#1")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "no GitHub token") {
		t.Errorf("stderr = %q, want the missing token named", stderr)
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
		var out, errBuf bytes.Buffer
		if code := emitEvents(&out, &errBuf, events, false, true); code != 0 {
			t.Fatalf("exit = %d", code)
		}
		if !strings.Contains(out.String(), "test failed") || !strings.Contains(out.String(), "boom") {
			t.Errorf("stdout = %q", out.String())
		}
	})

	t.Run("json is one object per line", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		if code := emitEvents(&out, &errBuf, events, true, true); code != 0 {
			t.Fatalf("exit = %d", code)
		}
		if lines := strings.Count(strings.TrimSpace(out.String()), "\n"); lines != 0 {
			t.Errorf("one event should be one line, got %d newlines", lines+1)
		}
		if !strings.Contains(out.String(), `"kind":"ci.failed"`) {
			t.Errorf("stdout = %q", out.String())
		}
	})

	t.Run("nothing new", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		emitEvents(&out, &errBuf, nil, false, true)
		if !strings.Contains(out.String(), "nothing new") {
			t.Errorf("stdout = %q", out.String())
		}

		out.Reset()
		emitEvents(&out, &errBuf, nil, false, false)
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

	code, stdout, stderr := runCLI("monitor", "events", "--follow", "--all", "--wait", "1ms", "--limit", "5", "--consumer", "session-9")
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
		// Following overrides the one-shot flags: --all would re-deliver the
		// whole journal on every pass, and the caller's 1ms would turn a quiet
		// feed into a hot loop of dials.
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
