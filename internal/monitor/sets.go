package monitor

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
)

// stringSet and int64Set are the tiny bounded "have I already reported this?"
// sets the poller keeps per target. They round-trip through the persisted
// state as sorted slices — small enough to write on every tick, ordered so the
// file does not churn, and trimmed so a long-lived PR does not grow them
// without bound.

type stringSet struct {
	m map[string]struct{}
	// order ranks two keys least-valuable-first. slice sorts by it and, on
	// overflow, drops from the front — so it is what decides which entry is
	// sacrificed. The two callers key on different things and must say which
	// they mean rather than inherit a default that happens to be wrong.
	order func(a, b string) int
}

func newStringSet(values []string, order func(a, b string) int) *stringSet {
	s := &stringSet{m: make(map[string]struct{}, len(values)), order: order}
	for _, v := range values {
		s.m[v] = struct{}{}
	}
	return s
}

func (s *stringSet) has(v string) bool { _, ok := s.m[v]; return ok }
func (s *stringSet) add(v string)      { s.m[v] = struct{}{} }

// slice returns the set sorted by its order function and trimmed to
// maxRemembered entries, keeping the ones that rank highest.
func (s *stringSet) slice() []string {
	out := make([]string, 0, len(s.m))
	for v := range s.m {
		out = append(out, v)
	}
	slices.SortFunc(out, s.order)
	if len(out) > maxRemembered {
		out = out[len(out)-maxRemembered:]
		s.m = make(map[string]struct{}, len(out))
		for _, v := range out {
			s.m[v] = struct{}{}
		}
	}
	return out
}

// byJobKey orders "<job id>/<attempt>" keys by age, oldest first, so an
// overflowing ReportedJobs set sacrifices the oldest job it has seen.
//
// This has to parse rather than compare as text. GitHub's job ids ascend over
// time but are plain decimals of varying width, and lexical order is not
// numeric order across a digit-width boundary: "9999999999/1" sorts *after*
// "123456789012/1", so a text sort would evict the newer job and re-report its
// failure into a session that already dealt with it. That is the one delivery
// mistake the journal's at-most-once design exists to avoid.
//
// A key that does not parse ranks oldest, so malformed state (a hand-edited
// file, or a format from a future version) is trimmed before anything real.
func byJobKey(a, b string) int {
	aID, aAttempt := parseJobKey(a)
	bID, bAttempt := parseJobKey(b)
	if c := cmp.Compare(aID, bID); c != 0 {
		return c
	}
	if c := cmp.Compare(aAttempt, bAttempt); c != 0 {
		return c
	}
	// Equal numerically: fall back to text so the order stays total and the
	// persisted file does not churn between runs.
	return strings.Compare(a, b)
}

// parseJobKey splits "<id>/<attempt>". Either part that will not parse becomes
// -1, which sorts before every real id.
func parseJobKey(key string) (id, attempt int64) {
	before, after, ok := strings.Cut(key, "/")
	if !ok {
		return -1, -1
	}
	id, err := strconv.ParseInt(before, 10, 64)
	if err != nil {
		return -1, -1
	}
	attempt, err = strconv.ParseInt(after, 10, 64)
	if err != nil {
		// Half-parsed is still garbage: a key this shape can never equal one
		// the poller wrote, so it can never suppress a real report. Rank the
		// whole thing oldest so it is evicted before anything load-bearing.
		return -1, -1
	}
	return id, attempt
}

type int64Set struct {
	m map[int64]struct{}
}

func newInt64Set(values []int64) *int64Set {
	s := &int64Set{m: make(map[int64]struct{}, len(values))}
	for _, v := range values {
		s.m[v] = struct{}{}
	}
	return s
}

func (s *int64Set) has(v int64) bool { _, ok := s.m[v]; return ok }
func (s *int64Set) add(v int64)      { s.m[v] = struct{}{} }

// slice returns the set sorted ascending and trimmed to the newest
// maxRemembered entries. GitHub's comment and review IDs increase over time, so
// dropping the smallest drops the oldest.
func (s *int64Set) slice() []int64 {
	out := make([]int64, 0, len(s.m))
	for v := range s.m {
		out = append(out, v)
	}
	slices.Sort(out)
	if len(out) > maxRemembered {
		out = out[len(out)-maxRemembered:]
		s.m = make(map[int64]struct{}, len(out))
		for _, v := range out {
			s.m[v] = struct{}{}
		}
	}
	return out
}
