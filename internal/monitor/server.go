package monitor

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"sort"
	"time"
)

// listen binds the daemon's local endpoint and returns how clients should reach
// it.
//
// A unix socket is the right answer wherever there is one: the 0700 directory
// it lives in already says who may connect, so there is no credential to
// manage. The listener doubles as the lock — a second daemon cannot bind the
// same path — which is why a stale socket is only removed after a dial proves
// nobody is behind it.
//
// The loopback fallback exists for the platforms and paths where a unix socket
// is not available. There the address alone grants no authority, so the
// endpoint file carries a random token that clients must present, and the file
// is owner-readable only.
func listen(p paths) (net.Listener, endpoint, error) {
	ln, err := listenUnix(p.socket)
	if err == nil {
		return ln, endpoint{Network: "unix", Address: p.socket, PID: os.Getpid()}, nil
	}
	if errors.Is(err, errAlreadyServing) {
		return nil, endpoint{}, ErrAlreadyRunning
	}

	tcp, terr := net.Listen("tcp", "127.0.0.1:0")
	if terr != nil {
		// Report the unix failure: it is the one that explains the situation,
		// and the TCP attempt was only ever the fallback.
		return nil, endpoint{}, fmt.Errorf("listen on %s: %w", p.socket, err)
	}
	token, terr := newToken()
	if terr != nil {
		_ = tcp.Close()
		return nil, endpoint{}, terr
	}
	return tcp, endpoint{Network: "tcp", Address: tcp.Addr().String(), Token: token, PID: os.Getpid()}, nil
}

// errAlreadyServing reports that something is already listening on the socket.
var errAlreadyServing = errors.New("socket is already served")

// listenUnix binds path, clearing a socket file left behind by a daemon that
// died. "Left behind" is established by dialing: a socket that answers belongs
// to a live daemon and must not be touched.
//
// Every exit from the recovery path re-dials before reporting failure. Another
// daemon can win this race at either step — it can bind (and so make the remove
// or the second bind fail) between our dial and our attempt — and a plain
// failure here sends listen() on to the loopback fallback, which would leave
// two daemons polling and appending to one journal. Declining to start is the
// only safe answer once anything is answering on the socket.
func listenUnix(path string) (net.Listener, error) {
	ln, err := net.Listen("unix", path)
	if err == nil {
		return ln, nil
	}
	if socketServed(path) {
		return nil, errAlreadyServing
	}
	if rerr := os.Remove(path); rerr != nil {
		if socketServed(path) {
			return nil, errAlreadyServing
		}
		return nil, err
	}
	ln, err = net.Listen("unix", path)
	if err != nil && socketServed(path) {
		return nil, errAlreadyServing
	}
	return ln, err
}

// socketServed reports whether something answers on the unix socket at path.
// Dialing is the only test that tells a live daemon apart from the socket file
// a killed one left behind. It is a package var because the races listenUnix
// recovers from cannot otherwise be staged in a test.
var socketServed = func(path string) bool {
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// newToken mints the bearer token that guards a loopback endpoint.
func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate monitor token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// accept serves client connections until ctx ends.
func (d *Daemon) accept(ctx context.Context, ln net.Listener, token string) {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err == nil {
			go d.handleConn(ctx, conn, token)
			continue
		}
		// A closed listener is the shutdown path and the loop is done. A
		// transient failure — the process momentarily out of file descriptors,
		// say — is not: giving up on it would leave a bound socket nobody
		// serves, which is worse than any single dropped connection because it
		// also blocks the next daemon from taking over.
		if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
			return
		}
		d.logf("accept: %v", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// handleConn reads one request, answers it, and closes. Clients are short-lived
// commands and hooks; keeping a connection open would buy nothing and cost a
// goroutine per session.
func (d *Daemon) handleConn(ctx context.Context, conn net.Conn, token string) {
	defer conn.Close()

	// A client that connects and says nothing must not pin a goroutine. The
	// deadline is generous enough for a long OpEvents wait, which the handler
	// extends as needed.
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))

	r := bufio.NewReaderSize(conn, 64<<10)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return
	}

	var req Request
	if err := decodeLine(line, &req); err != nil {
		writeResponse(conn, errResponse(err))
		return
	}
	if token != "" && subtle.ConstantTimeCompare([]byte(req.Auth), []byte(token)) != 1 {
		writeResponse(conn, errResponse(errors.New("monitor token mismatch")))
		return
	}

	if req.Op == OpEvents && req.Wait > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(min(req.Wait, MaxWait) + 30*time.Second))
	}
	writeResponse(conn, d.handle(ctx, req))
}

func writeResponse(conn net.Conn, resp Response) {
	_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	raw, err := json.Marshal(resp)
	if err != nil {
		raw, _ = json.Marshal(errResponse(fmt.Errorf("encode response: %w", err)))
	}
	_, _ = conn.Write(append(raw, '\n'))
}

// handle dispatches one request. Every op that touches a watch refreshes its
// last-seen time: a client asking about a watch is exactly the evidence that
// somebody still cares about it.
func (d *Daemon) handle(ctx context.Context, req Request) Response {
	switch req.Op {
	case OpPing:
		return Response{OK: true, Version: d.opts.Version}
	case OpStatus:
		return d.handleStatus(ctx, req)
	case OpWatch:
		return d.handleWatch(req)
	case OpUnwatch:
		return d.handleUnwatch(req)
	case OpEvents:
		return d.handleEvents(ctx, req)
	case OpSeek:
		return d.handleSeek(req)
	case OpPoke:
		return d.handlePoke(req)
	case OpStop:
		d.Shutdown()
		return Response{OK: true, Message: "monitor stopping"}
	default:
		return errResponse(fmt.Errorf("unknown monitor op %q", req.Op))
	}
}

func (d *Daemon) handleStatus(ctx context.Context, req Request) Response {
	d.mu.Lock()
	d.watches.TouchAll()
	watches := d.watches.List()
	targets := make([]TargetStatus, 0, len(d.targets))
	byTarget := map[string][]string{}
	for _, w := range watches {
		if t := w.Target(); t != "" && w.Number != 0 {
			byTarget[t] = append(byTarget[t], w.ID)
		}
	}
	for key, st := range d.targets {
		targets = append(targets, TargetStatus{
			Target:     key,
			Verdict:    st.Verdict,
			HeadSHA:    st.HeadSHA,
			Lifecycle:  st.Lifecycle,
			LastPolled: st.LastPolled,
			NextPoll:   st.NextPoll,
			LastError:  st.LastError,
			Watches:    byTarget[key],
		})
	}
	d.mu.Unlock()

	sortTargets(targets)
	status := &Status{
		Running:   true,
		PID:       os.Getpid(),
		Version:   d.opts.Version,
		StartedAt: d.startedAt,
		Uptime:    time.Since(d.startedAt).Round(time.Second),
		Watches:   watches,
		Targets:   targets,
		Events:    d.journal.Latest(),
		Pending:   d.journal.Pending(req.Consumer),
	}
	// The quota probe is free (GitHub does not count /rate_limit against it)
	// but it is still a network round trip, so a failure just leaves the
	// numbers at zero rather than failing the status call.
	if remaining, limit, err := d.poller.client.RateRemaining(ctx); err == nil {
		status.RateRemaining, status.RateLimit = remaining, limit
	}
	return Response{OK: true, Status: status}
}

func (d *Daemon) handleWatch(req Request) Response {
	if req.Watch == nil || req.Watch.ID == "" {
		return errResponse(errors.New("watch request carries no watch"))
	}
	d.mu.Lock()
	stored := *d.watches.Add(*req.Watch)
	d.mu.Unlock()

	// A brand-new watch is polled on the next tick rather than the next
	// interval: the whole point of registering one is that you want to know
	// now.
	return Response{OK: true, Watch: &stored}
}

func (d *Daemon) handleUnwatch(req Request) Response {
	if req.ID == "" {
		return errResponse(errors.New("unwatch needs a watch id"))
	}
	d.mu.Lock()
	removed := d.watches.Remove(req.ID)
	d.mu.Unlock()
	if !removed {
		return errResponse(fmt.Errorf("no watch %q", req.ID))
	}
	d.pruneTargets(time.Now())
	d.prunePins()
	return Response{OK: true, Message: "stopped watching " + req.ID}
}

func (d *Daemon) handleEvents(ctx context.Context, req Request) Response {
	d.mu.Lock()
	d.watches.TouchAll()
	d.mu.Unlock()

	if req.All {
		match := d.scopeFilter(req.Scope)
		scan := req.Limit
		if match != nil {
			scan = 0
		}
		events := scoped(d.journal.Since(0, scan), match, req.Limit)
		return Response{OK: true, Events: events, Cursor: d.journal.Latest()}
	}

	// Take the wake channel BEFORE looking for events. The other order loses
	// an event published in the gap: it closes the channel this call has not
	// taken yet, and the caller then waits on a fresh one for something that
	// has already happened.
	wake := d.waiter()
	events := d.drain(req)
	if len(events) > 0 || req.Wait <= 0 {
		return Response{OK: true, Events: events, Cursor: d.journal.Latest()}
	}

	// Nothing pending and the caller is willing to wait: this is how an agent
	// blocks on "tell me when CI finishes" without polling. The wait is capped
	// server-side — the handler holds a goroutine and a connection for its
	// duration, and neither should be a client's to reserve indefinitely.
	timer := time.NewTimer(min(req.Wait, MaxWait))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return Response{OK: true, Cursor: d.journal.Latest()}
		case <-timer.C:
			return Response{OK: true, Cursor: d.journal.Latest()}
		case <-wake:
			wake = d.waiter()
			if events := d.drain(req); len(events) > 0 {
				return Response{OK: true, Events: events, Cursor: d.journal.Latest()}
			}
		}
	}
}

// drain reads a consumer's pending events, honoring an explicit Since that
// overrides the stored cursor, a Peek that leaves the cursor alone, and a Scope
// that narrows the batch to the asking session's own watches.
//
// Scoping is applied strictly after the journal has been read, and never
// before, because the read is what moves the cursor: journal.Drain assigns the
// mark from the last event of the batch it produced, under its own lock, before
// it returns, and the Since branch seeks to the last of the raw batch. Either
// way the cursor lands past everything *scanned* rather than everything
// *returned*, so a filtered-out event is behind the cursor and can never come
// round again — which is what keeps a narrow scope from becoming an infinite
// redelivery loop.
//
// A scoped read also scans without the caller's cap and applies that cap to
// what survives the filter. The journal keeps the *newest* of what it scans
// when a read overflows, so cap-then-filter would let three quiet worktrees
// spend a stream's whole twenty-event batch and hand this session "nothing new"
// while its own failure sat one position further back. Widening the scan costs
// no delivery, because the cursor follows the scan either way.
func (d *Daemon) drain(req Request) []Event {
	match := d.scopeFilter(req.Scope)
	scan := req.Limit
	if match != nil {
		scan = 0
	}

	if req.Peek {
		return scoped(d.journal.Since(d.journal.Cursor(req.Consumer, req.Since), scan), match, req.Limit)
	}
	if req.Since > 0 {
		events := d.journal.Since(req.Since, scan)
		if req.Consumer != "" && len(events) > 0 {
			d.journal.Seek(req.Consumer, events[len(events)-1].ID)
		}
		return scoped(events, match, req.Limit)
	}
	return scoped(d.journal.Drain(req.Consumer, scan), match, req.Limit)
}

// scoped drops the events a scoped reader does not own and then applies its
// limit, keeping the newest — the same choice the journal makes when a read
// overflows, and for the same reason: a consumer that has fallen behind wants
// the current state of the world, not the start of a backlog. A nil predicate
// is the unscoped case, where the journal has already applied the limit and
// this is a pass-through.
func scoped(events []Event, match func(Event) bool, limit int) []Event {
	if match == nil {
		return events
	}
	var out []Event
	for _, e := range events {
		if match(e) {
			out = append(out, e)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// scopeFilter builds the predicate that decides whether an event belongs to the
// session working in scope, or returns nil when there is no scope to filter by.
//
// An event names its subject in one of four shapes — "owner/repo#42" for
// everything the poller produces, "owner/repo" for a pin finding, the working
// tree's own path for a note about a tree that will not resolve, and "" when
// even that is unknown — so the accept set holds a watch's pull request, its
// repository and its path. Two of the eleven producers also stamp the watch id,
// which both survives a target the watch has since moved off and settles
// ownership outright: an event stamped with a watch this scope does not own
// belongs to the scope that does, whatever its target says.
//
// The unrecognized cases fail toward delivery: an event with no target and no
// watch, and an event whose target no watch claims — a PR a tree watch was
// following a second ago, say. What excludes an event is another scope claiming
// it, never this scope failing to recognize it, because a watch that has just
// moved must not take its session's last failure with it.
func (d *Daemon) scopeFilter(scope string) func(Event) bool {
	if scope == "" {
		return nil
	}
	resolved := resolveTree(scope)

	d.mu.Lock()
	watches := d.watches.List()
	d.mu.Unlock()

	mine := map[string]bool{}
	ids := map[string]bool{}
	known := map[string]bool{}
	claimed := map[string]bool{}
	for _, w := range watches {
		inScope := w.inScope(resolved)
		known[w.ID] = true
		if inScope {
			ids[w.ID] = true
		}
		for _, key := range watchKeys(w) {
			claimed[key] = true
			if inScope {
				mine[key] = true
			}
		}
	}
	return func(e Event) bool {
		switch {
		case e.Watch != "" && ids[e.Watch]:
			return true
		case e.Watch != "" && known[e.Watch]:
			// Stamped with a watch that exists and is somebody else's. The id
			// is the most specific thing an event can carry, so it settles the
			// question here rather than falling through to a target that may be
			// empty or shared — which is how a note about another session's
			// unreadable tree used to reach every session on the machine.
			return false
		case e.Target == "":
			return true
		case mine[e.Target]:
			return true
		default:
			return !claimed[e.Target]
		}
	}
}

// watchKeys are the strings an event's Target can carry for one watch: the pull
// request it currently resolves to, the repository a pin finding belongs to,
// and the working tree itself, which is what the notes about a tree that cannot
// be read name.
//
// The repository key is what carries a pin finding to every worktree of a
// checkout. Pin findings are deduplicated per repository — the second worktree
// of a checkout has nothing new to say about the same file and line — so the
// one event has to belong to all of them, or the worktree that did not scan
// first would never hear it.
//
// That key is broad by construction: every worktree of one checkout answers to
// it. What keeps it from fanning anything *else* out that widely is a rule the
// producers hold to rather than a test made here — a repository is the subject
// of a pin finding and of nothing else. Everything the poller reports names the
// pull request; the notes about a watch that will not resolve name the working
// tree and stamp the watch id (see setNote and noteTarget), precisely so that
// "no open PR for main" is not addressed to every worktree of the repository it
// happens to be in. TestWatchNotesNameTheTreeTheyAreAbout is where that rule is
// held to; a new event kind that names a repository has to mean it.
func watchKeys(w Watch) []string {
	var keys []string
	if t := w.Target(); t != "" {
		keys = append(keys, t)
	}
	if w.Kind != WatchTree {
		return keys
	}
	// A pin finding is a property of a checkout, and only a tree watch has one,
	// so only a tree watch claims the repository-wide key.
	for _, key := range []string{pinRepoKey(w), w.Path} {
		if key != "" && !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	return keys
}

func (d *Daemon) handleSeek(req Request) Response {
	to := req.Since
	if to == 0 && !req.Exact {
		to = d.journal.Latest()
	}
	if req.IfNew {
		// A consumer that already has a cursor keeps it: this is the seek a
		// long-lived identity makes on every start, and moving an existing
		// cursor would drop what was published while it was not reading.
		return Response{OK: true, Cursor: d.journal.SeekNew(req.Consumer, to)}
	}
	d.journal.Seek(req.Consumer, to)
	return Response{OK: true, Cursor: to}
}

// handlePoke brings a target's next poll forward to now. It is what a client
// calls the instant after a push, when waiting out the interval is pure
// latency.
func (d *Daemon) handlePoke(req Request) Response {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	poked := 0
	for key, st := range d.targets {
		if req.ID != "" && key != req.ID {
			if w, ok := d.watches.Get(req.ID); !ok || w.Target() != key {
				continue
			}
		}
		st.NextPoll = now
		st.Failures = 0
		// A poll may be in flight right now, and its result carries the
		// deadline it computed before the poke arrived. The flag is what tells
		// store() to keep this one instead.
		st.Poked = true
		poked++
	}
	d.watches.TouchAll()
	return Response{OK: true, Message: fmt.Sprintf("%s queued for an immediate check", count(poked, "target"))}
}

// sortTargets orders the status view deterministically.
func sortTargets(targets []TargetStatus) {
	sort.Slice(targets, func(i, j int) bool { return targets[i].Target < targets[j].Target })
}
