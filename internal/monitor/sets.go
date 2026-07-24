package monitor

import (
	"slices"
	"sort"
)

// stringSet and int64Set are the tiny bounded "have I already reported this?"
// sets the poller keeps per target. They round-trip through the persisted
// state as sorted slices — small enough to write on every tick, ordered so the
// file does not churn, and trimmed so a long-lived PR does not grow them
// without bound.

type stringSet struct {
	m map[string]struct{}
}

func newStringSet(values []string) *stringSet {
	s := &stringSet{m: make(map[string]struct{}, len(values))}
	for _, v := range values {
		s.m[v] = struct{}{}
	}
	return s
}

func (s *stringSet) has(v string) bool { _, ok := s.m[v]; return ok }
func (s *stringSet) add(v string)      { s.m[v] = struct{}{} }

// slice returns the set as a sorted slice, trimmed to the most recent
// maxRemembered entries. "Most recent" is approximated by sort order, which for
// the job keys stored here ("<id>/<attempt>", ids ascending over time) is the
// right approximation.
func (s *stringSet) slice() []string {
	out := make([]string, 0, len(s.m))
	for v := range s.m {
		out = append(out, v)
	}
	sort.Strings(out)
	if len(out) > maxRemembered {
		out = out[len(out)-maxRemembered:]
		s.m = make(map[string]struct{}, len(out))
		for _, v := range out {
			s.m[v] = struct{}{}
		}
	}
	return out
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
