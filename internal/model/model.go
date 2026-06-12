// Package model holds the domain types shared across shuck's packages:
// the GitHub data we collect, the failure detail we render, and the shape we
// persist to the cache. It imports nothing internal to avoid import cycles.
package model

import "time"

// PR identifies a pull request and its head commit.
type PR struct {
	Owner      string    `json:"owner"`
	Repo       string    `json:"repo"`
	Number     int       `json:"number"`
	Title      string    `json:"title"`
	HeadSHA    string    `json:"head_sha"`
	HeadBranch string    `json:"head_branch"`
	UpdatedAt  time.Time `json:"updated_at"` // PR's last-updated time; feeds the cheap reviews-changed check
}

// AuthorType classifies who wrote a review or comment, so the output can flag
// non-human reviewers.
type AuthorType string

// Author classifications.
const (
	AuthorHuman AuthorType = "human"
	AuthorBot   AuthorType = "bot"
	AuthorAI    AuthorType = "ai"
)

// Review is a submitted PR review: its author, verdict, top-level body, and the
// inline comment threads that originated in it.
type Review struct {
	Author      string         `json:"author"`
	AuthorType  AuthorType     `json:"author_type"`
	State       string         `json:"state"` // approved|changes_requested|commented|dismissed
	Body        string         `json:"body,omitempty"`
	SubmittedAt time.Time      `json:"submitted_at"`
	Threads     []ReviewThread `json:"threads,omitempty"`
}

// ReviewThread is a conversation anchored to a code location. Resolved or
// outdated threads are collapsed: we report only why, not their contents.
type ReviewThread struct {
	Path           string          `json:"path"`
	Line           int             `json:"line"`
	Resolved       bool            `json:"resolved"`
	Outdated       bool            `json:"outdated"`
	Collapsed      bool            `json:"collapsed"` // resolved || outdated
	CollapseReason string          `json:"collapse_reason,omitempty"`
	TotalComments  int             `json:"total_comments"`
	HiddenComments int             `json:"hidden_comments,omitempty"` // comments hidden by the per-thread limit
	Comments       []ReviewComment `json:"comments,omitempty"`        // empty when collapsed
}

// ReviewComment is a single comment within a thread.
type ReviewComment struct {
	Author     string     `json:"author"`
	AuthorType AuthorType `json:"author_type"`
	Body       string     `json:"body"`
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

// FailureClass is a coarse, heuristic category for why a step failed, so an
// agent or script can route a failure (fix code vs. re-run) without re-parsing
// the excerpt. It is a hint, never authoritative.
type FailureClass string

// Failure classes. The operational classes (timeout/oom/infra) signal a likely
// re-run; the rest point at code or config to fix. Empty means unclassified.
const (
	ClassUnknown FailureClass = ""
	ClassLint    FailureClass = "lint"    // linters / formatters / static analysis
	ClassTest    FailureClass = "test"    // a test suite reported failures
	ClassBuild   FailureClass = "build"   // compilation / build step
	ClassTimeout FailureClass = "timeout" // step or job timed out
	ClassOOM     FailureClass = "oom"     // killed for running out of memory
	ClassInfra   FailureClass = "infra"   // runner / network / registry trouble
)

// FailedStep is the high-signal detail for a step that failed: what it ran and
// the extracted error excerpt from its logs.
type FailedStep struct {
	Number  int          `json:"number"`
	Name    string       `json:"name"`
	Command string       `json:"command"`
	Kind    StepKind     `json:"kind"`
	Class   FailureClass `json:"class,omitempty"`
	Excerpt string       `json:"excerpt"`
}

// Annotation is a GitHub check-run annotation: a structured file:line message
// attached to a job by a problem matcher (golangci-lint, go test, tsc, eslint,
// compilers, …). It points straight at the offending location, so shuck
// surfaces these alongside the scraped log excerpt.
type Annotation struct {
	Path        string `json:"path"`
	StartLine   int    `json:"start_line"`
	EndLine     int    `json:"end_line"`
	StartColumn int    `json:"start_column,omitempty"`
	EndColumn   int    `json:"end_column,omitempty"`
	Level       string `json:"level"` // notice | warning | failure
	Title       string `json:"title,omitempty"`
	Message     string `json:"message"`
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
	// CheckRunID is the job's corresponding check-run ID, used to fetch the
	// job's annotations. 0 when it could not be resolved.
	CheckRunID int64 `json:"check_run_id,omitempty"`
	// Annotations are the job's check-run annotations (file:line messages from
	// problem matchers), fetched as cheap metadata when the job is drilled.
	Annotations []Annotation `json:"annotations,omitempty"`
	Inspected   bool         `json:"inspected"` // logs were drilled for this (id, attempt)
}

// Artifact is a file bundle a workflow run uploaded (actions/upload-artifact).
// shuck lists a run target's artifacts alongside its jobs; Path is set only
// when a download was requested, to the local directory the artifact's archive
// was extracted into.
type Artifact struct {
	ID        int64     `json:"id"`
	RunID     int64     `json:"run_id"`
	Name      string    `json:"name"`
	SizeBytes int64     `json:"size_bytes"`
	Expired   bool      `json:"expired"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Path      string    `json:"path,omitempty"`
}

// ActionTag is a tag in a GitHub Actions repository paired with the commit SHA
// it resolves to. shuck uses it to pin a workflow `uses:` reference to an
// immutable SHA. The SHA is the peeled commit a checkout would land on, even
// for annotated tags.
type ActionTag struct {
	Name string `json:"name"`
	SHA  string `json:"sha"`
}

// ImageVersion is one published version of a container image: the immutable
// manifest digest (sha256:...) and the tags that currently point at it, with the
// time the version was last updated. shuck uses it to pin an image reference to
// its digest.
type ImageVersion struct {
	Tags      []string  `json:"tags"`
	Digest    string    `json:"digest"` // sha256:...
	UpdatedAt time.Time `json:"updated_at"`
}

// ImagePackage is a single container package (image) under an owner and the
// versions published for it, newest first as assembled by the caller.
type ImagePackage struct {
	Owner    string         `json:"owner"`
	Name     string         `json:"name"`
	Versions []ImageVersion `json:"versions"`
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

// RunInfo identifies a workflow-run inspection: shuck was pointed at a run URL
// (the whole run) or a single-job URL rather than a PR. When a Report's Run is
// non-nil, render and jsonout show a run-oriented header in place of the PR line
// and there is no associated PR number.
type RunInfo struct {
	Owner        string `json:"owner"`
	Repo         string `json:"repo"`
	RunID        int64  `json:"run_id"`
	JobID        int64  `json:"job_id,omitempty"`  // 0 when the whole run was targeted
	Attempt      int    `json:"attempt,omitempty"` // 0 when the latest attempt was used
	Title        string `json:"title"`
	HeadSHA      string `json:"head_sha"`
	HeadBranch   string `json:"head_branch"`
	WorkflowName string `json:"workflow_name"`
}

// Report is the full inspection result for a target: what we render and (for PR
// targets) what we cache. Exactly one of PR / Run is meaningful: Run is non-nil
// for run/job URL targets, otherwise the report is PR-anchored.
type Report struct {
	PR         PR          `json:"pr"`
	Run        *RunInfo    `json:"run,omitempty"`
	FailedJobs []JobResult `json:"failed_jobs"`
	// CancelledJobs are jobs whose run was cancelled. Their logs are drilled
	// best-effort (a cancelled job's log shows what was running when it was
	// interrupted), but cancellation alone never flips the exit code.
	CancelledJobs []JobResult  `json:"cancelled_jobs"`
	RunningJobs   []RunningJob `json:"running_jobs"`
	OtherChecks   []OtherCheck `json:"other_checks"`
	// Artifacts are the file bundles attached to the inspected workflow run.
	// They are listed only for run/job targets (Run non-nil), where the run is
	// unambiguous.
	Artifacts []Artifact `json:"artifacts,omitempty"`
	Reviews   []Review   `json:"reviews,omitempty"`
	// ReviewsFingerprint is a cheap signature of the PR's review state, persisted
	// so a later run can skip the full review pull when nothing changed.
	ReviewsFingerprint string `json:"reviews_fingerprint,omitempty"`
	// ReviewsOnly is a presentation hint (not persisted): CI was not inspected,
	// so render shows only the reviews and omits the CI verdict.
	ReviewsOnly bool      `json:"-"`
	CheckedAt   time.Time `json:"checked_at"`
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
// shuck surfaces cancelled jobs and drills their logs best-effort (to show what
// was interrupted), but cancellation alone never makes the exit code non-zero.
func IsCancelledConclusion(conclusion string) bool {
	return conclusion == "cancelled"
}

// IsDrillableConclusion reports whether a step's conclusion is worth pairing
// with the log's error sections: a genuine failure, or the cancellation marker
// GitHub puts on the step that was running when its job was cancelled.
func IsDrillableConclusion(conclusion string) bool {
	return IsFailureConclusion(conclusion) || IsCancelledConclusion(conclusion)
}
