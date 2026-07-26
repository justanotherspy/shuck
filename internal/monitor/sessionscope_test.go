package monitor

import "testing"

func TestSessionScopeIsNamespaced(t *testing.T) {
	if got := SessionScope("abc"); got != "session:abc" {
		t.Errorf("SessionScope = %q, want %q", got, "session:abc")
	}
	// An empty id is every caller that is not running under Claude Code, and it
	// must not produce a scope that half the registry then matches.
	if got := SessionScope(""); got != "" {
		t.Errorf("SessionScope(\"\") = %q, want empty", got)
	}
}

// A session scope must never be run through path resolution. This is the bug the
// namespace exists to prevent: the daemon and the stream have different working
// directories, so resolving "session:<id>" as a relative path gives each of them
// a different absolute path and they never match.
func TestSessionScopeMatchesRegardlessOfWorkingDirectory(t *testing.T) {
	w := Watch{ID: "t", Scopes: []string{SessionScope("s1")}}
	if !w.InScope(SessionScope("s1")) {
		t.Error("a watch should match the session that owns it")
	}
	if w.InScope(SessionScope("s2")) {
		t.Error("a watch must not match another session")
	}
	if w.InScope("/some/directory") {
		t.Error("a session-scoped watch must not match an unrelated directory")
	}
}

// The retarget. A session opens outside a repository, so the watch it first
// registers is the parent directory; it then moves into a checkout and a hook
// registers that tree under the same session id. The stream is still asking with
// the directory it was spawned in, which now matches nothing — and the session
// scope is the only thing that still reaches it.
func TestASessionKeepsItsWatchesAfterMovingDirectory(t *testing.T) {
	const session = "sess-1"
	moved := Watch{
		ID:     "tree:/home/u/repos/project",
		Scopes: []string{"/home/u/repos/project", SessionScope(session)},
	}

	if moved.inAnyScope([]string{resolveScope("/home/u")}) {
		t.Fatal("precondition: the new tree is not the directory the stream was spawned in")
	}
	if !moved.inAnyScope([]string{resolveScope("/home/u"), SessionScope(session)}) {
		t.Error("a stream asking with its spawn directory and its session should reach the moved watch")
	}
}

// A PR added by hand ("shuck monitor watch owner/repo#42") carries the caller's
// directory and no session at all. Querying by session alone would orphan it, so
// the two identities widen each other rather than replacing one another.
func TestAHandAddedWatchStillMatchesByDirectory(t *testing.T) {
	byHand := Watch{ID: "pr:owner/repo#42", Scopes: []string{"/home/u/repos/project"}}

	if !byHand.inAnyScope([]string{resolveScope("/home/u/repos/project"), SessionScope("sess-1")}) {
		t.Error("a watch scoped only to a directory must still match a session asking from it")
	}
	if byHand.inAnyScope([]string{SessionScope("sess-1")}) {
		t.Error("without the directory there is nothing to match on, and it must not match everything")
	}
}

// A watch predating the Scopes field decodes to an empty set, which has always
// meant "belongs to everyone" and must keep meaning that: dropping such a watch
// costs the failure nobody then hears about.
func TestAnUnscopedWatchStillBelongsToEveryone(t *testing.T) {
	w := Watch{ID: "legacy"}
	if !w.inAnyScope([]string{SessionScope("sess-1")}) {
		t.Error("an unscoped watch should match any asker")
	}
	if !w.inAnyScope(nil) {
		t.Error("an unscoped watch should match even an asker with no identity")
	}
}

func TestAddSessionScopeIgnoresAnEmptyID(t *testing.T) {
	var w Watch
	w.AddSessionScope("")
	if len(w.Scopes) != 0 {
		t.Errorf("Scopes = %v, want none for an empty session id", w.Scopes)
	}
	w.AddSessionScope("sess-1")
	if len(w.Scopes) != 1 || w.Scopes[0] != SessionScope("sess-1") {
		t.Errorf("Scopes = %v, want the one session scope", w.Scopes)
	}
}
