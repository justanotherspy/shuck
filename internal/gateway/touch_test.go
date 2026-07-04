package gateway

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

// fakeToucher records TouchToken calls on a channel so tests can wait for
// the asynchronous touch.
type fakeToucher struct {
	calls chan string // token hashes
	err   error
}

func newFakeToucher() *fakeToucher {
	return &fakeToucher{calls: make(chan string, 8)}
}

func (f *fakeToucher) TouchToken(_ context.Context, hash string, _ time.Time) error {
	f.calls <- hash
	return f.err
}

func TestHelloTouchesToken(t *testing.T) {
	hub, tokens, _, _, _ := newTestHub()
	toucher := newFakeToucher()
	hub.Toucher = toucher
	tokens.add("tok-a", TokenRecord{GitHubUserID: 1, GitHubLogin: "a"})
	srv := startGateway(t, hub)

	c := hello(t, srv, "tok-a", "s1", "")
	defer c.CloseNow() //nolint:errcheck // test teardown

	select {
	case hash := <-toucher.calls:
		if hash != HashToken("tok-a") {
			t.Errorf("touched hash %q, want HashToken(tok-a)", hash)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("token touch never fired after hello")
	}
}

func TestHelloTouchFailureDoesNotAffectConnection(t *testing.T) {
	hub, tokens, subs, _, _ := newTestHub()
	toucher := newFakeToucher()
	toucher.err = errors.New("dynamo down")
	hub.Toucher = toucher
	hub.Log = slog.New(slog.DiscardHandler)
	tokens.add("tok-a", TokenRecord{GitHubUserID: 1, GitHubLogin: "a"})
	srv := startGateway(t, hub)

	c := hello(t, srv, "tok-a", "s1", "")
	defer c.CloseNow() //nolint:errcheck // test teardown
	<-toucher.calls

	// The connection still works: a subscribe lands in the store.
	send(t, c, ClientFrame{Type: FrameSubscribe, Repo: "octo/repo", PR: 7})
	deadline := time.Now().Add(5 * time.Second)
	for subs.count(PRRef{Repo: "octo/repo", PR: 7}) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("subscribe never landed after failed touch")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRejectedHelloDoesNotTouch(t *testing.T) {
	hub, _, _, _, _ := newTestHub()
	toucher := newFakeToucher()
	hub.Toucher = toucher
	srv := startGateway(t, hub)

	c := hello(t, srv, "unknown-token", "s1", "")
	defer c.CloseNow() //nolint:errcheck // test teardown

	select {
	case <-toucher.calls:
		t.Fatal("rejected hello touched the token table")
	case <-time.After(200 * time.Millisecond):
	}
}
