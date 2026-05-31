// Package image resolves a GitHub Container Registry (ghcr.io) image reference —
// [registry/]owner/name, optionally constrained to a version ("v3", "3.1") or an
// exact tag ("latest") — to the newest matching tag and the immutable manifest
// digest (sha256:...) it points to, so an image reference can be pinned to a
// digest. A bare owner (no name) selects "list every image under the owner".
//
// Selection is pure: the caller fetches the image's published versions (from the
// network or a cache) and hands them to Select. The latest stable semver release
// wins; a prerelease, an exact tag match, or the most recently updated version is
// used as a fallback.
package image

import (
	"fmt"
	"strings"

	"github.com/justanotherspy/shuck/internal/model"
	"github.com/justanotherspy/shuck/internal/semver"
)

// DefaultRegistry is the registry shuck targets when a reference omits one.
const DefaultRegistry = "ghcr.io"

// Ref is a parsed image reference: the registry, the owner, the image name
// (empty for a bare-owner "list all" reference), and an optional version
// constraint or exact tag. An empty Constraint means "latest".
type Ref struct {
	Registry   string
	Owner      string
	Name       string
	Constraint string
}

// ListAll reports whether the reference is a bare owner (list every image).
func (r Ref) ListAll() bool { return r.Name == "" }

// Slug returns the registry/owner/name form, e.g. "ghcr.io/acme/api".
func (r Ref) Slug() string {
	if r.Name == "" {
		return r.Registry + "/" + r.Owner
	}
	return r.Registry + "/" + r.Owner + "/" + r.Name
}

// ParseRef interprets an image reference. Accepted forms:
//
//	owner                         list every image under owner
//	[ghcr.io/]owner/name          latest version
//	[ghcr.io/]owner/name:tag      a version constraint (v3, 3.1) or exact tag (latest)
//	[ghcr.io/]owner/name@sha256:… an explicit digest used as the constraint
//
// A leading "ghcr.io/" (or any registry host containing a dot) is stripped; the
// next segment is the owner and the remainder (minus any :tag / @digest) is the
// image name, so multi-segment names like "owner/team/api" are preserved.
func ParseRef(s string) (Ref, error) {
	in := strings.TrimSpace(s)
	if in == "" {
		return Ref{}, fmt.Errorf("invalid image %q (expected [ghcr.io/]owner[/name][:tag])", s)
	}

	registry := DefaultRegistry
	body := in
	// An explicit registry host is the first segment when it looks like a host
	// (contains a dot or a port). ghcr.io is the common case.
	if first, rest, ok := strings.Cut(body, "/"); ok && looksLikeHost(first) {
		registry = first
		body = rest
	}

	// Split off the version: prefer "@digest", else the last ":tag". A ':' inside
	// the registry host (a port) was already consumed above.
	constraint := ""
	if base, digest, ok := strings.Cut(body, "@"); ok {
		body, constraint = base, digest
	} else if i := strings.LastIndexByte(body, ':'); i >= 0 {
		// Guard against a ':' that is actually part of a path (it never is for a
		// well-formed ref, but be defensive): only treat it as a tag when no '/'
		// follows it.
		if !strings.ContainsAny(body[i+1:], "/") {
			constraint = body[i+1:]
			body = body[:i]
		}
	}
	constraint = strings.TrimSpace(constraint)

	parts := strings.Split(strings.Trim(body, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return Ref{}, fmt.Errorf("invalid image %q (expected [ghcr.io/]owner[/name][:tag])", s)
	}
	ref := Ref{Registry: registry, Owner: parts[0], Constraint: constraint}
	if len(parts) > 1 {
		ref.Name = strings.Join(parts[1:], "/")
		if ref.Name == "" {
			return Ref{}, fmt.Errorf("invalid image %q (empty image name)", s)
		}
	} else if constraint != "" {
		// A tag with no image name (e.g. "owner:tag") is meaningless.
		return Ref{}, fmt.Errorf("invalid image %q (a tag needs an owner/name)", s)
	}
	return ref, nil
}

// Resolved is the outcome of pinning a single image Ref: the chosen tag and the
// manifest digest it points to.
type Resolved struct {
	Ref    Ref
	Tag    string
	Digest string
}

// PinRef returns the digest-pinned reference, e.g. "ghcr.io/acme/api@sha256:…".
func (r Resolved) PinRef() string { return r.Ref.Slug() + "@" + r.Digest }

// PinLine returns the digest-pinned reference annotated with its tag,
// e.g. "ghcr.io/acme/api@sha256:… # v1.2.3".
func (r Resolved) PinLine() string { return r.PinRef() + " # " + r.Tag }

// Select picks the best (version, tag) pair from versions for a constraint:
//   - ""        latest overall (highest stable semver, else newest by UpdatedAt)
//   - "v3"/"3.1" highest semver matching the major / major.minor
//   - "latest"  an exact tag match (any non-semver tag works this way)
//
// The latest stable semver release is preferred; a prerelease is returned only
// when no stable tag matches. When the constraint is not a semver constraint it
// is matched as an exact tag name.
func Select(versions []model.ImageVersion, constraint string) (model.ImageVersion, string, error) {
	con, ok := semver.ParseConstraint(constraint)
	if !ok {
		// Not a semver constraint (e.g. "latest", "nightly"): exact-tag match.
		return selectExactTag(versions, constraint)
	}

	var best, bestStable *model.ImageVersion
	var bestTag, bestStableTag string
	var bestV, bestStableV semver.Version
	for i := range versions {
		for _, tag := range versions[i].Tags {
			v, ok := semver.Parse(tag)
			if !ok || !con.Matches(v) {
				continue
			}
			if best == nil || semver.Compare(v, bestV) > 0 {
				best, bestTag, bestV = &versions[i], tag, v
			}
			if v.Stable() && (bestStable == nil || semver.Compare(v, bestStableV) > 0) {
				bestStable, bestStableTag, bestStableV = &versions[i], tag, v
			}
		}
	}
	switch {
	case bestStable != nil:
		return *bestStable, bestStableTag, nil
	case best != nil:
		return *best, bestTag, nil
	case constraint != "":
		return model.ImageVersion{}, "", fmt.Errorf("no version matches %q", constraint)
	}

	// No semver-tagged version and no constraint: fall back to the most recently
	// updated version that carries a tag, preferring a "latest" tag.
	return selectNewest(versions)
}

// selectExactTag returns the version whose tag list contains tag exactly.
func selectExactTag(versions []model.ImageVersion, tag string) (model.ImageVersion, string, error) {
	for i := range versions {
		for _, t := range versions[i].Tags {
			if t == tag {
				return versions[i], tag, nil
			}
		}
	}
	return model.ImageVersion{}, "", fmt.Errorf("no version tagged %q", tag)
}

// selectNewest returns the most recently updated version that has at least one
// tag, choosing a "latest" tag when present, else the first tag.
func selectNewest(versions []model.ImageVersion) (model.ImageVersion, string, error) {
	var best *model.ImageVersion
	for i := range versions {
		if len(versions[i].Tags) == 0 {
			continue
		}
		if best == nil || versions[i].UpdatedAt.After(best.UpdatedAt) {
			best = &versions[i]
		}
	}
	if best == nil {
		return model.ImageVersion{}, "", fmt.Errorf("no tagged versions found")
	}
	tag := best.Tags[0]
	for _, t := range best.Tags {
		if t == "latest" {
			tag = t
			break
		}
	}
	return *best, tag, nil
}

// LatestVersion returns the package's newest version and the tag that names it,
// using the same selection as a constraint-free Select. It is the per-package
// summary used when listing every image under an owner.
func LatestVersion(versions []model.ImageVersion) (model.ImageVersion, string, bool) {
	v, tag, err := Select(versions, "")
	if err != nil {
		return model.ImageVersion{}, "", false
	}
	return v, tag, true
}

// looksLikeHost reports whether a leading path segment is a registry host (it
// contains a '.' or a ':' port) rather than an owner.
func looksLikeHost(s string) bool {
	return strings.ContainsAny(s, ".:")
}
