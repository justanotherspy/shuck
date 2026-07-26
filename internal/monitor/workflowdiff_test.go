package monitor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestIsWorkflowPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{".github/workflows/ci.yml", true},
		{".github/workflows/release.yaml", true},
		{"action.yml", true},
		{"action.yaml", true},
		{".github/actions/setup/action.yml", true},
		// Not a workflow: the right directory is not enough without the right
		// extension, and the right extension is not enough anywhere else.
		{".github/workflows/README.md", false},
		{".github/dependabot.yml", false},
		{"internal/monitor/pins.go", false},
		{"docs/actions.yml", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isWorkflowPath(tt.path); got != tt.want {
			t.Errorf("isWorkflowPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

// gitRepo builds a real checkout with a base branch, because the thing under
// test is a git question and a fabricated .git directory cannot answer it.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	writeAt(t, filepath.Join(dir, "README.md"), "hello\n")
	run("add", "-A")
	run("commit", "-qm", "initial")
	// origin/main is what baseRef looks for. A local ref under refs/remotes is
	// indistinguishable from a fetched one for rev-parse's purposes, and it
	// keeps the test off the network.
	run("update-ref", "refs/remotes/origin/main", "HEAD")
	run("checkout", "-qb", "feature")
	return dir
}

func writeAt(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitAll(t *testing.T, dir, msg string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", msg}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func TestBranchTouchesWorkflowsIgnoresABranchThatDidNot(t *testing.T) {
	dir := gitRepo(t)
	writeAt(t, filepath.Join(dir, "internal", "thing.go"), "package thing\n")
	commitAll(t, dir, "unrelated change")

	if branchTouchesWorkflows(context.Background(), dir) {
		t.Error("a branch that changed no workflow should not subscribe to pin findings")
	}
}

func TestBranchTouchesWorkflowsSeesAnEditedWorkflow(t *testing.T) {
	dir := gitRepo(t)
	writeAt(t, filepath.Join(dir, ".github", "workflows", "ci.yml"), "name: CI\n")
	commitAll(t, dir, "add a workflow")

	if !branchTouchesWorkflows(context.Background(), dir) {
		t.Error("a branch that added a workflow should subscribe to pin findings")
	}
}

func TestBranchTouchesWorkflowsSeesACompositeAction(t *testing.T) {
	dir := gitRepo(t)
	writeAt(t, filepath.Join(dir, ".github", "actions", "setup", "action.yml"), "name: setup\n")
	commitAll(t, dir, "add a composite action")

	if !branchTouchesWorkflows(context.Background(), dir) {
		t.Error("a composite action definition carries `uses:` lines and should subscribe")
	}
}

// A branch sitting exactly on its base has changed nothing, which is the state
// of every session that has not started work yet — and the one most likely to be
// watched for hours.
func TestBranchTouchesWorkflowsIsQuietOnTheBaseBranch(t *testing.T) {
	dir := gitRepo(t)
	if branchTouchesWorkflows(context.Background(), dir) {
		t.Error("a branch with no commits of its own should not subscribe")
	}
}

// Failing closed is the whole safety argument for shelling out to git here: a
// directory that is not a checkout must be silence, not noise.
func TestBranchTouchesWorkflowsFailsClosed(t *testing.T) {
	if branchTouchesWorkflows(context.Background(), t.TempDir()) {
		t.Error("a directory that is not a git checkout should not subscribe")
	}
	if branchTouchesWorkflows(context.Background(), "") {
		t.Error("an empty path should not subscribe")
	}
}
