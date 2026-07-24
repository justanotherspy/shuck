package monitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/justanotherspy/shuck/internal/gh"
	"github.com/justanotherspy/shuck/internal/logs"
	"github.com/justanotherspy/shuck/internal/pins"
)

// tick is how often the daemon wakes up to see whether anything is due. It is
// not the poll interval — each target carries its own deadline — just the
// resolution at which those deadlines are honored. A second is cheap (it costs
// a few map lookups and a stat of each watched HEAD) and it is what makes a
// branch switch show up immediately rather than at the next poll.
const tick = time.Second

// Options configures a daemon run.
type Options struct {
	// Token is the GitHub token to poll with. The daemon does not read the
	// environment itself: it is usually started detached by a client, and
	// having the client resolve the token means the failure ("no GitHub
	// token") lands in front of the person who can fix it.
	Token string
	// WatchTTL is how long a watch survives without a client asking about it.
	// Zero means DefaultWatchTTL; negative means never expire.
	WatchTTL time.Duration
	// ExitWhenIdle stops the daemon once it has no watches left. It is the
	// default for a daemon started on demand: sessions end, watches expire,
	// and nothing should keep polling GitHub afterwards.
	ExitWhenIdle bool
	// ContextLines is how many lines of a file surround a review comment in
	// its event body; 0 means distil's default.
	ContextLines int
	// NoPins turns off the workflow pin audit, for anyone who would rather
	// the monitor stayed out of their .github directory.
	NoPins bool
	// PinResolver resolves an action reference to its latest release for the
	// pin audit. The caller supplies it — the CLI hands over the same
	// cache-backed resolver `shuck action` and `shuck pins` use — so this
	// package neither duplicates that caching nor depends on it. A nil
	// resolver still reports unpinned references, just without a suggested
	// fix.
	PinResolver pins.Resolver
	// Extract tunes how much of a failing job's log survives into an event.
	Extract logs.Options
	// Log receives the daemon's diagnostics. Nil discards them.
	Log io.Writer
	// Version is reported to clients so a stale daemon can be spotted.
	Version string
}

// Daemon is the running monitor: the watch registry, the per-target poll state,
// the event journal, and the local listener that serves them to clients.
type Daemon struct {
	opts   Options
	paths  paths
	poller *poller

	mu       sync.Mutex
	watches  *registry
	targets  map[string]*prState
	pins     map[string]*pinState
	notifyCh chan struct{}

	journal   *journal
	startedAt time.Time

	// stop is closed to bring the run loop down; stopOnce guards it against
	// a client and a signal racing to shut down.
	stop     chan struct{}
	stopOnce sync.Once
}

// Run starts a daemon and blocks until it is stopped — by a client's stop
// request, by ctx being cancelled, or by running out of watches when
// ExitWhenIdle is set.
//
// It returns an error if another daemon already holds the lock, so a caller
// racing to start one can tell "someone beat me to it" (fine) from "this is
// broken" (not).
func Run(ctx context.Context, opts Options) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	d, err := newDaemon(dir, opts)
	if err != nil {
		return err
	}
	return d.serve(ctx)
}

// ErrAlreadyRunning reports that a healthy daemon already holds the lock.
var ErrAlreadyRunning = errors.New("a shuck monitor is already running")

func newDaemon(dir string, opts Options) (*Daemon, error) {
	if opts.WatchTTL == 0 {
		opts.WatchTTL = DefaultWatchTTL
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	if opts.Extract.Pattern == nil {
		opts.Extract = logs.DefaultOptions()
	}

	p := newPaths(dir)
	j, err := openJournal(p)
	if err != nil {
		return nil, err
	}

	d := &Daemon{
		opts:      opts,
		paths:     p,
		watches:   loadRegistry(p),
		targets:   map[string]*prState{},
		pins:      map[string]*pinState{},
		notifyCh:  make(chan struct{}),
		journal:   j,
		startedAt: time.Now(),
		stop:      make(chan struct{}),
		poller: &poller{
			client:       newPRClient(opts.Token),
			extract:      opts.Extract,
			contextLines: opts.ContextLines,
			log:          opts.Log,
		},
	}
	d.loadTargets()
	d.loadPins()
	return d, nil
}

// serve takes the lock, listens, and runs until stopped.
func (d *Daemon) serve(ctx context.Context) error {
	ln, ep, err := listen(d.paths)
	if err != nil {
		return err
	}
	defer func() {
		_ = ln.Close()
		d.cleanup()
	}()

	if err := writeJSONFile(d.paths.endpoint, ep); err != nil {
		return err
	}
	d.logf("monitor %s started (pid %d) on %s %s", d.opts.Version, os.Getpid(), ep.Network, ep.Address)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// A round polls GitHub without the lock and can sit in a call for as long
	// as the network takes. Folding the stop signal into the context is what
	// makes `shuck monitor stop` immediate rather than "immediate once the
	// current call returns".
	go func() {
		select {
		case <-d.stop:
			cancel()
		case <-ctx.Done():
		}
	}()

	go d.accept(ctx, ln, ep.Token)

	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			d.logf("monitor stopping: %v", ctx.Err())
			return nil
		case <-d.stop:
			d.logf("monitor stopping on request")
			return nil
		case now := <-ticker.C:
			if done := d.round(ctx, now); done {
				d.logf("monitor stopping: nothing left to watch")
				return nil
			}
		}
	}
}

// round is one wake-up: re-read the watched working trees, retire watches
// nobody is using, and poll whichever targets are due. It reports true when
// there is nothing left to do and the daemon should exit.
func (d *Daemon) round(ctx context.Context, now time.Time) bool {
	d.retarget(ctx, now)
	d.expire(now)

	for _, st := range d.due(now) {
		if d.stopping(ctx) {
			return false
		}
		updated, events := d.poller.Poll(ctx, st, now)
		d.store(updated, events)
	}
	if d.stopping(ctx) {
		return false
	}
	d.auditPins(ctx, now)

	d.mu.Lock()
	empty := d.watches.Len() == 0
	d.mu.Unlock()
	return empty && d.opts.ExitWhenIdle
}

// stopping reports whether the daemon is on its way down, so a round with
// several targets to poll gives up between them instead of working through the
// list after someone asked it to stop.
func (d *Daemon) stopping(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	case <-d.stop:
		return true
	default:
		return false
	}
}

// retarget re-reads every tree watch's working directory and follows it. This
// is the behavior the whole design exists for: a branch switch, a new worktree,
// or a PR opening for the current branch all land here, without anyone telling
// the monitor anything.
func (d *Daemon) retarget(ctx context.Context, now time.Time) {
	d.mu.Lock()
	trees := make([]Watch, 0, d.watches.Len())
	for _, w := range d.watches.List() {
		if w.Kind == WatchTree {
			trees = append(trees, w)
		}
	}
	d.mu.Unlock()

	for _, w := range trees {
		d.retargetOne(ctx, w, now)
	}
	d.pruneTargets()
}

// retargetOne resolves one tree watch and records any change as an event.
func (d *Daemon) retargetOne(ctx context.Context, w Watch, now time.Time) {
	checkout, err := ReadCheckout(w.Path)
	if err != nil {
		d.updateWatch(w.ID, func(cur *Watch) []Event {
			return d.setNote(cur, KindError, err.Error(), now)
		})
		return
	}
	if checkout.Branch == "" {
		d.updateWatch(w.ID, func(cur *Watch) []Event {
			cur.Owner, cur.Repo, cur.Branch = checkout.Owner, checkout.Repo, ""
			cur.Number = 0
			return d.setNote(cur, KindTarget, "HEAD is detached; no branch to match a PR against", now)
		})
		return
	}

	// Nothing moved: the common case, and it costs nothing but the HEAD read
	// that got us here. A watch that has already found its PR is done; one
	// that has not re-asks on the resolve interval rather than every tick.
	settled := w.Owner == checkout.Owner && w.Repo == checkout.Repo && w.Branch == checkout.Branch
	if settled && (w.Number != 0 || now.Sub(w.Resolved) < ResolveInterval) {
		return
	}

	number, findErr := d.poller.client.FindOpenPR(ctx, checkout.Owner, checkout.Repo, checkout.Owner, checkout.Branch)
	d.updateWatch(w.ID, func(cur *Watch) []Event {
		moved := cur.Owner != checkout.Owner || cur.Repo != checkout.Repo || cur.Branch != checkout.Branch
		previous := cur.Target()
		cur.Owner, cur.Repo, cur.Branch = checkout.Owner, checkout.Repo, checkout.Branch
		cur.Resolved = now

		if findErr != nil {
			cur.Number = 0
			// A branch with no PR yet and a lookup that failed are opposite
			// situations: the first is the normal state of a branch you have
			// not opened a PR for, the second means the monitor cannot do its
			// job. Reporting the second as the first would send someone
			// looking for a PR instead of at their token.
			if !errors.Is(findErr, gh.ErrNoOpenPR) {
				return d.setNote(cur, KindError,
					fmt.Sprintf("could not look up the PR for %s: %v", checkout.Branch, findErr), now)
			}
			note := fmt.Sprintf("no open PR for %s", checkout.Branch)
			if !moved && cur.Note == note {
				return nil
			}
			return d.setNote(cur, KindTarget, note, now)
		}

		cur.Note = ""
		if cur.Number == number && !moved {
			return nil
		}
		cur.Number = number
		return []Event{{
			Time:   now,
			Kind:   KindTarget,
			Watch:  cur.ID,
			Target: cur.Target(),
			Title:  targetChangeTitle(previous, *cur),
			URL:    prURL(cur.Owner, cur.Repo, cur.Number),
		}}
	})
}

// targetChangeTitle words a retarget for the feed, distinguishing "we found the
// PR for the branch you were already on" from "you moved".
func targetChangeTitle(previous string, w Watch) string {
	if previous == "" || previous == w.Target() {
		return fmt.Sprintf("now watching %s for %s", w.Target(), w.Branch)
	}
	return fmt.Sprintf("switched from %s to %s (%s)", previous, w.Target(), w.Branch)
}

// setNote records why a watch is not resolving to a PR, emitting an event only
// when the reason changes — a detached HEAD should say so once, not once a
// second. The kind separates the two reasons a watch can be stuck: a branch
// with no PR is just how things are (watch.target), while a lookup that failed
// is a problem the monitor cannot fix on its own (monitor.error).
func (d *Daemon) setNote(w *Watch, kind Kind, note string, now time.Time) []Event {
	if w.Note == note {
		return nil
	}
	w.Note = note
	return []Event{{
		Time:   now,
		Kind:   kind,
		Watch:  w.ID,
		Target: w.Target(),
		Title:  fmt.Sprintf("%s: %s", w.Path, note),
	}}
}

// updateWatch mutates a watch under the lock and journals whatever events the
// mutation produced.
func (d *Daemon) updateWatch(id string, fn func(*Watch) []Event) {
	d.mu.Lock()
	w, ok := d.watches.Get(id)
	if !ok {
		d.mu.Unlock()
		return
	}
	events := fn(w)
	d.watches.save()
	d.mu.Unlock()

	d.publish(events)
}

// expire retires watches nobody has asked about, and says so.
func (d *Daemon) expire(now time.Time) {
	d.mu.Lock()
	dropped := d.watches.Expire(d.opts.WatchTTL, now)
	d.mu.Unlock()

	for _, w := range dropped {
		d.logf("watch %s expired after %s idle", w.ID, d.opts.WatchTTL)
	}
	if len(dropped) > 0 {
		d.pruneTargets()
	}
}

// due returns the poll state of every target that has reached its deadline,
// creating state for targets seen for the first time.
func (d *Daemon) due(now time.Time) []prState {
	d.mu.Lock()
	defer d.mu.Unlock()

	var out []prState
	for _, w := range d.watches.List() {
		if w.Number == 0 || w.Owner == "" {
			continue
		}
		key := w.Target()
		st, ok := d.targets[key]
		if !ok {
			st = &prState{Target: key, Owner: w.Owner, Repo: w.Repo, Number: w.Number}
			d.targets[key] = st
		}
		if st.NextPoll.After(now) {
			continue
		}
		// Claim the slot now so a long poll does not queue up behind itself on
		// the next tick.
		st.NextPoll = now.Add(ActiveInterval)
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Target < out[j].Target })
	return out
}

// store writes a completed poll's state back and journals its events.
//
// A poll runs without the lock — it is network-bound and must not block the
// whole daemon — so anything that happened to this target while it was in
// flight is sitting in the stored state, and a blind overwrite would lose it.
// The one thing that actually changes mid-poll is the deadline: a poke asks
// for an immediate re-check, and a result that arrives afterwards must not
// push that back to its own leisurely interval.
func (d *Daemon) store(st prState, events []Event) {
	d.mu.Lock()
	if existing, ok := d.targets[st.Target]; ok {
		if existing.NextPoll.Before(st.NextPoll) {
			st.NextPoll = existing.NextPoll
			st.Failures = existing.Failures
		}
		d.targets[st.Target] = &st
		d.saveTargetsLocked()
	}
	d.mu.Unlock()

	d.publish(events)
}

// pruneTargets drops poll state for targets no watch points at any more, so a
// session that moves through ten branches does not leave ten pollers behind.
func (d *Daemon) pruneTargets() {
	d.mu.Lock()
	defer d.mu.Unlock()

	live := map[string]bool{}
	for _, w := range d.watches.List() {
		if t := w.Target(); t != "" && w.Number != 0 {
			live[t] = true
		}
	}
	changed := false
	for key := range d.targets {
		if !live[key] {
			delete(d.targets, key)
			changed = true
		}
	}
	if changed {
		d.saveTargetsLocked()
	}
}

// publish appends events to the journal and wakes anyone waiting on one.
func (d *Daemon) publish(events []Event) {
	if len(events) == 0 {
		return
	}
	for _, e := range events {
		stored := d.journal.Append(e)
		d.logf("event %d %s %s: %s", stored.ID, stored.Kind, stored.Target, stored.Title)
	}
	d.notify()
}

// notify releases every waiter. The channel is closed and replaced rather than
// signaled, so a waiter that arrives between two events still sees the second.
func (d *Daemon) notify() {
	d.mu.Lock()
	close(d.notifyCh)
	d.notifyCh = make(chan struct{})
	d.mu.Unlock()
}

// waiter returns a channel closed on the next published event.
func (d *Daemon) waiter() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.notifyCh
}

// Shutdown asks the daemon to stop.
func (d *Daemon) Shutdown() {
	d.stopOnce.Do(func() { close(d.stop) })
}

// loadTargets restores per-target poll state from disk.
func (d *Daemon) loadTargets() {
	var stored []prState
	if readJSONFile(d.targetsPath(), &stored) != nil {
		return
	}
	for i := range stored {
		st := stored[i]
		if st.Target != "" {
			d.targets[st.Target] = &st
		}
	}
}

func (d *Daemon) saveTargetsLocked() {
	out := make([]prState, 0, len(d.targets))
	for _, st := range d.targets {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Target < out[j].Target })
	_ = writeJSONFile(d.targetsPath(), out)
}

func (d *Daemon) targetsPath() string { return filepath.Join(d.paths.dir, "targets.json") }

func (d *Daemon) pinsPath() string { return filepath.Join(d.paths.dir, "pins.json") }

// loadPins restores the per-tree workflow-pin fingerprints.
func (d *Daemon) loadPins() {
	var stored []pinState
	if readJSONFile(d.pinsPath(), &stored) != nil {
		return
	}
	for i := range stored {
		st := stored[i]
		if st.Path != "" {
			d.pins[st.Path] = &st
		}
	}
}

// auditPins re-audits each watched working tree's workflow files, reporting
// references that are not SHA-pinned or whose pin has fallen behind. It runs
// after the PR polls so a round's expensive work is done before its cheap
// work, and it is skipped entirely when the tree's files have not changed.
func (d *Daemon) auditPins(ctx context.Context, now time.Time) {
	if d.opts.NoPins {
		return
	}
	d.mu.Lock()
	var trees []string
	for _, w := range d.watches.List() {
		if w.Kind == WatchTree && w.Path != "" {
			trees = append(trees, w.Path)
		}
	}
	states := make(map[string]pinState, len(trees))
	for _, path := range trees {
		if st, ok := d.pins[path]; ok {
			states[path] = *st
		} else {
			states[path] = pinState{Path: path}
		}
	}
	d.mu.Unlock()

	for _, path := range trees {
		updated, events := d.scanPins(ctx, states[path], now)
		d.mu.Lock()
		d.pins[path] = &updated
		d.savePinsLocked()
		d.mu.Unlock()
		d.publish(events)
	}
}

func (d *Daemon) savePinsLocked() {
	out := make([]pinState, 0, len(d.pins))
	for _, st := range d.pins {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	_ = writeJSONFile(d.pinsPath(), out)
}

// cleanup removes the files that advertise a running daemon. A client that
// finds them missing knows to start one; a client that finds them stale falls
// back to a ping, so leaving them behind after a crash is survivable rather
// than fatal.
func (d *Daemon) cleanup() {
	_ = os.Remove(d.paths.endpoint)
	_ = os.Remove(d.paths.socket)
}

func (d *Daemon) logf(format string, args ...any) {
	fmt.Fprintf(d.opts.Log, "%s "+format+"\n",
		append([]any{time.Now().Format(time.RFC3339)}, args...)...)
}
