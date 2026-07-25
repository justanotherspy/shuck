package model

import "testing"

// FuzzPRLifecycle holds the properties consumers rely on across any State
// string GitHub, a stale cache, or a hand-edited state file can produce: the
// vocabulary is closed (cli/monitor.go and the poller switch on these words),
// and the merged/not-merged boundary is decided by the Merged flag alone —
// never by text that happens to spell "merged" in State.
func FuzzPRLifecycle(f *testing.F) {
	f.Add("open", false, false)
	f.Add("closed", false, true)
	f.Add("closed", true, false)
	f.Add("merged", false, false) // state text must not stand in for the flag
	f.Add("OPEN", false, false)   // GraphQL casing
	f.Add("", true, false)
	f.Add("", false, false)

	f.Fuzz(func(t *testing.T, state string, draft, merged bool) {
		pr := PR{State: state, Draft: draft, Merged: merged}
		got := pr.Lifecycle()

		switch got {
		case "", "open", "draft", "merged", "closed":
		default:
			t.Fatalf("Lifecycle() = %q, outside the vocabulary consumers switch on", got)
		}

		switch {
		case merged && got != "merged":
			t.Fatalf("Merged PR reported as %q; a merge must never read as anything else", got)
		case !merged && got == "merged":
			t.Fatalf("unmerged PR (state %q) reported as merged", state)
		case got == "draft" && !draft:
			t.Fatalf("state %q reported as draft without the Draft flag", state)
		case got == "closed" && state != "closed":
			t.Fatalf("state %q reported as closed", state)
		case got == "open" && state != "open":
			t.Fatalf("state %q reported as open", state)
		}

		// Nothing known means nothing reported: "" is what tells the monitor's
		// lifecycleEvents there is no news, so an unknown state must not
		// masquerade as a real lifecycle.
		if !draft && !merged && state != "open" && state != "closed" && got != "" {
			t.Fatalf("unknown state %q reported as %q, want the empty no-news value", state, got)
		}
	})
}
