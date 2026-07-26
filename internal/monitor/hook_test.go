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
	for _, want := range []string{"background monitor is running", "retargets itself", "shuck monitor status"} {
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

// TestHookUserPromptNeverDelivers is the rule that replaced the stand-down.
//
// The prompt hook used to deliver whenever it believed no stream was serving
// this directory, and that belief was wrong the moment a session moved: a
// stream follows the session, so it kept delivering under the session's scope
// while the hook, seeing an unfamiliar directory, delivered the same events
// again. Predicting the other channel is the part that cannot be made correct,
// so there is no longer a prediction — the hook does not read at all.
//
// Registering is still its job, and leaving the cursor untouched is what keeps
// the stream's own reading of the journal honest.
func TestHookUserPromptNeverDelivers(t *testing.T) {
	d, home := hookDaemon(t, newFakeClient())
	tree := treeAt(t, "feature")
	runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": tree, "source": "startup"}, home)

	d.publish([]Event{{Kind: KindCIFailed, Target: "o/r#7", Title: "test failed", Body: "the error"}})

	if out := runHook(t, HookUserPromptSubmit, map[string]any{"session_id": "sess-1", "cwd": tree}, home); out != nil {
		t.Errorf("the prompt hook delivered events; the stream is the only channel:\n%+v", out)
	}
	if got := d.journal.Pending("sess-1"); got != 1 {
		t.Errorf("Pending = %d, want the failure left pending — no hook may consume it", got)
	}
	if _, ok := d.watches.Get(TreeWatchID(tree)); !ok {
		t.Error("the prompt hook stopped registering the session's working tree")
	}
}

// liveStream fabricates the marker a `shuck monitor stream` running in tree
// would have written. Writing the file is the whole seam: the hooks and the
// stream are separate processes and the marker is the only thing they share.
func liveStream(t *testing.T, tree string) {
	t.Helper()
	writeStreamMarker(t, streamRecord{
		Watch:     TreeWatchID(tree),
		Path:      tree,
		Consumer:  StreamConsumer(TreeWatchID(tree)),
		PID:       os.Getpid(),
		Heartbeat: time.Now(),
	})
}

// TestHookUserPromptStaysSilentWhateverTheStreamIsDoing pins the property that
// made the duplicate possible in the first place: the answer must not depend on
// what the hook believes about the other channel.
//
// Every row here is a state the old stand-down read differently — no marker, a
// dead one, one naming somebody else's tree, one naming this tree, and the case
// that actually bit, a session that has moved below the directory the stream
// registered. All five now behave identically, because none of them is
// consulted.
func TestHookUserPromptStaysSilentWhateverTheStreamIsDoing(t *testing.T) {
	tests := []struct {
		name   string
		marker func(t *testing.T, tree string)
		cwd    func(t *testing.T, tree string) string
	}{
		{name: "no stream has ever run here"},
		{
			name: "a stream whose heartbeat stopped",
			marker: func(t *testing.T, tree string) {
				writeStreamMarker(t, streamRecord{Watch: TreeWatchID(tree), Path: tree, PID: os.Getpid(),
					Heartbeat: time.Now().Add(-streamStaleAfter - time.Minute)})
			},
		},
		{
			name: "a stream on somebody else's working tree",
			marker: func(t *testing.T, _ string) {
				other := t.TempDir()
				writeStreamMarker(t, streamRecord{Watch: TreeWatchID(other), Path: other, PID: os.Getpid(),
					Heartbeat: time.Now()})
			},
		},
		{name: "a live stream on this tree", marker: liveStream},
		{
			name:   "a session that moved below the streamed tree",
			marker: liveStream,
			cwd: func(t *testing.T, tree string) string {
				sub := filepath.Join(tree, "internal")
				if err := os.MkdirAll(sub, 0o755); err != nil {
					t.Fatal(err)
				}
				return sub
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, home := hookDaemon(t, newFakeClient())
			tree := treeAt(t, "feature")
			runHook(t, HookSessionStart, map[string]any{"session_id": "sess-1", "cwd": tree, "source": "startup"}, home)
			d.publish([]Event{{Kind: KindCIFailed, Target: "o/r#7", Title: "test failed", Body: "the error"}})

			if tt.marker != nil {
				tt.marker(t, tree)
			}
			cwd := tree
			if tt.cwd != nil {
				cwd = tt.cwd(t, tree)
			}

			if out := runHook(t, HookUserPromptSubmit, map[string]any{"session_id": "sess-1", "cwd": cwd}, home); out != nil {
				t.Errorf("the prompt hook delivered:\n%+v", out)
			}
			if got := d.journal.Pending("sess-1"); got != 1 {
				t.Errorf("Pending = %d, want the failure untouched — no hook may consume it", got)
			}
		})
	}
}

// TestHookSessionStartSaysWhereEventsWillArrive covers the one sentence a
// session is told this by. There is a single channel now, so the sentence must
// never promise the conversation-block delivery that no longer exists — an
// agent that believes it is coming waits for something nothing will send.
// Without a stream marker the honest answer is not a second channel but a
// command, so `shuck monitor events` is what the sentence has to end at.
func TestHookSessionStartSaysWhereEventsWillArrive(t *testing.T) {
	tests := []struct {
		name        string
		streaming   bool
		want, avoid string
	}{
		{"with a stream running", true, "notifications", "<shuck-monitor> blocks"},
		{"with no stream yet", false, "shuck monitor events", "<shuck-monitor> blocks"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newFakeClient()
			c.openPR = 42
			_, home := hookDaemon(t, c)
			tree := treeAt(t, "feature")
			if tt.streaming {
				liveStream(t, tree)
			}

			out := runHook(t, HookSessionStart, map[string]any{
				"session_id": "sess-1", "cwd": tree, "source": "startup",
			}, home)
			if out == nil || out.Specific == nil {
				t.Fatal("session start should introduce the monitor to the session")
			}
			got := out.Specific.AdditionalContext
			if !strings.Contains(got, tt.want) {
				t.Errorf("context does not say events arrive as %s:\n%s", tt.want, got)
			}
			if strings.Contains(got, tt.avoid) {
				t.Errorf("context promises %s, which is not how this session will hear:\n%s", tt.avoid, got)
			}
		})
	}
}

func TestHookUserPromptIsSilentWhenNothingHappened(t *testing.T) {
	_, home := hookDaemon(t, newFakeClient())
	if out := runHook(t, HookUserPromptSubmit, map[string]any{"session_id": "sess-1"}, home); out != nil {
		t.Errorf("a quiet monitor should write nothing, got %+v", out)
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

// TestHookPostToolUseSeedsTheSession pins where a session's cursor starts. No
// hook reads the journal any more, so this is no longer about what gets
// delivered — it is about `shuck monitor events --consumer <session>` being
// answerable at all. A consumer with no cursor is served from the head of the
// retained journal, so without the seed the first read of any session would
// return hours of another branch's history.
func TestHookPostToolUseSeedsTheSession(t *testing.T) {
	d, home := hookDaemon(t, newFakeClient())
	d.publish([]Event{{Kind: KindCIFailed, Title: "another branch's old failure"}})

	runHook(t, HookPostToolUse, map[string]any{
		"session_id": "sess-1",
		"tool_name":  "Bash",
		"tool_input": map[string]any{"command": "git push"},
	}, home)

	if got := d.journal.Pending("sess-1"); got != 0 {
		t.Fatalf("Pending = %d, want the cursor seeded at the present", got)
	}

	d.publish([]Event{{Kind: KindCIFailed, Title: "the run that push started"}})

	if got := d.journal.Pending("sess-1"); got != 1 {
		t.Errorf("Pending = %d, want the seed to start the session at the present rather than mute it", got)
	}
}

// TestHookStopNeverBlocks is the retirement of the finish gate, stated as the
// property the rest of the suite used to deny.
//
// Every case below is one the old gate blocked on. What killed it was not any
// of them individually but the compound behavior: it blocked on the last
// *known* actionable event with no notion of a superseded head, so its normal
// day was to re-hand a session the failure it had already fixed and pushed,
// one poll interval before the pass arrived.
//
// This is asserted against the binary rather than the manifest on purpose. The
// entry point still exists for installed copies of an older hooks.json, and an
// entry point that still blocked would mean upgrading shuck left the gate
// standing for anyone who had not also re-installed the plugin.
func TestHookStopNeverBlocks(t *testing.T) {
	tests := []struct {
		name  string
		event Event
	}{
		{"a red build", Event{Kind: KindCIFailed, Target: "o/r#7", Title: "CI — Test failed"}},
		{"requested changes", Event{Kind: KindReviewSubmitted, Target: "o/r#7", Title: "changes requested", Body: "fix it"}},
		{"a review comment", Event{Kind: KindReviewComment, Target: "o/r#7", Title: "a reviewer asked something"}},
		{"a passing build", Event{Kind: KindCIPassed, Target: "o/r#7", Title: "all checks passed"}},
		{"a stale pin", Event{Kind: KindPinsStale, Target: "o/r", Title: "actions/checkout is unpinned"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, home := hookDaemon(t, newFakeClient())
			d.publish([]Event{tt.event})

			out := runHook(t, HookStop, map[string]any{"session_id": "sess-1", "cwd": treeAt(t, "feature")}, home)
			if out != nil {
				t.Fatalf("Stop is retired and must never hold a turn open, got %+v", out)
			}
		})
	}
}

// TestHookStopConsumesNothing is the other half of retirement. A hook that
// still advanced a cursor would silently eat events the stream had not yet
// delivered — the failure mode of a channel nobody thinks is running.
func TestHookStopConsumesNothing(t *testing.T) {
	d, home := hookDaemon(t, newFakeClient())
	d.publish([]Event{{Kind: KindCIFailed, Target: "o/r#7", Title: "CI — Test failed"}})
	before := d.journal.Pending("sess-1")

	runHook(t, HookStop, map[string]any{"session_id": "sess-1"}, home)

	if got := d.journal.Pending("sess-1"); got != before {
		t.Errorf("Pending = %d, want %d — Stop must not move any cursor", got, before)
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
	if got := d.journal.Pending("sess-1"); got != 1 {
		t.Errorf("Pending = %d, want the failure still deliverable after a resume", got)
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
	got := sessionStartContext(context.Background(), &Client{dir: dir}, mine, false)

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
	got := sessionStartContext(context.Background(), &Client{dir: dir}, mine, false)

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
