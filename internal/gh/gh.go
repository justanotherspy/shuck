// Package gh wraps go-github with the narrow set of calls shuck needs: resolving
// a PR, listing its Actions runs/jobs, downloading a job's logs, and enumerating
// non-Actions checks.
package gh

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/go-github/v88/github"

	"github.com/justanotherspy/shuck/internal/model"
)

// Client talks to the GitHub REST and GraphQL APIs.
type Client struct {
	gh    *github.Client
	http  *http.Client
	token string // retained for the hand-rolled GraphQL calls (reviews)
}

// New builds a client from a personal access token. An empty token yields an
// unauthenticated client (subject to GitHub's lower anonymous rate limit),
// which suffices for read-only calls against public repositories.
func New(token string) *Client {
	var opts []github.ClientOptionsFunc
	if token != "" {
		opts = append(opts, github.WithAuthToken(token))
	}
	// NewClient only errors for option funcs that can fail (e.g. enterprise
	// URLs); WithAuthToken never does, so the error is structurally nil here.
	gc, _ := github.NewClient(opts...)
	return &Client{
		gh:    gc,
		http:  &http.Client{Timeout: 60 * time.Second},
		token: token,
	}
}

// GetPR resolves a PR's head SHA, branch, title, and last-updated time.
func (c *Client) GetPR(ctx context.Context, owner, repo string, number int) (model.PR, error) {
	pr, _, err := c.gh.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return model.PR{}, fmt.Errorf("get PR %s/%s#%d: %w", owner, repo, number, err)
	}
	return model.PR{
		Owner:      owner,
		Repo:       repo,
		Number:     number,
		Title:      pr.GetTitle(),
		HeadSHA:    pr.GetHead().GetSHA(),
		HeadBranch: pr.GetHead().GetRef(),
		UpdatedAt:  pr.GetUpdatedAt().Time,
	}, nil
}

// DefaultBranchSHA returns the latest commit SHA on the repo's default branch
// (ref "HEAD") in a single call — the cheap invalidation signal for the action
// and security caches.
func (c *Client) DefaultBranchSHA(ctx context.Context, owner, repo string) (string, error) {
	sha, _, err := c.gh.Repositories.GetCommitSHA1(ctx, owner, repo, "HEAD", "")
	if err != nil {
		return "", fmt.Errorf("get default branch SHA %s/%s: %w", owner, repo, err)
	}
	return sha, nil
}

// FindOpenPR returns the number of the open PR whose head is headOwner:branch.
func (c *Client) FindOpenPR(ctx context.Context, owner, repo, headOwner, branch string) (int, error) {
	opts := &github.PullRequestListOptions{
		State:       "open",
		Head:        headOwner + ":" + branch,
		ListOptions: github.ListOptions{PerPage: 10},
	}
	prs, _, err := c.gh.PullRequests.List(ctx, owner, repo, opts)
	if err != nil {
		return 0, fmt.Errorf("list PRs for %s/%s head %s:%s: %w", owner, repo, headOwner, branch, err)
	}
	if len(prs) == 0 {
		return 0, fmt.Errorf("no open PR found for branch %q in %s/%s", branch, owner, repo)
	}
	return prs[0].GetNumber(), nil
}

// ListJobs returns the failed, cancelled, and still-running Actions jobs for a
// head commit. Failed jobs come back with their step overview populated but no
// log detail yet; cancelled jobs are listed without log detail.
func (c *Client) ListJobs(ctx context.Context, owner, repo, headSHA string) (failed []model.JobResult, cancelled []model.CancelledJob, running []model.RunningJob, err error) {
	runs, err := c.listRuns(ctx, owner, repo, headSHA)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, run := range runs {
		jobs, jerr := c.listRunJobs(ctx, owner, repo, run.GetID())
		if jerr != nil {
			return nil, nil, nil, jerr
		}
		f, c2, r2 := classifyJobs(run, jobs)
		failed = append(failed, f...)
		cancelled = append(cancelled, c2...)
		running = append(running, r2...)
	}
	return failed, cancelled, running, nil
}

// RunReport inspects a single workflow run by ID. When jobID is non-zero it
// restricts the result to that one job (a job-URL target); otherwise it
// classifies every job in the run. It also returns the run's head context for
// the report header. Non-Actions checks do not apply to a run, so none are
// returned.
func (c *Client) RunReport(ctx context.Context, owner, repo string, runID, jobID int64) (model.RunInfo, []model.JobResult, []model.CancelledJob, []model.RunningJob, error) {
	run, _, err := c.gh.Actions.GetWorkflowRunByID(ctx, owner, repo, runID)
	if err != nil {
		return model.RunInfo{}, nil, nil, nil, fmt.Errorf("get run %d for %s/%s: %w", runID, owner, repo, err)
	}
	info := model.RunInfo{
		Owner:        owner,
		Repo:         repo,
		RunID:        runID,
		JobID:        jobID,
		Title:        run.GetDisplayTitle(),
		HeadSHA:      run.GetHeadSHA(),
		HeadBranch:   run.GetHeadBranch(),
		WorkflowName: run.GetName(),
	}

	var jobs []*github.WorkflowJob
	if jobID != 0 {
		job, _, jerr := c.gh.Actions.GetWorkflowJobByID(ctx, owner, repo, jobID)
		if jerr != nil {
			return info, nil, nil, nil, fmt.Errorf("get job %d for %s/%s: %w", jobID, owner, repo, jerr)
		}
		jobs = []*github.WorkflowJob{job}
	} else if jobs, err = c.listRunJobs(ctx, owner, repo, runID); err != nil {
		return info, nil, nil, nil, err
	}

	failed, cancelled, running := classifyJobs(run, jobs)
	return info, failed, cancelled, running, nil
}

// classifyJobs sorts a run's jobs into the failed (drillable), cancelled, and
// still-running buckets shuck reports.
func classifyJobs(run *github.WorkflowRun, jobs []*github.WorkflowJob) (failed []model.JobResult, cancelled []model.CancelledJob, running []model.RunningJob) {
	for _, job := range jobs {
		if job.GetStatus() != "completed" {
			running = append(running, model.RunningJob{
				Name:         job.GetName(),
				Status:       job.GetStatus(),
				WorkflowName: job.GetWorkflowName(),
			})
			continue
		}
		switch {
		case model.IsFailureConclusion(job.GetConclusion()):
			failed = append(failed, jobResult(run, job))
		case model.IsCancelledConclusion(job.GetConclusion()):
			cancelled = append(cancelled, model.CancelledJob{
				Name:         job.GetName(),
				Conclusion:   job.GetConclusion(),
				WorkflowName: job.GetWorkflowName(),
			})
		}
	}
	return failed, cancelled, running
}

func jobResult(run *github.WorkflowRun, job *github.WorkflowJob) model.JobResult {
	jr := model.JobResult{
		ID:           job.GetID(),
		RunID:        run.GetID(),
		Name:         job.GetName(),
		Status:       job.GetStatus(),
		Conclusion:   job.GetConclusion(),
		RunAttempt:   int(job.GetRunAttempt()),
		WorkflowName: job.GetWorkflowName(),
		WorkflowPath: run.GetPath(),
	}
	for _, st := range job.Steps {
		jr.Steps = append(jr.Steps, model.StepOverview{
			Number:     int(st.GetNumber()),
			Name:       st.GetName(),
			Status:     st.GetStatus(),
			Conclusion: st.GetConclusion(),
		})
	}
	return jr
}

func (c *Client) listRuns(ctx context.Context, owner, repo, sha string) ([]*github.WorkflowRun, error) {
	opts := &github.ListWorkflowRunsOptions{
		HeadSHA:     sha,
		ListOptions: github.ListOptions{PerPage: 100},
	}
	var out []*github.WorkflowRun
	for {
		runs, resp, err := c.gh.Actions.ListRepositoryWorkflowRuns(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("list workflow runs for %s: %w", sha, err)
		}
		out = append(out, runs.WorkflowRuns...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

func (c *Client) listRunJobs(ctx context.Context, owner, repo string, runID int64) ([]*github.WorkflowJob, error) {
	opts := &github.ListWorkflowJobsOptions{
		Filter:      "latest",
		ListOptions: github.ListOptions{PerPage: 100},
	}
	var out []*github.WorkflowJob
	for {
		jobs, resp, err := c.gh.Actions.ListWorkflowJobs(ctx, owner, repo, runID, opts)
		if err != nil {
			return nil, fmt.Errorf("list jobs for run %d: %w", runID, err)
		}
		out = append(out, jobs.Jobs...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

// JobLog downloads the plain-text log for a single job by following the
// short-lived redirect GitHub returns.
func (c *Client) JobLog(ctx context.Context, owner, repo string, jobID int64) (string, error) {
	u, _, err := c.gh.Actions.GetWorkflowJobLogs(ctx, owner, repo, jobID, 4)
	if err != nil {
		return "", fmt.Errorf("get log URL for job %d: %w", jobID, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("download log for job %d: %w", jobID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download log for job %d: status %s", jobID, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read log for job %d: %w", jobID, err)
	}
	return string(body), nil
}

// OtherChecks lists failing non-Actions checks: external app check runs and
// legacy commit statuses. No logs are available for these.
func (c *Client) OtherChecks(ctx context.Context, owner, repo, sha string) ([]model.OtherCheck, error) {
	var out []model.OtherCheck

	copts := &github.ListCheckRunsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		res, resp, err := c.gh.Checks.ListCheckRunsForRef(ctx, owner, repo, sha, copts)
		if err != nil {
			return nil, fmt.Errorf("list check runs for %s: %w", sha, err)
		}
		for _, cr := range res.CheckRuns {
			if cr.GetApp().GetSlug() == "github-actions" {
				continue
			}
			if cr.GetStatus() != "completed" {
				continue
			}
			if model.IsFailureConclusion(cr.GetConclusion()) {
				out = append(out, model.OtherCheck{
					Name:       cr.GetName(),
					Conclusion: cr.GetConclusion(),
					URL:        cr.GetDetailsURL(),
				})
			}
		}
		if resp.NextPage == 0 {
			break
		}
		copts.Page = resp.NextPage
	}

	sopts := &github.ListOptions{PerPage: 100}
	for {
		cs, resp, err := c.gh.Repositories.GetCombinedStatus(ctx, owner, repo, sha, sopts)
		if err != nil {
			return nil, fmt.Errorf("get combined status for %s: %w", sha, err)
		}
		for _, s := range cs.Statuses {
			switch s.GetState() {
			case "failure", "error":
				out = append(out, model.OtherCheck{
					Name:       s.GetContext(),
					Conclusion: s.GetState(),
					URL:        s.GetTargetURL(),
				})
			}
		}
		if resp.NextPage == 0 {
			break
		}
		sopts.Page = resp.NextPage
	}

	return out, nil
}
