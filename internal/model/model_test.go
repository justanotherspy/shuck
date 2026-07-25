package model

import "testing"

// The full truth table: every State the API can hand us crossed with Draft and
// Merged. The monitor stores this word on a target, picks DormantInterval off
// it, and prints it in the pr.state event, so each cell is behavior someone
// reads.
func TestPRLifecycle(t *testing.T) {
	tests := []struct {
		name string
		pr   PR
		want string
	}{
		{"zero value", PR{}, ""},

		{"open", PR{State: "open"}, "open"},
		{"open draft", PR{State: "open", Draft: true}, "draft"},
		// GitHub flips state to "closed" on merge, but a PR carried in from a
		// stale cache can still say "open" while Merged is set; the flag wins.
		{"open but merged", PR{State: "open", Merged: true}, "merged"},
		{"open draft merged", PR{State: "open", Draft: true, Merged: true}, "merged"},

		{"closed unmerged", PR{State: "closed"}, "closed"},
		// A draft can be closed without ever leaving draft; ended beats draft.
		{"closed draft", PR{State: "closed", Draft: true}, "closed"},
		{"closed merged", PR{State: "closed", Merged: true}, "merged"},
		{"closed draft merged", PR{State: "closed", Draft: true, Merged: true}, "merged"},

		// State never populated (the report paths don't fill it): "" means "no
		// news" to the monitor's lifecycleEvents, so it must stay empty rather
		// than default to open. The flags still speak for themselves.
		{"no state", PR{State: ""}, ""},
		{"no state draft", PR{Draft: true}, "draft"},
		{"no state merged", PR{Merged: true}, "merged"},
		{"no state draft merged", PR{Draft: true, Merged: true}, "merged"},

		// Matching is exact and fails closed: an unrecognized state reports
		// nothing rather than guessing a lifecycle the caller would act on.
		// GraphQL casing would land here if a PR were ever sourced that way.
		{"uppercase open", PR{State: "OPEN"}, ""},
		{"uppercase open draft", PR{State: "OPEN", Draft: true}, "draft"},
		{"uppercase open merged", PR{State: "OPEN", Merged: true}, "merged"},
		{"uppercase closed", PR{State: "CLOSED"}, ""},
		// "merged" is a flag, not a state string: text alone must not claim it.
		{"state says merged", PR{State: "merged"}, ""},
		{"state says merged, draft", PR{State: "merged", Draft: true}, "draft"},
		{"nonsense state", PR{State: "banana"}, ""},
		{"nonsense state merged", PR{State: "banana", Merged: true}, "merged"},

		// Exactness, spelled out as the near misses that separate `==` from a
		// prefix/suffix/substring match. "OPEN"/"banana" above only prove the
		// comparison is not case-insensitive or wide open; each of these is a
		// real state with characters glued on, so a loosened comparison would
		// hand the monitor a lifecycle to act on for a word GitHub never sent.
		{"open plus a suffix", PR{State: "open0"}, ""},
		{"open as a suffix", PR{State: "reopen"}, ""},
		{"open as a substring", PR{State: "unopened"}, ""},
		{"closed plus a suffix", PR{State: "closed-ish"}, ""},
		{"closed as a suffix", PR{State: "not-closed"}, ""},
		{"closed as a substring", PR{State: "half-closed-ish"}, ""},

		// The arms are ordered, so a loose match on an earlier one outranks a
		// later one: "closed-ish" matching the closed arm would downgrade a
		// live draft to ended, and the monitor would park it on
		// DormantInterval and stop reporting its CI.
		{"closed-ish draft", PR{State: "closed-ish", Draft: true}, "draft"},
		{"reopen draft", PR{State: "reopen", Draft: true}, "draft"},
		// Draft outranks an unknown state in the other direction too: the flag
		// is news even when the word is not, so dropping the draft arm cannot
		// hide behind "" being a legitimate answer for "banana".
		{"nonsense state draft", PR{State: "banana", Draft: true}, "draft"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.pr.Lifecycle(); got != tt.want {
				t.Errorf("PR{State:%q, Draft:%v, Merged:%v}.Lifecycle() = %q, want %q",
					tt.pr.State, tt.pr.Draft, tt.pr.Merged, got, tt.want)
			}
		})
	}
}

// The distinction the pr.state event text hangs on. Both outcomes park the
// target on DormantInterval, so a collapse would go unnoticed by the cadence
// logic while telling everyone reading the event the wrong story.
func TestPRLifecycleMergedIsNotClosed(t *testing.T) {
	merged := PR{State: "closed", Merged: true}.Lifecycle()
	abandoned := PR{State: "closed"}.Lifecycle()
	if merged == abandoned {
		t.Fatalf("merged and closed-unmerged both report %q", merged)
	}
	if merged != "merged" || abandoned != "closed" {
		t.Fatalf("got merged=%q abandoned=%q, want %q and %q", merged, abandoned, "merged", "closed")
	}
}

func TestConclusionPredicates(t *testing.T) {
	tests := []struct {
		conclusion string
		failure    bool
		cancelled  bool
		drillable  bool
	}{
		{"failure", true, false, true},
		{"timed_out", true, false, true},
		{"startup_failure", true, false, true},
		{"action_required", true, false, true},
		{"cancelled", false, true, true},
		{"success", false, false, false},
		{"skipped", false, false, false},
		{"neutral", false, false, false},
		{"", false, false, false},
	}
	for _, tt := range tests {
		if got := IsFailureConclusion(tt.conclusion); got != tt.failure {
			t.Errorf("IsFailureConclusion(%q) = %v, want %v", tt.conclusion, got, tt.failure)
		}
		if got := IsCancelledConclusion(tt.conclusion); got != tt.cancelled {
			t.Errorf("IsCancelledConclusion(%q) = %v, want %v", tt.conclusion, got, tt.cancelled)
		}
		if got := IsDrillableConclusion(tt.conclusion); got != tt.drillable {
			t.Errorf("IsDrillableConclusion(%q) = %v, want %v", tt.conclusion, got, tt.drillable)
		}
	}
}
