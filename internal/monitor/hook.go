package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

// hookTimeout bounds a hook's whole interaction with the daemon. Hooks run
// between the user pressing enter and the agent starting to think; a monitor
// that is wedged must cost that moment a fraction of a second, not seconds.
const hookTimeout = 3 * time.Second

// HookEvent names the Claude Code hook a shuck invocation is serving.
type HookEvent string

// The hooks shuck knows. Neither delivering events nor gating a finish is among
// their jobs any more — `shuck monitor stream` is the one channel and the only
// voice. What is left is the single thing a separate process cannot do: see the
// session's current directory and its tool calls, so that entering a worktree
// retargets the monitor and a push is picked up at once rather than at the next
// interval.
//
// Only HookPostToolUse is installed. The rest are entry points kept alive for
// installed copies of an older hooks.json, and each is now either a no-op or a
// registration; none of them speaks.
const (
	// HookSessionStart registers the session's working tree and reports what
	// the monitor is already watching. No longer installed — the stream
	// registers the directory it is spawned in.
	HookSessionStart HookEvent = "session-start"
	// HookUserPromptSubmit re-registers the session's working tree. It is no
	// longer installed and no longer delivers; see hookUserPrompt.
	HookUserPromptSubmit HookEvent = "user-prompt-submit"
	// HookPostToolUse re-registers the session's working tree on every tool
	// call — which is what follows an agent into a worktree — and notices a
	// push, asking the monitor to re-check now instead of at the next interval.
	HookPostToolUse HookEvent = "post-tool-use"
	// HookStop is retired and does nothing; see hookStop.
	HookStop HookEvent = "stop"
	// HookSessionEnd drops the session's cursor.
	HookSessionEnd HookEvent = "session-end"
)

// hookInput is the subset of Claude Code's hook payload shuck reads. Every
// field is optional: an unrecognized or future payload degrades to a hook that
// does nothing rather than one that fails.
type hookInput struct {
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`
	Source    string `json:"source"`
	// StopHookActive marks a Stop hook firing after a previous block. Ignoring
	// it is how a Stop hook turns into an infinite loop.
	StopHookActive bool `json:"stop_hook_active"`
	// ToolName and ToolInput describe the tool call a PostToolUse hook follows.
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
}

// hookOutput is what shuck writes back: a top-level decision for Stop, and
// hookSpecificOutput for the hooks that inject context. The two never carry the
// same text — emitting a Stop reason in both places risks it being injected
// twice, and one copy of a CI failure is enough.
type hookOutput struct {
	Decision string           `json:"decision,omitempty"`
	Reason   string           `json:"reason,omitempty"`
	Specific *hookSpecificOut `json:"hookSpecificOutput,omitempty"`
}

// hookSpecificOut is the per-event half of a hook response.
type hookSpecificOut struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext,omitempty"`
}

// hookEventNames maps shuck's hook subcommands to the names Claude Code uses in
// hookSpecificOutput.
var hookEventNames = map[HookEvent]string{
	HookSessionStart:     "SessionStart",
	HookUserPromptSubmit: "UserPromptSubmit",
	HookPostToolUse:      "PostToolUse",
	HookStop:             "Stop",
	HookSessionEnd:       "SessionEnd",
}

// RunHook serves one Claude Code hook invocation and returns the process exit
// code.
//
// It is written to be impossible to blame. Every failure path — no daemon, no
// token, an unusable cache directory, a malformed payload, an unknown event —
// exits 0 and writes nothing that changes what the session does, because a
// background convenience must never be the reason a session stalls or a prompt
// is rejected. The single exception is deliberate and is not a decision:
// SessionStart says once, as plain context, that the monitor could not be
// started, so nobody sits waiting for a feed that is never coming. The only
// thing a broken monitor should cost you is the monitoring.
func RunHook(ctx context.Context, event HookEvent, stdin io.Reader, stdout io.Writer) int {
	if os.Getenv("SHUCK_MONITOR_DISABLE") != "" {
		return 0
	}
	in := readHookInput(stdin)

	ctx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()

	client, err := NewClient()
	if err != nil {
		return 0
	}

	var out *hookOutput
	switch event {
	case HookSessionStart:
		out = hookSessionStart(ctx, client, in)
	case HookUserPromptSubmit:
		out = hookUserPrompt(ctx, client, in)
	case HookPostToolUse:
		out = hookPostToolUse(ctx, client, in)
	case HookStop:
		out = hookStop(ctx, client, in)
	case HookSessionEnd:
		out = hookSessionEnd(ctx, client, in)
	}

	if out != nil {
		if raw, err := json.Marshal(out); err == nil {
			fmt.Fprintln(stdout, string(raw))
		}
	}
	return 0
}

// readHookInput parses the hook payload, tolerating anything.
func readHookInput(r io.Reader) hookInput {
	var in hookInput
	if r == nil {
		return in
	}
	raw, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return in
	}
	_ = json.Unmarshal(raw, &in)
	return in
}

// hookSessionStart registers the session's working tree with the monitor —
// starting the daemon if this is the first session — and reports what is being
// watched.
//
// It also fast-forwards the session's cursor. A new session should be told
// about what happens from now on, not handed an hour of another session's CI
// history as if it had just arrived.
func hookSessionStart(ctx context.Context, c *Client, in hookInput) *hookOutput {
	dir, ok := hookTree(in)
	if !ok {
		return nil
	}

	spec, err := ParseWatchSpec(nil, dir)
	if err != nil {
		return nil
	}
	spec.AddSessionScope(in.SessionID)
	watch, err := c.Watch(ctx, spec)
	if err != nil {
		// No daemon and none could be started — most often a missing GitHub
		// token. Say so once, quietly, rather than failing the session.
		return context_(HookSessionStart, fmt.Sprintf(
			"The shuck background monitor is not running (%v). CI and review feedback will "+
				"not arrive on its own; run `shuck <pr>` or `shuck logs <pr>` when you need it.", err))
	}
	// Fast-forward only for a session that is genuinely starting. SessionStart
	// also fires on resume and on compaction, and seeking then would throw
	// away events the ongoing conversation has not been shown yet — the exact
	// moment a CI failure is most likely to be waiting.
	if in.SessionID != "" && isNewSession(in.Source) {
		if _, err := c.Seek(ctx, in.SessionID); err != nil {
			return nil
		}
	}

	return context_(HookSessionStart, sessionStartContext(ctx, c, watch, StreamServes(dir)))
}

// registerSessionTree registers the directory a hook payload arrived from as a
// watch belonging to this session, and reports the watch when there is one.
//
// This is what makes the monitor follow a session that moves. A plugin monitor
// is spawned once, in the directory the session opened in, and a long-running
// process cannot see the agent change directory afterwards — that happens inside
// Claude Code, not in any process the monitor owns. The hooks are the only part
// of the integration handed the session's *current* directory, on every payload,
// so registering here is the only way a session that opened outside a repository
// and then moved into one is ever watched at all. Tagging the watch with the
// session id is what lets the already-running stream pick it up without
// restarting: it is reading with that same id as its scope.
//
// Failure is silence by design, like every other path in this file. A session
// whose watch cannot be registered gets no notifications, which is the cost of
// the feature, never the cost of the turn.
func registerSessionTree(ctx context.Context, c *Client, in hookInput) (*Watch, string, bool) {
	dir, ok := hookTree(in)
	if !ok {
		return nil, "", false
	}
	spec, err := ParseWatchSpec(nil, dir)
	if err != nil {
		return nil, dir, false
	}
	spec.AddSessionScope(in.SessionID)
	watch, err := c.Watch(ctx, spec)
	if err != nil {
		return nil, dir, false
	}
	return watch, dir, true
}

// hookTree resolves the working tree a hook payload is about, reporting whether
// there is one. The payload's own cwd is the only authority worth having; the
// two fallbacks exist because a hook that cannot name a directory can neither
// register a watch nor tell whether a stream is already serving it.
func hookTree(in hookInput) (string, bool) {
	dir := in.CWD
	if dir == "" {
		dir = os.Getenv("CLAUDE_PROJECT_DIR")
	}
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return "", false
		}
	}
	return dir, true
}

// isNewSession reports whether a SessionStart is a fresh conversation rather
// than a resumed or compacted one. An unfamiliar source is treated as fresh:
// the alternative is replaying an hour of another session's CI into a new one,
// which is the worse mistake of the two.
func isNewSession(source string) bool {
	switch source {
	case "resume", "compact":
		return false
	default:
		return true
	}
}

// sessionStartContext words the one paragraph a session gets at startup: what
// the monitor is watching, and what will happen without anyone asking.
//
// There is one delivery channel now, so the paragraph names it and stops
// hedging. streaming only decides how confident that sentence is allowed to be:
// with a marker present the feed is already running, and without one the honest
// thing is to say events are being collected and name the command that reads
// them, rather than promise notifications that arrive only if the plugin is
// installed. A stream that has not yet written its marker reads as the
// unconfirmed case, which is the harmless way round — the sentence undersells,
// the delivery still happens.
func sessionStartContext(ctx context.Context, c *Client, w *Watch, streaming bool) string {
	var b strings.Builder
	b.WriteString("The shuck background monitor is running. ")
	switch {
	case w == nil:
		b.WriteString("It is starting up.")
	case w.Number > 0:
		fmt.Fprintf(&b, "It is following %s (branch %s).", w.Target(), w.Branch)
	case w.Note != "":
		fmt.Fprintf(&b, "It is following this working tree; %s.", w.Note)
	default:
		b.WriteString("It is following this working tree.")
	}
	b.WriteString(
		"\nIt tracks whichever pull request the current branch belongs to and retargets itself " +
			"when you switch branches or worktrees. New CI failures, review comments, and stale " +
			"GitHub Action pins ")
	if streaming {
		b.WriteString("will reach you automatically, as monitor notifications — you do not need " +
			"to poll for them, or to wait for them in a shell.")
	} else {
		b.WriteString("are being collected for you. They are delivered by the shuck plugin " +
			"monitor; if it is not running in this session, read them with " +
			"`shuck monitor events` rather than waiting in a shell.")
	}
	b.WriteString(" To ask it something directly, run `shuck monitor status`.")

	if st, err := c.Status(ctx, ""); err == nil && len(st.Targets) > 1 {
		fmt.Fprintf(&b, "\nIt is also watching: %s.", otherTargets(st, w))
	}
	return b.String()
}

// otherTargets lists the targets the monitor is following besides this
// session's own, so a session knows the feed may mention a PR it did not ask
// about.
func otherTargets(st *Status, w *Watch) string {
	mine := ""
	if w != nil {
		mine = w.Target()
	}
	var others []string
	for _, t := range st.Targets {
		if t.Target != mine {
			others = append(others, t.Target)
		}
	}
	return strings.Join(others, ", ")
}

// hookUserPrompt keeps the session's watch alive. It no longer delivers
// anything, and it is no longer installed: `monitors.json` is the channel, and
// this stays in the binary only because an older installed hooks.json still
// calls it.
//
// Delivering here as well was a duplicate waiting to happen, and it happened.
// The stand-down it used to rely on asked StreamServes whether a stream was
// following *this directory*, but a stream follows the *session* — so the
// moment an agent entered a worktree the two disagreed: the stream carried on
// delivering under the session's scope while the hook, seeing a directory no
// marker named, decided nobody was serving it and delivered the same CI failure
// a second time. Every event in that session arrived twice.
//
// The fix is not a better predicate. Two channels racing to be the one that
// speaks is the bug; one channel is the design. The read is gone, so this
// cannot duplicate however wrong any prediction about the other channel gets.
//
// Registering is kept because it costs one round trip and is the only thing
// here that was ever this hook's own: a prompt is the earliest moment a session
// that has moved can be re-watched, sooner than the tool call that follows it.
func hookUserPrompt(ctx context.Context, c *Client, in hookInput) *hookOutput {
	c.AutoStart = false // a prompt is not the moment to start a daemon
	registerSessionTree(ctx, c, in)
	return nil
}

// pushPattern matches the commands that change what CI will say. After one of
// these the interesting answer is seconds away, and waiting out a poll interval
// to ask for it is latency for nothing.
var pushPattern = regexp.MustCompile(`(?m)\b(git\s+push|gh\s+pr\s+create|gh\s+pr\s+ready|gh\s+workflow\s+run|gh\s+run\s+rerun)\b`)

// hookPostToolUse pokes the monitor after a push so the new run is picked up
// immediately, and takes the first chance in the session to seed its cursor.
func hookPostToolUse(ctx context.Context, c *Client, in hookInput) *hookOutput {
	c.AutoStart = false // a tool call is not the moment to start a daemon

	// Re-register the session's directory before anything else, and for every
	// tool rather than only for Bash. This is the hook that fires most often, so
	// it is the one that notices a moved session soonest — and the move that
	// matters most is the agent entering a git worktree, which it does with a
	// dedicated tool and not with a shell command. Registering an unchanged
	// directory is what the daemon already does on every prompt; it costs one
	// socket round trip and keeps the watch alive.
	if _, _, ok := registerSessionTree(ctx, c, in); !ok {
		return nil
	}

	if !strings.EqualFold(in.ToolName, "Bash") {
		return nil
	}

	// Seed here as well as in Stop, and before the push is even recognized,
	// because the seed has to land *before* whatever the turn is about to
	// cause. A turn that pushes and then keeps working is exactly when a
	// failure lands mid-turn, and a session first seeded at the end of that
	// turn would seed straight past the failure it was meant to gate on.
	seedSessionCursor(ctx, c, in)

	var input struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(in.ToolInput, &input) != nil || !pushPattern.MatchString(input.Command) {
		return nil
	}
	_, _ = c.Poke(ctx, "")
	return nil
}

// seedSessionCursor starts a session the daemon has never seen where that
// session began.
//
// A consumer with no cursor is served from the head of the retained journal, so
// something has to seed it, or the first read of every session hands over hours
// of history — most of it another branch's, none of it this session's doing.
// SessionStart used to do that at the one moment that is unambiguously the
// start; it is no longer wired, so it falls to whichever hook sees the session
// id first, which can be an hour into the conversation.
//
// Seeding *at that moment* would be the bug this exists to avoid, in reverse: a
// failure that landed during the first turn would be seeded straight past, and
// the Stop gate would never see the very event it exists for — the case the
// skill promises is covered ("seeing a ci.failed is delivery, not
// acknowledgement"). So the position comes from the session's own stream, whose
// lifetime is the session's: StreamOrigin is where the delivery channel for
// this tree started reading, which is where the session opened. Only a tree
// nothing is streaming falls back to the present, and there the exposure ends
// at the first hook rather than at the end of the first turn.
//
// IfNew is what makes this safe to call from more than one place and on every
// event: a session that already has a cursor keeps it, including one Stop has
// deliberately seeked forward.
//
// A failure is nothing to report. It means no daemon is reachable, and the read
// that follows fails the same way.
func seedSessionCursor(ctx context.Context, c *Client, in hookInput) {
	if in.SessionID == "" {
		return
	}
	if dir, ok := hookTree(in); ok {
		if origin, known := StreamOrigin(dir); known {
			_, _ = c.SeekNewAt(ctx, in.SessionID, origin)
			return
		}
	}
	_, _ = c.SeekNew(ctx, in.SessionID)
}

// hookStop does nothing, deliberately, and that is the whole of its
// implementation.
//
// It used to be the finish gate: it peeked at the session's cursor and, if the
// monitor was holding a red build or an unanswered review comment, blocked the
// turn and handed the batch over. The gate is gone. What it protected against —
// an agent reading a `ci.failed` notification and finishing anyway — is real,
// but it was never what the gate actually did in practice. Because it blocked
// on the last *known* actionable event, with no notion of whether a newer head
// had superseded it, its everyday behavior was to re-hand a session the
// failure it had already fixed and pushed, one poll interval before the pass
// arrived. That cost a turn every time, and the case it existed for cost
// nothing in any session where the agent simply acted on the notification.
//
// So the stream is now the only thing that speaks, on purpose. Delivery without
// enforcement is the trade: `shuck monitor stream` says what happened, and what
// the agent does about it is the agent's business.
//
// The entry point stays because removing it is not how you remove a hook. An
// installed copy of an older hooks.json still execs `shuck monitor hook stop`,
// and the binary — not the manifest — is what has to be authoritative, or
// upgrading shuck would leave the gate standing for everyone who had not also
// re-installed the plugin. Returning nil here is what actually retires it.
func hookStop(context.Context, *Client, hookInput) *hookOutput { return nil }

// hookSessionEnd retires the session's cursor. It is a courtesy — cursors are
// pruned as they age out of the journal anyway — but it keeps the state file
// honest for anyone reading it.
func hookSessionEnd(ctx context.Context, c *Client, in hookInput) *hookOutput {
	if in.SessionID == "" {
		return nil
	}
	c.AutoStart = false
	_, _ = c.Seek(ctx, in.SessionID)
	return nil
}

// context_ builds a context-injecting hook response. The trailing underscore
// keeps it from colliding with the context package, which every function here
// also needs.
func context_(event HookEvent, text string) *hookOutput {
	if text == "" {
		return nil
	}
	return &hookOutput{Specific: &hookSpecificOut{
		HookEventName:     hookEventNames[event],
		AdditionalContext: text,
	}}
}
