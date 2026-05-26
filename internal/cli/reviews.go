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

Flags:
`

// runReviews implements `shuck reviews [target]`: the PR-reviews half of the
// report. Reviews are PR-only, so run/job targets are rejected.
func runReviews(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shuck reviews", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	registerInspectFlags(fs, &o)
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
	code, err := emit(stdout, report, o.json)
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	return code
}
