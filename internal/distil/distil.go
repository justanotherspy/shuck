// Package distil turns a raw GitHub Actions job log plus the job's step
// metadata into the distilled failure detail shuck reports: which steps
// failed, what they ran, the high-signal error excerpt, and a heuristic
// failure class. It is the shared parser core behind the CLI, the MCP server,
// and the background monitor, which turns the same distillation into the body
// of an event.
//
// The package is pure and deterministic: strings and structs in, structs
// out — no GitHub calls, no filesystem, no clock. Fetching logs and step
// metadata stays with the callers.
//
// The step↔section pairing works from the log itself plus the Actions
// API's ordered step list — not from workflow YAML: failed (or
// interrupted) API steps are matched with ##[error]-bearing log sections
// by order, with a whole-log fallback when the log carries no error
// marker at all.
package distil

import (
	"fmt"

	"github.com/justanotherspy/shuck/internal/classify"
	"github.com/justanotherspy/shuck/internal/logs"
	"github.com/justanotherspy/shuck/internal/model"
)

// Options tunes distillation. The zero value means "no excerpt budget"
// (nearly-empty excerpts) just like a zero logs.Options — use
// DefaultOptions() for the documented defaults.
type Options struct {
	// Extract tunes how much of a failing section's output survives into
	// the excerpt.
	Extract logs.Options
	// MaxCommandLines caps how many lines of a step's recovered command are
	// kept; longer commands are truncated with a marker. <= 0 means no limit.
	MaxCommandLines int
}

// DefaultOptions returns the documented defaults, matching the CLI's flag
// defaults.
func DefaultOptions() Options {
	return Options{Extract: logs.DefaultOptions(), MaxCommandLines: logs.DefaultMaxCommandLines}
}

// Input is one failed (or cancelled) job's raw material: the plain-text
// job log as GitHub serves it (timestamps included) and the job metadata
// from the Actions API.
type Input struct {
	// JobName names the job in the Summary; "" falls back to "job".
	JobName string
	// JobConclusion is the job's terminal conclusion ("failure",
	// "cancelled", "timed_out", …). It steers the cancelled-job pairing cap
	// and the failure classification.
	JobConclusion string
	// Steps is the job's authoritative ordered step list from the Actions
	// API; the failed/interrupted entries are paired with the log's error
	// sections by order.
	Steps []model.StepOverview
	// RawLog is the whole plain-text job log.
	RawLog string
	// Options tunes the distillation; the zero value is NOT the defaults
	// (see Options).
	Options Options
}

// Result is the distilled failure: the per-step detail the CLI renders,
// plus an agent-ready one-paragraph summary.
type Result struct {
	FailedSteps []model.FailedStep `json:"failed_steps"`
	Summary     string             `json:"summary"`
}

// CIFailure distills a failed job's log into per-step failure detail. It
// pairs the API's failed (or interrupted) steps with the log's
// error-bearing sections (by order) to recover each step's command and
// error excerpt. For a cancelled job the interrupted step carries an
// "##[error]The operation was canceled." marker, so the same pairing
// applies. The only error is invalid Options (negative extraction knobs);
// callers that pre-validate can treat it as impossible.
func CIFailure(in Input) (Result, error) {
	if err := validate(in.Options); err != nil {
		return Result{}, err
	}

	sections := logs.Parse(in.RawLog)
	errSecs := logs.ErrorSections(sections)

	var failedSteps []model.StepOverview
	for _, s := range in.Steps {
		if model.IsDrillableConclusion(s.Conclusion) {
			failedSteps = append(failedSteps, s)
		}
	}

	if len(errSecs) == 0 {
		var all []string
		for _, sec := range sections {
			all = append(all, sec.Body...)
		}
		fs := model.FailedStep{Name: "(job log)", Excerpt: logs.Extract(all, in.Options.Extract)}
		if len(failedSteps) > 0 {
			fs.Name = failedSteps[0].Name
			fs.Number = failedSteps[0].Number
		}
		fs.Class = classify.Classify(fs, in.JobConclusion)
		steps := []model.FailedStep{fs}
		return Result{FailedSteps: steps, Summary: summarize(in, steps)}, nil
	}

	n := max(len(errSecs), len(failedSteps))
	if model.IsCancelledConclusion(in.JobConclusion) {
		// A cancelled job often marks every not-yet-run step "cancelled", but
		// only the step that was actually interrupted has an error section.
		// Cap at the sections found so the queued steps don't each emit a
		// noisy "(no matching error log section found)" entry.
		n = len(errSecs)
	}
	out := make([]model.FailedStep, 0, n)
	for i := range n {
		fs := model.FailedStep{Name: "(unnamed step)"}
		if i < len(failedSteps) {
			fs.Number = failedSteps[i].Number
			fs.Name = failedSteps[i].Name
		}
		if i < len(errSecs) {
			sec := errSecs[i]
			fs.Command = logs.ClampCommand(sec.FullCommand(), in.Options.MaxCommandLines)
			fs.Kind = sec.Kind()
			fs.Excerpt = logs.Extract(sec.Body, in.Options.Extract)
			fs.Class = classify.Classify(fs, in.JobConclusion)
		} else {
			fs.Excerpt = "(no matching error log section found)"
		}
		out = append(out, fs)
	}
	return Result{FailedSteps: out, Summary: summarize(in, out)}, nil
}

// validate rejects extraction knobs no caller can mean: the CLI
// pre-validates these with flag-named errors, so these plain messages are
// for programmatic callers passing untrusted values.
func validate(o Options) error {
	for _, f := range []struct {
		name string
		val  int
	}{
		{"short threshold", o.Extract.ShortThreshold},
		{"context", o.Extract.Context},
		{"tail", o.Extract.Tail},
	} {
		if f.val < 0 {
			return fmt.Errorf("distil: %s must be non-negative, got %d", f.name, f.val)
		}
	}
	return nil
}
