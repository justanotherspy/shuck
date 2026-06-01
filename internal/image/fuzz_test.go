package image

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/justanotherspy/shuck/internal/model"
	"github.com/justanotherspy/shuck/internal/semver"
)

// FuzzImageParseRef exercises ParseRef with arbitrary reference strings. It
// must never panic; on success the registry and owner are non-empty, the owner
// contains no "/", a bare-owner (list-all) ref never carries a constraint, and
// the Slug/pin renderers are panic-free.
func FuzzImageParseRef(f *testing.F) {
	f.Add("ghcr.io/acme/api:v1.2.3")
	f.Add("acme/api")
	f.Add("acme")
	f.Add("ghcr.io/acme/team/api:latest")
	f.Add("acme/api@sha256:abc123")
	f.Add("registry.example.com:5000/acme/api:v2")
	f.Add("acme:v1")
	f.Add("")
	f.Add(":")
	f.Add("ghcr.io/")

	f.Fuzz(func(t *testing.T, s string) {
		ref, err := ParseRef(s)
		if err != nil {
			return
		}
		if ref.Registry == "" || ref.Owner == "" {
			t.Fatalf("ParseRef(%q) succeeded with empty registry/owner: %+v", s, ref)
		}
		if strings.Contains(ref.Owner, "/") {
			t.Fatalf("ParseRef(%q): owner contains a slash: %+v", s, ref)
		}
		// A bare owner means "list everything"; a constraint on it is meaningless
		// and must have been rejected.
		if ref.ListAll() && ref.Constraint != "" {
			t.Fatalf("ParseRef(%q): list-all ref carries constraint %q", s, ref.Constraint)
		}
		if ref.ListAll() != (ref.Name == "") {
			t.Fatalf("ParseRef(%q): ListAll()=%v disagrees with Name=%q", s, ref.ListAll(), ref.Name)
		}
		// Slug and the pin renderers must not panic on any parsed ref.
		if !strings.HasPrefix(ref.Slug(), ref.Registry+"/"+ref.Owner) {
			t.Fatalf("Slug() %q does not start with registry/owner", ref.Slug())
		}
		_ = Resolved{Ref: ref, Tag: "v1", Digest: "sha256:abc"}.PinLine()
	})
}

// FuzzImageSelect exercises Select with a fuzzed version list and constraint.
// Versions are built from the fuzzed input: one version per line, tags
// comma-separated. Select must never panic; a returned (version, tag) pair must
// come from the input; a semver constraint's result must match it with stable
// tags preferred; a non-semver constraint must be matched as an exact tag.
func FuzzImageSelect(f *testing.F) {
	f.Add("v1.2.3,latest\nv1.2.2", "")
	f.Add("v2.0.0-rc.1\nv1.9.9,stable", "v2")
	f.Add("latest\nnightly", "latest")
	f.Add("", "v1")
	f.Add("v1.0.0\nv1.0.0", "1.0.0")

	f.Fuzz(func(t *testing.T, versionList, constraint string) {
		var versions []model.ImageVersion
		for i, line := range strings.Split(versionList, "\n") {
			var tags []string
			if line != "" {
				tags = strings.Split(line, ",")
			}
			versions = append(versions, model.ImageVersion{
				Tags:      tags,
				Digest:    "sha256:" + line,
				UpdatedAt: time.Unix(int64(i), 0),
			})
		}

		got, tag, err := Select(versions, constraint)
		if err != nil {
			return
		}

		// The returned version must be one of the inputs and carry the returned
		// tag.
		found := false
		for _, v := range versions {
			if v.Digest == got.Digest {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("Select returned a version not present in the input: %+v", got)
		}
		inTags := slices.Contains(got.Tags, tag)
		if !inTags {
			t.Fatalf("Select returned tag %q not carried by the returned version %+v", tag, got)
		}

		con, conOK := semver.ParseConstraint(constraint)
		if !conOK {
			// Non-semver constraint: must be an exact tag match.
			if tag != constraint {
				t.Fatalf("non-semver constraint %q: Select returned tag %q, want exact match", constraint, tag)
			}
			return
		}

		v, isSemver := semver.Parse(tag)
		if isSemver {
			if !con.Matches(v) {
				t.Fatalf("Select returned %q which does not match constraint %q", tag, constraint)
			}
			// Stable preference: a prerelease result implies no stable tag in any
			// version matches the constraint.
			if !v.Stable() {
				for _, ver := range versions {
					for _, vt := range ver.Tags {
						if tv, ok := semver.Parse(vt); ok && tv.Stable() && con.Matches(tv) {
							t.Fatalf("Select returned prerelease %q but stable %q matches %q", tag, vt, constraint)
						}
					}
				}
			}
			return
		}

		// A non-semver tag may only be returned by the newest-fallback, which
		// runs when the constraint is empty and no version has a semver tag.
		if constraint != "" {
			t.Fatalf("constraint %q returned non-semver tag %q", constraint, tag)
		}
		for _, ver := range versions {
			for _, vt := range ver.Tags {
				if _, ok := semver.Parse(vt); ok {
					t.Fatalf("Select fell back to non-semver tag %q but semver tag %q exists", tag, vt)
				}
			}
		}
	})
}
