package monitor

import (
	"context"
	"os/exec"
	"path"
	"strings"
	"time"
)

// Stale action pins are repo hygiene, and repo hygiene belongs to whoever
// chose to touch it. A branch that never opened a workflow file inherits every
// pin the repository has drifted on since it was written — findings that are
// real, that the agent did not cause, and that it cannot land a fix for without
// widening a pull request it was not asked to widen. Reported anyway they are
// pure noise, and noise on this channel is expensive twice over: every line a
// plugin monitor writes wakes the session, and a monitor that writes too many
// risks being stopped for volume, taking the CI failures with it.
//
// So the subscription is opt-in by action: edit a workflow, and the pins in that
// checkout become yours to hear about. Leave them alone and they stay silent.

// workflowDiffTimeout bounds the two git calls below. They are local plumbing
// against an already-open repository, so this is a guard against a pathological
// checkout rather than a budget anything is expected to spend.
const workflowDiffTimeout = 5 * time.Second

// baseRefCandidates are the refs a branch is compared against, in order. The
// symbolic ref is what a cloned repository actually carries; the two named
// fallbacks cover a checkout whose origin/HEAD was never set, which is the
// common state of a repository created by `git init` and pushed.
var baseRefCandidates = []string{"origin/HEAD", "origin/main", "origin/master"}

// branchTouchesWorkflows reports whether the checkout at dir has changed any
// GitHub Actions definition relative to its base branch.
//
// It answers false for anything it cannot determine — no git, no base ref, a
// detached HEAD, an unreadable tree. Failing closed is deliberate: the cost of a
// wrong false is a stale pin nobody is told about in a branch that probably did
// not touch workflows anyway, and the cost of a wrong true is the noise this
// whole file exists to stop.
func branchTouchesWorkflows(ctx context.Context, dir string) bool {
	if dir == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, workflowDiffTimeout)
	defer cancel()

	base, ok := baseRef(ctx, dir)
	if !ok {
		return false
	}
	// Three dots: what this branch changed since it diverged, not everything
	// that has happened on the base since. A two-dot diff would hand a branch
	// every workflow edit that landed on main while it was open, which is
	// precisely the "somebody else's hygiene" case being excluded here.
	out, err := gitOutput(ctx, dir, "diff", "--name-only", base+"...HEAD")
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(out, "\n") {
		if isWorkflowPath(strings.TrimSpace(line)) {
			return true
		}
	}
	return false
}

// baseRef picks the first candidate ref the checkout actually has.
func baseRef(ctx context.Context, dir string) (string, bool) {
	for _, ref := range baseRefCandidates {
		if _, err := gitOutput(ctx, dir, "rev-parse", "--verify", "--quiet", ref); err == nil {
			return ref, true
		}
	}
	return "", false
}

// isWorkflowPath reports whether a repository-relative path is one `shuck pins`
// would audit: the workflows directory, a composite action, or the repository's
// own action definition. It mirrors pins.WorkflowFiles rather than sharing code
// with it because that function walks a filesystem and this one classifies
// strings git printed.
func isWorkflowPath(p string) bool {
	if p == "" {
		return false
	}
	ext := path.Ext(p)
	if ext != ".yml" && ext != ".yaml" {
		return false
	}
	if strings.HasPrefix(p, ".github/workflows/") {
		return true
	}
	// action.yml at the repository root, or in any directory — a composite
	// action's definition is the other thing that carries `uses:` lines.
	return strings.TrimSuffix(path.Base(p), ext) == "action"
}

// gitOutput runs one read-only git command in dir and returns its stdout.
//
// Nothing here is attacker-controlled in the way the linter has to assume. The
// subcommands and their flags are the package constants above; the only values
// that vary are dir, which is a working tree the user themselves asked the
// monitor to watch, and a ref drawn from baseRefCandidates. All of them arrive
// as separate argv entries rather than through a shell, so a directory with a
// space or a semicolon in its name is one argument and not two commands.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	// #nosec G204 -- fixed subcommands; dir is a user-registered working tree,
	// passed as argv rather than through a shell.
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
