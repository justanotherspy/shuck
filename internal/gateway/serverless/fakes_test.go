package serverless

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/justanotherspy/shuck/internal/gateway"
)

// errFake is a reusable injectable error.
var errFake = errors.New("fake store failure")

// fakeTokens is an in-memory gateway.TokenStore.
type fakeTokens struct {
	rows map[string]gateway.TokenRecord // token hash -> record
	err  error
}

func newFakeTokens() *fakeTokens {
	return &fakeTokens{rows: make(map[string]gateway.TokenRecord)}
}

func (f *fakeTokens) add(token string, rec gateway.TokenRecord) {
	f.rows[gateway.HashToken(token)] = rec
}

func (f *fakeTokens) Lookup(_ context.Context, hash string) (gateway.TokenRecord, error) {
	if f.err != nil {
		return gateway.TokenRecord{}, f.err
	}
	rec, ok := f.rows[hash]
	if !ok {
		return gateway.TokenRecord{}, gateway.ErrTokenNotFound
	}
	return rec, nil
}

// fakeToucher records TouchToken calls.
type fakeToucher struct {
	hashes []string
	err    error
}

func (f *fakeToucher) TouchToken(_ context.Context, hash string, _ time.Time) error {
	f.hashes = append(f.hashes, hash)
	return f.err
}

// fakeSubs is an in-memory gateway.SubscriptionStore.
type fakeSubs struct {
	subs map[gateway.PRRef]map[gateway.SubscriberKey]bool
	err  error
	ops  []string
}

func newFakeSubs() *fakeSubs {
	return &fakeSubs{subs: make(map[gateway.PRRef]map[gateway.SubscriberKey]bool)}
}

func (f *fakeSubs) Subscribe(_ context.Context, ref gateway.PRRef, sub gateway.SubscriberKey) error {
	f.ops = append(f.ops, "subscribe:"+ref.String()+":"+sub.String())
	if f.err != nil {
		return f.err
	}
	if f.subs[ref] == nil {
		f.subs[ref] = make(map[gateway.SubscriberKey]bool)
	}
	f.subs[ref][sub] = true
	return nil
}

func (f *fakeSubs) Unsubscribe(_ context.Context, ref gateway.PRRef, sub gateway.SubscriberKey) error {
	f.ops = append(f.ops, "unsubscribe:"+ref.String()+":"+sub.String())
	if f.err != nil {
		return f.err
	}
	delete(f.subs[ref], sub)
	return nil
}

func (f *fakeSubs) Subscribers(_ context.Context, ref gateway.PRRef) ([]gateway.SubscriberKey, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []gateway.SubscriberKey
	for sub := range f.subs[ref] {
		out = append(out, sub)
	}
	return out, nil
}

func (f *fakeSubs) BySubscriber(_ context.Context, sub gateway.SubscriberKey) ([]gateway.PRRef, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []gateway.PRRef
	for ref, members := range f.subs {
		if members[sub] {
			out = append(out, ref)
		}
	}
	return out, nil
}

func (f *fakeSubs) RemoveAllForPR(_ context.Context, ref gateway.PRRef) error {
	f.ops = append(f.ops, "removeAllForPR:"+ref.String())
	if f.err != nil {
		return f.err
	}
	delete(f.subs, ref)
	return nil
}

func (f *fakeSubs) RemoveAllForSubscriber(_ context.Context, sub gateway.SubscriberKey) error {
	f.ops = append(f.ops, "removeAllForSubscriber:"+sub.String())
	if f.err != nil {
		return f.err
	}
	for _, members := range f.subs {
		delete(members, sub)
	}
	return nil
}

// fakeBuffer is an in-memory gateway.EventBuffer with an ordered op log.
type fakeBuffer struct {
	events    map[string][]gateway.Event
	seq       map[string]int64
	markers   map[string]int64
	ops       []string
	appendErr error
	afterErr  error
	ackErr    error
}

func newFakeBuffer() *fakeBuffer {
	return &fakeBuffer{
		events:  make(map[string][]gateway.Event),
		seq:     make(map[string]int64),
		markers: make(map[string]int64),
	}
}

func (f *fakeBuffer) Append(_ context.Context, sub gateway.SubscriberKey, ev gateway.Event) (seq int64, duplicate bool, err error) {
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

func (f *fakeBuffer) After(_ context.Context, sub gateway.SubscriberKey, afterSeq int64) ([]gateway.Event, error) {
	if f.afterErr != nil {
		return nil, f.afterErr
	}
	var out []gateway.Event
	for _, ev := range f.events[sub.String()] {
		if ev.Seq > afterSeq {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (f *fakeBuffer) SeqOf(_ context.Context, sub gateway.SubscriberKey, eventID string) (seq int64, ok bool, err error) {
	seq, ok = f.markers[sub.String()+"|"+eventID]
	return seq, ok, nil
}

func (f *fakeBuffer) Ack(_ context.Context, sub gateway.SubscriberKey, eventID string) error {
	f.ops = append(f.ops, "ack:"+sub.String()+":"+eventID)
	if f.ackErr != nil {
		return f.ackErr
	}
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

func (f *fakeBuffer) Purge(_ context.Context, sub gateway.SubscriberKey) error {
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

// fakePresence is an in-memory gateway.PresenceStore.
type fakePresence struct {
	lastSeen     map[gateway.SubscriberKey]time.Time
	disconnected map[gateway.SubscriberKey]time.Time
	err          error
}

func newFakePresence() *fakePresence {
	return &fakePresence{
		lastSeen:     make(map[gateway.SubscriberKey]time.Time),
		disconnected: make(map[gateway.SubscriberKey]time.Time),
	}
}

func (f *fakePresence) Touch(_ context.Context, sub gateway.SubscriberKey, at time.Time) error {
	if f.err != nil {
		return f.err
	}
	f.lastSeen[sub] = at
	delete(f.disconnected, sub)
	return nil
}

func (f *fakePresence) MarkDisconnected(_ context.Context, sub gateway.SubscriberKey, at time.Time) error {
	if f.err != nil {
		return f.err
	}
	f.disconnected[sub] = at
	return nil
}

func (f *fakePresence) Stale(_ context.Context, cutoff time.Time) ([]gateway.SubscriberKey, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []gateway.SubscriberKey
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

func (f *fakePresence) Delete(_ context.Context, sub gateway.SubscriberKey) error {
	delete(f.lastSeen, sub)
	delete(f.disconnected, sub)
	return nil
}

// fakeRegistry is an in-memory RegistryStore mirroring the DynamoDB
// adapter's semantics, including the conditional forward-row delete.
type fakeRegistry struct {
	forward   map[gateway.SubscriberKey]string
	reverse   map[string]gateway.SubscriberKey
	setErr    error
	getErr    error
	lookupErr error
	removeErr error
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{
		forward: make(map[gateway.SubscriberKey]string),
		reverse: make(map[string]gateway.SubscriberKey),
	}
}

func (f *fakeRegistry) Set(_ context.Context, sub gateway.SubscriberKey, connID string) (string, error) {
	if f.setErr != nil {
		return "", f.setErr
	}
	prev := f.forward[sub]
	f.forward[sub] = connID
	f.reverse[connID] = sub
	if prev != "" && prev != connID {
		delete(f.reverse, prev)
	}
	return prev, nil
}

func (f *fakeRegistry) Get(_ context.Context, sub gateway.SubscriberKey) (string, bool, error) {
	if f.getErr != nil {
		return "", false, f.getErr
	}
	connID, ok := f.forward[sub]
	return connID, ok, nil
}

func (f *fakeRegistry) Lookup(_ context.Context, connID string) (gateway.SubscriberKey, bool, error) {
	if f.lookupErr != nil {
		return gateway.SubscriberKey{}, false, f.lookupErr
	}
	sub, ok := f.reverse[connID]
	return sub, ok, nil
}

func (f *fakeRegistry) Remove(_ context.Context, connID string) (gateway.SubscriberKey, bool, error) {
	if f.removeErr != nil {
		return gateway.SubscriberKey{}, false, f.removeErr
	}
	sub, ok := f.reverse[connID]
	if !ok {
		return gateway.SubscriberKey{}, false, nil
	}
	delete(f.reverse, connID)
	if f.forward[sub] == connID {
		delete(f.forward, sub)
	}
	return sub, true, nil
}

// fakeConns records posted frames and closes per connection; posts and
// closes to ids in gone report ErrGone / succeed silently.
type fakeConns struct {
	mu      sync.Mutex
	posts   map[string][]string // conn id -> frames as strings
	closed  []string
	gone    map[string]bool
	postErr error
}

func newFakeConns() *fakeConns {
	return &fakeConns{posts: make(map[string][]string), gone: make(map[string]bool)}
}

func (f *fakeConns) Post(_ context.Context, connID string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone[connID] {
		return ErrGone
	}
	if f.postErr != nil {
		return f.postErr
	}
	f.posts[connID] = append(f.posts[connID], string(data))
	return nil
}

func (f *fakeConns) Close(_ context.Context, connID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, connID)
	return nil
}

func (f *fakeConns) sent(connID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.posts[connID]...)
}

func (f *fakeConns) closedConns() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.closed...)
}
