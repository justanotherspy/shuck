package awsx

import (
	"context"
	"sync"
	"time"

	"github.com/justanotherspy/shuck/internal/gateway"
	"github.com/justanotherspy/shuck/internal/gateway/serverless"
)

// Minimal in-memory store implementations backing the Lambda adapter tests
// (the DynamoDB adapters have their own scripted-fake tests; here the
// subject is the event plumbing, not the stores).

type memTokens map[string]gateway.TokenRecord

func (m memTokens) Lookup(_ context.Context, hash string) (gateway.TokenRecord, error) {
	rec, ok := m[hash]
	if !ok {
		return gateway.TokenRecord{}, gateway.ErrTokenNotFound
	}
	return rec, nil
}

type memSubs map[gateway.PRRef]map[gateway.SubscriberKey]bool

func (m memSubs) Subscribe(_ context.Context, ref gateway.PRRef, sub gateway.SubscriberKey) error {
	if m[ref] == nil {
		m[ref] = make(map[gateway.SubscriberKey]bool)
	}
	m[ref][sub] = true
	return nil
}

func (m memSubs) Unsubscribe(_ context.Context, ref gateway.PRRef, sub gateway.SubscriberKey) error {
	delete(m[ref], sub)
	return nil
}

func (m memSubs) Subscribers(_ context.Context, ref gateway.PRRef) ([]gateway.SubscriberKey, error) {
	var out []gateway.SubscriberKey
	for sub := range m[ref] {
		out = append(out, sub)
	}
	return out, nil
}

func (m memSubs) BySubscriber(_ context.Context, sub gateway.SubscriberKey) ([]gateway.PRRef, error) {
	var out []gateway.PRRef
	for ref, members := range m {
		if members[sub] {
			out = append(out, ref)
		}
	}
	return out, nil
}

func (m memSubs) RemoveAllForPR(_ context.Context, ref gateway.PRRef) error {
	delete(m, ref)
	return nil
}

func (m memSubs) RemoveAllForSubscriber(_ context.Context, sub gateway.SubscriberKey) error {
	for _, members := range m {
		delete(members, sub)
	}
	return nil
}

type memBuffer struct {
	mu     sync.Mutex
	events map[string][]gateway.Event
	seq    map[string]int64
}

func (m *memBuffer) Append(_ context.Context, sub gateway.SubscriberKey, ev gateway.Event) (int64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.events == nil {
		m.events = make(map[string][]gateway.Event)
		m.seq = make(map[string]int64)
	}
	key := sub.String()
	for _, existing := range m.events[key] {
		if existing.ID == ev.ID {
			return existing.Seq, true, nil
		}
	}
	m.seq[key]++
	ev.Seq = m.seq[key]
	m.events[key] = append(m.events[key], ev)
	return ev.Seq, false, nil
}

func (m *memBuffer) After(_ context.Context, sub gateway.SubscriberKey, afterSeq int64) ([]gateway.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []gateway.Event
	for _, ev := range m.events[sub.String()] {
		if ev.Seq > afterSeq {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (m *memBuffer) SeqOf(_ context.Context, sub gateway.SubscriberKey, eventID string) (int64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ev := range m.events[sub.String()] {
		if ev.ID == eventID {
			return ev.Seq, true, nil
		}
	}
	return 0, false, nil
}

func (m *memBuffer) Ack(_ context.Context, sub gateway.SubscriberKey, eventID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := sub.String()
	kept := m.events[key][:0]
	for _, ev := range m.events[key] {
		if ev.ID != eventID {
			kept = append(kept, ev)
		}
	}
	m.events[key] = kept
	return nil
}

func (m *memBuffer) Purge(_ context.Context, sub gateway.SubscriberKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.events, sub.String())
	return nil
}

type memPresence struct {
	mu       sync.Mutex
	lastSeen map[gateway.SubscriberKey]time.Time
}

func newMemPresence() *memPresence {
	return &memPresence{lastSeen: make(map[gateway.SubscriberKey]time.Time)}
}

func (m *memPresence) Touch(_ context.Context, sub gateway.SubscriberKey, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastSeen[sub] = at
	return nil
}

func (m *memPresence) MarkDisconnected(_ context.Context, sub gateway.SubscriberKey, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastSeen[sub] = at
	return nil
}

func (m *memPresence) Stale(_ context.Context, cutoff time.Time) ([]gateway.SubscriberKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []gateway.SubscriberKey
	for sub, seen := range m.lastSeen {
		if seen.Before(cutoff) {
			out = append(out, sub)
		}
	}
	return out, nil
}

func (m *memPresence) Delete(_ context.Context, sub gateway.SubscriberKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.lastSeen, sub)
	return nil
}

type memRegistryStore struct {
	mu      sync.Mutex
	forward map[gateway.SubscriberKey]string
	reverse map[string]gateway.SubscriberKey
}

func newMemRegistryStore() *memRegistryStore {
	return &memRegistryStore{
		forward: make(map[gateway.SubscriberKey]string),
		reverse: make(map[string]gateway.SubscriberKey),
	}
}

func (m *memRegistryStore) Set(_ context.Context, sub gateway.SubscriberKey, connID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.forward[sub]
	m.forward[sub] = connID
	m.reverse[connID] = sub
	if prev != "" && prev != connID {
		delete(m.reverse, prev)
	}
	return prev, nil
}

func (m *memRegistryStore) Get(_ context.Context, sub gateway.SubscriberKey) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	connID, ok := m.forward[sub]
	return connID, ok, nil
}

func (m *memRegistryStore) Lookup(_ context.Context, connID string) (gateway.SubscriberKey, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sub, ok := m.reverse[connID]
	return sub, ok, nil
}

func (m *memRegistryStore) Remove(_ context.Context, connID string) (gateway.SubscriberKey, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sub, ok := m.reverse[connID]
	if !ok {
		return gateway.SubscriberKey{}, false, nil
	}
	delete(m.reverse, connID)
	if m.forward[sub] == connID {
		delete(m.forward, sub)
	}
	return sub, true, nil
}

type memConnAPI struct {
	mu    sync.Mutex
	posts map[string][]string
}

func newMemConnAPI() *memConnAPI {
	return &memConnAPI{posts: make(map[string][]string)}
}

func (m *memConnAPI) Post(_ context.Context, connID string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.posts[connID] = append(m.posts[connID], string(data))
	return nil
}

func (m *memConnAPI) Close(_ context.Context, _ string) error {
	return nil
}

var _ serverless.RegistryStore = (*memRegistryStore)(nil)
var _ serverless.ConnAPI = (*memConnAPI)(nil)
