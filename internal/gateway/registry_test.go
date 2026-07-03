package gateway

import "testing"

func TestMemRegistryNewestWins(t *testing.T) {
	r := NewMemRegistry()
	key := SubscriberKey{UserID: "1", SessionID: "s"}
	c1 := &Conn{Key: key}
	c2 := &Conn{Key: key}

	if prev := r.Register(key, c1); prev != nil {
		t.Fatalf("first register displaced %v", prev)
	}
	if prev := r.Register(key, c2); prev != c1 {
		t.Fatalf("second register displaced %v, want the first conn", prev)
	}
	if got, ok := r.Get(key); !ok || got != c2 {
		t.Fatalf("Get = %v, %v; want the newest conn", got, ok)
	}
	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1", r.Len())
	}

	// The replaced conn's teardown must not remove its successor.
	if r.Unregister(key, c1) {
		t.Fatal("unregistering a replaced conn reported current")
	}
	if got, ok := r.Get(key); !ok || got != c2 {
		t.Fatalf("replaced conn's unregister removed the successor: %v, %v", got, ok)
	}
	if !r.Unregister(key, c2) {
		t.Fatal("unregistering the current conn reported not current")
	}
	if _, ok := r.Get(key); ok {
		t.Fatal("conn still registered after unregister")
	}
}

func TestMemRegistrySnapshot(t *testing.T) {
	r := NewMemRegistry()
	a := &Conn{Key: SubscriberKey{UserID: "1", SessionID: "a"}}
	b := &Conn{Key: SubscriberKey{UserID: "2", SessionID: "b"}}
	r.Register(a.Key, a)
	r.Register(b.Key, b)
	snap := r.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("Snapshot len = %d, want 2", len(snap))
	}
	seen := map[*Conn]bool{}
	for _, c := range snap {
		seen[c] = true
	}
	if !seen[a] || !seen[b] {
		t.Fatalf("Snapshot missing conns: %v", seen)
	}
}
