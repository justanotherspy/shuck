package worker

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/justanotherspy/shuck/internal/gh"
	"github.com/justanotherspy/shuck/internal/ingest"
)

// DefaultMaxJobs caps how many drillable jobs one envelope fetches logs for.
// The summary cap bounds the payload regardless; this bounds fetch time
// against the queue's visibility timeout on pathological runs.
const DefaultMaxJobs = 20

// GHFetcher is the production RunFetcher: it lists the run's jobs and
// downloads each drillable job's log via internal/gh, authenticated with the
// minted installation token. It implements RunFetcher.
type GHFetcher struct {
	// APIBase is the GitHub API base URL; "" means the public API.
	APIBase string
	// MaxJobs caps drillable jobs per run; 0 means DefaultMaxJobs, negative
	// means unlimited.
	MaxJobs int
	// Log may be nil, which means slog.Default().
	Log *slog.Logger
}

// FetchRun fetches the failed run's drillable jobs. A single job's log
// download degrading (expired, cancelled-before-start) is recorded on the
// job, never an error — only failing to enumerate the run's jobs at all
// fails the fetch.
func (f *GHFetcher) FetchRun(ctx context.Context, token string, env ingest.Envelope) (RunFailure, error) {
	owner, repo, ok := splitRepo(env.Repo)
	if !ok {
		return RunFailure{}, fmt.Errorf("envelope repo %q is not owner/name", env.Repo)
	}
	c, err := gh.NewEnterprise(token, f.APIBase)
	if err != nil {
		return RunFailure{}, err
	}

	_, failed, cancelled, _, err := c.RunReport(ctx, owner, repo, env.RunID, 0, 0)
	if err != nil {
		return RunFailure{}, err
	}

	drillable := slices.Concat(failed, cancelled)
	if maxJobs := f.maxJobs(); maxJobs > 0 && len(drillable) > maxJobs {
		f.log().Warn("capping drillable jobs", "repo", env.Repo, "run", env.RunID,
			"jobs", len(drillable), "max", maxJobs)
		drillable = drillable[:maxJobs]
	}

	out := RunFailure{RateRemaining: -1}
	for _, job := range drillable {
		jf := JobFailure{ID: job.ID, Name: job.Name, Conclusion: job.Conclusion, Steps: job.Steps}
		raw, lerr := c.JobLog(ctx, owner, repo, job.ID)
		if lerr != nil {
			// Degrade per job: a cancelled-before-start job has no log, and
			// an expired one must not sink the other jobs' summaries.
			f.log().Warn("job log unavailable", "repo", env.Repo, "run", env.RunID,
				"job", job.ID, "err", lerr)
			jf.LogError = lerr.Error()
		} else {
			jf.RawLog = raw
		}
		out.Jobs = append(out.Jobs, jf)
	}

	if remaining, _, rerr := c.RateRemaining(ctx); rerr == nil {
		out.RateRemaining = remaining
	}
	return out, nil
}

// splitRepo splits an envelope's "owner/name" repo field.
func splitRepo(repo string) (owner, name string, ok bool) {
	owner, name, found := strings.Cut(repo, "/")
	if !found || owner == "" || name == "" || strings.ContainsRune(name, '/') {
		return "", "", false
	}
	return owner, name, true
}

func (f *GHFetcher) maxJobs() int {
	if f.MaxJobs == 0 {
		return DefaultMaxJobs
	}
	return f.MaxJobs
}

func (f *GHFetcher) log() *slog.Logger {
	if f.Log == nil {
		return slog.Default()
	}
	return f.Log
}

// Interface conformance is part of the package contract.
var (
	_ RunFetcher  = (*GHFetcher)(nil)
	_ TokenSource = (*AppTokenSource)(nil)
	_ Deliverer   = (*HTTPDeliverer)(nil)
)
