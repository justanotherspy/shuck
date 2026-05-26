// Package action resolves a GitHub Actions reference — owner/action, optionally
// constrained to a major ("v3") or major.minor ("v3.1") version — to the latest
// matching release tag and the immutable commit SHA it points to, so a workflow
// `uses:` line can be pinned to a SHA.
//
// Selection is pure: the caller fetches the repository's tags (from the network
// or a cache) and hands them to Select. The latest stable release wins; a
// prerelease is chosen only when nothing stable matches the constraint.
package action

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/justanotherspy/shuck/internal/model"
)

// Ref is a parsed action reference: the owner/repo slug, an optional subpath
// within the repo (e.g. "init" for github/codeql-action/init), and an optional
// version constraint ("v3", "3.1", ...). An empty Constraint means "latest
// stable".
type Ref struct {
	Owner      string
	Repo       string
	Subpath    string
	Constraint string
}

// RepoSlug returns the owner/repo form used to query tags.
func (r Ref) RepoSlug() string { return r.Owner + "/" + r.Repo }

// Slug returns the full owner/repo[/subpath] form used in a `uses:` reference.
func (r Ref) Slug() string {
	if r.Subpath != "" {
		return r.Owner + "/" + r.Repo + "/" + r.Subpath
	}
	return r.Owner + "/" + r.Repo
}

// ParseRef interprets an action reference in "owner/action[/subpath][@version]"
// form. The leading two path segments are the repository; any further segments
// are a subpath preserved for the pin; the part after "@" is the version
// constraint.
func ParseRef(s string) (Ref, error) {
	slug, constraint, hasAt := strings.Cut(strings.TrimSpace(s), "@")
	parts := strings.Split(strings.Trim(slug, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return Ref{}, fmt.Errorf("invalid action %q (expected owner/action[@version])", s)
	}
	constraint = strings.TrimSpace(constraint)
	if hasAt && constraint == "" {
		return Ref{}, fmt.Errorf("invalid action %q (empty version after @)", s)
	}
	return Ref{
		Owner:      parts[0],
		Repo:       parts[1],
		Subpath:    strings.Join(parts[2:], "/"),
		Constraint: constraint,
	}, nil
}

// Resolved is the outcome of pinning a Ref: the chosen tag and the commit SHA
// it points to.
type Resolved struct {
	Ref Ref
	Tag string
	SHA string
}

// UsesRef returns the SHA-pinned reference, e.g. "actions/checkout@<sha>".
func (r Resolved) UsesRef() string { return r.Ref.Slug() + "@" + r.SHA }

// PinLine returns the SHA-pinned reference annotated with its human-readable
// tag, e.g. "actions/checkout@<sha> # v4.2.2" — ready to drop after "uses:".
func (r Resolved) PinLine() string { return r.UsesRef() + " # " + r.Tag }

// Select picks the best tag from tags for a version constraint ("" = latest
// overall, "v3" = latest 3.x.x, "3.1" = latest 3.1.x). Non-semver tags are
// ignored. The latest stable release is preferred; a prerelease is returned
// only when no stable tag matches the constraint.
func Select(tags []model.ActionTag, constraint string) (model.ActionTag, error) {
	con, err := parseConstraint(constraint)
	if err != nil {
		return model.ActionTag{}, err
	}
	var best, bestStable *parsed
	for i := range tags {
		p, ok := parseVersion(tags[i].Name)
		if !ok || !con.matches(p) {
			continue
		}
		p.tag = tags[i]
		cur := p
		if best == nil || cmp(cur, *best) > 0 {
			b := cur
			best = &b
		}
		if p.pre == "" && (bestStable == nil || cmp(cur, *bestStable) > 0) {
			b := cur
			bestStable = &b
		}
	}
	switch {
	case bestStable != nil:
		return bestStable.tag, nil
	case best != nil:
		return best.tag, nil
	case constraint == "":
		return model.ActionTag{}, fmt.Errorf("no semver-tagged releases found")
	default:
		return model.ActionTag{}, fmt.Errorf("no release matches %q", constraint)
	}
}

// parsed is a semver tag broken into its numeric components and prerelease
// suffix, carrying the originating tag for the final tie-break and the result.
type parsed struct {
	major, minor, patch int
	pre                 string // prerelease suffix after '-', empty when stable
	tag                 model.ActionTag
}

// parseVersion parses a tag name like "v4.2.2" or "1.2.3-rc.1" into its semver
// parts. Build metadata (after '+') is dropped. It returns ok=false for any tag
// that is not MAJOR[.MINOR[.PATCH]] with optional v prefix and prerelease.
func parseVersion(name string) (parsed, bool) {
	s := strings.TrimSpace(name)
	s = strings.TrimPrefix(s, "v")
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
		return parsed{}, false
	}
	var nums [3]int
	for i, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return parsed{}, false
		}
		nums[i] = n
	}
	return parsed{major: nums[0], minor: nums[1], patch: nums[2], pre: pre}, true
}

// constraint is a parsed version filter. specificity is how many components the
// user pinned (0 = none, 1 = major, 2 = major.minor, 3 = exact).
type constraint struct {
	specificity         int
	major, minor, patch int
}

func parseConstraint(s string) (constraint, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return constraint{}, nil
	}
	raw := s
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '-'); i >= 0 { // a constraint ignores any prerelease
		s = s[:i]
	}
	fields := strings.Split(s, ".")
	if len(fields) == 0 || len(fields) > 3 {
		return constraint{}, fmt.Errorf("invalid version %q", raw)
	}
	c := constraint{specificity: len(fields)}
	nums := []*int{&c.major, &c.minor, &c.patch}
	for i, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return constraint{}, fmt.Errorf("invalid version %q", raw)
		}
		*nums[i] = n
	}
	return c, nil
}

func (c constraint) matches(p parsed) bool {
	if c.specificity >= 1 && p.major != c.major {
		return false
	}
	if c.specificity >= 2 && p.minor != c.minor {
		return false
	}
	if c.specificity >= 3 && p.patch != c.patch {
		return false
	}
	return true
}

// cmp orders two parsed versions: by (major, minor, patch); then a stable
// release outranks a prerelease of the same version; then prerelease suffixes
// compare lexically; finally the raw tag name breaks ties for determinism.
func cmp(a, b parsed) int {
	if a.major != b.major {
		return sign(a.major - b.major)
	}
	if a.minor != b.minor {
		return sign(a.minor - b.minor)
	}
	if a.patch != b.patch {
		return sign(a.patch - b.patch)
	}
	if (a.pre == "") != (b.pre == "") {
		if a.pre == "" {
			return 1 // stable > prerelease
		}
		return -1
	}
	if a.pre != b.pre {
		return strings.Compare(a.pre, b.pre)
	}
	return strings.Compare(a.tag.Name, b.tag.Name)
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
