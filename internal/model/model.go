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

// CancelledJob is a completed job whose run was cancelled. We surface it so it
// is not silently dropped, but we do not drill its logs: a cancelled job has no
// genuine failure to extract.
type CancelledJob struct {
	Name         string `json:"name"`
	Conclusion   string `json:"conclusion"`
	WorkflowName string `json:"workflow_name"`
}

// RunInfo identifies a workflow-run inspection: shuck was pointed at a run URL
// (the whole run) or a single-job URL rather than a PR. When a Report's Run is
// non-nil, render and jsonout show a run-oriented header in place of the PR line
// and there is no associated PR number.
type RunInfo struct {
	Owner        string `json:"owner"`
	Repo         string `json:"repo"`
	RunID        int64  `json:"run_id"`
	JobID        int64  `json:"job_id,omitempty"` // 0 when the whole run was targeted
	Title        string `json:"title"`
	HeadSHA      string `json:"head_sha"`
	HeadBranch   string `json:"head_branch"`
	WorkflowName string `json:"workflow_name"`
}

// Report is the full inspection result for a target: what we render and (for PR
// targets) what we cache. Exactly one of PR / Run is meaningful: Run is non-nil
// for run/job URL targets, otherwise the report is PR-anchored.
type Report struct {
	PR            PR             `json:"pr"`
	Run           *RunInfo       `json:"run,omitempty"`
	FailedJobs    []JobResult    `json:"failed_jobs"`
	CancelledJobs []CancelledJob `json:"cancelled_jobs"`
	RunningJobs   []RunningJob   `json:"running_jobs"`
	OtherChecks   []OtherCheck   `json:"other_checks"`
	CheckedAt     time.Time      `json:"checked_at"`
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

// IsCancelledConclusion reports whether a terminal conclusion is a cancellation.
// shuck surfaces cancelled jobs in the summary but does not drill their logs.
func IsCancelledConclusion(conclusion string) bool {
	return conclusion == "cancelled"
}
