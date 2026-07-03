package gateway

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// fakeTokens is an in-memory TokenStore.
type fakeTokens struct {
	mu   sync.Mutex
	rows map[string]TokenRecord // token hash -> record
	err  error
}

func newFakeTokens() *fakeTokens {
	return &fakeTokens{rows: make(map[string]TokenRecord)}
}

// add seeds a token (the raw value, hashed here like the portal would).
func (f *fakeTokens) add(token string, rec TokenRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[HashToken(token)] = rec
}

func (f *fakeTokens) Lookup(_ context.Context, hash string) (TokenRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return TokenRecord{}, f.err
	}
	rec, ok := f.rows[hash]
	if !ok {
		return TokenRecord{}, ErrTokenNotFound
	}
	return rec, nil
}

// fakeSubs is an in-memory SubscriptionStore recording operations.
type fakeSubs struct {
	mu   sync.Mutex
	subs map[PRRef]map[SubscriberKey]bool
	err  error
	ops  []string
}

func newFakeSubs() *fakeSubs {
	return &fakeSubs{subs: make(map[PRRef]map[SubscriberKey]bool)}
}

func (f *fakeSubs) Subscribe(_ context.Context, ref PRRef, sub SubscriberKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "subscribe:"+ref.String()+":"+sub.String())
	if f.err != nil {
		return f.err
	}
	if f.subs[ref] == nil {
		f.subs[ref] = make(map[SubscriberKey]bool)
	}
	f.subs[ref][sub] = true
	return nil
}

func (f *fakeSubs) Unsubscribe(_ context.Context, ref PRRef, sub SubscriberKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "unsubscribe:"+ref.String()+":"+sub.String())
	if f.err != nil {
		return f.err
	}
	delete(f.subs[ref], sub)
	return nil
}

func (f *fakeSubs) Subscribers(_ context.Context, ref PRRef) ([]SubscriberKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var out []SubscriberKey
	for sub := range f.subs[ref] {
		out = append(out, sub)
	}
	return out, nil
}

func (f *fakeSubs) BySubscriber(_ context.Context, sub SubscriberKey) ([]PRRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var out []PRRef
	for ref, members := range f.subs {
		if members[sub] {
			out = append(out, ref)
		}
	}
	return out, nil
}

func (f *fakeSubs) RemoveAllForPR(_ context.Context, ref PRRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "removeAllForPR:"+ref.String())
	if f.err != nil {
		return f.err
	}
	delete(f.subs, ref)
	return nil
}

func (f *fakeSubs) RemoveAllForSubscriber(_ context.Context, sub SubscriberKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "removeAllForSubscriber:"+sub.String())
	if f.err != nil {
		return f.err
	}
	for _, members := range f.subs {
		delete(members, sub)
	}
	return nil
}

// count reports the live subscription count for ref.
func (f *fakeSubs) count(ref PRRef) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs[ref])
}

// fakeBuffer is an in-memory EventBuffer. It records an ordered op log so
// tests can assert write-then-push ordering.
type fakeBuffer struct {
	mu        sync.Mutex
	events    map[string][]Event // subscriber -> events ascending by seq
	seq       map[string]int64
	markers   map[string]int64 // subscriber + "|" + event id -> seq
	ops       []string
	appendErr error
	afterErr  error
}

func newFakeBuffer() *fakeBuffer {
	return &fakeBuffer{
		events:  make(map[string][]Event),
		seq:     make(map[string]int64),
		markers: make(map[string]int64),
	}
}

func (f *fakeBuffer) Append(_ context.Context, sub SubscriberKey, ev Event) (seq int64, duplicate bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "append:"+sub.String()+":"+ev.ID)
	if f.appendErr != nil {
		return 0, false, f.appendErr
	}
	key := sub.String()
	marker := key + "|" + ev.ID
	if seq, ok := f.markers[marker]; ok {
		return seq, true, nil
	}
	f.seq[key]++
	ev.Seq = f.seq[key]
	f.events[key] = append(f.events[key], ev)
	f.markers[marker] = ev.Seq
	return ev.Seq, false, nil
}

func (f *fakeBuffer) After(_ context.Context, sub SubscriberKey, afterSeq int64) ([]Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.afterErr != nil {
		return nil, f.afterErr
	}
	var out []Event
	for _, ev := range f.events[sub.String()] {
		if ev.Seq > afterSeq {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (f *fakeBuffer) SeqOf(_ context.Context, sub SubscriberKey, eventID string) (seq int64, ok bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seq, ok = f.markers[sub.String()+"|"+eventID]
	return seq, ok, nil
}

func (f *fakeBuffer) Ack(_ context.Context, sub SubscriberKey, eventID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "ack:"+sub.String()+":"+eventID)
	key := sub.String()
	kept := f.events[key][:0]
	for _, ev := range f.events[key] {
		if ev.ID != eventID {
			kept = append(kept, ev)
		}
	}
	f.events[key] = kept
	return nil
}

func (f *fakeBuffer) Purge(_ context.Context, sub SubscriberKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "purge:"+sub.String())
	key := sub.String()
	delete(f.events, key)
	delete(f.seq, key)
	for marker := range f.markers {
		if len(marker) > len(key) && marker[:len(key)+1] == key+"|" {
			delete(f.markers, marker)
		}
	}
	return nil
}

// depth reports the buffered event count for sub.
func (f *fakeBuffer) depth(sub SubscriberKey) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events[sub.String()])
}

// opLog returns a copy of the ordered operation log.
func (f *fakeBuffer) opLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

// fakePresence is an in-memory PresenceStore.
type fakePresence struct {
	mu           sync.Mutex
	lastSeen     map[SubscriberKey]time.Time
	disconnected map[SubscriberKey]time.Time
	err          error
}

func newFakePresence() *fakePresence {
	return &fakePresence{
		lastSeen:     make(map[SubscriberKey]time.Time),
		disconnected: make(map[SubscriberKey]time.Time),
	}
}

func (f *fakePresence) Touch(_ context.Context, sub SubscriberKey, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.lastSeen[sub] = at
	delete(f.disconnected, sub)
	return nil
}

func (f *fakePresence) MarkDisconnected(_ context.Context, sub SubscriberKey, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.disconnected[sub] = at
	return nil
}

func (f *fakePresence) Stale(_ context.Context, cutoff time.Time) ([]SubscriberKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var out []SubscriberKey
	for sub, seen := range f.lastSeen {
		latest := seen
		if disc, ok := f.disconnected[sub]; ok && disc.After(latest) {
			latest = disc
		}
		if latest.Before(cutoff) {
			out = append(out, sub)
		}
	}
	return out, nil
}

func (f *fakePresence) Delete(_ context.Context, sub SubscriberKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.lastSeen, sub)
	delete(f.disconnected, sub)
	return nil
}

// disconnectedAt reports whether sub is marked disconnected.
func (f *fakePresence) disconnectedAt(sub SubscriberKey) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	at, ok := f.disconnected[sub]
	return at, ok
}

// errFake is a reusable injectable error.
var errFake = fmt.Errorf("fake store failure")
