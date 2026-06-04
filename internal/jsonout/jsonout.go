// Package jsonout renders an inspection Report as machine-readable JSON with a
// stable, versioned schema. Its view types are deliberately separate from
// internal/model so internal refactors and cache-only fields don't leak into,
// or break, the contract programmatic consumers depend on.
package jsonout

import (
	"encoding/json"
	"io"
	"time"

	"github.com/justanotherspy/shuck/internal/model"
)

// SchemaVersion is the version of the document shuck emits with --json. It is
// bumped only on a breaking change; additive fields keep the same version.
const SchemaVersion = 1

// Document is the top-level JSON value emitted for an inspection. Exactly one
// of pr / run carries the target's head context: run is present (and pr left
// zero-valued) for run/job URL targets, otherwise the report is PR-anchored.
type Document struct {
	SchemaVersion int     `json:"schema_version"`
	PR            PR      `json:"pr"`
	Run           *Run    `json:"run,omitempty"`
	Summary       Summary `json:"summary"`
	FailedJobs    []Job   `json:"failed_jobs"`
	// CancelledJobs share the Job shape: when a cancelled job's logs were
	// drilled, failed_steps holds the interrupted step and its last output.
	CancelledJobs []Job        `json:"cancelled_jobs"`
	OtherChecks   []OtherCheck `json:"other_checks"`
	RunningJobs   []RunningJob `json:"running_jobs"`
	Reviews       []Review     `json:"reviews"`
}

// Run identifies a workflow-run (or single-job) target and its head context. It
// is present only when shuck was pointed at a GitHub Actions URL.
type Run struct {
	Owner        string `json:"owner"`
	Repo         string `json:"repo"`
	RunID        int64  `json:"run_id"`
	JobID        int64  `json:"job_id,omitempty"`
	Title        string `json:"title"`
	HeadSHA      string `json:"head_sha"`
	HeadBranch   string `json:"head_branch"`
	WorkflowName string `json:"workflow_name"`
}

// PR identifies the inspected pull request and its head commit.
type PR struct {
	Owner      string `json:"owner"`
	Repo       string `json:"repo"`
	Number     int    `json:"number"`
	Title      string `json:"title"`
	HeadSHA    string `json:"head_sha"`
	HeadBranch string `json:"head_branch"`
}

// Summary is a quick count of what was found, so a consumer can branch without
// walking every list.
type Summary struct {
	Failed      int `json:"failed"`
	Cancelled   int `json:"cancelled"`
	Running     int `json:"running"`
	OtherFailed int `json:"other_failed"`
	Reviews     int `json:"reviews"`
}

// Job is a failed (or cancelled) GitHub Actions job and its failing — or, for a
// cancelled job, interrupted — steps.
type Job struct {
	ID           int64        `json:"id"`
	RunID        int64        `json:"run_id"`
	Name         string       `json:"name"`
	Conclusion   string       `json:"conclusion"`
	WorkflowName string       `json:"workflow_name"`
	WorkflowPath string       `json:"workflow_path"`
	FailedSteps  []FailedStep `json:"failed_steps"`
	// Annotations are the job's check-run annotations (file:line messages from
	// problem matchers), independent of the step↔log pairing.
	Annotations []Annotation `json:"annotations"`
}

// FailedStep is one failing step: what it ran and the extracted error excerpt.
// Class is a coarse heuristic category ("lint", "test", "build", "timeout",
// "oom", "infra"); it is omitted when shuck could not classify the failure.
type FailedStep struct {
	Number  int    `json:"number"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Class   string `json:"class,omitempty"`
	Command string `json:"command"`
	Excerpt string `json:"excerpt"`
}

// Annotation is a check-run annotation: a structured file:line message from a
// problem matcher, pointing straight at the offending location.
type Annotation struct {
	Path        string `json:"path"`
	StartLine   int    `json:"start_line"`
	EndLine     int    `json:"end_line"`
	StartColumn int    `json:"start_column,omitempty"`
	EndColumn   int    `json:"end_column,omitempty"`
	Level       string `json:"level"`
	Title       string `json:"title,omitempty"`
	Message     string `json:"message"`
}

// OtherCheck is a non-Actions failing check (no logs available).
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

// Review is a submitted PR review with its inline threads.
type Review struct {
	Author      string         `json:"author"`
	AuthorType  string         `json:"author_type"`
	State       string         `json:"state"`
	Body        string         `json:"body"`
	SubmittedAt string         `json:"submitted_at"`
	Threads     []ReviewThread `json:"threads"`
}

// ReviewThread is a code-anchored conversation; collapsed when resolved/outdated.
type ReviewThread struct {
	Path           string          `json:"path"`
	Line           int             `json:"line"`
	Resolved       bool            `json:"resolved"`
	Outdated       bool            `json:"outdated"`
	Collapsed      bool            `json:"collapsed"`
	CollapseReason string          `json:"collapse_reason"`
	TotalComments  int             `json:"total_comments"`
	HiddenComments int             `json:"hidden_comments"`
	Comments       []ReviewComment `json:"comments"`
}

// ReviewComment is one comment within a thread.
type ReviewComment struct {
	Author     string `json:"author"`
	AuthorType string `json:"author_type"`
	Body       string `json:"body"`
}

// Encode writes r to w as an indented JSON Document with a trailing newline.
func Encode(w io.Writer, r *model.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(NewDocument(r))
}

// NewDocument projects an inspection Report onto the stable, versioned JSON
// view. Non-CLI consumers (such as the MCP server) use it to return the same
// document shape as --json without going through the byte encoder.
func NewDocument(r *model.Report) Document {
	doc := Document{
		SchemaVersion: SchemaVersion,
		PR: PR{
			Owner:      r.PR.Owner,
			Repo:       r.PR.Repo,
			Number:     r.PR.Number,
			Title:      r.PR.Title,
			HeadSHA:    r.PR.HeadSHA,
			HeadBranch: r.PR.HeadBranch,
		},
		Summary: Summary{
			Failed:      len(r.FailedJobs),
			Cancelled:   len(r.CancelledJobs),
			Running:     len(r.RunningJobs),
			OtherFailed: len(r.OtherChecks),
			Reviews:     len(r.Reviews),
		},
		// Initialize as empty (not nil) so each list serializes as [] not null.
		FailedJobs:    make([]Job, 0, len(r.FailedJobs)),
		CancelledJobs: make([]Job, 0, len(r.CancelledJobs)),
		OtherChecks:   make([]OtherCheck, 0, len(r.OtherChecks)),
		RunningJobs:   make([]RunningJob, 0, len(r.RunningJobs)),
		Reviews:       make([]Review, 0, len(r.Reviews)),
	}

	if r.Run != nil {
		doc.Run = &Run{
			Owner:        r.Run.Owner,
			Repo:         r.Run.Repo,
			RunID:        r.Run.RunID,
			JobID:        r.Run.JobID,
			Title:        r.Run.Title,
			HeadSHA:      r.Run.HeadSHA,
			HeadBranch:   r.Run.HeadBranch,
			WorkflowName: r.Run.WorkflowName,
		}
	}

	for _, j := range r.FailedJobs {
		doc.FailedJobs = append(doc.FailedJobs, newJob(j))
	}

	for _, j := range r.CancelledJobs {
		doc.CancelledJobs = append(doc.CancelledJobs, newJob(j))
	}

	for _, c := range r.OtherChecks {
		doc.OtherChecks = append(doc.OtherChecks, OtherCheck{
			Name:       c.Name,
			Conclusion: c.Conclusion,
			URL:        c.URL,
		})
	}

	for _, j := range r.RunningJobs {
		doc.RunningJobs = append(doc.RunningJobs, RunningJob{
			Name:         j.Name,
			Status:       j.Status,
			WorkflowName: j.WorkflowName,
		})
	}

	for _, rv := range r.Reviews {
		review := Review{
			Author:      rv.Author,
			AuthorType:  string(rv.AuthorType),
			State:       rv.State,
			Body:        rv.Body,
			SubmittedAt: rv.SubmittedAt.Format(time.RFC3339),
			Threads:     make([]ReviewThread, 0, len(rv.Threads)),
		}
		for _, t := range rv.Threads {
			thread := ReviewThread{
				Path:           t.Path,
				Line:           t.Line,
				Resolved:       t.Resolved,
				Outdated:       t.Outdated,
				Collapsed:      t.Collapsed,
				CollapseReason: t.CollapseReason,
				TotalComments:  t.TotalComments,
				HiddenComments: t.HiddenComments,
				Comments:       make([]ReviewComment, 0, len(t.Comments)),
			}
			for _, c := range t.Comments {
				thread.Comments = append(thread.Comments, ReviewComment{
					Author:     c.Author,
					AuthorType: string(c.AuthorType),
					Body:       c.Body,
				})
			}
			review.Threads = append(review.Threads, thread)
		}
		doc.Reviews = append(doc.Reviews, review)
	}

	return doc
}

// newJob projects a model job (failed or cancelled) onto the stable Job view.
func newJob(j model.JobResult) Job {
	job := Job{
		ID:           j.ID,
		RunID:        j.RunID,
		Name:         j.Name,
		Conclusion:   j.Conclusion,
		WorkflowName: j.WorkflowName,
		WorkflowPath: j.WorkflowPath,
		FailedSteps:  make([]FailedStep, 0, len(j.FailedSteps)),
		Annotations:  make([]Annotation, 0, len(j.Annotations)),
	}
	for _, s := range j.FailedSteps {
		job.FailedSteps = append(job.FailedSteps, FailedStep{
			Number:  s.Number,
			Name:    s.Name,
			Kind:    string(s.Kind),
			Class:   string(s.Class),
			Command: s.Command,
			Excerpt: s.Excerpt,
		})
	}
	for _, an := range j.Annotations {
		job.Annotations = append(job.Annotations, Annotation{
			Path:        an.Path,
			StartLine:   an.StartLine,
			EndLine:     an.EndLine,
			StartColumn: an.StartColumn,
			EndColumn:   an.EndColumn,
			Level:       an.Level,
			Title:       an.Title,
			Message:     an.Message,
		})
	}
	return job
}
