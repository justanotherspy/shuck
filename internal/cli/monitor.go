package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/justanotherspy/shuck/internal/monitor"
)

const monitorUsage = `shuck monitor — a background monitor that feeds you CI, reviews, and pin drift as they happen.

The monitor is a local daemon. Point it at a working tree and it follows that
tree: it reads the branch from HEAD, finds the open PR for it, and re-checks on
a cadence that tightens while CI is running. Switch branches, switch worktrees,
or open a PR for the branch you are on, and it retargets itself — you never
tell it a PR number. Everything is local: no webhook, no server, the same
GitHub token the rest of shuck uses.

Usage:
  shuck monitor                    what is being watched, and where it stands
  shuck monitor watch [target]     follow something (default: this working tree)
  shuck monitor unwatch [target]   stop following it
  shuck monitor events             hand over what has happened since you last looked
  shuck monitor stream             keep printing this tree's events, for a plugin monitor
  shuck monitor poke [target]      re-check now, without waiting for the interval
  shuck monitor stop               shut the monitor down
  shuck monitor run                run the monitor in the foreground (what start execs)
  shuck monitor log                print the monitor's own log

A target is a directory to follow, or a pull request — owner/repo#42, a PR URL,
"owner/repo 42", or a bare number for the local repository.

In Claude Code the monitor needs none of this: the plugin's own monitor runs
"shuck monitor stream", which registers the session's working tree and turns
each new event into a notification as it happens. Use "watch" to add a PR the
working tree cannot imply. That command and "shuck monitor hook <event>" are the
integration's entry points and are not meant to be run by hand.

Flags:
`

// runMonitor dispatches the `shuck monitor` subcommands.
func runMonitor(args []string, stdout, stderr io.Writer) int {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}

	switch sub {
	case "", "status":
		return monitorStatus(args, stdout, stderr)
	case "watch":
		return monitorWatch(args, stdout, stderr)
	case "unwatch":
		return monitorUnwatch(args, stdout, stderr)
	case "events":
		return monitorEvents(args, stdout, stderr)
	case "stream":
		return monitorStream(args, stdout, stderr)
	case "poke":
		return monitorPoke(args, stdout, stderr)
	case "stop":
		return monitorStop(args, stdout, stderr)
	case "run", "start":
		return monitorRun(args, stdout, stderr)
	case "log", "logs":
		return monitorLog(stdout, stderr)
	case "hook":
		return monitorHook(args, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "shuck monitor: unknown subcommand %q\n\n%s", sub, monitorUsage)
		return 2
	}
}

// monitorFlags wires a monitor subcommand's flag set to the shared usage text.
// It deliberately does not register --json: unwatch, poke and stop have a
// sentence of output each and no document to emit, and a registered flag that
// nothing reads is worse than a rejected one — `shuck monitor unwatch --json |
// jq` would exit 0 having printed prose.
func monitorFlags(fs *flag.FlagSet, stderr io.Writer) {
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, monitorUsage)
		fs.PrintDefaults()
	}
}

// monitorSchemaVersion is the version of the JSON envelopes the monitor
// subcommands emit. It versions the CLI's output only: monitor.Status and
// monitor.Event are the daemon's IPC payload and the journal's durable record,
// and stamping a schema on those would version the wire format instead — a
// number the CLI could then never move without breaking a daemon it did not
// ship with.
const monitorSchemaVersion = 1

// statusDocument is the `shuck monitor status --json` envelope: the daemon's
// status with the schema handle every other shuck --json document carries. The
// status is embedded, so its fields stay where a consumer already reads them.
type statusDocument struct {
	SchemaVersion int `json:"schema_version"`
	monitor.Status
}

// eventDocument is one line of `shuck monitor events --json`. The feed is
// line-delimited JSON — one object per event — so the version travels on every
// line rather than in a header a consumer reading a tail would never see.
type eventDocument struct {
	SchemaVersion int `json:"schema_version"`
	monitor.Event
}

// watchDocument is the `shuck monitor watch --json` envelope. Watch was the one
// monitor document emitted bare, which is worse than either choice on its own:
// a consumer that branches on schema_version got nothing from it while status
// and events both carried one. Like those two it embeds rather than nests, so
// the watch's own fields — `.id` above all, the identity the daemon keys every
// watch on — stay where a consumer already reads them.
type watchDocument struct {
	SchemaVersion int `json:"schema_version"`
	monitor.Watch
}

// newMonitorClient builds the client the monitor subcommands talk through. It
// is a package var so tests can point the CLI at a fake daemon.
var newMonitorClient = monitor.NewClient

// monitorStatus reports what the monitor is following. It is the bare
// `shuck monitor`, so it stays readable at a glance: one line per watch, one
// per target, and the quota headroom that explains a monitor gone quiet.
func monitorStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shuck monitor status", flag.ContinueOnError)
	var jsonOut bool
	var noStart bool
	monitorFlags(fs, stderr)
	fs.BoolVar(&jsonOut, "json", false, "emit the status as one JSON document (with schema_version) instead of text")
	fs.BoolVar(&noStart, "no-start", false, "do not start the monitor if it is not running")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	client, err := newMonitorClient()
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	client.AutoStart = !noStart

	st, err := client.Status(context.Background(), "")
	if err != nil {
		if noStart {
			// "Not running" is an answer, not a failure — but a caller that
			// asked for JSON has to get JSON for it too, or the one case they
			// most need to branch on arrives as an unparseable line.
			if jsonOut {
				return emitJSON(stdout, stderr, statusDocument{monitorSchemaVersion, monitor.Status{Running: false}})
			}
			fmt.Fprintln(stdout, "shuck monitor: not running")
			return 0
		}
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	if jsonOut {
		return emitJSON(stdout, stderr, statusDocument{monitorSchemaVersion, *st})
	}
	renderStatus(stdout, st)
	return 0
}

// renderStatus prints the monitor's state.
func renderStatus(w io.Writer, st *monitor.Status) {
	fmt.Fprintf(w, "shuck monitor %s — running (pid %d, up %s)\n", st.Version, st.PID, st.Uptime)
	if st.RateLimit > 0 {
		fmt.Fprintf(w, "GitHub quota: %d/%d remaining\n", st.RateRemaining, st.RateLimit)
	}

	fmt.Fprintf(w, "\nWatching (%d):\n", len(st.Watches))
	if len(st.Watches) == 0 {
		fmt.Fprintln(w, "  nothing — run `shuck monitor watch` in a repository")
	}
	for _, watch := range st.Watches {
		fmt.Fprintf(w, "  %s\n", watch.Describe())
	}

	if len(st.Targets) > 0 {
		fmt.Fprintf(w, "\nPull requests (%d):\n", len(st.Targets))
	}
	for _, t := range st.Targets {
		fmt.Fprintf(w, "  %s  %s\n", t.Target, targetLine(t))
	}

	fmt.Fprintf(w, "\n%d event(s) recorded", st.Events)
	if st.Pending > 0 {
		fmt.Fprintf(w, ", %d waiting for you", st.Pending)
	}
	fmt.Fprintln(w)
}

// targetLine words one polled PR's state.
func targetLine(t monitor.TargetStatus) string {
	var parts []string
	switch t.Verdict {
	case "passed":
		parts = append(parts, "CI green")
	case "failed":
		parts = append(parts, "CI FAILING")
	default:
		parts = append(parts, "CI running or not started")
	}
	if t.Lifecycle != "" && t.Lifecycle != "open" {
		parts = append(parts, t.Lifecycle)
	}
	if !t.NextPoll.IsZero() {
		if d := time.Until(t.NextPoll); d > 0 {
			parts = append(parts, "next check in "+d.Round(time.Second).String())
		} else {
			parts = append(parts, "checking now")
		}
	}
	if t.LastError != "" {
		parts = append(parts, "error: "+t.LastError)
	}
	return strings.Join(parts, " · ")
}

// monitorWatch registers something to follow, starting the daemon if this is
// the first thing anyone has asked it to watch.
func monitorWatch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shuck monitor watch", flag.ContinueOnError)
	var jsonOut bool
	monitorFlags(fs, stderr)
	fs.BoolVar(&jsonOut, "json", false, "emit the registered watch as one JSON document (with schema_version) instead of text")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	spec, err := monitor.ParseWatchSpec(fs.Args(), cwd)
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}

	client, err := newMonitorClient()
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	watch, err := watchWithToken(context.Background(), client, spec)
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	if watch == nil {
		// The daemon in this binary always answers a watch with the record it
		// stored, so a nil one means something else is on the socket — a daemon
		// from an older build, most likely. The registration itself went
		// through, which is all the text line ever claimed, so that stays a
		// success. A --json caller is asking for the record: embedding a zero
		// Watch would hand back an empty id and a year-1 `added` that read as a
		// real watch, so it gets the failure instead.
		if jsonOut {
			fmt.Fprintln(stderr, "shuck: the monitor registered the watch but returned no record of it")
			return 2
		}
		fmt.Fprintln(stdout, "watching")
		return 0
	}
	if jsonOut {
		return emitJSON(stdout, stderr, watchDocument{monitorSchemaVersion, *watch})
	}
	fmt.Fprintf(stdout, "watching %s\n", watch.Describe())
	return 0
}

// monitorUnwatch drops a watch.
func monitorUnwatch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shuck monitor unwatch", flag.ContinueOnError)
	monitorFlags(fs, stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	spec, err := monitor.ParseWatchSpec(fs.Args(), cwd)
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}

	client, err := newMonitorClient()
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	client.AutoStart = false
	if err := client.Unwatch(context.Background(), spec.ID); err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	fmt.Fprintf(stdout, "stopped watching %s\n", spec.ID)
	return 0
}

// monitorEvents hands over what the monitor has noticed. With --follow it keeps
// handing them over as they arrive, which is the closest thing to tailing the
// feed by hand.
func monitorEvents(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shuck monitor events", flag.ContinueOnError)
	var (
		jsonOut  bool
		all      bool
		follow   bool
		consumer string
		limit    int
		wait     time.Duration
	)
	monitorFlags(fs, stderr)
	fs.BoolVar(&jsonOut, "json", false, "emit one JSON object per event, line-delimited, each carrying schema_version")
	fs.BoolVar(&all, "all", false, "show the whole retained journal, not just what is new")
	fs.BoolVar(&follow, "follow", false, "keep printing events as they arrive, until interrupted")
	fs.StringVar(&consumer, "consumer", "cli", "the identity whose cursor advances; sessions use their own")
	fs.IntVar(&limit, "limit", 0, "at most this many events (0 = no limit)")
	fs.DurationVar(&wait, "wait", 0, "block up to this long for an event when nothing is pending")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if follow && wait > 0 {
		// A follow blocks until it is interrupted, so it sets its own read
		// horizon and there is nothing --wait could mean. Accepting it would
		// let a caller believe `--follow --wait 30m` stops after half an hour.
		fmt.Fprintln(stderr, "shuck: --wait cannot be combined with --follow: a follow already blocks until you interrupt it")
		return 2
	}

	client, err := newMonitorClient()
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	client.AutoStart = false

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	req := monitor.Request{Consumer: consumer, Limit: limit, Wait: wait, All: all}
	if follow {
		return monitorFollow(ctx, client, req, jsonOut, stdout, stderr)
	}

	events, err := eventsWithin(ctx, client, req)
	if err != nil {
		if ctx.Err() != nil {
			// Ctrl-C during a wait is how the call ends, not a fault.
			return 0
		}
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	if err := emitEvents(stdout, events, jsonOut, true); err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	return 0
}

// eventsWithin performs the one-shot read, re-asking until the caller's own
// --wait has elapsed.
//
// The daemon caps a single blocking read at monitor.MaxWait so no client can
// reserve a goroutine and a connection indefinitely. That cap is the daemon's
// to set, but it must not leak into the contract: every doc tells an agent to
// run `shuck monitor events --wait 30m` and treat it as blocking for the full
// half hour, and a single call would return "nothing new" at ten minutes —
// exit 0 either way, so the caller cannot tell a quiet half hour from a daemon
// that hung up two thirds of the way through and walks off with CI still red.
// Re-asking makes the documented wait the real one.
func eventsWithin(ctx context.Context, client *monitor.Client, req monitor.Request) ([]monitor.Event, error) {
	// --all is answered straight from the journal without ever blocking, and a
	// caller who did not ask to wait has nothing to wait out.
	if req.All || req.Wait <= 0 {
		events, _, err := client.Events(ctx, req)
		return events, err
	}

	deadline := time.Now().Add(req.Wait)
	backoff := eventsRetryFloor
	for {
		ask := min(time.Until(deadline), monitor.MaxWait)
		req.Wait = ask
		start := time.Now()
		events, _, err := client.Events(ctx, req)
		if err != nil || len(events) > 0 {
			return events, err
		}
		// Checked after the read rather than before it: a read that comes back
		// empty is the only thing that moves the clock, and an interrupt has to
		// end the wait here rather than on the next round trip.
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return nil, nil
		}
		if time.Since(start) >= ask {
			// The daemon held the read for everything it was handed, so the
			// next ask is simply the next slice of the caller's wait: one dial
			// per monitor.MaxWait, nothing to pace.
			continue
		}
		// It answered early instead — which the daemon in this binary never
		// does, but one left over from an older binary might. Re-asking at once
		// would turn a 30-minute wait into a 30-minute busy loop, so back off
		// toward the cap and let the deadline end it.
		if !sleepCtx(ctx, min(backoff, time.Until(deadline))) {
			return nil, nil
		}
		backoff = min(backoff*2, monitor.MaxWait)
	}
}

// eventsRetryFloor is the first pause eventsWithin takes after a read that came
// back sooner than it was asked to block for. It doubles from there, so a
// daemon that never blocks costs a handful of dials rather than a spinning core.
const eventsRetryFloor = 10 * time.Millisecond

// followInterval is how long a --follow read blocks before asking again. The
// daemon wakes it the moment an event lands, so this is only the ceiling on how
// long a quiet feed holds a connection open.
const followInterval = 30 * time.Second

// followEvents reads batch after batch until the caller interrupts it or the
// daemon stops answering, handing each batch — empty ones included, which is
// what lets a caller use the loop as a heartbeat — to emit.
//
// It reports what happened rather than deciding what it means: nil for an
// interruption, the reason otherwise. Its two callers word that reason in
// opposite ways. `monitor events --follow` is somebody's terminal and a daemon
// that went away is a failure worth two on the exit code; a plugin monitor's
// stream is a notification channel, and there the same event is one quiet
// sentence and exit 0.
func followEvents(ctx context.Context, client *monitor.Client, req monitor.Request, emit func([]monitor.Event) error) error {
	req.All = false
	req.Wait = followInterval
	for {
		events, _, err := client.Events(ctx, req)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if err := emit(events); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

// monitorFollow streams events until interrupted.
func monitorFollow(ctx context.Context, client *monitor.Client, req monitor.Request, jsonOut bool, stdout, stderr io.Writer) int {
	err := followEvents(ctx, client, req, func(events []monitor.Event) error {
		return emitEvents(stdout, events, jsonOut, false)
	})
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	return 0
}

// emitEvents prints a batch. quietWhenEmpty says whether "nothing new" deserves
// a line of its own — it does for a one-shot read and does not while following.
func emitEvents(stdout io.Writer, events []monitor.Event, jsonOut, quietWhenEmpty bool) error {
	if jsonOut {
		enc := json.NewEncoder(stdout)
		for _, e := range events {
			if err := enc.Encode(eventDocument{monitorSchemaVersion, e}); err != nil {
				return err
			}
		}
		return nil
	}
	if len(events) == 0 {
		if quietWhenEmpty {
			fmt.Fprintln(stdout, "nothing new")
		}
		return nil
	}
	for _, e := range events {
		fmt.Fprintln(stdout, e.Text())
		fmt.Fprintln(stdout)
	}
	return nil
}

// streamBatchLimit caps one notification's worth of events. A stream's consumer
// id is stable, which is what lets a restarted one resume rather than replay —
// but the first stream on a machine has no cursor at all, and an uncapped read
// would hand the session the whole retained journal in a single notification.
// The journal keeps the newest events when a read overflows, so the cap costs a
// first run the stale half of a backlog and nothing else; in steady state a
// batch is one or two events and it never applies.
const streamBatchLimit = 20

// monitorStream is what a Claude Code plugin monitor runs. It registers the
// session's working tree and then prints every event the monitor produces for
// it, until the session ends and the process is killed; Claude Code turns each
// block it writes into a notification, which is how a session hears that its
// build went red without anyone asking.
//
// Nothing that can go wrong here is worth a stack trace in a notification. A
// monitor that exits is simply a monitor that is not running, so every start-up
// failure — no token, an unusable cache directory, no daemon — is one plain
// sentence on stdout and exit 0. That sentence is the whole of what the session
// gets: the two hooks that remain do not deliver, and the causes above stand
// them down as well, so a stand-down is a total loss of delivery and says so
// rather than implying a fallback (see streamStandDown).
func monitorStream(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shuck monitor stream", flag.ContinueOnError)
	var jsonOut bool
	monitorFlags(fs, stderr)
	fs.BoolVar(&jsonOut, "json", false, "emit one JSON object per event, line-delimited, each carrying schema_version")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// The opt-out the hooks honor has to reach the loudest half of the
	// integration too, and it is read here rather than in the plugin's shim
	// because a shim cannot know what the binary it found supports.
	if os.Getenv("SHUCK_MONITOR_DISABLE") != "" {
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	spec, client, err := streamRegister(ctx)
	if err != nil {
		return streamStandDown(stdout, jsonOut, err)
	}

	// A caller-supplied identity is for the host that would rather name its own
	// cursor; the derived one is prefixed so it can never be mistaken for — or
	// collide with — a Claude Code session's own, and takes a process of its own
	// when a second stream is already live on this tree. It is chosen before the
	// marker below is written, which is what lets it see the sibling rather than
	// this process.
	consumer := os.Getenv("SHUCK_MONITOR_CONSUMER")
	if consumer == "" {
		consumer = monitor.StreamIdentity(spec)
	}
	stream, err := monitor.BeginStream(spec, consumer)
	if err != nil {
		// Without the marker the hooks cannot tell that this stream is
		// delivering, and the session would be handed every CI failure twice.
		// One channel is the point; two is the bug.
		return streamStandDown(stdout, jsonOut, err)
	}
	defer stream.Close()
	// Drop the claim the instant an interrupt arrives rather than waiting for
	// the read in flight to come back: the hooks have to take delivery over
	// promptly, and the deferred Close above can be up to a follow interval
	// away.
	go func() {
		<-ctx.Done()
		stream.Close()
	}()

	// The daemon was started above if it needed starting. Leaving AutoStart on
	// for the follow would turn `shuck monitor stop` in another terminal into a
	// thirty-second pause, since the next read would simply spawn another one.
	client.AutoStart = false

	// Start this identity at the present the first time it is ever used. The id
	// is derived from the working tree, so a stream in a tree nobody has streamed
	// before — a new repo, a new git worktree — is a consumer with no cursor, and
	// a cursorless read is served from the start of the retained journal: a
	// session would open to yesterday's failures on another branch, rendered as
	// something to address now. Only a *new* identity is moved, so a restarted
	// stream still resumes. A daemon that will not answer this is not worth
	// standing down over: the read that follows fails the same way and says so.
	//
	// Where that cursor lands is also recorded on the marker, because it is the
	// only durable trace of when this session began. Nothing else knows: the
	// hooks are the only part of the plugin that learns the session id, and the
	// first of them can fire an hour in. Without this they would seed the
	// session's cursor to whenever they happened to run, and a failure that
	// arrived during the first turn would be seeded past rather than gated on.
	cursor, err := client.SeekNew(ctx, consumer)
	if err == nil {
		stream.SetOrigin(cursor)
	}

	// Scope the feed to this session's own working tree. Without it every
	// stream on the machine is handed every watch's events, so two sessions in
	// two worktrees each get the other's CI and pin findings — observed, and
	// the reason the scope exists.
	req := monitor.Request{Consumer: consumer, Limit: streamBatchLimit, Scope: spec.Path}
	err = followEvents(ctx, client, req, func(events []monitor.Event) error {
		stream.Beat()
		return emitStream(stdout, events, jsonOut)
	})
	if err != nil {
		return streamStandDown(stdout, jsonOut, err)
	}
	return 0
}

// streamRegister resolves the working directory to a watch and registers it,
// starting the daemon if this is the first thing to ask for one.
func streamRegister(ctx context.Context) (monitor.Watch, *monitor.Client, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return monitor.Watch{}, nil, err
	}
	spec, err := monitor.ParseWatchSpec(nil, cwd)
	if err != nil {
		return monitor.Watch{}, nil, err
	}
	client, err := newMonitorClient()
	if err != nil {
		return monitor.Watch{}, nil, err
	}
	if _, err := watchWithToken(ctx, client, spec); err != nil {
		return monitor.Watch{}, nil, err
	}
	return spec, client, nil
}

// watchWithToken registers a watch, resolving a GitHub token only on the path
// that has to start a daemon to hold it.
//
// The token belongs to the daemon, never to the client: a daemon that is
// already listening polls with the token it was started with, and nothing a
// client sends changes that. Demanding one up front therefore stood a session's
// notifications down over a credential nothing was about to use — an observed
// failure, with a perfectly healthy monitor polling GitHub at the time.
//
// The order is what makes this race-free. Asking "is a daemon running?" and
// then resolving on the answer leaves a window in which the daemon exits
// between the two, and the spawn that follows goes ahead tokenless: the caller
// is then told the monitor did not come up in 1.5s, naming a socket, when the
// actionable problem was the missing token. Attempting the watch first and
// resolving only once the daemon has said it is not there cannot get that
// wrong.
func watchWithToken(ctx context.Context, client *monitor.Client, spec monitor.Watch) (*monitor.Watch, error) {
	// Whether this client may start a daemon at all is the caller's decision;
	// the first attempt only borrows it, so that a daemon that is already
	// listening is reached without a token in hand.
	autoStart := client.AutoStart
	defer func() { client.AutoStart = autoStart }()

	client.AutoStart = false
	watch, err := client.Watch(ctx, spec)
	if !errors.Is(err, monitor.ErrNotRunning) {
		return watch, err
	}

	// Nothing was listening, so this second attempt is the one that starts a
	// daemon, and the token resolved here is the one it will poll with. Its
	// absence is reported in place of the connection error because it is the
	// half a person can do something about.
	token, tokenErr := resolveToken("")
	if tokenErr != nil {
		return nil, tokenErr
	}
	client.Token = token
	client.AutoStart = autoStart
	return client.Watch(ctx, spec)
}

// standDownDocument is the --json form of a stand-down: the one line a consumer
// most needs to branch on — the feed has ended, and why — as something it can
// parse. It carries `streaming` so the shape is a statement about the stream
// rather than a bare error string, matching how `monitor status --json` reports
// a daemon that is not running.
type standDownDocument struct {
	SchemaVersion int    `json:"schema_version"`
	Streaming     bool   `json:"streaming"`
	Reason        string `json:"reason"`
}

// streamStandDown says once that this session will not be getting notifications
// and why. It exits 0: a monitor that could not start is not a failed command,
// and a non-zero exit in a notification tells a session about a problem it
// cannot do anything about.
//
// Under --json it says the same thing as one more line of line-delimited JSON.
// The prose is for Claude Code, which turns stdout into notifications; a caller
// that asked for JSON gets JSON for the terminal line too, or the feed ends in
// the one thing its decoder cannot read.
func streamStandDown(stdout io.Writer, jsonOut bool, err error) int {
	if jsonOut {
		// Compact and unindented, like every other line of this feed: a document
		// spread over several lines is not line-delimited JSON.
		if encErr := json.NewEncoder(stdout).Encode(standDownDocument{
			SchemaVersion: monitorSchemaVersion,
			Streaming:     false,
			Reason:        err.Error(),
		}); encErr != nil {
			return 0
		}
		return 0
	}
	fmt.Fprintf(stdout, "The shuck monitor is not streaming into this session (%v). "+
		"Nothing will tell you about CI failures or review comments as they happen; "+
		"run `shuck <pr>` or `shuck logs <pr>` when you need to know.\n", err)
	return 0
}

// emitStream writes one batch the way a notification wants it: the agent-facing
// rendering, in a single write, so the whole block lands inside Claude Code's
// batching window and arrives as one notification rather than a line at a time.
//
// FormatFeed rather than the terminal rendering emitEvents uses. The block says
// where it came from and that it is not a message from the user, and that
// framing is the only thing standing between a wall of CI output and an agent
// reading it as part of the request it was given.
func emitStream(stdout io.Writer, events []monitor.Event, jsonOut bool) error {
	if len(events) == 0 {
		return nil
	}
	if jsonOut {
		return emitEvents(stdout, events, true, false)
	}
	_, err := fmt.Fprintln(stdout, monitor.FormatFeed(events))
	return err
}

// monitorPoke brings the next check forward, for the moment right after a push.
func monitorPoke(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shuck monitor poke", flag.ContinueOnError)
	monitorFlags(fs, stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	client, err := newMonitorClient()
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	client.AutoStart = false

	id := ""
	if fs.NArg() > 0 {
		cwd, _ := os.Getwd()
		spec, err := monitor.ParseWatchSpec(fs.Args(), cwd)
		if err != nil {
			fmt.Fprintln(stderr, "shuck:", err)
			return 2
		}
		id = spec.ID
	}
	msg, err := client.Poke(context.Background(), id)
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	fmt.Fprintln(stdout, msg)
	return 0
}

// monitorStop shuts the daemon down.
func monitorStop(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shuck monitor stop", flag.ContinueOnError)
	monitorFlags(fs, stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	client, err := newMonitorClient()
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	if err := client.Stop(context.Background()); err != nil {
		fmt.Fprintln(stdout, "shuck monitor: not running")
		return 0
	}
	fmt.Fprintln(stdout, "shuck monitor: stopped")
	return 0
}

// monitorLog prints the daemon's own log, which is where a monitor that is
// misbehaving explains itself.
func monitorLog(stdout, stderr io.Writer) int {
	dir, err := monitor.Dir()
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	f, err := os.Open(dir + "/daemon.log")
	if err != nil {
		fmt.Fprintln(stdout, "shuck monitor: no log yet")
		return 0
	}
	defer f.Close()
	if _, err := io.Copy(stdout, f); err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	return 0
}

// monitorHook serves one Claude Code hook invocation. It is wired up by the
// shuck plugin and is not something to run by hand — which is why it is left
// out of the usage text above.
func monitorHook(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "shuck monitor hook: which hook? (session-start, user-prompt-submit, post-tool-use, stop, session-end)")
		return 2
	}
	return monitor.RunHook(context.Background(), monitor.HookEvent(args[0]), os.Stdin, stdout)
}

// monitorRun runs the daemon itself. `shuck monitor start` is the same thing:
// a client that finds no daemon re-execs this, detached, so in practice nobody
// types it.
func monitorRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shuck monitor run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		detached bool
		stay     bool
		noPins   bool
		ttl      time.Duration
		token    string
	)
	fs.BoolVar(&detached, "detached", false, "run as a background daemon, logging to the monitor's log file")
	fs.BoolVar(&stay, "stay", false, "keep running even with nothing left to watch")
	fs.BoolVar(&noPins, "no-pins", false, "do not audit workflow files for stale action pins")
	fs.DurationVar(&ttl, "watch-ttl", monitor.DefaultWatchTTL, "drop every watch after this long with no client asking how things stand (0 = never)")
	fs.StringVar(&token, "token", "", "GitHub token (overrides GITHUB_TOKEN/GH_TOKEN)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Open the log before anything that can fail. A detached daemon is spawned
	// with its stderr discarded, so every word written there is lost — and the
	// client that spawned it tells the user to read the monitor's log, which
	// this is the only thing that ever creates. Resolving the token first left
	// the one failure that stops a monitor before it starts ("no GitHub token
	// found") in the discarded stream, pointing at a file that did not exist.
	logTo := stderr
	if detached {
		dir, err := monitor.Dir()
		if err != nil {
			fmt.Fprintln(stderr, "shuck:", err)
			return 2
		}
		f, err := os.OpenFile(dir+"/daemon.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintln(stderr, "shuck:", err)
			return 2
		}
		defer f.Close()
		logTo = f
	}

	resolved, err := resolveToken(token)
	if err != nil {
		fmt.Fprintln(logTo, "shuck:", err)
		return 2
	}

	// A daemon started by hand keeps running with nothing to watch, because
	// somebody is about to add something. One started on demand exits when its
	// last watch expires, because nothing should outlive the sessions that
	// wanted it.
	opts := monitor.Options{
		Token:        resolved,
		PinResolver:  newPinResolver(resolved, false),
		WatchTTL:     ttl,
		ExitWhenIdle: detached && !stay,
		NoPins:       noPins,
		Log:          logTo,
		Version:      versionString(),
	}
	if ttl == 0 {
		opts.WatchTTL = -1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := monitor.Run(ctx, opts); err != nil {
		if errors.Is(err, monitor.ErrAlreadyRunning) {
			// Two clients racing to start one is normal and not a failure.
			fmt.Fprintln(stdout, "shuck monitor: already running")
			return 0
		}
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	return 0
}

// emitJSON writes v as indented JSON.
func emitJSON(stdout, stderr io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	return 0
}
