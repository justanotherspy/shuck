package serverless

import (
	"context"
	"slices"
	"testing"

	"github.com/justanotherspy/shuck/internal/gateway"
)

// FuzzServerlessGatewayMessage drives arbitrary bytes through Message on a
// fresh (never-helloed) connection and asserts the auth invariant: the
// connection ends up registered if and only if the frame was a valid hello
// carrying the seeded token — anything else must leave it closed and
// unregistered, with no subscription writes.
func FuzzServerlessGatewayMessage(f *testing.F) {
	f.Add([]byte(`{"type":"hello","token":"shk_good","session_id":"sess-1"}`))
	f.Add([]byte(`{"type":"hello","token":"shk_bad","session_id":"sess-1"}`))
	f.Add([]byte(`{"type":"subscribe","repo":"octo/repo","pr":7}`))
	f.Add([]byte(`{"type":"ack","id":"ev-1"}`))
	f.Add([]byte(`{"type":"ping"}`))
	f.Add([]byte(`{nope`))
	f.Add([]byte(``))
	f.Fuzz(func(t *testing.T, data []byte) {
		tokens := newFakeTokens()
		tokens.add("shk_good", gateway.TokenRecord{GitHubUserID: 42, GitHubLogin: "octocat"})
		registry := newFakeRegistry()
		conns := newFakeConns()
		subs := newFakeSubs()
		g := &Gateway{
			Tokens:   tokens,
			Subs:     subs,
			Buffer:   newFakeBuffer(),
			Presence: newFakePresence(),
			Registry: registry,
			Conns:    conns,
		}

		const connID = "fuzz-conn"
		if err := g.Message(context.Background(), connID, data); err != nil {
			t.Fatalf("Message returned error: %v", err)
		}

		frame, parseErr := gateway.ParseClientFrame(data)
		goodHello := parseErr == nil &&
			frame.Type == gateway.FrameHello &&
			frame.Token == "shk_good"

		_, registered, err := registry.Lookup(context.Background(), connID)
		if err != nil {
			t.Fatalf("registry lookup: %v", err)
		}
		closed := slices.Contains(conns.closedConns(), connID)
		if goodHello && !registered {
			t.Fatalf("valid hello did not register the connection (frame %+v)", frame)
		}
		if !goodHello && registered {
			t.Fatalf("input registered a connection without a valid hello: %q", data)
		}
		if !goodHello && !closed {
			t.Fatalf("rejected input left the connection open: %q", data)
		}
		if len(subs.ops) != 0 {
			t.Fatalf("frame on an unauthenticated connection reached the subscription store: %v", subs.ops)
		}
	})
}
