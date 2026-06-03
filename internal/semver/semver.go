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
// outranks a prerelease of the same version; then prerelease suffixes compare by
// semver precedence (numeric fields numerically, a numeric field below an
// alphanumeric one, more fields winning a tie); finally the raw tag text breaks
// ties for determinism.
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
	if c := comparePre(a.Pre, b.Pre); c != 0 {
		return c
	}
	return strings.Compare(a.Raw, b.Raw)
}

// comparePre orders two prerelease strings by Semantic Versioning precedence
// (§11): each is split on '.' and compared field by field; a purely numeric
// field compares numerically and ranks below an alphanumeric one; other fields
// compare lexically in ASCII order; and when one is a prefix of the other, the
// longer (more fields) wins. Both inputs are empty for stable releases, so this
// returns 0 and the raw-tag tie-break decides.
func comparePre(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := range min(len(as), len(bs)) {
		if c := comparePreField(as[i], bs[i]); c != 0 {
			return c
		}
	}
	return sign(len(as) - len(bs))
}

// comparePreField compares a single prerelease identifier per semver §11.
func comparePreField(a, b string) int {
	aNum, bNum := isNumeric(a), isNumeric(b)
	switch {
	case aNum && bNum:
		return compareNumericField(a, b)
	case aNum:
		return -1 // a numeric identifier always ranks below an alphanumeric one
	case bNum:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

// compareNumericField compares two all-digit identifiers by numeric value
// without overflowing on long inputs: by trimmed length, then lexically.
func compareNumericField(a, b string) int {
	a = strings.TrimLeft(a, "0")
	b = strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		return sign(len(a) - len(b))
	}
	return strings.Compare(a, b)
}

// isNumeric reports whether s is a non-empty run of ASCII digits.
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
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
