package target

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// initRepo creates a git repo in a temp dir with one commit on a named branch
// and, when remoteURL is non-empty, an "origin" remote pointing at it. It
// returns the repo's working-tree directory.
func initRepo(t *testing.T, branch, remoteURL string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		// Keep the commit hermetic and free of any user/global git config.
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", branch)
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "initial")
	if remoteURL != "" {
		run("remote", "add", "origin", remoteURL)
	}
	return dir
}

func TestLocalRepoSuccess(t *testing.T) {
	dir := initRepo(t, "feature-x", "git@github.com:acme/widgets.git")
	t.Chdir(dir)

	owner, repo, branch, err := localRepo()
	if err != nil {
		t.Fatalf("localRepo: %v", err)
	}
	if owner != "acme" || repo != "widgets" || branch != "feature-x" {
		t.Errorf("localRepo = %q/%q@%q, want acme/widgets@feature-x", owner, repo, branch)
	}
}

func TestLocalRepoNotAGitRepo(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, _, _, err := localRepo(); err == nil {
		t.Error("expected error outside a git repository")
	}
}

func TestLocalRepoNoOriginRemote(t *testing.T) {
	dir := initRepo(t, "main", "")
	t.Chdir(dir)
	if _, _, _, err := localRepo(); err == nil {
		t.Error("expected error with no 'origin' remote")
	}
}

func TestLocalRepoNonGitHubRemoteStillParses(t *testing.T) {
	// ParseRemote only needs owner/repo path segments; a non-github host still
	// yields a result, so localRepo succeeds here.
	dir := initRepo(t, "main", "git@gitlab.com:team/proj.git")
	t.Chdir(dir)
	owner, repo, _, err := localRepo()
	if err != nil {
		t.Fatalf("localRepo: %v", err)
	}
	if owner != "team" || repo != "proj" {
		t.Errorf("localRepo = %q/%q, want team/proj", owner, repo)
	}
}

func TestLocalRepoUnparseableRemote(t *testing.T) {
	// A remote URL with no owner/repo path makes ParseRemote fail.
	dir := initRepo(t, "main", "https://github.com/")
	t.Chdir(dir)
	if _, _, _, err := localRepo(); err == nil {
		t.Error("expected error for an unparseable origin URL")
	}
}

func TestResolveBarePRNumberUsesLocalRepo(t *testing.T) {
	dir := initRepo(t, "main", "git@github.com:acme/widgets.git")
	t.Chdir(dir)
	tgt, err := Resolve([]string{"42"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tgt.Owner != "acme" || tgt.Repo != "widgets" || tgt.Number != 42 {
		t.Errorf("Resolve([42]) = %+v, want acme/widgets#42", tgt)
	}
}

func TestResolveNoArgsUsesLocalBranch(t *testing.T) {
	dir := initRepo(t, "topic", "git@github.com:acme/widgets.git")
	t.Chdir(dir)
	tgt, err := Resolve(nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tgt.Owner != "acme" || tgt.Repo != "widgets" || tgt.Branch != "topic" || tgt.Number != 0 {
		t.Errorf("Resolve(nil) = %+v, want acme/widgets@topic", tgt)
	}
}

func TestResolveBarePRNumberNoRepoErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := Resolve([]string{"42"}); err == nil {
		t.Error("expected error: bare PR number with no local repo")
	}
}

func TestResolveNoArgsNoRepoErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := Resolve(nil); err == nil {
		t.Error("expected error: no args with no local repo")
	}
}

func TestResolveRepoNoArgsUsesLocalRepo(t *testing.T) {
	dir := initRepo(t, "main", "git@github.com:acme/widgets.git")
	t.Chdir(dir)
	owner, repo, err := ResolveRepo(nil)
	if err != nil {
		t.Fatalf("ResolveRepo: %v", err)
	}
	if owner != "acme" || repo != "widgets" {
		t.Errorf("ResolveRepo(nil) = %q/%q, want acme/widgets", owner, repo)
	}
}

func TestResolveRepoNoArgsNoRepoErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, _, err := ResolveRepo(nil); err == nil {
		t.Error("expected error: ResolveRepo with no args outside a repo")
	}
}
