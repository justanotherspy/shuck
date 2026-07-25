package monitor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"
)

// startTimeout bounds how long a client waits for a daemon it just spawned to
// come up. Starting one is a process exec plus a listen; a second and a half of
// patience covers a cold cache on a busy laptop without leaving a hook hanging.
const startTimeout = 1500 * time.Millisecond

// Client is a short-lived connection to the monitor daemon. Every call opens a
// connection, sends one request, reads one response, and closes — which means
// there is no session to keep alive, no reconnect logic, and nothing to leak
// when a hook is killed mid-call.
type Client struct {
	dir string
	// AutoStart spawns a daemon when none is running. It is on by default:
	// the intended experience is that you never start the monitor by hand.
	AutoStart bool
	// Token is passed to a daemon this client starts. It is not used for
	// talking to one that is already running — that daemon has its own.
	Token string
	// Executable is the shuck binary to spawn; empty means this one.
	Executable string
}

// NewClient builds a client against the default monitor directory.
func NewClient() (*Client, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	return &Client{dir: dir, AutoStart: true}, nil
}

// ErrNotRunning reports that no daemon is listening and none was started.
var ErrNotRunning = errors.New("no shuck monitor is running")

// Do sends one request and returns the response. When no daemon is listening
// and AutoStart is set, it starts one and retries; the daemon it starts inherits
// this process's environment, so it polls with the same GitHub token.
func (c *Client) Do(ctx context.Context, req Request) (Response, error) {
	resp, err := c.send(ctx, req)
	if err == nil {
		return resp, nil
	}
	if !c.AutoStart || errors.Is(err, context.Canceled) {
		return Response{}, err
	}
	if err := c.start(); err != nil {
		return Response{}, err
	}
	return c.send(ctx, req)
}

// Running reports whether a daemon is currently answering, without starting
// one.
func (c *Client) Running(ctx context.Context) bool {
	resp, err := c.send(ctx, Request{Op: OpPing})
	return err == nil && resp.OK
}

// send dials the recorded endpoint and exchanges one message.
func (c *Client) send(ctx context.Context, req Request) (Response, error) {
	ep, err := c.endpoint()
	if err != nil {
		return Response{}, err
	}
	req.Auth = ep.Token

	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, ep.Network, ep.Address)
	if err != nil {
		return Response{}, fmt.Errorf("%w (%w)", ErrNotRunning, err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		// A waiting events call sets its own horizon; everything else is a
		// local round trip that should never take this long.
		_ = conn.SetDeadline(time.Now().Add(req.Wait + 30*time.Second))
	}

	raw, err := json.Marshal(req)
	if err != nil {
		return Response{}, fmt.Errorf("encode monitor request: %w", err)
	}
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return Response{}, fmt.Errorf("send monitor request: %w", err)
	}

	r := bufio.NewReaderSize(conn, 64<<10)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return Response{}, fmt.Errorf("read monitor response: %w", err)
	}
	var resp Response
	if err := decodeLine(line, &resp); err != nil {
		return Response{}, err
	}
	if !resp.OK {
		return resp, fmt.Errorf("monitor: %s", resp.Error)
	}
	return resp, nil
}

// endpoint reads how to reach the daemon.
func (c *Client) endpoint() (endpoint, error) {
	var ep endpoint
	if err := readJSONFile(newPaths(c.dir).endpoint, &ep); err != nil {
		return endpoint{}, fmt.Errorf("%w (%w)", ErrNotRunning, err)
	}
	if ep.Network == "" || ep.Address == "" {
		return endpoint{}, ErrNotRunning
	}
	return ep, nil
}

// start spawns a detached daemon and waits for it to answer.
//
// The child is deliberately not a child for long: it is re-exec'd as
// `shuck monitor run`, its standard streams are pointed away from the parent's,
// and it is left to outlive whatever started it. A hook that starts the monitor
// and exits half a second later must not take the monitor with it.
func (c *Client) start() error {
	bin := c.Executable
	if bin == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("locate the shuck binary: %w", err)
		}
		bin = exe
	}

	// bin is this process's own executable, or a path the caller configured.
	cmd := exec.Command(bin, "monitor", "run", "--detached")
	cmd.Env = os.Environ()
	if c.Token != "" {
		cmd.Env = append(cmd.Env, "GITHUB_TOKEN="+c.Token)
	}
	// The daemon writes its own log file; anything that reaches these streams
	// is a startup failure nobody is positioned to read.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = detachAttr()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the shuck monitor: %w", err)
	}
	// Release the child so it is never this process's zombie to reap.
	go func() { _ = cmd.Wait() }()

	return c.awaitReady()
}

// awaitReady polls for the daemon's socket to start answering.
func (c *Client) awaitReady() error {
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()

	for delay := 15 * time.Millisecond; ; delay = min(delay*2, 200*time.Millisecond) {
		if c.Running(ctx) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the shuck monitor did not come up within %s (see %s)",
				startTimeout, newPaths(c.dir).log)
		case <-time.After(delay):
		}
	}
}

// --- typed convenience wrappers ---------------------------------------------

// Status fetches the daemon's state. consumer, when set, adds that consumer's
// pending-event count to the reply.
func (c *Client) Status(ctx context.Context, consumer string) (*Status, error) {
	resp, err := c.Do(ctx, Request{Op: OpStatus, Consumer: consumer})
	if err != nil {
		return nil, err
	}
	if resp.Status == nil {
		return nil, errors.New("monitor returned no status")
	}
	return resp.Status, nil
}

// Watch registers something to follow and returns the stored watch.
func (c *Client) Watch(ctx context.Context, w Watch) (*Watch, error) {
	resp, err := c.Do(ctx, Request{Op: OpWatch, Watch: &w})
	if err != nil {
		return nil, err
	}
	return resp.Watch, nil
}

// Unwatch drops a watch by ID.
func (c *Client) Unwatch(ctx context.Context, id string) error {
	_, err := c.Do(ctx, Request{Op: OpUnwatch, ID: id})
	return err
}

// Events drains a consumer's pending events, waiting up to wait for the first
// one when nothing is pending.
func (c *Client) Events(ctx context.Context, req Request) ([]Event, uint64, error) {
	req.Op = OpEvents
	resp, err := c.Do(ctx, req)
	if err != nil {
		return nil, 0, err
	}
	return resp.Events, resp.Cursor, nil
}

// Seek moves a consumer's cursor to the present without delivering anything, so
// a session that has just started is not handed the previous session's backlog.
func (c *Client) Seek(ctx context.Context, consumer string) (uint64, error) {
	resp, err := c.Do(ctx, Request{Op: OpSeek, Consumer: consumer})
	if err != nil {
		return 0, err
	}
	return resp.Cursor, nil
}

// Poke brings the next poll forward, for the moment right after a push.
func (c *Client) Poke(ctx context.Context, id string) (string, error) {
	resp, err := c.Do(ctx, Request{Op: OpPoke, ID: id})
	if err != nil {
		return "", err
	}
	return resp.Message, nil
}

// Stop shuts the daemon down. It never starts one first.
func (c *Client) Stop(ctx context.Context) error {
	auto := c.AutoStart
	c.AutoStart = false
	defer func() { c.AutoStart = auto }()
	_, err := c.Do(ctx, Request{Op: OpStop})
	return err
}
