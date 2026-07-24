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
  shuck monitor poke [target]      re-check now, without waiting for the interval
  shuck monitor stop               shut the monitor down
  shuck monitor run                run the monitor in the foreground (what start execs)
  shuck monitor log                print the monitor's own log

A target is a directory to follow, or a pull request — owner/repo#42, a PR URL,
"owner/repo 42", or a bare number for the local repository.

In Claude Code the monitor needs none of this: the shuck plugin registers the
session's working tree on start and delivers events into the conversation as
they arrive. "shuck monitor hook <event>" is that integration's entry point and
is not meant to be run by hand.

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

// monitorFlags registers the flags every monitor client shares.
func monitorFlags(fs *flag.FlagSet, jsonOut *bool, stderr io.Writer) {
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, monitorUsage)
		fs.PrintDefaults()
	}
	fs.BoolVar(jsonOut, "json", false, "emit machine-readable JSON instead of text")
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
	monitorFlags(fs, &jsonOut, stderr)
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
			fmt.Fprintln(stdout, "shuck monitor: not running")
			return 0
		}
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	if jsonOut {
		return emitJSON(stdout, stderr, st)
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
	monitorFlags(fs, &jsonOut, stderr)
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
	// The daemon a client starts inherits this process's environment, so it
	// polls with whatever token resolved here — and a missing token is
	// reported now, by the command the person just ran, rather than silently
	// inside a background process.
	token, err := resolveToken("")
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	client.Token = token

	watch, err := client.Watch(context.Background(), spec)
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	if jsonOut {
		return emitJSON(stdout, stderr, watch)
	}
	if watch == nil {
		fmt.Fprintln(stdout, "watching")
		return 0
	}
	fmt.Fprintf(stdout, "watching %s\n", watch.Describe())
	return 0
}

// monitorUnwatch drops a watch.
func monitorUnwatch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shuck monitor unwatch", flag.ContinueOnError)
	var jsonOut bool
	monitorFlags(fs, &jsonOut, stderr)
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
	monitorFlags(fs, &jsonOut, stderr)
	fs.BoolVar(&all, "all", false, "show the whole retained journal, not just what is new")
	fs.BoolVar(&follow, "follow", false, "keep printing events as they arrive, until interrupted")
	fs.StringVar(&consumer, "consumer", "cli", "the identity whose cursor advances; sessions use their own")
	fs.IntVar(&limit, "limit", 0, "at most this many events (0 = no limit)")
	fs.DurationVar(&wait, "wait", 0, "wait up to this long for an event when nothing is pending")
	if err := fs.Parse(args); err != nil {
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

	events, _, err := client.Events(ctx, req)
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	return emitEvents(stdout, stderr, events, jsonOut, true)
}

// followInterval is how long a --follow read blocks before asking again. The
// daemon wakes it the moment an event lands, so this is only the ceiling on how
// long a quiet feed holds a connection open.
const followInterval = 30 * time.Second

// monitorFollow streams events until interrupted.
func monitorFollow(ctx context.Context, client *monitor.Client, req monitor.Request, jsonOut bool, stdout, stderr io.Writer) int {
	req.All = false
	req.Wait = followInterval
	for {
		events, _, err := client.Events(ctx, req)
		if err != nil {
			if ctx.Err() != nil {
				return 0
			}
			fmt.Fprintln(stderr, "shuck:", err)
			return 2
		}
		if code := emitEvents(stdout, stderr, events, jsonOut, false); code != 0 {
			return code
		}
		if ctx.Err() != nil {
			return 0
		}
	}
}

// emitEvents prints a batch. quietWhenEmpty says whether "nothing new" deserves
// a line of its own — it does for a one-shot read and does not while following.
func emitEvents(stdout, stderr io.Writer, events []monitor.Event, jsonOut, quietWhenEmpty bool) int {
	if jsonOut {
		enc := json.NewEncoder(stdout)
		for _, e := range events {
			if err := enc.Encode(e); err != nil {
				fmt.Fprintln(stderr, "shuck:", err)
				return 2
			}
		}
		return 0
	}
	if len(events) == 0 {
		if quietWhenEmpty {
			fmt.Fprintln(stdout, "nothing new")
		}
		return 0
	}
	for _, e := range events {
		fmt.Fprintln(stdout, e.Text())
		fmt.Fprintln(stdout)
	}
	return 0
}

// monitorPoke brings the next check forward, for the moment right after a push.
func monitorPoke(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shuck monitor poke", flag.ContinueOnError)
	var jsonOut bool
	monitorFlags(fs, &jsonOut, stderr)
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
	var jsonOut bool
	monitorFlags(fs, &jsonOut, stderr)
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
	fs.DurationVar(&ttl, "watch-ttl", monitor.DefaultWatchTTL, "drop a watch nobody has asked about for this long (0 = never)")
	fs.StringVar(&token, "token", "", "GitHub token (overrides GITHUB_TOKEN/GH_TOKEN)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	resolved, err := resolveToken(token)
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}

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
