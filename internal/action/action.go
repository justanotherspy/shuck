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
	"strings"

	"github.com/justanotherspy/shuck/internal/model"
	"github.com/justanotherspy/shuck/internal/semver"
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
	con, ok := semver.ParseConstraint(constraint)
	if !ok {
		return model.ActionTag{}, fmt.Errorf("invalid version %q", constraint)
	}
	var best, bestStable *model.ActionTag
	var bestV, bestStableV semver.Version
	for i := range tags {
		v, ok := semver.Parse(tags[i].Name)
		if !ok || !con.Matches(v) {
			continue
		}
		if best == nil || semver.Compare(v, bestV) > 0 {
			best, bestV = &tags[i], v
		}
		if v.Stable() && (bestStable == nil || semver.Compare(v, bestStableV) > 0) {
			bestStable, bestStableV = &tags[i], v
		}
	}
	switch {
	case bestStable != nil:
		return *bestStable, nil
	case best != nil:
		return *best, nil
	case constraint == "":
		return model.ActionTag{}, fmt.Errorf("no semver-tagged releases found")
	default:
		return model.ActionTag{}, fmt.Errorf("no release matches %q", constraint)
	}
}
