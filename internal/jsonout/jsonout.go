// Package jsonout renders an inspection Report as machine-readable JSON with a
// stable, versioned schema. Its view types are deliberately separate from
// internal/model so internal refactors and cache-only fields don't leak into,
// or break, the contract programmatic consumers depend on.
package jsonout

import (
	"encoding/json"
	"io"

	"github.com/justanotherspy/shuck/internal/model"
)

// SchemaVersion is the version of the document shuck emits with --json. It is
// bumped only on a breaking change; additive fields keep the same version.
const SchemaVersion = 1

// Document is the top-level JSON value emitted for an inspection. Exactly one
// of pr / run carries the target's head context: run is present (and pr left
// zero-valued) for run/job URL targets, otherwise the report is PR-anchored.
type Document struct {
	SchemaVersion int            `json:"schema_version"`
	PR            PR             `json:"pr"`
	Run           *Run           `json:"run,omitempty"`
	Summary       Summary        `json:"summary"`
	FailedJobs    []Job          `json:"failed_jobs"`
	CancelledJobs []CancelledJob `json:"cancelled_jobs"`
	OtherChecks   []OtherCheck   `json:"other_checks"`
	RunningJobs   []RunningJob   `json:"running_jobs"`
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
}

// Job is a failed GitHub Actions job and its failing steps.
type Job struct {
	ID           int64        `json:"id"`
	RunID        int64        `json:"run_id"`
	Name         string       `json:"name"`
	Conclusion   string       `json:"conclusion"`
	WorkflowName string       `json:"workflow_name"`
	WorkflowPath string       `json:"workflow_path"`
	FailedSteps  []FailedStep `json:"failed_steps"`
}

// FailedStep is one failing step: what it ran and the extracted error excerpt.
type FailedStep struct {
	Number  int    `json:"number"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Command string `json:"command"`
	Excerpt string `json:"excerpt"`
}

// OtherCheck is a non-Actions failing check (no logs available).
type OtherCheck struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"url"`
}

// CancelledJob is a completed job whose run was cancelled (no logs drilled).
type CancelledJob struct {
	Name         string `json:"name"`
	Conclusion   string `json:"conclusion"`
	WorkflowName string `json:"workflow_name"`
}

// RunningJob is a job not yet in a terminal state.
type RunningJob struct {
	Name         string `json:"name"`
	Status       string `json:"status"`
	WorkflowName string `json:"workflow_name"`
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
		},
		// Initialize as empty (not nil) so each list serializes as [] not null.
		FailedJobs:    make([]Job, 0, len(r.FailedJobs)),
		CancelledJobs: make([]CancelledJob, 0, len(r.CancelledJobs)),
		OtherChecks:   make([]OtherCheck, 0, len(r.OtherChecks)),
		RunningJobs:   make([]RunningJob, 0, len(r.RunningJobs)),
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
		job := Job{
			ID:           j.ID,
			RunID:        j.RunID,
			Name:         j.Name,
			Conclusion:   j.Conclusion,
			WorkflowName: j.WorkflowName,
			WorkflowPath: j.WorkflowPath,
			FailedSteps:  make([]FailedStep, 0, len(j.FailedSteps)),
		}
		for _, s := range j.FailedSteps {
			job.FailedSteps = append(job.FailedSteps, FailedStep{
				Number:  s.Number,
				Name:    s.Name,
				Kind:    string(s.Kind),
				Command: s.Command,
				Excerpt: s.Excerpt,
			})
		}
		doc.FailedJobs = append(doc.FailedJobs, job)
	}

	for _, j := range r.CancelledJobs {
		doc.CancelledJobs = append(doc.CancelledJobs, CancelledJob{
			Name:         j.Name,
			Conclusion:   j.Conclusion,
			WorkflowName: j.WorkflowName,
		})
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

	return doc
}
