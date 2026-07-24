package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// runHook drives one hook against a live daemon, with SHUCK_HOME pointed at
// the daemon's directory so the hook's own client finds it. It returns the
// parsed response, or nil when the hook stayed silent.
func runHook(t *testing.T, event HookEvent, payload any, dir string) *hookOutput {
	t.Helper()
	t.Setenv("SHUCK_HOME", dir)

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := RunHook(context.Background(), event, bytes.NewReader(raw), &out); code != 0 {
		t.Fatalf("hook exited %d — a hook must never fail a session", code)
	}
	if out.Len() == 0 {
		return nil
	}
	var parsed hookOutput
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("hook wrote something that is not hook JSON: %v\n%s", err, out.String())
	}
	return &parsed
}

// hookDaemon starts a daemon whose SHUCK_HOME-visible directory the hooks will
// find. The daemon lives under <home>/monitor, which is where Dir() looks.
func hookDaemon(t *testing.T, c prClient) (d *Daemon, home string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("SHUCK_HOME", home)

	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}

	original := newPRClient
	newPRClient = func(string) prClient { return c }
	t.Cleanup(func() { newPRClient = original })

	d, err = newDaemon(dir, Options{Version: "test", NoPins: true, WatchTTL: -1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.serve(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	client := &Client{dir: dir}
	waitFor(t, func() bool { return client.Running(context.Background()) })
	return d, home
}

func TestHookSessionStartRegistersTheTree(t *testing.T) {
	c := newFakeClient()
	c.openPR = 42
	d, home := hookDaemon(t, c)

	tree := treeAt(t, "feature")
	out := runHook(t, HookSessionStart, map[string]any{
		"session_id": "sess-1",
		"cwd":        tree,
		"source":     "startup",
	}, home)

	if out == nil || out.Specific == nil {
		t.Fatal("session start should introduce the monitor to the session")
	}
	if out.Specific.HookEventName != "SessionStart" {
		t.Errorf("hookEventName = %q", out.Specific.HookEventName)
	}
	ctx := out.Specific.AdditionalContext
	for _, want := range []string{"background monitor is running", "retargets itself", "monitor_events"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("context is missing %q:\n%s", want, ctx)
		}
	}
	if _, ok := d.watches.Get(TreeWatchID(tree)); !ok {
		t.Error("the session's working tree was not registered")
	}
}

// TestHookSessionStartDoesNotReplayHistory is the rule that keeps a new session
// from being handed the previous one's backlog as if it had just happened.
func TestHookSessionStartDoesNotReplayHistory(t *testing.T) {
	c := newFakeClient()
	c.openPR = 42
	d, home := hookDaemon(t, c)
	d.publish([]Event{{Kind: KindCIFailed, Title: "an hour-old failure"}})

	tree := treeAt(t, "feature")
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": tree}, home)

	if out := runHook(t, HookUserPromptSubmit, map[string]any{"session_id": "sess-1"}, home); out != nil {
		t.Errorf("the first prompt replayed history:\n%s", out.Specific.AdditionalContext)
	}
}

func TestHookUserPromptDeliversNewEvents(t *testing.T) {
	d, home := hookDaemon(t, newFakeClient())
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": treeAt(t, "feature")}, home)

	d.publish([]Event{{Kind: KindCIFailed, Target: "o/r#7", Title: "test failed", Body: "the error"}})

	out := runHook(t, HookUserPromptSubmit, map[string]any{"session_id": "sess-1"}, home)
	if out == nil || out.Specific == nil {
		t.Fatal("a new event should be delivered on the next prompt")
	}
	if out.Specific.HookEventName != "UserPromptSubmit" {
		t.Errorf("hookEventName = %q", out.Specific.HookEventName)
	}
	for _, want := range []string{"<shuck-monitor>", "test failed", "the error"} {
		if !strings.Contains(out.Specific.AdditionalContext, want) {
			t.Errorf("delivered context is missing %q:\n%s", want, out.Specific.AdditionalContext)
		}
	}

	// Delivered once: the second prompt says nothing.
	if out := runHook(t, HookUserPromptSubmit, map[string]any{"session_id": "sess-1"}, home); out != nil {
		t.Errorf("the same event was delivered twice:\n%s", out.Specific.AdditionalContext)
	}
}

func TestHookUserPromptIsSilentWhenNothingHappened(t *testing.T) {
	_, home := hookDaemon(t, newFakeClient())
	if out := runHook(t, HookUserPromptSubmit, map[string]any{"session_id": "sess-1"}, home); out != nil {
		t.Errorf("a quiet monitor should write nothing, got %+v", out)
	}
}

func TestHookUserPromptCapsWhatItInjects(t *testing.T) {
	d, home := hookDaemon(t, newFakeClient())
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": treeAt(t, "feature")}, home)

	// Claude Code truncates a large additionalContext silently, so shuck has
	// to do the cut itself and say where the rest is.
	d.publish([]Event{{
		Kind:  KindCIFailed,
		Title: "test failed",
		Body:  strings.Repeat("a very long line of log output\n", 500),
	}})

	out := runHook(t, HookUserPromptSubmit, map[string]any{"session_id": "sess-1"}, home)
	if out == nil {
		t.Fatal("expected a delivery")
	}
	got := out.Specific.AdditionalContext
	if len(got) > feedLimit {
		t.Errorf("injected %d bytes, want at most %d", len(got), feedLimit)
	}
	if !strings.Contains(got, "shuck monitor events --all") {
		t.Errorf("a truncated feed must say where the rest is:\n%s", got[len(got)-300:])
	}
}

func TestHookPostToolUsePokesAfterAPush(t *testing.T) {
	c := newFakeClient()
	c.pr = openPR("abc")
	c.fingerprint = "fp"
	d, home := hookDaemon(t, c)

	d.watches.Add(Watch{ID: "pr:o/r#7", Kind: WatchPR, Owner: "o", Repo: "r", Number: 7})
	// Create the target, then park its next check well into the future so a
	// poke has something to pull forward.
	d.due(time.Now())
	d.targets["o/r#7"].NextPoll = time.Now().Add(time.Hour)

	before := d.targets["o/r#7"].NextPoll
	runHook(t, HookPostToolUse, map[string]any{
		"tool_name":  "Bash",
		"tool_input": map[string]string{"command": "git push -u origin HEAD"},
	}, home)

	if !d.targets["o/r#7"].NextPoll.Before(before) {
		t.Error("a push should bring the next check forward")
	}
}

func TestHookPostToolUseIgnoresEverythingElse(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]any
	}{
		{"a different tool", map[string]any{"tool_name": "Read", "tool_input": map[string]string{"file_path": "x"}}},
		{"a command that is not a push", map[string]any{"tool_name": "Bash", "tool_input": map[string]string{"command": "go test ./..."}}},
		{"a command that only mentions pushing", map[string]any{"tool_name": "Bash", "tool_input": map[string]string{"command": "echo pushed"}}},
		{"unparseable tool input", map[string]any{"tool_name": "Bash", "tool_input": "not an object"}},
	}
	c := newFakeClient()
	c.pr = openPR("abc")
	c.fingerprint = "fp"
	d, home := hookDaemon(t, c)
	d.watches.Add(Watch{ID: "pr:o/r#7", Kind: WatchPR, Owner: "o", Repo: "r", Number: 7})
	d.due(time.Now())
	d.targets["o/r#7"].NextPoll = time.Now().Add(time.Hour)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := d.targets["o/r#7"].NextPoll
			runHook(t, HookPostToolUse, tt.payload, home)
			if !d.targets["o/r#7"].NextPoll.Equal(before) {
				t.Error("this should not have poked the monitor")
			}
		})
	}
}

func TestHookStopBlocksOnActionableEvents(t *testing.T) {
	d, home := hookDaemon(t, newFakeClient())
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": treeAt(t, "feature")}, home)

	d.publish([]Event{{Kind: KindCIFailed, Target: "o/r#7", Title: "test failed", Body: "the error"}})

	out := runHook(t, HookStop, map[string]any{"session_id": "sess-1"}, home)
	if out == nil {
		t.Fatal("the agent should not finish quietly on a red build")
	}
	if out.Decision != "block" {
		t.Errorf("decision = %q, want block", out.Decision)
	}
	// Both spellings are emitted so the hook works whichever one this build
	// reads.
	if out.Specific == nil || out.Specific.Decision != "block" {
		t.Error("the decision should also ride in hookSpecificOutput")
	}
	if !strings.Contains(out.Reason, "test failed") {
		t.Errorf("reason should carry the event:\n%s", out.Reason)
	}
	if !strings.Contains(out.Reason, "before finishing") {
		t.Errorf("reason should say what is expected:\n%s", out.Reason)
	}
}

// TestHookStopStandsDownWhenAlreadyActive is the loop guard. Without it a Stop
// hook that keeps finding something to say never lets the agent finish.
func TestHookStopStandsDownWhenAlreadyActive(t *testing.T) {
	d, home := hookDaemon(t, newFakeClient())
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": treeAt(t, "feature")}, home)
	d.publish([]Event{{Kind: KindCIFailed, Title: "test failed"}})

	if out := runHook(t, HookStop, map[string]any{"session_id": "sess-1", "stop_hook_active": true}, home); out != nil {
		t.Errorf("a Stop hook already in a block must stand down, got %+v", out)
	}
}

func TestHookStopIgnoresInformationalEvents(t *testing.T) {
	d, home := hookDaemon(t, newFakeClient())
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": treeAt(t, "feature")}, home)

	d.publish([]Event{{Kind: KindCIPassed, Title: "all checks passed"}})

	if out := runHook(t, HookStop, map[string]any{"session_id": "sess-1"}, home); out != nil {
		t.Fatalf("a passing build must not delay a finish, got %+v", out)
	}
	// And because Stop peeks rather than drains, the news is still waiting for
	// the next prompt.
	if out := runHook(t, HookUserPromptSubmit, map[string]any{"session_id": "sess-1"}, home); out == nil {
		t.Error("the event Stop declined to act on should still be delivered later")
	}
}

func TestHookStopDoesNotRepeatWhatItHandedOver(t *testing.T) {
	d, home := hookDaemon(t, newFakeClient())
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": treeAt(t, "feature")}, home)
	d.publish([]Event{{Kind: KindCIFailed, Title: "test failed"}})

	if out := runHook(t, HookStop, map[string]any{"session_id": "sess-1"}, home); out == nil {
		t.Fatal("expected a block")
	}
	if out := runHook(t, HookUserPromptSubmit, map[string]any{"session_id": "sess-1"}, home); out != nil {
		t.Errorf("the blocked-on event was delivered again:\n%s", out.Specific.AdditionalContext)
	}
}

func TestHookStopRespectsItsOptOut(t *testing.T) {
	d, home := hookDaemon(t, newFakeClient())
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": treeAt(t, "feature")}, home)
	d.publish([]Event{{Kind: KindCIFailed, Title: "test failed"}})

	t.Setenv("SHUCK_MONITOR_NO_STOP", "1")
	if out := runHook(t, HookStop, map[string]any{"session_id": "sess-1"}, home); out != nil {
		t.Errorf("SHUCK_MONITOR_NO_STOP should silence just this hook, got %+v", out)
	}
}

func TestHooksRespectTheGlobalOptOut(t *testing.T) {
	d, home := hookDaemon(t, newFakeClient())
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": treeAt(t, "feature")}, home)
	d.publish([]Event{{Kind: KindCIFailed, Title: "test failed"}})

	t.Setenv("SHUCK_MONITOR_DISABLE", "1")
	for _, event := range []HookEvent{HookSessionStart, HookUserPromptSubmit, HookPostToolUse, HookStop, HookSessionEnd} {
		if out := runHook(t, event, map[string]any{"session_id": "sess-1"}, home); out != nil {
			t.Errorf("%s spoke up despite SHUCK_MONITOR_DISABLE", event)
		}
	}
}

func TestHookSessionEnd(t *testing.T) {
	d, home := hookDaemon(t, newFakeClient())
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": treeAt(t, "feature")}, home)
	d.publish([]Event{{Kind: KindCIFailed, Title: "test failed"}})

	if out := runHook(t, HookSessionEnd, map[string]any{"session_id": "sess-1"}, home); out != nil {
		t.Errorf("session end should write nothing, got %+v", out)
	}
	if got := d.journal.Pending("sess-1"); got != 0 {
		t.Errorf("Pending = %d after session end, want the cursor retired", got)
	}
}

// TestHooksSurviveNoDaemon is the promise the whole integration rests on: with
// nothing running, every hook exits 0 and costs the session nothing.
func TestHooksSurviveNoDaemon(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SHUCK_HOME", home)

	for _, event := range []HookEvent{HookUserPromptSubmit, HookPostToolUse, HookStop, HookSessionEnd, "unknown-event"} {
		var out bytes.Buffer
		code := RunHook(context.Background(), event, strings.NewReader(`{"session_id":"s"}`), &out)
		if code != 0 {
			t.Errorf("%s exited %d with no daemon, want 0", event, code)
		}
		if out.Len() != 0 {
			t.Errorf("%s wrote %q with no daemon, want silence", event, out.String())
		}
	}
}

func TestHookTolerateGarbageInput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SHUCK_HOME", home)

	var out bytes.Buffer
	if code := RunHook(context.Background(), HookUserPromptSubmit, strings.NewReader("not json at all"), &out); code != 0 {
		t.Errorf("exit = %d on garbage input, want 0", code)
	}
	if code := RunHook(context.Background(), HookUserPromptSubmit, nil, &out); code != 0 {
		t.Errorf("exit = %d on no input at all, want 0", code)
	}
}

func TestCapFeed(t *testing.T) {
	short := "<shuck-monitor>\nnothing much\n</shuck-monitor>"
	if got := capFeed(short); got != short {
		t.Errorf("a short feed should pass through unchanged")
	}
	long := strings.Repeat("a line of text\n", 1000)
	got := capFeed(long)
	if len(got) > feedLimit {
		t.Errorf("capped feed is %d bytes, want at most %d", len(got), feedLimit)
	}
	if !strings.HasSuffix(got, "</shuck-monitor>") {
		t.Errorf("a truncated feed should still close its block:\n%s", got[len(got)-120:])
	}
}

func TestHookTimeoutIsShort(t *testing.T) {
	// Hooks run between the user pressing enter and the agent starting to
	// think. A wedged monitor must cost that moment a fraction of a second.
	if hookTimeout > 5*time.Second {
		t.Errorf("hookTimeout = %s, too long to sit in front of a prompt", hookTimeout)
	}
}
