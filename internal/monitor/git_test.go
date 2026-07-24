package monitor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRepo lays out a minimal git directory: enough for ReadCheckout, which
// only reads HEAD, a ref, and config. Building it by hand rather than shelling
// out to git keeps the test hermetic and fast, and — more usefully — lets it
// construct the shapes that are awkward to reach with real commands, like a
// linked worktree or a repository with no origin.
func writeRepo(t *testing.T, dir, head, ref, sha, config string) {
	t.Helper()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(gitDir, "HEAD"), head)
	if ref != "" {
		refPath := filepath.Join(gitDir, filepath.FromSlash(ref))
		if err := os.MkdirAll(filepath.Dir(refPath), 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, refPath, sha)
	}
	write(t, filepath.Join(gitDir, "config"), config)
}

func write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

const originConfig = `[core]
	repositoryformatversion = 0
[remote "origin"]
	url = git@github.com:justanotherspy/shuck.git
	fetch = +refs/heads/*:refs/remotes/origin/*
[branch "main"]
	remote = origin
`

func TestReadCheckout(t *testing.T) {
	tests := []struct {
		name       string
		head       string
		ref        string
		config     string
		wantBranch string
		wantHead   string
	}{
		{
			name:       "branch",
			head:       "ref: refs/heads/feature/x\n",
			ref:        "refs/heads/feature/x",
			config:     originConfig,
			wantBranch: "feature/x",
			wantHead:   "abc123",
		},
		{
			name:       "detached HEAD reports no branch",
			head:       "0123456789abcdef0123456789abcdef01234567\n",
			config:     originConfig,
			wantBranch: "",
			wantHead:   "0123456789abcdef0123456789abcdef01234567",
		},
		{
			// A freshly created branch has no loose ref until the first
			// commit; the checkout is still usable, just without a SHA.
			name:       "branch whose ref file is missing",
			head:       "ref: refs/heads/new\n",
			config:     originConfig,
			wantBranch: "new",
			wantHead:   "",
		},
		{
			name:       "compact remote section spelling",
			head:       "ref: refs/heads/main\n",
			ref:        "refs/heads/main",
			config:     "[remote.origin]\n\turl = https://github.com/justanotherspy/shuck\n",
			wantBranch: "main",
		},
		{
			name:       "comments and blank lines are ignored",
			head:       "ref: refs/heads/main\n",
			ref:        "refs/heads/main",
			config:     "# a comment\n\n; another\n[remote \"upstream\"]\n\turl = git@github.com:other/repo.git\n[remote \"origin\"]\n\turl = git@github.com:justanotherspy/shuck.git\n",
			wantBranch: "main",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeRepo(t, dir, tt.head, tt.ref, "abc123\n", tt.config)

			got, err := ReadCheckout(dir)
			if err != nil {
				t.Fatalf("ReadCheckout: %v", err)
			}
			if got.Owner != "justanotherspy" || got.Repo != "shuck" {
				t.Errorf("repo = %s/%s, want justanotherspy/shuck", got.Owner, got.Repo)
			}
			if got.Branch != tt.wantBranch {
				t.Errorf("Branch = %q, want %q", got.Branch, tt.wantBranch)
			}
			if tt.wantHead != "" && got.Head != tt.wantHead {
				t.Errorf("Head = %q, want %q", got.Head, tt.wantHead)
			}
		})
	}
}

func TestReadCheckoutFromSubdirectory(t *testing.T) {
	root := t.TempDir()
	writeRepo(t, root, "ref: refs/heads/main\n", "refs/heads/main", "deadbeef\n", originConfig)

	sub := filepath.Join(root, "internal", "monitor")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := ReadCheckout(sub)
	if err != nil {
		t.Fatalf("ReadCheckout: %v", err)
	}
	if got.Branch != "main" {
		t.Errorf("Branch = %q, want main", got.Branch)
	}
}

// TestReadCheckoutWorktree covers the shape the monitor exists to handle: a
// linked worktree, where .git is a file pointing at a per-worktree directory
// that holds its own HEAD but shares the main repository's config.
func TestReadCheckoutWorktree(t *testing.T) {
	main := t.TempDir()
	writeRepo(t, main, "ref: refs/heads/main\n", "refs/heads/main", "aaa\n", originConfig)

	// The main repository's per-worktree directory, as `git worktree add`
	// creates it.
	wtGit := filepath.Join(main, ".git", "worktrees", "feature")
	if err := os.MkdirAll(wtGit, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(wtGit, "HEAD"), "ref: refs/heads/feature\n")
	write(t, filepath.Join(wtGit, "commondir"), "../..\n")

	// The main repository holds the branch ref, shared across worktrees.
	refPath := filepath.Join(main, ".git", "refs", "heads", "feature")
	if err := os.MkdirAll(filepath.Dir(refPath), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, refPath, "bbb\n")

	// The linked worktree itself: a .git file, not a directory.
	wt := t.TempDir()
	write(t, filepath.Join(wt, ".git"), "gitdir: "+wtGit+"\n")

	got, err := ReadCheckout(wt)
	if err != nil {
		t.Fatalf("ReadCheckout: %v", err)
	}
	if got.Branch != "feature" {
		t.Errorf("Branch = %q, want feature", got.Branch)
	}
	if got.Owner != "justanotherspy" || got.Repo != "shuck" {
		t.Errorf("repo = %s/%s, want justanotherspy/shuck (from the shared config)", got.Owner, got.Repo)
	}
}

func TestReadCheckoutRelativeWorktreePointer(t *testing.T) {
	root := t.TempDir()
	writeRepo(t, root, "ref: refs/heads/main\n", "refs/heads/main", "aaa\n", originConfig)

	wt := filepath.Join(root, "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	// git writes a relative gitdir for a worktree inside the repository.
	write(t, filepath.Join(wt, ".git"), "gitdir: ../.git\n")

	got, err := ReadCheckout(wt)
	if err != nil {
		t.Fatalf("ReadCheckout: %v", err)
	}
	if got.Branch != "main" {
		t.Errorf("Branch = %q, want main", got.Branch)
	}
}

func TestReadCheckoutErrors(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, dir string)
		wantErr string
	}{
		{
			name:    "not a repository",
			setup:   func(*testing.T, string) {},
			wantErr: "not inside a git repository",
		},
		{
			name: "git file does not name a git directory",
			setup: func(t *testing.T, dir string) {
				write(t, filepath.Join(dir, ".git"), "not a gitdir pointer\n")
			},
			wantErr: "does not name a git directory",
		},
		{
			name: "no HEAD",
			setup: func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "read HEAD",
		},
		{
			name: "no config",
			setup: func(t *testing.T, dir string) {
				gitDir := filepath.Join(dir, ".git")
				if err := os.MkdirAll(gitDir, 0o755); err != nil {
					t.Fatal(err)
				}
				write(t, filepath.Join(gitDir, "HEAD"), "ref: refs/heads/main\n")
			},
			wantErr: "read git config",
		},
		{
			name: "no origin remote",
			setup: func(t *testing.T, dir string) {
				writeRepo(t, dir, "ref: refs/heads/main\n", "", "", "[remote \"upstream\"]\n\turl = git@github.com:other/repo.git\n")
			},
			wantErr: `no "origin" remote`,
		},
		{
			name: "origin remote with no URL",
			setup: func(t *testing.T, dir string) {
				writeRepo(t, dir, "ref: refs/heads/main\n", "", "", "[remote \"origin\"]\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n")
			},
			wantErr: `no "origin" remote`,
		},
		{
			name: "origin URL is not a GitHub remote",
			setup: func(t *testing.T, dir string) {
				writeRepo(t, dir, "ref: refs/heads/main\n", "", "", "[remote \"origin\"]\n\turl = nonsense\n")
			},
			wantErr: "cannot parse owner/repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A temp dir under the repository would find the repository's own
			// .git by walking up, so the "not a repository" case needs a root
			// that has none above it. t.TempDir is under /tmp, which does.
			dir := t.TempDir()
			tt.setup(t, dir)

			_, err := ReadCheckout(dir)
			if err == nil {
				t.Fatalf("ReadCheckout(%s) succeeded, want error containing %q", tt.name, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestCheckoutSameAndString(t *testing.T) {
	a := Checkout{Owner: "o", Repo: "r", Branch: "main", Head: "aaa"}
	b := Checkout{Owner: "o", Repo: "r", Branch: "main", Head: "bbb"}
	if !a.Same(b) {
		t.Error("checkouts differing only in Head should compare equal: an unpushed commit does not retarget a watch")
	}
	if a.Same(Checkout{Owner: "o", Repo: "r", Branch: "other"}) {
		t.Error("a different branch is a different checkout")
	}
	if got := a.String(); got != "o/r@main" {
		t.Errorf("String() = %q, want o/r@main", got)
	}
	detached := Checkout{Owner: "o", Repo: "r"}
	if got := detached.String(); got != "o/r (detached)" {
		t.Errorf("String() = %q, want o/r (detached)", got)
	}
}
