package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/justanotherspy/shuck/internal/target"
)

const reviewsUsage = `shuck reviews (alias: r) — summarize a PR's reviews and review-comment threads.

Usage:
  shuck reviews <owner>/<repo> <pr>
  shuck reviews <pr-url>
  shuck reviews <pr>          owner/repo from the local repo
  shuck reviews               the open PR for the current branch

Exit codes:
  0 a report was produced; 2 an operational error. --exit-code is accepted here
  (it is one of the shared report flags) but has nothing to gate on: reviews
  carry no pass/fail verdict, and this command fetches only the review half, so
  it never reaches 1. To gate on a verdict use "shuck logs --exit-code" or
  "shuck --exit-code" (failing CI checks), "shuck security --exit-code" (open
  alerts), or "shuck pins --exit-code" (unpinned or stale actions).

Flags:
`

// runReviews implements `shuck reviews [target]`: the PR-reviews half of the
// report. Reviews are PR-only, so run/job targets are rejected.
func runReviews(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shuck reviews", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	registerInspectFlags(fs, &o)
	// --exit-code arrives with the shared report flags and can never fire on
	// this path: reviews have no pass/fail verdict, and the report this command
	// builds carries no CI half for exitFor to read. The shared description
	// promises "exit 1 when failing CI checks are found", which would be a plain
	// lie in `shuck reviews --help`, so the flag keeps its registration —
	// dropping it would break the callers already passing it — and loses the
	// promise. registerInspectFlags builds a fresh flag per FlagSet, so this
	// rewrites only this command's copy.
	if f := fs.Lookup("exit-code"); f != nil {
		f.Usage = "accepted for parity with the other report commands; reviews carry no verdict, so it never changes the exit code"
	}
	fs.Usage = func() {
		fmt.Fprint(stderr, reviewsUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	o.reviewsOnly = true

	tgt, err := target.Resolve(fs.Args())
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	if tgt.RunID != 0 {
		fmt.Fprintln(stderr, "shuck: reviews require a PR target, not a run/job URL")
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
