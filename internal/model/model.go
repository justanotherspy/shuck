// Package model holds the domain types shared across shuck's packages:
// the GitHub data we collect, the failure detail we render, and the shape we
// persist to the cache. It imports nothing internal to avoid import cycles.
package model

import "time"

// PR identifies a pull request and its head commit.
type PR struct {
	Owner      string `json:"owner"`
	Repo       string `json:"repo"`
	Number     int    `json:"number"`
	Title      string `json:"title"`
	HeadSHA    string `json:"head_sha"`
	HeadBranch string `json:"head_branch"`
}

// StepOverview is one entry in a job's authoritative ordered step list.
type StepOverview struct {
	Number     int    `json:"number"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// StepKind classifies how a step ran, derived from its log group header.
type StepKind string

// Step command kinds, derived from a step's log group header.
const (
	KindAction  StepKind = "action" // uses: owner/action@ref
	KindBash    StepKind = "bash"   // run: shell command
	KindUnknown StepKind = ""
)

// FailedStep is the high-signal detail for a step that failed: what it ran and
// the extracted error excerpt from its logs.
type FailedStep struct {
	Number  int      `json:"number"`
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Kind    StepKind `json:"kind"`
	Excerpt string   `json:"excerpt"`
}

// JobResult is a single GitHub Actions job, including its step overview and (for
// failed jobs that were drilled) the per-step failure detail.
type JobResult struct {
	ID           int64          `json:"id"`
	RunID        int64          `json:"run_id"`
	Name         string         `json:"name"`
	Status       string         `json:"status"`
	Conclusion   string         `json:"conclusion"`
	RunAttempt   int            `json:"run_attempt"`
	WorkflowName string         `json:"workflow_name"`
	WorkflowPath string         `json:"workflow_path"`
	Steps        []StepOverview `json:"steps"`
	FailedSteps  []FailedStep   `json:"failed_steps"`
	Inspected    bool           `json:"inspected"` // logs were drilled for this (id, attempt)
}

// OtherCheck is a non-Actions check (external app check run or legacy commit
// status). We list these by name/conclusion/url; no logs are available.
type OtherCheck struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"url"`
}

// RunningJob is a job not yet in a terminal state.
type RunningJob struct {
	Name         string `json:"name"`
	Status       string `json:"status"`
	WorkflowName string `json:"workflow_name"`
}

// Report is the full inspection result for a PR: what we render and what we cache.
type Report struct {
	PR          PR           `json:"pr"`
	FailedJobs  []JobResult  `json:"failed_jobs"`
	RunningJobs []RunningJob `json:"running_jobs"`
	OtherChecks []OtherCheck `json:"other_checks"`
	CheckedAt   time.Time    `json:"checked_at"`
}

// HasFailures reports whether any failing checks were found.
func (r *Report) HasFailures() bool {
	return len(r.FailedJobs) > 0 || len(r.OtherChecks) > 0
}

// IsTerminal reports whether every check has finished (no jobs still running).
func (r *Report) IsTerminal() bool {
	return len(r.RunningJobs) == 0
}

// IsFailureConclusion reports whether a terminal conclusion counts as a CI
// failure worth drilling into or listing.
func IsFailureConclusion(conclusion string) bool {
	switch conclusion {
	case "failure", "timed_out", "startup_failure", "action_required":
		return true
	default:
		return false
	}
}
