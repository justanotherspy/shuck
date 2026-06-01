package action

import (
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
	"github.com/justanotherspy/shuck/internal/semver"
)

// FuzzActionParseRef exercises ParseRef with arbitrary reference strings. It
// must never panic; on success the owner and repo are non-empty and contain no
// "/", an "@" in the input never leaks into the slug, and the Slug accessors
// are panic-free and consistent.
func FuzzActionParseRef(f *testing.F) {
	f.Add("actions/checkout@v4")
	f.Add("github/codeql-action/init@v3")
	f.Add("owner/repo")
	f.Add("owner/repo@1.2.3")
	f.Add("//owner//repo//")
	f.Add("@v1")
	f.Add("owner/repo@")
	f.Add("")

	f.Fuzz(func(t *testing.T, s string) {
		ref, err := ParseRef(s)
		if err != nil {
			return
		}
		if ref.Owner == "" || ref.Repo == "" {
			t.Fatalf("ParseRef(%q) succeeded with empty owner/repo: %+v", s, ref)
		}
		if strings.Contains(ref.Owner, "/") || strings.Contains(ref.Repo, "/") {
			t.Fatalf("ParseRef(%q): owner/repo contain a slash: %+v", s, ref)
		}
		// The version split happens before the slug split, so no slug component
		// may contain an "@".
		if strings.ContainsAny(ref.Owner+ref.Repo+ref.Subpath, "@") {
			t.Fatalf("ParseRef(%q): slug contains '@': %+v", s, ref)
		}
		if ref.RepoSlug() != ref.Owner+"/"+ref.Repo {
			t.Fatalf("RepoSlug() mismatch for %+v", ref)
		}
		if !strings.HasPrefix(ref.Slug(), ref.RepoSlug()) {
			t.Fatalf("Slug() %q does not start with RepoSlug() %q", ref.Slug(), ref.RepoSlug())
		}
		// The pin renderers must not panic on any parsed ref.
		_ = Resolved{Ref: ref, Tag: "v1.0.0", SHA: "deadbeef"}.PinLine()
	})
}

// FuzzActionSelect exercises Select with a fuzzed tag list (one tag per line)
// and constraint. It must never panic; a returned tag must come from the input,
// parse as semver, and match the constraint; and a prerelease is returned only
// when no stable tag matches (the stable-preference contract).
func FuzzActionSelect(f *testing.F) {
	f.Add("v4.2.2\nv4.2.1\nv3.0.0", "v4")
	f.Add("v1.0.0-rc.1\nv1.0.0", "")
	f.Add("v2.0.0-beta\nnot-semver\nv1.9.9", "v2")
	f.Add("", "v1")
	f.Add("v1\nv1.1\nv1.1.1", "1.1")

	f.Fuzz(func(t *testing.T, tagList, constraint string) {
		var tags []model.ActionTag
		for name := range strings.SplitSeq(tagList, "\n") {
			tags = append(tags, model.ActionTag{Name: name, SHA: "sha-" + name})
		}

		got, err := Select(tags, constraint)

		con, conOK := semver.ParseConstraint(constraint)
		if !conOK {
			if err == nil {
				t.Fatalf("Select accepted invalid constraint %q (returned %+v)", constraint, got)
			}
			return
		}
		if err != nil {
			// No match claimed: verify no tag actually parses and matches.
			for _, tag := range tags {
				if v, ok := semver.Parse(tag.Name); ok && con.Matches(v) {
					t.Fatalf("Select returned error %v but %q matches %q", err, tag.Name, constraint)
				}
			}
			return
		}

		v, ok := semver.Parse(got.Name)
		if !ok {
			t.Fatalf("Select returned non-semver tag %q", got.Name)
		}
		if !con.Matches(v) {
			t.Fatalf("Select returned %q which does not match constraint %q", got.Name, constraint)
		}
		if got.SHA != "sha-"+got.Name {
			t.Fatalf("Select returned a tag not present in the input: %+v", got)
		}
		// Stable preference: a prerelease result implies no stable tag matched.
		if !v.Stable() {
			for _, tag := range tags {
				if tv, ok := semver.Parse(tag.Name); ok && tv.Stable() && con.Matches(tv) {
					t.Fatalf("Select returned prerelease %q but stable %q matches %q", got.Name, tag.Name, constraint)
				}
			}
		}
	})
}
