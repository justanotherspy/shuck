// Package gh wraps go-github with the narrow set of calls shuck needs: resolving
// a PR, listing its Actions runs/jobs, downloading a job's logs, and enumerating
// non-Actions checks.
package gh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v89/github"

	"github.com/justanotherspy/shuck/internal/model"
	"github.com/justanotherspy/shuck/internal/target"
)

// Client talks to the GitHub REST and GraphQL APIs.
type Client struct {
	gh    *github.Client
	http  *http.Client
	token string // retained for the hand-rolled GraphQL calls (reviews)
	// graphqlURL and registryURL default to the public GitHub GraphQL endpoint
	// and the GHCR registry host; they are fields (rather than the consts) only
	// so tests can point the hand-rolled clients at a local httptest server.
	graphqlURL  string
	registryURL string
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
		gh:          gc,
		http:        &http.Client{Timeout: 60 * time.Second},
		token:       token,
		graphqlURL:  graphQLEndpoint,
		registryURL: registryHost,
	}
}

// NewEnterprise builds a client like New but pointed at a non-public GitHub
// API base URL (a GHES instance, or an httptest server in tests). The URL is
// normalized by go-github's enterprise rules (an "/api/v3/" suffix is added
// unless the host already looks like an API host). An empty base falls back
// to New. Used by the v2 worker so its GitHub calls are configurable and
// testable; the CLI keeps calling New.
func NewEnterprise(token, baseURL string) (*Client, error) {
	if baseURL == "" {
		return New(token), nil
	}
	opts := []github.ClientOptionsFunc{github.WithEnterpriseURLs(baseURL, baseURL)}
	if token != "" {
		opts = append(opts, github.WithAuthToken(token))
	}
	gc, err := github.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("build GitHub client for %q: %w", baseURL, err)
	}
	return &Client{
		gh:          gc,
		http:        &http.Client{Timeout: 60 * time.Second},
		token:       token,
		graphqlURL:  graphQLEndpoint,
		registryURL: registryHost,
	}, nil
}

// RateRemaining reports the core REST rate limit's remaining and total
// quota. The /rate_limit endpoint does not count against the quota, so this
// is a free observability probe (the v2 worker exports it as a gauge — all
// installations share one App quota).
func (c *Client) RateRemaining(ctx context.Context) (remaining, limit int, err error) {
	rl, _, err := c.gh.RateLimit.Get(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("get rate limit: %w", err)
	}
	core := rl.GetCore()
	if core == nil {
		return 0, 0, errors.New("rate limit response has no core rate")
	}
	return core.Remaining, core.Limit, nil
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
// head commit. Failed and cancelled jobs come back with their step overview
// populated but no log detail yet; the caller drills the logs.
func (c *Client) ListJobs(ctx context.Context, owner, repo, headSHA string) (failed, cancelled []model.JobResult, running []model.RunningJob, err error) {
	runs, err := c.listRuns(ctx, owner, repo, headSHA)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, run := range runs {
		jobs, jerr := c.listRunJobs(ctx, owner, repo, run.GetID(), 0)
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
// classifies every job in the run. When attempt is non-zero it inspects that
// specific run attempt rather than the latest. It also returns the run's head
// context for the report header. Non-Actions checks do not apply to a run, so
// none are returned.
func (c *Client) RunReport(ctx context.Context, owner, repo string, runID, jobID int64, attempt int) (info model.RunInfo, failed, cancelled []model.JobResult, running []model.RunningJob, err error) {
	var run *github.WorkflowRun
	if attempt != 0 {
		run, _, err = c.gh.Actions.GetWorkflowRunAttempt(ctx, owner, repo, runID, attempt, nil)
	} else {
		run, _, err = c.gh.Actions.GetWorkflowRunByID(ctx, owner, repo, runID)
	}
	if err != nil {
		return model.RunInfo{}, nil, nil, nil, fmt.Errorf("get run %d for %s/%s: %w", runID, owner, repo, err)
	}
	info = model.RunInfo{
		Owner:        owner,
		Repo:         repo,
		RunID:        runID,
		JobID:        jobID,
		Attempt:      attempt,
		Title:        run.GetDisplayTitle(),
		HeadSHA:      run.GetHeadSHA(),
		HeadBranch:   run.GetHeadBranch(),
		WorkflowName: run.GetName(),
	}

	var jobs []*github.WorkflowJob
	switch {
	case jobID != 0:
		job, _, jerr := c.gh.Actions.GetWorkflowJobByID(ctx, owner, repo, jobID)
		if jerr != nil {
			return info, nil, nil, nil, fmt.Errorf("get job %d for %s/%s: %w", jobID, owner, repo, jerr)
		}
		jobs = []*github.WorkflowJob{job}
	default:
		if jobs, err = c.listRunJobs(ctx, owner, repo, runID, attempt); err != nil {
			return info, nil, nil, nil, err
		}
	}

	failed, cancelled, running = classifyJobs(run, jobs)
	return info, failed, cancelled, running, nil
}

// CheckRunTarget resolves a check-run ID (from a PR "Checks" tab URL) to the
// GitHub Actions run and job it represents, by reading the check run's details
// URL. It returns (0, 0, nil) when the check run is not an Actions check (its
// details URL is not a run/job URL), so callers can fall back to a PR-wide
// inspection — a non-Actions check has no downloadable logs anyway.
func (c *Client) CheckRunTarget(ctx context.Context, owner, repo string, checkRunID int64) (runID, jobID int64, err error) {
	cr, _, err := c.gh.Checks.GetCheckRun(ctx, owner, repo, checkRunID)
	if err != nil {
		return 0, 0, fmt.Errorf("get check run %d for %s/%s: %w", checkRunID, owner, repo, err)
	}
	ref, ok := target.ParseActionsURL(cr.GetDetailsURL())
	if !ok {
		return 0, 0, nil
	}
	return ref.RunID, ref.JobID, nil
}

// classifyJobs sorts a run's jobs into the failed, cancelled, and still-running
// buckets shuck reports. Failed and cancelled jobs are both drillable: they
// carry the full step overview and IDs needed to download their logs.
func classifyJobs(run *github.WorkflowRun, jobs []*github.WorkflowJob) (failed, cancelled []model.JobResult, running []model.RunningJob) {
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
			cancelled = append(cancelled, jobResult(run, job))
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
		CheckRunID:   checkRunID(job.GetCheckRunURL()),
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

// listRunJobs lists a run's jobs. With attempt == 0 it returns the latest
// attempt's jobs; with a specific attempt it returns that attempt's jobs via
// the per-attempt API.
func (c *Client) listRunJobs(ctx context.Context, owner, repo string, runID int64, attempt int) ([]*github.WorkflowJob, error) {
	opts := &github.ListWorkflowJobsOptions{
		Filter:      "latest",
		ListOptions: github.ListOptions{PerPage: 100},
	}
	var out []*github.WorkflowJob
	for {
		var (
			jobs *github.Jobs
			resp *github.Response
			err  error
		)
		if attempt != 0 {
			jobs, resp, err = c.gh.Actions.ListWorkflowJobsAttempt(ctx, owner, repo, runID, int64(attempt), &opts.ListOptions)
		} else {
			jobs, resp, err = c.gh.Actions.ListWorkflowJobs(ctx, owner, repo, runID, opts)
		}
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

// checkRunID extracts the trailing numeric ID from a job's check-run URL
// (".../check-runs/123456"). An Actions job's ID and its check-run ID differ,
// and only the latter keys the annotations API. Returns 0 when the URL is empty
// or unparseable, which callers treat as "no annotations to fetch".
func checkRunID(url string) int64 {
	i := strings.LastIndex(url, "/")
	if i < 0 || i+1 >= len(url) {
		return 0
	}
	id, err := strconv.ParseInt(url[i+1:], 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// JobAnnotations returns a job's check-run annotations (file:line messages from
// problem matchers). Annotations are cheap metadata, so shuck re-fetches them
// each run rather than caching them. A zero checkRunID yields no annotations.
func (c *Client) JobAnnotations(ctx context.Context, owner, repo string, checkRunID int64) ([]model.Annotation, error) {
	if checkRunID == 0 {
		return nil, nil
	}
	var out []model.Annotation
	opts := &github.ListOptions{PerPage: 100}
	for {
		anns, resp, err := c.gh.Checks.ListCheckRunAnnotations(ctx, owner, repo, checkRunID, opts)
		if err != nil {
			return nil, fmt.Errorf("list annotations for check run %d: %w", checkRunID, err)
		}
		for _, a := range anns {
			out = append(out, model.Annotation{
				Path:        a.GetPath(),
				StartLine:   a.GetStartLine(),
				EndLine:     a.GetEndLine(),
				StartColumn: a.GetStartColumn(),
				EndColumn:   a.GetEndColumn(),
				Level:       a.GetAnnotationLevel(),
				Title:       a.GetTitle(),
				Message:     a.GetMessage(),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
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
