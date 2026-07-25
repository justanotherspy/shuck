package monitor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// serveTestDaemon starts a daemon on a real socket in a temp directory and
// returns a client pointed at it. It is the end-to-end path — a client
// process talks to a daemon process this way — exercised in one test binary.
func serveTestDaemon(t *testing.T, c prClient) (*Daemon, *Client) {
	t.Helper()
	dir := t.TempDir()

	original := newPRClient
	newPRClient = func(string) prClient { return c }
	t.Cleanup(func() { newPRClient = original })

	d, err := newDaemon(dir, Options{Version: "test", NoPins: true, WatchTTL: -1})
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := d.serve(ctx); err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the daemon did not shut down")
		}
	})

	client := &Client{dir: dir} // AutoStart off: this daemon is already up
	waitFor(t, func() bool { return client.Running(context.Background()) })
	return d, client
}

// waitFor polls cond until it holds or the test gives up.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never held")
}

func TestServeRoundTrip(t *testing.T) {
	c := newFakeClient()
	c.pr = openPR("abc1234def")
	c.fingerprint = "fp-1"
	d, client := serveTestDaemon(t, c)

	ctx := context.Background()

	t.Run("ping", func(t *testing.T) {
		resp, err := client.Do(ctx, Request{Op: OpPing})
		if err != nil {
			t.Fatal(err)
		}
		if resp.Version != "test" {
			t.Errorf("Version = %q, want test", resp.Version)
		}
	})

	t.Run("watch and status", func(t *testing.T) {
		w, err := client.Watch(ctx, Watch{ID: "pr:o/r#7", Kind: WatchPR, Owner: "o", Repo: "r", Number: 7})
		if err != nil {
			t.Fatal(err)
		}
		if w.ID != "pr:o/r#7" {
			t.Errorf("stored watch = %+v", w)
		}

		st, err := client.Status(ctx, "session-a")
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Watches) != 1 {
			t.Fatalf("%d watches in status, want 1", len(st.Watches))
		}
		if st.RateLimit != 5000 {
			t.Errorf("RateLimit = %d, want the fake's 5000", st.RateLimit)
		}
		if st.PID == 0 || st.Version != "test" {
			t.Errorf("status is missing identity: %+v", st)
		}
	})

	t.Run("events are delivered once per consumer", func(t *testing.T) {
		d.publish([]Event{{Kind: KindCIFailed, Title: "red"}})

		events, cursor, err := client.Events(ctx, Request{Consumer: "session-a"})
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 || events[0].Title != "red" {
			t.Fatalf("got %d events, want the failure", len(events))
		}
		if cursor == 0 {
			t.Error("the reply should carry the journal's cursor")
		}

		again, _, err := client.Events(ctx, Request{Consumer: "session-a"})
		if err != nil {
			t.Fatal(err)
		}
		if len(again) != 0 {
			t.Errorf("the same consumer was served %d events twice", len(again))
		}
	})

	t.Run("peek leaves the cursor alone", func(t *testing.T) {
		d.publish([]Event{{Kind: KindReviewComment, Title: "alice commented"}})

		peeked, _, err := client.Events(ctx, Request{Consumer: "session-a", Peek: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(peeked) != 1 {
			t.Fatalf("peeked %d events, want 1", len(peeked))
		}
		// The Stop hook depends on this: an event it decides not to act on
		// must still be there for the next prompt.
		drained, _, err := client.Events(ctx, Request{Consumer: "session-a"})
		if err != nil {
			t.Fatal(err)
		}
		if len(drained) != 1 {
			t.Errorf("a peek consumed the event: drained %d, want 1", len(drained))
		}
	})

	t.Run("all re-reads the journal", func(t *testing.T) {
		events, _, err := client.Events(ctx, Request{Consumer: "session-a", All: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(events) < 2 {
			t.Errorf("--all returned %d events, want the whole journal", len(events))
		}
	})

	t.Run("seek fast-forwards", func(t *testing.T) {
		d.publish([]Event{{Kind: KindCIPassed, Title: "green"}})
		cursor, err := client.Seek(ctx, "session-b")
		if err != nil {
			t.Fatal(err)
		}
		if cursor == 0 {
			t.Error("Seek should report where the cursor landed")
		}
		events, _, err := client.Events(ctx, Request{Consumer: "session-b"})
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 0 {
			t.Errorf("a session that just seeked was served %d events, want 0", len(events))
		}
	})

	t.Run("poke", func(t *testing.T) {
		msg, err := client.Poke(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(msg, "immediate") {
			t.Errorf("message = %q", msg)
		}
	})

	t.Run("unwatch", func(t *testing.T) {
		if err := client.Unwatch(ctx, "pr:o/r#7"); err != nil {
			t.Fatal(err)
		}
		if err := client.Unwatch(ctx, "pr:o/r#7"); err == nil {
			t.Error("unwatching something not watched should report it")
		}
	})
}

// TestServeEventsWait covers the blocking read an agent uses to wait for CI
// without polling.
func TestServeEventsWait(t *testing.T) {
	c := newFakeClient()
	d, client := serveTestDaemon(t, c)

	go func() {
		time.Sleep(50 * time.Millisecond)
		d.publish([]Event{{Kind: KindCIPassed, Title: "all checks passed"}})
	}()

	start := time.Now()
	events, _, err := client.Events(context.Background(), Request{Consumer: "waiter", Wait: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("waited and got %d events, want 1", len(events))
	}
	if time.Since(start) > 4*time.Second {
		t.Error("the wait should return as soon as the event lands, not at the timeout")
	}
}

func TestServeEventsWaitTimesOut(t *testing.T) {
	_, client := serveTestDaemon(t, newFakeClient())

	events, _, err := client.Events(context.Background(), Request{Consumer: "waiter", Wait: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("got %d events from an idle monitor, want 0", len(events))
	}
}

func TestServeRejectsBadRequests(t *testing.T) {
	_, client := serveTestDaemon(t, newFakeClient())
	ctx := context.Background()

	if _, err := client.Do(ctx, Request{Op: "nonsense"}); err == nil {
		t.Error("an unknown op should be rejected")
	}
	if _, err := client.Do(ctx, Request{Op: OpWatch}); err == nil {
		t.Error("a watch request with no watch should be rejected")
	}
	if _, err := client.Do(ctx, Request{Op: OpUnwatch}); err == nil {
		t.Error("an unwatch with no id should be rejected")
	}
}

func TestServeMalformedLine(t *testing.T) {
	_, client := serveTestDaemon(t, newFakeClient())

	ep, err := client.endpoint()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial(ep.Network, ep.Address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("{not json\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(buf[:n]), "malformed") {
		t.Errorf("reply = %q, want it to name the problem", buf[:n])
	}
}

func TestServeStopShutsDown(t *testing.T) {
	dir := t.TempDir()
	original := newPRClient
	newPRClient = func(string) prClient { return newFakeClient() }
	t.Cleanup(func() { newPRClient = original })

	d, err := newDaemon(dir, Options{Version: "test", NoPins: true, WatchTTL: -1})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.serve(context.Background()) }()

	client := &Client{dir: dir}
	waitFor(t, func() bool { return client.Running(context.Background()) })

	if err := client.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the daemon did not stop")
	}

	// The files that advertise a running daemon are cleaned up, so the next
	// client knows to start one.
	if _, err := os.Stat(newPaths(dir).endpoint); !os.IsNotExist(err) {
		t.Error("the endpoint file should be removed on shutdown")
	}
}

// TestListenIsTheLock covers single-instance behavior: the listener itself is
// the lock, so a second daemon cannot bind the same socket.
func TestListenIsTheLock(t *testing.T) {
	p := newPaths(t.TempDir())

	ln, ep, err := listen(p)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if ep.Network != "unix" {
		t.Skipf("no unix socket available (%s); the lock story is platform-specific", ep.Network)
	}

	if _, _, err := listen(p); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("second listen = %v, want ErrAlreadyRunning", err)
	}
}

// TestListenClearsAStaleSocket covers the crash case: a socket file with
// nothing behind it must not block the next daemon forever.
func TestListenClearsAStaleSocket(t *testing.T) {
	p := newPaths(t.TempDir())

	ln, ep, err := listen(p)
	if err != nil {
		t.Fatal(err)
	}
	if ep.Network != "unix" {
		ln.Close()
		t.Skip("no unix socket available")
	}
	// Close the listener without removing the file, as a killed process would.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()

	if _, err := os.Stat(p.socket); err != nil {
		t.Skipf("the socket file did not survive the close: %v", err)
	}
	again, _, err := listen(p)
	if err != nil {
		t.Fatalf("a stale socket blocked a new daemon: %v", err)
	}
	again.Close()
}

// TestListenUnixLosesTheRace covers the two ways another daemon can beat this
// one to the socket after the stale-socket check said nobody was there. Both
// have to come back as errAlreadyServing: any other error sends listen() on to
// the loopback fallback, and two daemons polling GitHub and appending to one
// journal is far worse than one that declines to start.
//
// The races cannot be staged for real — the winner has to appear between our
// own two syscalls — so the dial is stubbed to fail once (nothing there yet)
// and answer afterwards (somebody bound it), while the filesystem makes the
// step in between fail for real.
func TestListenUnixLosesTheRace(t *testing.T) {
	tests := []struct {
		name string
		// path returns a socket path rigged so the step under test fails.
		path func(t *testing.T) string
	}{
		{
			// A path whose parent is gone: the bind fails and so does the
			// remove, which is the "somebody else already cleaned up and took
			// it" shape.
			name: "the remove fails",
			path: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "gone", "daemon.sock")
			},
		},
		{
			// A path too long for sun_path: the file is removable, so recovery
			// gets as far as the second bind, and that is what fails.
			name: "the second bind fails",
			path: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), strings.Repeat("n", 120)+".sock")
				if err := os.WriteFile(p, []byte("stale"), 0o600); err != nil {
					t.Fatal(err)
				}
				return p
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.path(t)
			if ln, err := net.Listen("unix", path); err == nil {
				ln.Close()
				t.Skip("this platform binds the rigged path; the race cannot be staged here")
			}

			original := socketServed
			calls := 0
			socketServed = func(string) bool {
				calls++
				return calls > 1 // nobody is there, then somebody is
			}
			t.Cleanup(func() { socketServed = original })

			ln, err := listenUnix(path)
			if ln != nil {
				ln.Close()
			}
			if !errors.Is(err, errAlreadyServing) {
				t.Errorf("listenUnix = %v, want errAlreadyServing so listen() reports ErrAlreadyRunning instead of starting a second daemon on TCP", err)
			}
		})
	}
}

func TestClientReportsNoDaemon(t *testing.T) {
	client := &Client{dir: t.TempDir()} // AutoStart off
	if _, err := client.Do(context.Background(), Request{Op: OpPing}); !errors.Is(err, ErrNotRunning) {
		t.Errorf("err = %v, want ErrNotRunning", err)
	}
	if client.Running(context.Background()) {
		t.Error("Running should be false with no daemon")
	}
	// Stop never starts one just to stop it.
	if err := client.Stop(context.Background()); err == nil {
		t.Error("stopping a monitor that is not running should report it")
	}
}

func TestClientRejectsAnUnusableEndpoint(t *testing.T) {
	dir := t.TempDir()
	if err := writeJSONFile(newPaths(dir).endpoint, endpoint{}); err != nil {
		t.Fatal(err)
	}
	client := &Client{dir: dir}
	if _, err := client.Do(context.Background(), Request{Op: OpPing}); !errors.Is(err, ErrNotRunning) {
		t.Errorf("err = %v, want ErrNotRunning for an endpoint file naming nothing", err)
	}
}

// titles reduces an event batch to what it delivered, for compact assertions.
func titles(events []Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Title)
	}
	return out
}

// TestDaemonDrainDeliveryPaths pins the three reads the journal serves, which
// together are the at-most-once delivery contract: a peek must leave a consumer
// exactly where it was, an explicit Since must beat the stored cursor, and a
// read with no session behind it must spend nobody's backlog.
func TestDaemonDrainDeliveryPaths(t *testing.T) {
	tests := []struct {
		name string
		// cursor is where "sess" sits before the read; 0 means it has seen
		// nothing.
		cursor uint64
		req    Request
		want   []string
		// wantCursor is where the read leaves the consumer it names. An
		// anonymous read has no cursor of its own, so it is wantOwed below —
		// what the next session-shaped read hands over — that pins its half of
		// the contract.
		wantCursor uint64
		// wantOwed is what a plain read as "sess" gets once the read under test
		// is done: the same contract stated as delivery rather than as state. A
		// read that consumed nothing must leave sess owed everything it was
		// owed before.
		wantOwed []string
	}{
		{
			name:       "a peek hands over the backlog without consuming it",
			req:        Request{Consumer: "sess", Peek: true},
			want:       []string{"one", "two", "three"},
			wantCursor: 0,
			wantOwed:   []string{"one", "two", "three"},
		},
		{
			name:       "a peek starts from the consumer's own cursor",
			cursor:     2,
			req:        Request{Consumer: "sess", Peek: true},
			want:       []string{"three"},
			wantCursor: 2,
			wantOwed:   []string{"three"},
		},
		{
			// The Stop hook peeks to decide whether to act; a caller that also
			// names a floor must get that floor, and still consume nothing.
			name:       "a peek takes an explicit since over the stored cursor",
			cursor:     3,
			req:        Request{Consumer: "sess", Peek: true, Since: 1},
			want:       []string{"two", "three"},
			wantCursor: 3,
		},
		{
			name:       "a peek respects the limit",
			req:        Request{Consumer: "sess", Peek: true, Limit: 1},
			want:       []string{"three"},
			wantCursor: 0,
			wantOwed:   []string{"one", "two", "three"},
		},
		{
			name:       "since overrides the cursor and drags the consumer forward",
			req:        Request{Consumer: "sess", Since: 1},
			want:       []string{"two", "three"},
			wantCursor: 3,
		},
		{
			name:       "since re-delivers what the consumer already saw, and leaves it caught up",
			cursor:     3,
			req:        Request{Consumer: "sess", Since: 1},
			want:       []string{"two", "three"},
			wantCursor: 3,
		},
		{
			// The cursor follows what was handed over, not the floor that was
			// asked for: seeking back to Since would hand this consumer the
			// same events again on its next read. "Handed over" and "the
			// journal's latest" are one event apart only under a concurrent
			// append — a capped batch keeps the newest — so nothing here can
			// tell those two apart, and nothing here should claim to.
			name:       "since with a limit still advances past what it delivered",
			req:        Request{Consumer: "sess", Since: 1, Limit: 1},
			want:       []string{"three"},
			wantCursor: 3,
		},
		{
			name:       "a since past the end delivers nothing and moves nothing",
			cursor:     1,
			req:        Request{Consumer: "sess", Since: 99},
			wantCursor: 1,
			wantOwed:   []string{"two", "three"},
		},
		{
			// `shuck monitor events --since` run from a shell: the events come
			// back, but the session sitting at cursor 1 is still owed them. A
			// read that spent them would leave that session never told about
			// the failure it swallowed.
			name:     "since without a consumer spends nobody's backlog",
			cursor:   1,
			req:      Request{Since: 1},
			want:     []string{"two", "three"},
			wantOwed: []string{"two", "three"},
		},
		{
			// The same anonymous read without a floor, which is the path
			// through journal.Drain rather than journal.Since: with no cursor
			// to start from it hands over the whole journal, and still
			// consumes none of it.
			name:     "a plain read without a consumer starts from the beginning and keeps it",
			cursor:   1,
			req:      Request{},
			want:     []string{"one", "two", "three"},
			wantOwed: []string{"two", "three"},
		},
		{
			name:       "the plain read drains and advances",
			cursor:     1,
			req:        Request{Consumer: "sess"},
			want:       []string{"two", "three"},
			wantCursor: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, _ := newTestDaemon(t, newFakeClient())
			d.publish([]Event{
				{Kind: KindCIFailed, Title: "one"},
				{Kind: KindCIPassed, Title: "two"},
				{Kind: KindReviewComment, Title: "three"},
			})
			if tt.cursor > 0 {
				d.journal.Seek("sess", tt.cursor)
			}

			got := titles(d.drain(tt.req))
			if !slices.Equal(got, tt.want) {
				t.Errorf("drain returned %v, want %v", got, tt.want)
			}

			// A read that consumes nothing must be repeatable — that is the
			// whole reason Stop peeks instead of draining, and the reason a
			// shell command cannot spend a session's backlog.
			if tt.req.Peek || tt.req.Consumer == "" {
				if again := titles(d.drain(tt.req)); !slices.Equal(again, got) {
					t.Errorf("the same read returned %v the second time, want %v", again, got)
				}
			}

			if tt.req.Consumer != "" {
				if cursor := d.journal.Cursor(tt.req.Consumer, 0); cursor != tt.wantCursor {
					t.Errorf("%s cursor = %d after the read, want %d", tt.req.Consumer, cursor, tt.wantCursor)
				}
			}

			// Read last, because this one consumes: what the session is still
			// owed is the only visible consequence of an anonymous read, and
			// the check that a peek really did leave the backlog alone.
			if owed := titles(d.drain(Request{Consumer: "sess"})); !slices.Equal(owed, tt.wantOwed) {
				t.Errorf("sess was owed %v after the read, want %v", owed, tt.wantOwed)
			}
		})
	}
}

// TestDaemonDrainPastTheRetainedWindow covers the reads that outlive the
// journal. The window is bounded and rotation drops the oldest events, so a
// session that has been away long enough — or a --since naming an event from
// last week — asks for events that are no longer there. The rule is that such a
// read gets the whole retained window and lands caught up: what aged out is
// gone, not replayed forever, and not a reason to hand back nothing.
func TestDaemonDrainPastTheRetainedWindow(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())

	// One event past the cap is what triggers a rotation: the window is trimmed
	// back to rotateTo while the IDs keep climbing.
	published := make([]Event, 0, maxJournalEvents+1)
	for i := 1; i <= maxJournalEvents+1; i++ {
		published = append(published, Event{Kind: KindCIFailed, Title: fmt.Sprintf("failure %d", i)})
	}
	d.publish(published)

	latest := uint64(maxJournalEvents + 1)
	oldest := latest - rotateTo + 1
	if got := d.journal.Latest(); got != latest {
		t.Fatalf("Latest = %d after rotation, want %d — IDs must keep climbing", got, latest)
	}

	// A cursor and a Since, both pointing at an event that has aged out.
	reads := []struct {
		name string
		req  Request
	}{
		{"a cursor from before the rotation", Request{Consumer: "stale"}},
		{"an explicit since naming an event that aged out", Request{Consumer: "asker", Since: 1}},
	}
	for _, r := range reads {
		t.Run(r.name, func(t *testing.T) {
			d.journal.Seek(r.req.Consumer, 1)

			got := d.drain(r.req)
			if len(got) != rotateTo {
				t.Fatalf("delivered %d events, want the whole retained window of %d", len(got), rotateTo)
			}
			if got[0].ID != oldest || got[0].Title != fmt.Sprintf("failure %d", oldest) {
				t.Errorf("the window starts at %d (%q), want %d — a stale floor must clamp to what is retained",
					got[0].ID, got[0].Title, oldest)
			}
			if last := got[len(got)-1]; last.ID != latest {
				t.Errorf("the window ends at %d, want the newest event %d", last.ID, latest)
			}
			if cursor := d.journal.Cursor(r.req.Consumer, 0); cursor != latest {
				t.Errorf("cursor = %d after the read, want %d", cursor, latest)
			}
			// And it is caught up rather than stuck re-reading a window it can
			// never exhaust, which is what a cursor advanced by count instead
			// of by delivered ID would leave behind.
			if again := d.drain(Request{Consumer: r.req.Consumer}); len(again) != 0 {
				t.Errorf("the next read returned %d events, want 0", len(again))
			}
		})
	}
}

// TestSortTargetsIsDeterministic guards the diffability of `shuck monitor
// status`: the targets come out of a map, so the order they arrive in is
// whatever the runtime felt like that second.
func TestSortTargetsIsDeterministic(t *testing.T) {
	// Ordering is byte-wise, which puts #10 before #2. That is the price of a
	// stable order that needs no parsing, and it is the order tests and readers
	// diff against.
	input := []TargetStatus{
		{Target: "o/r#2", Verdict: "failed"},
		{Target: "a/a#1", Verdict: "passed"},
		{Target: "o/r#10", Verdict: "passed"},
		{Target: "o/s#2", Verdict: "failed"},
	}
	want := []TargetStatus{
		{Target: "a/a#1", Verdict: "passed"},
		{Target: "o/r#10", Verdict: "passed"},
		{Target: "o/r#2", Verdict: "failed"},
		{Target: "o/s#2", Verdict: "failed"},
	}

	for _, p := range permutations(input) {
		sortTargets(p)
		// The verdicts ride along: sorting a projection and reassembling would
		// pass a target-only check and report the wrong PR as red.
		if !reflect.DeepEqual(p, want) {
			t.Fatalf("sortTargets produced %+v, want %+v", p, want)
		}
	}

	// Already sorted stays sorted, so a status read twice in a row does not
	// shuffle under a reader.
	sorted := slices.Clone(want)
	sortTargets(sorted)
	if !reflect.DeepEqual(sorted, want) {
		t.Errorf("sorting an ordered list changed it to %+v", sorted)
	}

	// Degenerate inputs are what the daemon actually has most of the time.
	sortTargets(nil)
	one := []TargetStatus{{Target: "o/r#1"}}
	sortTargets(one)
	if len(one) != 1 || one[0].Target != "o/r#1" {
		t.Errorf("sorting a single target changed it to %+v", one)
	}
}

// permutations enumerates every ordering of in, which is how a test over a
// map-derived slice covers "whatever order the runtime hands it over in".
func permutations(in []TargetStatus) [][]TargetStatus {
	if len(in) <= 1 {
		return [][]TargetStatus{slices.Clone(in)}
	}
	var out [][]TargetStatus
	for i := range in {
		rest := slices.Concat(in[:i], in[i+1:])
		for _, tail := range permutations(rest) {
			out = append(out, append([]TargetStatus{in[i]}, tail...))
		}
	}
	return out
}

func TestNewToken(t *testing.T) {
	a, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newToken()
	if a == b {
		t.Error("tokens must not repeat")
	}
	if len(a) != 64 {
		t.Errorf("token length = %d, want 64 hex characters", len(a))
	}
}
