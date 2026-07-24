package monitor

import (
	"context"
	"errors"
	"net"
	"os"
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
