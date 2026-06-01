package target

import (
	"strconv"
	"strings"
	"testing"
)

// FuzzParseRemote exercises ParseRemote with arbitrary remote URLs. It must
// never panic; on success the owner and repo are non-empty and contain no "/".
func FuzzParseRemote(f *testing.F) {
	f.Add("git@github.com:owner/repo.git")
	f.Add("https://github.com/owner/repo.git")
	f.Add("ssh://git@github.com/owner/repo")
	f.Add("https://github.com/owner/repo")
	f.Add("owner/repo")
	f.Add("")
	f.Add("git@github.com:")
	f.Add("https://")

	f.Fuzz(func(t *testing.T, raw string) {
		owner, repo, err := ParseRemote(raw)
		if err != nil {
			return
		}
		if owner == "" || repo == "" {
			t.Fatalf("ParseRemote(%q) succeeded with empty owner/repo: %q/%q", raw, owner, repo)
		}
		if strings.Contains(owner, "/") || strings.Contains(repo, "/") {
			t.Fatalf("ParseRemote(%q): owner/repo contain a slash: %q/%q", raw, owner, repo)
		}
	})
}

// FuzzResolveRepo exercises the single-argument (pure, no-git) form of
// ResolveRepo with arbitrary slugs and URLs. It must never panic; on success
// the owner is non-empty and slash-free and the repo is non-empty.
func FuzzResolveRepo(f *testing.F) {
	f.Add("owner/repo")
	f.Add("https://github.com/owner/repo")
	f.Add("https://github.com/owner/repo/pull/12")
	f.Add("https://github.com/owner/repo/actions/runs/123/job/456")
	f.Add("github.com/owner/repo?tab=readme#section")
	f.Add("not a repo")
	f.Add("://")
	f.Add("")

	f.Fuzz(func(t *testing.T, arg string) {
		owner, repo, err := ResolveRepo([]string{arg})
		if err != nil {
			return
		}
		if owner == "" || repo == "" {
			t.Fatalf("ResolveRepo(%q) succeeded with empty owner/repo: %q/%q", arg, owner, repo)
		}
		if strings.Contains(owner, "/") {
			t.Fatalf("ResolveRepo(%q): owner contains a slash: %q", arg, owner)
		}
	})
}

// FuzzResolveTarget exercises the two-argument (pure, no-git) form of Resolve:
// an owner/repo slug plus a PR number. It must never panic; it succeeds exactly
// when the slug splits into two non-empty halves and the number parses, and the
// resolved target reproduces both.
func FuzzResolveTarget(f *testing.F) {
	f.Add("owner/repo", "12")
	f.Add("owner/repo", "-1")
	f.Add("owner", "12")
	f.Add("/repo", "0")
	f.Add("a/b/c", "99")
	f.Add("", "")

	f.Fuzz(func(t *testing.T, slug, prArg string) {
		tgt, err := Resolve([]string{slug, prArg})

		o, r, slugOK := strings.Cut(slug, "/")
		n, atoiErr := strconv.Atoi(prArg)
		wantOK := slugOK && o != "" && r != "" && atoiErr == nil

		if (err == nil) != wantOK {
			t.Fatalf("Resolve(%q, %q): err=%v, want success=%v", slug, prArg, err, wantOK)
		}
		if err != nil {
			return
		}
		if tgt.Owner != o || tgt.Repo != r {
			t.Fatalf("Resolve(%q, %q): got %q/%q, want %q/%q", slug, prArg, tgt.Owner, tgt.Repo, o, r)
		}
		if tgt.Number != n {
			t.Fatalf("Resolve(%q, %q): got number %d, want %d", slug, prArg, tgt.Number, n)
		}
	})
}
