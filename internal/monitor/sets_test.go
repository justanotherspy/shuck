package monitor

import (
	"fmt"
	"slices"
	"testing"
)

// jobKey is the shape stringSet actually holds: "<job id>/<attempt>", with the
// ids fixed-width as GitHub's are, so byte order and age order agree.
func jobKey(id int) string { return fmt.Sprintf("%011d/1", id) }

func TestStringSetSliceSortsDeduplicatesAndRoundTrips(t *testing.T) {
	s := newStringSet([]string{jobKey(12), jobKey(3), jobKey(12)})
	s.add(jobKey(7))
	s.add(jobKey(3)) // already there

	got := s.slice()
	want := []string{jobKey(3), jobKey(7), jobKey(12)}
	if !slices.Equal(got, want) {
		t.Fatalf("slice = %v, want %v", got, want)
	}

	// The slice is how the set survives a daemon restart, so re-loading it has
	// to produce the same set — otherwise a restart re-reports a job.
	restored := newStringSet(got)
	if !slices.Equal(restored.slice(), got) {
		t.Errorf("round trip = %v, want %v", restored.slice(), got)
	}
	for _, key := range want {
		if !restored.has(key) {
			t.Errorf("%q did not survive the round trip", key)
		}
	}
	if restored.has(jobKey(99)) {
		t.Error("has reported a job that was never added")
	}
	if got := newStringSet(nil).slice(); len(got) != 0 {
		t.Errorf("an empty set sliced to %v", got)
	}
}

// TestStringSetSliceIsBounded is why slice() exists rather than a plain map
// dump: the poller writes this list on every tick, and a PR that runs for a
// week must not grow it without end.
func TestStringSetSliceIsBounded(t *testing.T) {
	s := newStringSet(nil)
	for i := 1; i <= maxRemembered+50; i++ {
		s.add(jobKey(i))
	}

	got := s.slice()
	if len(got) != maxRemembered {
		t.Fatalf("slice kept %d entries, want %d", len(got), maxRemembered)
	}
	// The newest survive: dropping the newest instead would re-report the job
	// that just failed, which is the one thing the set exists to prevent.
	if got[0] != jobKey(51) || got[len(got)-1] != jobKey(maxRemembered+50) {
		t.Errorf("kept %s…%s, want %s…%s", got[0], got[len(got)-1], jobKey(51), jobKey(maxRemembered+50))
	}
	if !slices.IsSorted(got) {
		t.Error("the slice must be sorted; an unstable order churns the state file on every tick")
	}

	// The trim is applied to the set itself, not just to the copy handed back —
	// otherwise the bound is cosmetic and the map grows forever.
	if len(s.m) != maxRemembered {
		t.Errorf("the set still holds %d entries, want it trimmed to %d", len(s.m), maxRemembered)
	}
	if s.has(jobKey(1)) {
		t.Error("a trimmed entry is still in the set")
	}
	if !s.has(jobKey(maxRemembered + 50)) {
		t.Error("the newest entry was trimmed")
	}

	// And the trimmed set keeps working: the next tick adds to it and slices
	// again.
	s.add(jobKey(maxRemembered + 51))
	next := s.slice()
	if len(next) != maxRemembered || next[len(next)-1] != jobKey(maxRemembered+51) {
		t.Errorf("after a trim, slice = %d entries ending %s", len(next), next[len(next)-1])
	}
}

func TestInt64SetSliceSortsDeduplicatesAndRoundTrips(t *testing.T) {
	s := newInt64Set([]int64{9001, 12, 9001})
	s.add(750)
	s.add(12)

	got := s.slice()
	want := []int64{12, 750, 9001}
	if !slices.Equal(got, want) {
		t.Fatalf("slice = %v, want %v", got, want)
	}

	restored := newInt64Set(got)
	if !slices.Equal(restored.slice(), got) {
		t.Errorf("round trip = %v, want %v", restored.slice(), got)
	}
	if !restored.has(9001) || restored.has(9002) {
		t.Error("membership did not survive the round trip")
	}
	if got := newInt64Set(nil).slice(); len(got) != 0 {
		t.Errorf("an empty set sliced to %v", got)
	}
}

// TestInt64SetSliceIsBounded is the review half of the same bound: comment and
// review ids climb over time, so the smallest are the oldest and are the ones
// that may be forgotten.
func TestInt64SetSliceIsBounded(t *testing.T) {
	s := newInt64Set(nil)
	// Ascending ids interleaved with a much older one, so a sort that never
	// ran would show up in which entries were kept.
	for i := range int64(maxRemembered + 50) {
		s.add(1_000_000 + (maxRemembered+49-i)*7)
	}
	s.add(1)

	got := s.slice()
	if len(got) != maxRemembered {
		t.Fatalf("slice kept %d entries, want %d", len(got), maxRemembered)
	}
	if !slices.IsSorted(got) {
		t.Fatalf("slice is unsorted: %v", got[:5])
	}
	newest := int64(1_000_000 + (maxRemembered+49)*7)
	if got[len(got)-1] != newest {
		t.Errorf("newest kept = %d, want %d", got[len(got)-1], newest)
	}
	if slices.Contains(got, int64(1)) {
		t.Error("the oldest id survived a trim that should have dropped it first")
	}

	if len(s.m) != maxRemembered {
		t.Errorf("the set still holds %d entries, want it trimmed to %d", len(s.m), maxRemembered)
	}
	if s.has(1) {
		t.Error("a trimmed id is still in the set")
	}
	if !s.has(newest) {
		t.Error("the newest id was trimmed")
	}
}
