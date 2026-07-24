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

	"github.com/justanotherspy/shuck/internal/distil"
)

// feedLimit bounds the text one hook injects into a session. Claude Code
// truncates a large additionalContext silently, so shuck does the cut itself
// and says where the rest is — a summary that ends mid-sentence is worse than
// one that ends with a command you can run.
const feedLimit = 3500

// hookTimeout bounds a hook's whole interaction with the daemon. Hooks run
// between the user pressing enter and the agent starting to think; a monitor
// that is wedged must cost that moment a fraction of a second, not seconds.
const hookTimeout = 3 * time.Second

// HookEvent names the Claude Code hook a shuck invocation is serving.
type HookEvent string

// The hooks shuck installs. Each maps to one Claude Code hook event, and
// together they are the whole integration: register on start, feed on every
// prompt, re-check right after a push, and speak up if the agent tries to
// finish with a red build.
const (
	// HookSessionStart registers the session's working tree and reports what
	// the monitor is already watching.
	HookSessionStart HookEvent = "session-start"
	// HookUserPromptSubmit delivers whatever the monitor has noticed since the
	// session last looked. This is the hook that makes the monitor feel live.
	HookUserPromptSubmit HookEvent = "user-prompt-submit"
	// HookPostToolUse notices a push and asks the monitor to re-check now,
	// instead of at the next interval.
	HookPostToolUse HookEvent = "post-tool-use"
	// HookStop refuses to let the agent finish quietly on a red build or an
	// unanswered review comment.
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

// hookOutput is what shuck writes back. The Stop decision is emitted both at
// the top level and inside hookSpecificOutput: the two shapes have both been
// current, unknown fields are ignored by the reader, and a monitor that is
// wrong about which one this build wants is a monitor that silently stops
// working.
type hookOutput struct {
	Decision string           `json:"decision,omitempty"`
	Reason   string           `json:"reason,omitempty"`
	Specific *hookSpecificOut `json:"hookSpecificOutput,omitempty"`
}

// hookSpecificOut is the per-event half of a hook response.
type hookSpecificOut struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext,omitempty"`
	Decision          string `json:"decision,omitempty"`
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
// token, a malformed payload, an unknown event — writes nothing and exits 0,
// because a background convenience must never be the reason a session stalls
// or a prompt is rejected. The only thing a broken monitor should cost you is
// the monitoring.
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
	dir := in.CWD
	if dir == "" {
		dir = os.Getenv("CLAUDE_PROJECT_DIR")
	}
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return nil
		}
	}

	spec, err := ParseWatchSpec(nil, dir)
	if err != nil {
		return nil
	}
	watch, err := c.Watch(ctx, spec)
	if err != nil {
		// No daemon and none could be started — most often a missing GitHub
		// token. Say so once, quietly, rather than failing the session.
		return context_(HookSessionStart, fmt.Sprintf(
			"The shuck background monitor is not running (%v). CI and review feedback will "+
				"not arrive on its own; use `shuck <pr>` or the shuck MCP tools when you need it.", err))
	}
	if _, err := c.Seek(ctx, in.SessionID); err != nil {
		return nil
	}

	return context_(HookSessionStart, sessionStartContext(ctx, c, watch))
}

// sessionStartContext words the one paragraph a session gets at startup: what
// the monitor is watching, and what will happen without anyone asking.
func sessionStartContext(ctx context.Context, c *Client, w *Watch) string {
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
			"GitHub Action pins will be delivered to you automatically as <shuck-monitor> blocks — " +
			"you do not need to poll for them. To ask it something directly, use the monitor_status " +
			"/ monitor_events / monitor_watch MCP tools, or `shuck monitor status`.")

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

// hookUserPrompt delivers what the monitor has noticed since this session last
// looked. It is the hook that makes the whole thing feel live: no polling, no
// tool call, the news simply arrives with the next thing the user says.
func hookUserPrompt(ctx context.Context, c *Client, in hookInput) *hookOutput {
	c.AutoStart = false // a prompt is not the moment to start a daemon
	events, _, err := c.Events(ctx, Request{Consumer: in.SessionID})
	if err != nil || len(events) == 0 {
		return nil
	}
	return context_(HookUserPromptSubmit, capFeed(FormatFeed(events)))
}

// pushPattern matches the commands that change what CI will say. After one of
// these the interesting answer is seconds away, and waiting out a poll interval
// to ask for it is latency for nothing.
var pushPattern = regexp.MustCompile(`(?m)\b(git\s+push|gh\s+pr\s+create|gh\s+pr\s+ready|gh\s+workflow\s+run|gh\s+run\s+rerun)\b`)

// hookPostToolUse pokes the monitor after a push so the new run is picked up
// immediately.
func hookPostToolUse(ctx context.Context, c *Client, in hookInput) *hookOutput {
	if !strings.EqualFold(in.ToolName, "Bash") {
		return nil
	}
	var input struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(in.ToolInput, &input) != nil || !pushPattern.MatchString(input.Command) {
		return nil
	}
	c.AutoStart = false
	_, _ = c.Poke(ctx, "")
	return nil
}

// hookStop is the hook that closes the loop. When the agent is about to finish
// and the monitor is holding something that wants doing — a red build, a
// reviewer's comment — it hands that over and asks for another turn.
//
// Three things keep it from becoming a trap. It stands down the moment
// stop_hook_active is set, so it can never loop. It only blocks on events that
// actually ask for something, so a passing build never delays a finish. And it
// peeks rather than drains, so events it decides not to act on are still there
// for the next prompt.
func hookStop(ctx context.Context, c *Client, in hookInput) *hookOutput {
	if in.StopHookActive || os.Getenv("SHUCK_MONITOR_NO_STOP") != "" {
		return nil
	}
	c.AutoStart = false

	events, _, err := c.Events(ctx, Request{Consumer: in.SessionID, Peek: true})
	if err != nil || len(events) == 0 {
		return nil
	}
	actionable := make([]Event, 0, len(events))
	for _, e := range events {
		if e.Severity() == SeverityAction {
			actionable = append(actionable, e)
		}
	}
	if len(actionable) == 0 {
		return nil
	}

	// Consume everything up to and including what we are about to hand over,
	// so the next prompt does not deliver it again.
	if _, err := c.Do(ctx, Request{Op: OpSeek, Consumer: in.SessionID, Since: events[len(events)-1].ID}); err != nil {
		return nil
	}

	reason := capFeed(FormatFeed(actionable) +
		"\n\nAct on this before finishing: fix what is broken and push, or reply to the reviewer. " +
		"If it is genuinely not yours to fix, say so and stop.")
	return &hookOutput{
		Decision: "block",
		Reason:   reason,
		Specific: &hookSpecificOut{
			HookEventName:     hookEventNames[HookStop],
			AdditionalContext: reason,
			Decision:          "block",
		},
	}
}

// hookSessionEnd retires the session's cursor. It is a courtesy — cursors are
// pruned as they age out of the journal anyway — but it keeps the state file
// honest for anyone reading it.
func hookSessionEnd(ctx context.Context, c *Client, in hookInput) *hookOutput {
	c.AutoStart = false
	if in.SessionID != "" {
		_, _ = c.Seek(ctx, in.SessionID)
	}
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

// capFeed trims injected context to what a hook may deliver, pointing at the
// command that has the rest.
func capFeed(text string) string {
	capped, _ := distil.CapSummary(text, feedLimit,
		"… truncated — run `shuck monitor events --all` for the full detail.\n</shuck-monitor>")
	return capped
}
