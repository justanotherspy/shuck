// Package semver provides the small slice of semantic-version handling shuck
// needs to pick the "latest" tag from a list: parsing MAJOR[.MINOR[.PATCH]] tags
// (with an optional v prefix and prerelease suffix), filtering by a version
// constraint, and ordering versions so the highest stable release wins.
//
// It is deliberately lenient and dependency-free: non-conforming tags simply
// fail to parse, and callers decide what to do with them. Both `shuck action`
// (tags → commit SHA) and `shuck image` (tags → image digest) select over it.
package semver

import (
	"strconv"
	"strings"
)

// Version is a tag broken into its numeric components and prerelease suffix.
// Raw is the original tag text, kept for a deterministic final tie-break.
type Version struct {
	Major, Minor, Patch int
	Pre                 string // prerelease suffix after '-', empty when stable
	Raw                 string
}

// Stable reports whether the version has no prerelease suffix.
func (v Version) Stable() bool { return v.Pre == "" }

// Parse parses a tag name like "v4.2.2" or "1.2.3-rc.1" into its semver parts.
// Build metadata (after '+') is dropped. It returns ok=false for any tag that is
// not MAJOR[.MINOR[.PATCH]] with an optional v prefix and prerelease suffix.
func Parse(name string) (Version, bool) {
	raw := strings.TrimSpace(name)
	s := strings.TrimPrefix(raw, "v")
	s = strings.TrimPrefix(s, "V")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var pre string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre = s[i+1:]
		s = s[:i]
	}
	fields := strings.Split(s, ".")
	if len(fields) == 0 || len(fields) > 3 {
		return Version{}, false
	}
	var nums [3]int
	for i, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return Version{}, false
		}
		nums[i] = n
	}
	return Version{Major: nums[0], Minor: nums[1], Patch: nums[2], Pre: pre, Raw: raw}, true
}

// Compare orders two versions: by (major, minor, patch); then a stable release
// outranks a prerelease of the same version; then prerelease suffixes compare
// lexically; finally the raw tag text breaks ties for determinism.
func Compare(a, b Version) int {
	if a.Major != b.Major {
		return sign(a.Major - b.Major)
	}
	if a.Minor != b.Minor {
		return sign(a.Minor - b.Minor)
	}
	if a.Patch != b.Patch {
		return sign(a.Patch - b.Patch)
	}
	if a.Stable() != b.Stable() {
		if a.Stable() {
			return 1 // stable > prerelease
		}
		return -1
	}
	if a.Pre != b.Pre {
		return strings.Compare(a.Pre, b.Pre)
	}
	return strings.Compare(a.Raw, b.Raw)
}

// Constraint is a parsed version filter. Specificity is how many components the
// caller pinned (0 = none, 1 = major, 2 = major.minor, 3 = exact).
type Constraint struct {
	Specificity         int
	major, minor, patch int
}

// ParseConstraint parses a constraint like "", "v3", "3.1", or "3.1.0". An empty
// string matches everything (latest overall). Any prerelease/build metadata on
// the constraint itself is ignored.
func ParseConstraint(s string) (Constraint, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Constraint{}, true
	}
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		s = s[:i]
	}
	fields := strings.Split(s, ".")
	if len(fields) == 0 || len(fields) > 3 {
		return Constraint{}, false
	}
	c := Constraint{Specificity: len(fields)}
	nums := []*int{&c.major, &c.minor, &c.patch}
	for i, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return Constraint{}, false
		}
		*nums[i] = n
	}
	return c, true
}

// Matches reports whether v satisfies the constraint.
func (c Constraint) Matches(v Version) bool {
	if c.Specificity >= 1 && v.Major != c.major {
		return false
	}
	if c.Specificity >= 2 && v.Minor != c.minor {
		return false
	}
	if c.Specificity >= 3 && v.Patch != c.patch {
		return false
	}
	return true
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}
