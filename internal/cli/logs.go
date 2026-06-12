package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/justanotherspy/shuck/internal/target"
)

const logsUsage = `shuck logs (alias: l) — show the exact failing CI step logs for a PR or a single run.

Usage:
  shuck logs [target]         inspect a PR's failing CI (target forms below)
  shuck logs --run <id|url>   inspect a single GitHub Actions run

Target forms (when --run is not given):
  shuck logs <owner>/<repo> <pr>
  shuck logs <pr-url> | <run-url> | <job-url>
  shuck logs <pr>             owner/repo from the local repo
  shuck logs                  the open PR for the current branch

--run accepts a run ID (repo inferred from the local repo, or pass owner/repo)
or a github.com/.../actions/runs/<id> URL.

Run targets also list the artifacts the run uploaded; add
--download-artifacts <dir> to download them, each extracted to <dir>/<name>/.

Flags:
`

// runLogs implements `shuck logs [target] [--run <id|url>]`: the failing-CI
// half of the report, for a PR or a single workflow run.
func runLogs(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shuck logs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	registerInspectFlags(fs, &o)
	registerArtifactFlags(fs, &o)
	var runRef string
	fs.StringVar(&runRef, "run", "", "inspect a single workflow run by run ID or run/job URL instead of a PR")
	fs.Usage = func() {
		fmt.Fprint(stderr, logsUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	o.ciOnly = true

	var tgt target.Target
	var err error
	if runRef != "" {
		tgt, err = runTarget(runRef, fs.Args())
	} else {
		tgt, err = target.Resolve(fs.Args())
	}
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}

	report, err := inspectWith(context.Background(), tgt, o)
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	code, err := emit(stdout, report, o)
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	return code
}

// runTarget builds a run/job Target from --run, accepting either a run/job URL
// or a bare run ID (repo from a positional owner/repo, else the local repo).
func runTarget(runRef string, positional []string) (target.Target, error) {
	if strings.ContainsAny(runRef, "/:") {
		tgt, err := target.Resolve([]string{runRef})
		if err != nil {
			return target.Target{}, err
		}
		if tgt.RunID == 0 {
			return target.Target{}, fmt.Errorf("--run %q is not a GitHub Actions run or job URL", runRef)
		}
		return tgt, nil
	}
	id, err := strconv.ParseInt(runRef, 10, 64)
	if err != nil || id <= 0 {
		return target.Target{}, fmt.Errorf("--run must be a run ID or a github.com/.../actions/runs/<id> URL, got %q", runRef)
	}
	owner, repo, err := target.ResolveRepo(positional)
	if err != nil {
		return target.Target{}, err
	}
	return target.Target{Owner: owner, Repo: repo, RunID: id}, nil
}
