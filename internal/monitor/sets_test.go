package monitor

import (
	"fmt"
	"slices"
	"testing"
)

// jobKey builds a key exactly as the poller does — fmt.Sprintf("%d/%d", id,
// attempt), unpadded. Padding it here would be a fiction that makes byte order
// and age order agree, which is precisely the assumption byJobKey exists to
// stop relying on.
func jobKey(id int) string { return fmt.Sprintf("%d/1", id) }

func TestStringSetSliceSortsDeduplicatesAndRoundTrips(t *testing.T) {
	s := newStringSet([]string{jobKey(12), jobKey(3), jobKey(12)}, byJobKey)
	s.add(jobKey(7))
	s.add(jobKey(3)) // already there

	got := s.slice()
	want := []string{jobKey(3), jobKey(7), jobKey(12)}
	if !slices.Equal(got, want) {
		t.Fatalf("slice = %v, want %v", got, want)
	}

	// The slice is how the set survives a daemon restart, so re-loading it has
	// to produce the same set — otherwise a restart re-reports a job.
	restored := newStringSet(got, byJobKey)
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
	if got := newStringSet(nil, byJobKey).slice(); len(got) != 0 {
		t.Errorf("an empty set sliced to %v", got)
	}
}

// TestStringSetSliceIsBounded is why slice() exists rather than a plain map
// dump: the poller writes this list on every tick, and a PR that runs for a
// week must not grow it without end.
func TestStringSetSliceIsBounded(t *testing.T) {
	s := newStringSet(nil, byJobKey)
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
	if !slices.IsSortedFunc(got, byJobKey) {
		t.Error("the slice must be ordered; an unstable order churns the state file on every tick")
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

// TestByJobKeyOrdersNumericallyNotLexically is the regression test for a real
// eviction bug.
//
// The poller writes job keys as fmt.Sprintf("%d/%d", id, attempt) — plain
// decimals of whatever width GitHub's ids currently have. A text sort is not a
// numeric sort across a digit-width boundary: "9999999999/1" sorts *after*
// "123456789012/1", so trimming an overflowing set by byte order drops the
// newer job and keeps the older one. The dropped job is then no longer known to
// have been reported, and its failure is delivered a second time into a session
// that already acted on it — the exact delivery mistake the journal's
// at-most-once design exists to prevent.
func TestByJobKeyOrdersNumericallyNotLexically(t *testing.T) {
	// Ids spanning 10, 11, and 12 digits. GitHub is on 11 today; the boundary
	// is a matter of time, not of hypotheticals.
	old10 := "9999999999/1"
	mid11 := "89660393168/1"
	new12 := "123456789012/1"

	got := []string{new12, old10, mid11}
	slices.SortFunc(got, byJobKey)
	want := []string{old10, mid11, new12}
	if !slices.Equal(got, want) {
		t.Fatalf("byJobKey ordered %v, want oldest-first %v", got, want)
	}

	// The property that actually matters: trimming keeps the newest.
	if kept := got[len(got)-1]; kept != new12 {
		t.Errorf("trimming to one kept %s, want the newest id %s", kept, new12)
	}

	// A higher attempt on the same job is newer than attempt 1.
	if byJobKey("500/1", "500/2") >= 0 {
		t.Error("attempt 2 of a job should rank after attempt 1")
	}

	// Malformed keys rank oldest so they are sacrificed before real entries,
	// rather than displacing a job whose failure would then repeat.
	for _, junk := range []string{"", "nope", "12", "abc/1", "12/xyz"} {
		if byJobKey(junk, "1/1") >= 0 {
			t.Errorf("%q should rank before a well-formed key", junk)
		}
	}
	// And the order stays total: equal inputs compare equal, so sorting is
	// deterministic and the persisted file does not churn.
	if byJobKey(mid11, mid11) != 0 {
		t.Error("a key must compare equal to itself")
	}
}

// TestStringSetEvictsTheOldestJobAcrossADigitBoundary drives the same bug
// through the set itself, at the size where the trim actually fires.
func TestStringSetEvictsTheOldestJobAcrossADigitBoundary(t *testing.T) {
	s := newStringSet(nil, byJobKey)
	// Fill past the bound with ids that straddle 10 and 11 digits.
	base := 9999999000
	for i := range maxRemembered + 10 {
		s.add(jobKey(base + i))
	}
	got := s.slice()

	if len(got) != maxRemembered {
		t.Fatalf("kept %d, want %d", len(got), maxRemembered)
	}
	newest := jobKey(base + maxRemembered + 9)
	if !s.has(newest) {
		t.Errorf("the newest job %s was evicted; its failure will be reported twice", newest)
	}
	if s.has(jobKey(base)) {
		t.Errorf("the oldest job %s survived while newer ones were dropped", jobKey(base))
	}
}
