package monitor

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/justanotherspy/shuck/internal/gh"
	"github.com/justanotherspy/shuck/internal/logs"
	"github.com/justanotherspy/shuck/internal/model"
)

// fakeClient stands in for the GitHub client so a whole polling round can be
// exercised without a network. Each field is the canned answer for one call;
// counts records how often each was asked, which is how the tests assert the
// poller's cost discipline — that a quiet PR does not download logs, and that an
// unchanged review fingerprint does not trigger the REST listings.
type fakeClient struct {
	mu sync.Mutex

	pr        model.PR
	prErr     error
	openPR    int
	openPRErr error

	failed    []model.JobResult
	cancelled []model.JobResult
	running   []model.RunningJob
	jobsErr   error

	other    []model.OtherCheck
	otherErr error

	jobLog    string
	jobLogErr error

	fingerprint    string
	fingerprintErr error

	reviews    []gh.PRReview
	reviewsErr error

	comments    []gh.PRReviewComment
	commentsErr error

	thread []gh.PRReviewComment

	file    []byte
	fileErr error

	rateRemaining int
	rateLimit     int
	rateErr       error

	tags map[string][]model.ActionTag

	counts map[string]int
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		rateRemaining: 4000,
		rateLimit:     5000,
		counts:        map[string]int{},
	}
}

func (f *fakeClient) count(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts[name]++
}

func (f *fakeClient) calls(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[name]
}

func (f *fakeClient) GetPR(context.Context, string, string, int) (model.PR, error) {
	f.count("GetPR")
	return f.pr, f.prErr
}

func (f *fakeClient) FindOpenPR(context.Context, string, string, string, string) (int, error) {
	f.count("FindOpenPR")
	return f.openPR, f.openPRErr
}

func (f *fakeClient) ListJobs(context.Context, string, string, string) (failed, cancelled []model.JobResult, running []model.RunningJob, err error) {
	f.count("ListJobs")
	return f.failed, f.cancelled, f.running, f.jobsErr
}

func (f *fakeClient) OtherChecks(context.Context, string, string, string) ([]model.OtherCheck, error) {
	f.count("OtherChecks")
	return f.other, f.otherErr
}

func (f *fakeClient) JobLog(context.Context, string, string, int64) (string, error) {
	f.count("JobLog")
	return f.jobLog, f.jobLogErr
}

func (f *fakeClient) ReviewsFingerprint(context.Context, string, string, int) (string, error) {
	f.count("ReviewsFingerprint")
	return f.fingerprint, f.fingerprintErr
}

func (f *fakeClient) PRReviewsSince(_ context.Context, _, _ string, _ int, since time.Time) ([]gh.PRReview, error) {
	f.count("PRReviewsSince")
	if f.reviewsErr != nil {
		return nil, f.reviewsErr
	}
	var out []gh.PRReview
	for _, r := range f.reviews {
		if r.SubmittedAt.After(since) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeClient) PRReviewCommentsSince(_ context.Context, _, _ string, _ int, since time.Time) ([]gh.PRReviewComment, error) {
	f.count("PRReviewCommentsSince")
	if f.commentsErr != nil {
		return nil, f.commentsErr
	}
	var out []gh.PRReviewComment
	for _, c := range f.comments {
		if !c.CreatedAt.Before(since) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeClient) PRCommentThread(context.Context, string, string, int, int64) ([]gh.PRReviewComment, error) {
	f.count("PRCommentThread")
	return f.thread, nil
}

func (f *fakeClient) FileContent(context.Context, string, string, string, string) ([]byte, error) {
	f.count("FileContent")
	return f.file, f.fileErr
}

func (f *fakeClient) RateRemaining(context.Context) (remaining, limit int, err error) {
	f.count("RateRemaining")
	return f.rateRemaining, f.rateLimit, f.rateErr
}

func (f *fakeClient) ListActionTags(_ context.Context, owner, repo string) ([]model.ActionTag, error) {
	f.count("ListActionTags")
	slug := owner + "/" + repo
	tags, ok := f.tags[slug]
	if !ok {
		return nil, fmt.Errorf("no tags for %s", slug)
	}
	return tags, nil
}

// testPoller builds a poller wired to a fake client, with the extraction
// defaults the daemon would use.
func testPoller(c prClient) *poller {
	return &poller{client: c, extract: logs.DefaultOptions()}
}
