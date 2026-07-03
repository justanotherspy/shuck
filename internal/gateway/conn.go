package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// closeGrace bounds how long a close handshake may wait for the peer's
// reply before the socket is force-closed. A live shim replies immediately;
// a dead one never will.
const closeGrace = time.Second

// Conn is one authenticated shim connection. Data frames are written by a
// single writer goroutine that drains the event buffer from a seq cursor;
// deliveries persist first and then Nudge the writer, which is what makes
// write-then-push and replay-then-live ordering structural.
type Conn struct {
	// Key is the authenticated subscriber identity.
	Key SubscriberKey

	ws     *websocket.Conn
	nudge  chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
}

func newConn(ctx context.Context, key SubscriberKey, ws *websocket.Conn) *Conn {
	cctx, cancel := context.WithCancel(ctx)
	return &Conn{
		Key:    key,
		ws:     ws,
		nudge:  make(chan struct{}, 1),
		ctx:    cctx,
		cancel: cancel,
	}
}

// Nudge signals the writer goroutine that the buffer may hold new events.
// It never blocks; a pending signal is coalesced.
func (c *Conn) Nudge() {
	select {
	case c.nudge <- struct{}{}:
	default:
	}
}

// shutdown closes the socket with code and stops the connection's
// goroutines. Safe to call from any goroutine, any number of times; only
// the first call's code is sent.
//
// It never blocks. Close performs the full close handshake — write the
// close frame, then await the peer's reply, which a dead peer never sends —
// so it runs off-thread. Canceling the conn context force-closes the
// underlying socket (the library aborts a pending read's socket on context
// cancel), so the watchdog cancel bounds a dead peer's handshake; it fires
// only after Close has already put the close frame on the wire, so a live
// peer still receives the code.
func (c *Conn) shutdown(code websocket.StatusCode, reason string) {
	c.once.Do(func() {
		go func() {
			defer c.cancel()
			watchdog := time.AfterFunc(closeGrace, c.cancel)
			defer watchdog.Stop()
			_ = c.ws.Close(code, reason)
		}()
	})
}

// done reports the channel closed when the connection is shut down.
func (c *Conn) done() <-chan struct{} {
	return c.ctx.Done()
}
