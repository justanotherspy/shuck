package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
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
	for _, want := range []string{"background monitor is running", "retargets itself", "shuck monitor events"} {
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
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": tree, "source": "startup"}, home)

	if out := runHook(t, HookUserPromptSubmit, map[string]any{"session_id": "sess-1"}, home); out != nil {
		t.Errorf("the first prompt replayed history:\n%s", out.Specific.AdditionalContext)
	}
}

func TestHookUserPromptDeliversNewEvents(t *testing.T) {
	d, home := hookDaemon(t, newFakeClient())
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": treeAt(t, "feature"), "source": "startup"}, home)

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
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": treeAt(t, "feature"), "source": "startup"}, home)

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
		{"no tool input at all", map[string]any{"tool_name": "Bash"}},
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
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": treeAt(t, "feature"), "source": "startup"}, home)

	d.publish([]Event{{Kind: KindCIFailed, Target: "o/r#7", Title: "test failed", Body: "the error"}})

	out := runHook(t, HookStop, map[string]any{"session_id": "sess-1"}, home)
	if out == nil {
		t.Fatal("the agent should not finish quietly on a red build")
	}
	if out.Decision != "block" {
		t.Errorf("decision = %q, want block", out.Decision)
	}
	// One copy of the reason, not two: emitting it in both shapes risks it
	// being injected twice.
	if out.Specific != nil {
		t.Errorf("the Stop response should carry only the top-level reason, got %+v", out.Specific)
	}
	if !strings.Contains(out.Reason, "test failed") {
		t.Errorf("reason should carry the event:\n%s", out.Reason)
	}
	// The instruction leads, so truncation can never remove it.
	if !strings.HasPrefix(out.Reason, "Before finishing:") {
		t.Errorf("reason should open with what is expected:\n%s", out.Reason)
	}
}

// TestHookStopHandsOverTheWholeBatch covers a defect a review found: blocking
// on a failure while consuming the passing re-run that followed it would leave
// the agent acting on news the monitor already knew was out of date.
func TestHookStopHandsOverTheWholeBatch(t *testing.T) {
	d, home := hookDaemon(t, newFakeClient())
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": treeAt(t, "feature"), "source": "startup"}, home)

	d.publish([]Event{
		{Kind: KindCIFailed, Title: "test failed"},
		{Kind: KindCIPassed, Title: "all checks passed on the re-run"},
	})

	out := runHook(t, HookStop, map[string]any{"session_id": "sess-1"}, home)
	if out == nil {
		t.Fatal("expected a block on the failure")
	}
	if !strings.Contains(out.Reason, "all checks passed on the re-run") {
		t.Errorf("the passing re-run was consumed but not handed over:\n%s", out.Reason)
	}
}

// TestHookStopIgnoresAMonitorError checks that a failed poll does not hold a
// finished turn open: it is the monitor's problem, not the agent's.
func TestHookStopIgnoresAMonitorError(t *testing.T) {
	d, home := hookDaemon(t, newFakeClient())
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": treeAt(t, "feature"), "source": "startup"}, home)

	d.publish([]Event{{Kind: KindError, Title: "could not check o/r#7", Body: "dial tcp: timeout"}})

	if out := runHook(t, HookStop, map[string]any{"session_id": "sess-1"}, home); out != nil {
		t.Errorf("a transient poll failure must not block the turn, got %+v", out)
	}
}

// TestHookSessionStartKeepsPendingEventsOnResume covers the other half of the
// fast-forward rule: SessionStart also fires on resume and compaction, and
// seeking then would throw away what the ongoing conversation has not seen.
func TestHookSessionStartKeepsPendingEventsOnResume(t *testing.T) {
	d, home := hookDaemon(t, newFakeClient())
	tree := treeAt(t, "feature")
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": tree, "source": "startup"}, home)

	d.publish([]Event{{Kind: KindCIFailed, Title: "test failed"}})

	for _, source := range []string{"resume", "compact"} {
		runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": tree, "source": source}, home)
		if got := d.journal.Pending("sess-1"); got != 1 {
			t.Fatalf("source %q left %d pending, want the failure still waiting", source, got)
		}
	}
	if out := runHook(t, HookUserPromptSubmit, map[string]any{"session_id": "sess-1"}, home); out == nil {
		t.Error("the pending failure should still be delivered after a resume")
	}
}

// TestHooksIgnoreAnUnidentifiedSession is the guard on the worst failure mode
// of a malformed payload: with no session id there is no cursor, and a
// cursorless read hands over the whole journal on every prompt.
func TestHooksIgnoreAnUnidentifiedSession(t *testing.T) {
	d, home := hookDaemon(t, newFakeClient())
	d.publish([]Event{{Kind: KindCIFailed, Title: "test failed"}})

	for _, event := range []HookEvent{HookUserPromptSubmit, HookStop, HookSessionEnd} {
		if out := runHook(t, event, map[string]any{"cwd": "/tmp"}, home); out != nil {
			t.Errorf("%s spoke up for a session it cannot identify: %+v", event, out)
		}
	}
}

// TestHookStopStandsDownWhenAlreadyActive is the loop guard. Without it a Stop
// hook that keeps finding something to say never lets the agent finish.
func TestHookStopStandsDownWhenAlreadyActive(t *testing.T) {
	d, home := hookDaemon(t, newFakeClient())
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": treeAt(t, "feature"), "source": "startup"}, home)
	d.publish([]Event{{Kind: KindCIFailed, Title: "test failed"}})

	if out := runHook(t, HookStop, map[string]any{"session_id": "sess-1", "stop_hook_active": true}, home); out != nil {
		t.Errorf("a Stop hook already in a block must stand down, got %+v", out)
	}
}

func TestHookStopIgnoresInformationalEvents(t *testing.T) {
	d, home := hookDaemon(t, newFakeClient())
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": treeAt(t, "feature"), "source": "startup"}, home)

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

// TestHookStopIgnoresAnApprovingReview is the end-to-end half of the verdict
// rule. Every submitted review carries the same kind, so without the verdict a
// reviewer saying "LGTM" holds the turn open and the agent is handed an
// approval under the words "address this as part of the current task".
func TestHookStopIgnoresAnApprovingReview(t *testing.T) {
	d, home := hookDaemon(t, newFakeClient())
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": treeAt(t, "feature"), "source": "startup"}, home)

	d.publish([]Event{reviewEvent("octocat", "approved", "o/r#7")})

	if out := runHook(t, HookStop, map[string]any{"session_id": "sess-1"}, home); out != nil {
		t.Fatalf("an approval must not delay a finish, got %+v", out)
	}
	// And because Stop peeked rather than drained, the good news is still there
	// for the next prompt — declining to block is not declining to deliver.
	if out := runHook(t, HookUserPromptSubmit, map[string]any{"session_id": "sess-1"}, home); out == nil {
		t.Error("the approval should still be delivered on the next prompt")
	}
}

// TestHookStopBlocksOnRequestedChanges is the other side of that rule, and the
// reason it is keyed on the verdict rather than on the kind: a review that asks
// for something still has to stop the agent walking away from it.
func TestHookStopBlocksOnRequestedChanges(t *testing.T) {
	d, home := hookDaemon(t, newFakeClient())
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": treeAt(t, "feature"), "source": "startup"}, home)

	d.publish([]Event{reviewEvent("octocat", "changes_requested", "o/r#7")})

	out := runHook(t, HookStop, map[string]any{"session_id": "sess-1"}, home)
	if out == nil || out.Decision != "block" {
		t.Fatalf("a reviewer asking for changes should hold the turn open, got %+v", out)
	}
}

// TestHookStopIgnoresStalePins covers the same unrequested-work rule for pin
// drift: a superseded action pin is repo hygiene in a checkout the user may
// never have mentioned, and refusing to let a turn end over one is the monitor
// hijacking the session for its own errand.
func TestHookStopIgnoresStalePins(t *testing.T) {
	d, home := hookDaemon(t, newFakeClient())
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": treeAt(t, "feature"), "source": "startup"}, home)

	d.publish([]Event{{
		Kind:   KindPinsStale,
		Target: "o/r",
		Title:  "2 workflow action references need attention",
		Body:   "ci.yml:12  actions/checkout@v4 → pin to a SHA",
	}})

	if out := runHook(t, HookStop, map[string]any{"session_id": "sess-1"}, home); out != nil {
		t.Fatalf("stale pins must not delay a finish, got %+v", out)
	}
	// Demoting the severity must not silence the finding — the prompt hook
	// delivers every event whatever its severity.
	out := runHook(t, HookUserPromptSubmit, map[string]any{"session_id": "sess-1"}, home)
	if out == nil || out.Specific == nil {
		t.Fatal("pin drift should still be delivered on the next prompt")
	}
	if !strings.Contains(out.Specific.AdditionalContext, "actions/checkout@v4") {
		t.Errorf("the pin finding lost its detail:\n%s", out.Specific.AdditionalContext)
	}
}

func TestHookStopDoesNotRepeatWhatItHandedOver(t *testing.T) {
	d, home := hookDaemon(t, newFakeClient())
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": treeAt(t, "feature"), "source": "startup"}, home)
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
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": treeAt(t, "feature"), "source": "startup"}, home)
	d.publish([]Event{{Kind: KindCIFailed, Title: "test failed"}})

	t.Setenv("SHUCK_MONITOR_NO_STOP", "1")
	if out := runHook(t, HookStop, map[string]any{"session_id": "sess-1"}, home); out != nil {
		t.Errorf("SHUCK_MONITOR_NO_STOP should silence just this hook, got %+v", out)
	}
}

func TestHooksRespectTheGlobalOptOut(t *testing.T) {
	d, home := hookDaemon(t, newFakeClient())
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": treeAt(t, "feature"), "source": "startup"}, home)
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
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": treeAt(t, "feature"), "source": "startup"}, home)
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
	// A stdin that errors mid-read is the same story as no stdin: whatever
	// could not be read simply is not known, and an unidentifiable payload is
	// one the hook stays out of.
	if code := RunHook(context.Background(), HookUserPromptSubmit, iotest.ErrReader(errors.New("boom")), &out); code != 0 {
		t.Errorf("exit = %d on an unreadable stdin, want 0", code)
	}
	if out.Len() != 0 {
		t.Errorf("a payload shuck cannot read produced %q, want silence", out.String())
	}
}

// TestHooksSurviveAnUnusableHome covers the failure that lands before a hook
// has a client at all: a cache directory that cannot be created. Nothing is
// reachable from there — not even a daemon that is running perfectly well — so
// every hook has to fall silent, SessionStart included, rather than announce a
// state it could not check.
func TestHooksSurviveAnUnusableHome(t *testing.T) {
	// A regular file where the cache directory belongs: the MkdirAll under it
	// fails on every platform, and nothing is spawned on the way.
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHUCK_HOME", blocked)

	for _, event := range []HookEvent{
		HookSessionStart, HookUserPromptSubmit, HookPostToolUse, HookStop, HookSessionEnd,
	} {
		var out bytes.Buffer
		code := RunHook(context.Background(), event, strings.NewReader(`{"session_id":"s","cwd":"."}`), &out)
		if code != 0 {
			t.Errorf("%s exited %d with an unusable SHUCK_HOME, want 0", event, code)
		}
		if out.Len() != 0 {
			t.Errorf("%s wrote %q with an unusable SHUCK_HOME, want silence", event, out.String())
		}
	}
}

// TestHookUnknownEventIsSilent is the forward-compatibility clause of the same
// invariant, checked with a daemon actually running and news waiting: Claude
// Code gaining a hook name shuck has never heard of must produce nothing at
// all, not an empty decision the harness has to interpret.
func TestHookUnknownEventIsSilent(t *testing.T) {
	d, home := hookDaemon(t, newFakeClient())
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": treeAt(t, "feature"), "source": "startup"}, home)
	d.publish([]Event{{Kind: KindCIFailed, Title: "test failed"}})

	var out bytes.Buffer
	code := RunHook(context.Background(), "pre-compact", strings.NewReader(`{"session_id":"sess-1"}`), &out)
	if code != 0 {
		t.Errorf("exit = %d for a hook event shuck does not serve, want 0", code)
	}
	if out.Len() != 0 {
		t.Errorf("an unknown hook event wrote %q, want silence", out.String())
	}
	// And it consumed nothing on its way out: the failure is still waiting for
	// the hook that is meant to deliver it.
	if got := d.journal.Pending("sess-1"); got != 1 {
		t.Errorf("Pending = %d after an unknown hook event, want the failure untouched", got)
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

// TestOtherTargets covers the sentence a session gets when the monitor is
// following more than its own pull request. It is the warning that the feed may
// name a PR this session never asked about — so the one thing it must not do is
// list the session's own target back at it.
func TestOtherTargets(t *testing.T) {
	// Deliberately not in the order sortTargets would produce. otherTargets
	// hands back the order it was given, and the list arrives already ordered
	// by the one place that decides that — a second sort here would be a copy
	// of that policy, silently disagreeing with `shuck monitor status` the day
	// the first one changes. Sorted input could not tell the two apart.
	watched := []TargetStatus{
		{Target: "other/repo#1"},
		{Target: "o/r#7", Verdict: "failed"},
		{Target: "o/r#8"},
	}

	tests := []struct {
		name  string
		watch *Watch
		want  string
	}{
		{
			name:  "the session's own target is the one left out",
			watch: &Watch{Owner: "o", Repo: "r", Number: 7},
			want:  "other/repo#1, o/r#8",
		},
		{
			name:  "a watch whose branch has no PR yet excludes nothing",
			watch: &Watch{Owner: "o", Repo: "r"},
			want:  "other/repo#1, o/r#7, o/r#8",
		},
		{
			name:  "a watch on a PR nobody is polling excludes nothing",
			watch: &Watch{Owner: "o", Repo: "r", Number: 99},
			want:  "other/repo#1, o/r#7, o/r#8",
		},
		{
			// SessionStart words its paragraph before the daemon has
			// necessarily answered, so a nil watch has to be survivable.
			name:  "no watch at all",
			watch: nil,
			want:  "other/repo#1, o/r#7, o/r#8",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := otherTargets(&Status{Targets: watched}, tt.watch); got != tt.want {
				t.Errorf("otherTargets = %q, want %q", got, tt.want)
			}
		})
	}

	// Nothing but your own PR is not "also watching: " with nothing after it.
	own := &Status{Targets: []TargetStatus{{Target: "o/r#7"}}}
	if got := otherTargets(own, &Watch{Owner: "o", Repo: "r", Number: 7}); got != "" {
		t.Errorf("otherTargets = %q with only the session's own target, want empty", got)
	}
}

// TestSessionStartContextNamesTheOtherPRs is the same rule end to end: two
// sessions on two branches share one daemon, and each is told what the other
// brought along.
func TestSessionStartContextNamesTheOtherPRs(t *testing.T) {
	d, _ := hookDaemon(t, newFakeClient())
	dir, err := Dir() // hookDaemon already pointed SHUCK_HOME here
	if err != nil {
		t.Fatal(err)
	}

	d.watches.Add(Watch{ID: "pr:o/r#7", Kind: WatchPR, Owner: "o", Repo: "r", Number: 7})
	d.watches.Add(Watch{ID: "pr:o/r#8", Kind: WatchPR, Owner: "o", Repo: "r", Number: 8})
	d.due(time.Now()) // materialize both targets, as a poll round would

	mine := &Watch{ID: "pr:o/r#7", Kind: WatchPR, Owner: "o", Repo: "r", Number: 7, Branch: "feature"}
	got := sessionStartContext(context.Background(), &Client{dir: dir}, mine)

	if !strings.Contains(got, "It is following o/r#7 (branch feature).") {
		t.Errorf("context should open with this session's own target:\n%s", got)
	}
	_, also, ok := strings.Cut(got, "\nIt is also watching: ")
	if !ok {
		t.Fatalf("a second target should be named:\n%s", got)
	}
	if also != "o/r#8." {
		t.Errorf("also watching = %q, want just the other session's target", also)
	}
}

// TestSessionStartContextStaysQuietAboutOneTarget is the other side of that
// rule, and the reason it is keyed on more than one target rather than on any:
// when the only PR being polled is this session's own there is nothing to warn
// about, and the sentence would come out as "It is also watching: ." — a
// promise of detail with nothing behind it.
func TestSessionStartContextStaysQuietAboutOneTarget(t *testing.T) {
	d, _ := hookDaemon(t, newFakeClient())
	dir, err := Dir() // hookDaemon already pointed SHUCK_HOME here
	if err != nil {
		t.Fatal(err)
	}

	d.watches.Add(Watch{ID: "pr:o/r#7", Kind: WatchPR, Owner: "o", Repo: "r", Number: 7})
	d.due(time.Now()) // materialize the one target, as a poll round would

	mine := &Watch{ID: "pr:o/r#7", Kind: WatchPR, Owner: "o", Repo: "r", Number: 7, Branch: "feature"}
	got := sessionStartContext(context.Background(), &Client{dir: dir}, mine)

	if !strings.Contains(got, "It is following o/r#7 (branch feature).") {
		t.Errorf("context should still open with this session's own target:\n%s", got)
	}
	if strings.Contains(got, "also watching") {
		t.Errorf("the monitor is watching nothing else, so it should not say it is:\n%s", got)
	}
}

func TestHookTimeoutIsShort(t *testing.T) {
	// Hooks run between the user pressing enter and the agent starting to
	// think. A wedged monitor must cost that moment a fraction of a second.
	if hookTimeout > 5*time.Second {
		t.Errorf("hookTimeout = %s, too long to sit in front of a prompt", hookTimeout)
	}
}
