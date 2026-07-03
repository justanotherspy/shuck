package gateway

import "sync"

// Registry is the live-connection lookup: subscriber key → connection. The
// v1 implementation is in-memory (single replica); the interface is the
// seam where post-v1 HA (JUS-95) slots in a distributed lookup.
type Registry interface {
	// Register makes c the current connection for key and returns the
	// connection it displaced, if any (newest wins — the caller closes it).
	Register(key SubscriberKey, c *Conn) (prev *Conn)
	// Unregister removes c if it is still the current connection for key
	// and reports whether it was. A replaced connection's teardown must
	// not disturb its successor's registration.
	Unregister(key SubscriberKey, c *Conn) bool
	// Get returns the current connection for key, if any.
	Get(key SubscriberKey) (*Conn, bool)
	// Snapshot returns every current connection (drain).
	Snapshot() []*Conn
	// Len reports the number of live connections.
	Len() int
}

// memRegistry is the single-replica in-memory Registry.
type memRegistry struct {
	mu    sync.Mutex
	conns map[SubscriberKey]*Conn
}

// NewMemRegistry returns the in-memory Registry used in v1.
func NewMemRegistry() Registry {
	return &memRegistry{conns: make(map[SubscriberKey]*Conn)}
}

func (r *memRegistry) Register(key SubscriberKey, c *Conn) *Conn {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := r.conns[key]
	r.conns[key] = c
	return prev
}

func (r *memRegistry) Unregister(key SubscriberKey, c *Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns[key] != c {
		return false
	}
	delete(r.conns, key)
	return true
}

func (r *memRegistry) Get(key SubscriberKey) (*Conn, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.conns[key]
	return c, ok
}

func (r *memRegistry) Snapshot() []*Conn {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Conn, 0, len(r.conns))
	for _, c := range r.conns {
		out = append(out, c)
	}
	return out
}

func (r *memRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
}
