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
func listenUnix(path string) (net.Listener, error) {
	ln, err := net.Listen("unix", path)
	if err == nil {
		return ln, nil
	}
	conn, derr := net.DialTimeout("unix", path, time.Second)
	if derr == nil {
		_ = conn.Close()
		return nil, errAlreadyServing
	}
	if rerr := os.Remove(path); rerr != nil {
		return nil, err
	}
	return net.Listen("unix", path)
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
		if err != nil {
			return
		}
		go d.handleConn(ctx, conn, token)
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
		_ = conn.SetReadDeadline(time.Now().Add(req.Wait + 30*time.Second))
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
	d.pruneTargets()
	return Response{OK: true, Message: "stopped watching " + req.ID}
}

func (d *Daemon) handleEvents(ctx context.Context, req Request) Response {
	d.mu.Lock()
	d.watches.TouchAll()
	d.mu.Unlock()

	if req.All {
		return Response{OK: true, Events: d.journal.Since(0, req.Limit), Cursor: d.journal.Latest()}
	}

	events := d.drain(req)
	if len(events) > 0 || req.Wait <= 0 {
		return Response{OK: true, Events: events, Cursor: d.journal.Latest()}
	}

	// Nothing pending and the caller is willing to wait: this is how an agent
	// blocks on "tell me when CI finishes" without polling.
	timer := time.NewTimer(req.Wait)
	defer timer.Stop()
	for {
		wake := d.waiter()
		select {
		case <-ctx.Done():
			return Response{OK: true, Cursor: d.journal.Latest()}
		case <-timer.C:
			return Response{OK: true, Cursor: d.journal.Latest()}
		case <-wake:
			if events := d.drain(req); len(events) > 0 {
				return Response{OK: true, Events: events, Cursor: d.journal.Latest()}
			}
		}
	}
}

// drain reads a consumer's pending events, honoring an explicit Since that
// overrides the stored cursor and a Peek that leaves the cursor alone.
func (d *Daemon) drain(req Request) []Event {
	if req.Peek {
		return d.journal.Since(d.journal.Cursor(req.Consumer, req.Since), req.Limit)
	}
	if req.Since > 0 {
		events := d.journal.Since(req.Since, req.Limit)
		if req.Consumer != "" && len(events) > 0 {
			d.journal.Seek(req.Consumer, events[len(events)-1].ID)
		}
		return events
	}
	return d.journal.Drain(req.Consumer, req.Limit)
}

func (d *Daemon) handleSeek(req Request) Response {
	to := req.Since
	if to == 0 {
		to = d.journal.Latest()
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
		poked++
	}
	d.watches.TouchAll()
	return Response{OK: true, Message: fmt.Sprintf("%s queued for an immediate check", count(poked, "target"))}
}

// sortTargets orders the status view deterministically.
func sortTargets(targets []TargetStatus) {
	sort.Slice(targets, func(i, j int) bool { return targets[i].Target < targets[j].Target })
}
