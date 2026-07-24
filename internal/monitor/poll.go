package monitor

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/justanotherspy/shuck/internal/distil"
	"github.com/justanotherspy/shuck/internal/gh"
	"github.com/justanotherspy/shuck/internal/logs"
	"github.com/justanotherspy/shuck/internal/model"
	"github.com/justanotherspy/shuck/internal/render"
	"github.com/justanotherspy/shuck/internal/target"
)

// summaryLimit bounds one event's body. A failing job's log can run to
// megabytes; what an agent needs is the first error and its context, and what a
// session can afford is a few kilobytes. distil.CapSummary does the cut on a
// line boundary and says so.
const summaryLimit = 12 << 10

// prClient is the slice of gh.Client the poller needs. It is an interface so
// the whole polling round — the part with all the interesting logic — is
// testable with a fake and no network at all.
type prClient interface {
	GetPR(ctx context.Context, owner, repo string, number int) (model.PR, error)
	FindOpenPR(ctx context.Context, owner, repo, headOwner, branch string) (int, error)
	ListJobs(ctx context.Context, owner, repo, headSHA string) (failed, cancelled []model.JobResult, running []model.RunningJob, err error)
	OtherChecks(ctx context.Context, owner, repo, sha string) ([]model.OtherCheck, error)
	JobLog(ctx context.Context, owner, repo string, jobID int64) (string, error)
	ReviewsFingerprint(ctx context.Context, owner, repo string, number int) (string, error)
	PRReviewsSince(ctx context.Context, owner, repo string, number int, since time.Time) ([]gh.PRReview, error)
	PRReviewCommentsSince(ctx context.Context, owner, repo string, number int, since time.Time) ([]gh.PRReviewComment, error)
	PRCommentThread(ctx context.Context, owner, repo string, number int, rootID int64) ([]gh.PRReviewComment, error)
	FileContent(ctx context.Context, owner, repo, path, ref string) ([]byte, error)
	RateRemaining(ctx context.Context) (remaining, limit int, err error)
}

// newPRClient builds the GitHub client the daemon polls with. It is a package
// var so tests can swap the network out from under the daemon.
var newPRClient = func(token string) prClient { return gh.New(token) }

// resolveTarget is target.Resolve, indirected so watch-spec parsing can be
// tested without a git repository underfoot.
var resolveTarget = target.Resolve

// prState is everything the poller remembers about one pull request between
// rounds. It is persisted, because the alternative — starting fresh after a
// daemon restart — would replay a PR's whole review history into a session as
// if it had all just happened.
//
// The state is keyed by target ("owner/repo#42") rather than by watch, so a
// working tree and an explicitly pinned watch that land on the same PR share
// one poll and produce one event each time, not two.
type prState struct {
	Target string `json:"target"`
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`

	// HeadSHA is the commit the last round saw. A change means a push, which
	// resets the CI half of the state.
	HeadSHA string `json:"head_sha,omitempty"`
	// Announced records that ci.started has fired for HeadSHA, so a run that
	// takes ten polls to finish announces itself once.
	Announced bool `json:"announced,omitempty"`
	// Verdict is the terminal CI verdict already reported for HeadSHA
	// ("passed" or "failed"), so a green PR polled every 90 seconds does not
	// say so every 90 seconds.
	Verdict string `json:"verdict,omitempty"`
	// ReportedJobs holds the "<job id>/<attempt>" keys already reported as
	// failed, so a re-run reports again but a re-poll does not.
	ReportedJobs []string `json:"reported_jobs,omitempty"`
	// Lifecycle is the last reported PR state (open/draft/merged/closed).
	Lifecycle string `json:"lifecycle,omitempty"`

	// ReviewFingerprint is the cheap GraphQL probe's last value. While it is
	// unchanged the review half costs exactly one small query per poll.
	ReviewFingerprint string `json:"review_fingerprint,omitempty"`
	// ReviewsSince and CommentsSince are the high-water marks for the two
	// review feeds.
	ReviewsSince  time.Time `json:"reviews_since"`
	CommentsSince time.Time `json:"comments_since"`
	// ReportedComments holds recently reported comment IDs. GitHub's `since`
	// filter is inclusive to the second and matches on update as well as
	// creation, so an edited comment or a same-second pair would otherwise
	// repeat.
	ReportedComments []int64 `json:"reported_comments,omitempty"`
	// ReportedReviews is the same guard for submitted reviews.
	ReportedReviews []int64 `json:"reported_reviews,omitempty"`

	// Running records that the last round saw jobs still in flight. The
	// cadence keys off it rather than off Verdict, because a PR with no CI at
	// all also has no verdict, and pacing that at the active interval would
	// poll a dormant branch every twelve seconds for as long as it is watched.
	Running bool `json:"running,omitempty"`
	// Poked marks a deadline a client brought forward. A poll running at the
	// time computes its own, much later, deadline and would otherwise put the
	// poke straight back — so the flag survives the round and the store
	// honors it.
	Poked bool `json:"poked,omitempty"`

	// NextPoll and Failures drive the cadence. Failures backs an exponential
	// backoff so a PR that has been deleted, or a token that has expired, stops
	// costing a request every twelve seconds.
	NextPoll   time.Time `json:"next_poll,omitzero"`
	Failures   int       `json:"failures,omitempty"`
	LastPolled time.Time `json:"last_polled,omitzero"`
	// LastError is the most recent poll failure, kept for `monitor status`.
	LastError string `json:"last_error,omitempty"`
	// Unreferenced is when the last watch stopped pointing at this target;
	// zero while one still does. See Daemon.pruneTargets.
	Unreferenced time.Time `json:"unreferenced,omitzero"`
}

// maxRemembered bounds the "already reported" lists. They exist to suppress
// duplicates across a couple of polls, not to be a permanent record, and an
// unbounded list on a PR with a thousand comments is a slow leak.
const maxRemembered = 200

// poller turns GitHub's state into events for one target at a time.
type poller struct {
	client  prClient
	extract logs.Options
	// contextLines is how many lines of the file around a review comment
	// survive into its event body.
	contextLines int
	// log receives the daemon's diagnostics.
	log io.Writer
}

// Poll runs one round for a target and returns the events it produced along
// with the updated state. It never returns an error: a failed round is itself
// reportable (as a monitor.error event, once — repeats are counted into the
// backoff instead of repeated into the feed), and a monitor that stopped
// because one call failed would be worse than useless.
func (p *poller) Poll(ctx context.Context, st prState, now time.Time) (prState, []Event) {
	pr, err := p.client.GetPR(ctx, st.Owner, st.Repo, st.Number)
	if err != nil {
		return p.fail(st, now, err)
	}

	var events []Event
	events = append(events, p.lifecycleEvents(&st, pr, now)...)
	events = append(events, p.ciEvents(ctx, &st, pr, now)...)
	events = append(events, p.reviewEvents(ctx, &st, pr, now)...)

	st.Failures = 0
	st.LastError = ""
	st.LastPolled = now
	st.NextPoll = now.Add(p.interval(ctx, st, pr))
	return st, events
}

// fail records a failed round: the first failure is reported, subsequent ones
// only lengthen the backoff. Repeating the same "could not reach GitHub" every
// interval would drown the feed it is supposed to serve.
func (p *poller) fail(st prState, now time.Time, err error) (prState, []Event) {
	st.Failures++
	st.LastPolled = now
	st.NextPoll = now.Add(backoff(st.Failures))

	msg := err.Error()
	if st.LastError == msg {
		return st, nil
	}
	st.LastError = msg
	return st, []Event{{
		Time:   now,
		Kind:   KindError,
		Target: st.Target,
		Title:  fmt.Sprintf("could not check %s", st.Target),
		Body:   msg,
	}}
}

// backoff grows the retry delay geometrically from the active interval up to
// MaxBackoff.
func backoff(failures int) time.Duration {
	d := ActiveInterval
	for range failures - 1 {
		d *= 3
		if d >= MaxBackoff {
			return MaxBackoff
		}
	}
	return min(d, MaxBackoff)
}

// interval picks the next poll delay. A run in flight is worth watching
// closely; an open PR at rest is worth a glance a minute and a half; a merged
// or closed one barely at all. When the token's remaining quota gets low
// everything stretches, so a monitor left running never becomes the reason a
// push cannot be checked.
func (p *poller) interval(ctx context.Context, st prState, pr model.PR) time.Duration {
	var d time.Duration
	switch {
	case pr.Lifecycle() == "merged" || pr.Lifecycle() == "closed":
		d = DormantInterval
	case st.Running:
		// Something is in flight, and the answer changes every few seconds.
		d = ActiveInterval
	default:
		d = IdleInterval
	}
	if remaining, _, err := p.client.RateRemaining(ctx); err == nil && remaining < LowRateThreshold {
		d *= 2
	}
	return d
}

// lifecycleEvents reports a PR moving between open, draft, merged, and closed.
func (p *poller) lifecycleEvents(st *prState, pr model.PR, now time.Time) []Event {
	life := pr.Lifecycle()
	if life == "" || life == st.Lifecycle {
		return nil
	}
	previous := st.Lifecycle
	st.Lifecycle = life
	if previous == "" {
		// First sighting: the PR's current state is not news.
		return nil
	}
	return []Event{{
		Time:   now,
		Kind:   KindPRState,
		Target: st.Target,
		Title:  fmt.Sprintf("%s is now %s — %s", st.Target, life, pr.Title),
		URL:    prURL(st.Owner, st.Repo, st.Number),
	}}
}

// ciEvents is the CI half of a round: notice a new head commit, notice checks
// starting, drill the jobs that newly went red, and report the all-green
// verdict exactly once per commit.
func (p *poller) ciEvents(ctx context.Context, st *prState, pr model.PR, now time.Time) []Event {
	if pr.HeadSHA == "" {
		return nil
	}
	if pr.HeadSHA != st.HeadSHA {
		// A push invalidates every CI conclusion we were holding.
		st.HeadSHA = pr.HeadSHA
		st.Announced = false
		st.Verdict = ""
		st.Running = false
		st.ReportedJobs = nil
	}

	failed, cancelled, running, err := p.client.ListJobs(ctx, st.Owner, st.Repo, pr.HeadSHA)
	if err != nil {
		p.logf("list jobs for %s: %v", st.Target, err)
		return nil
	}
	// Jobs in flight on a commit that already has a verdict means a re-run:
	// the previous conclusion is no longer the answer, so the commit is open
	// for judgement again. Without this a job re-run after a failure could
	// never report the pass that followed it.
	if len(running) > 0 && st.Verdict != "" {
		st.Verdict = ""
	}
	st.Running = len(running) > 0
	// OtherChecks reports only non-Actions checks that have completed and
	// failed — a pending external check is invisible to it, so the verdict is
	// about the Actions jobs plus whatever non-Actions checks have already
	// gone red.
	other, err := p.client.OtherChecks(ctx, st.Owner, st.Repo, pr.HeadSHA)
	if err != nil {
		// Non-Actions checks are supporting detail; losing them costs the
		// verdict some precision, not the round its point.
		p.logf("other checks for %s: %v", st.Target, err)
	}

	var events []Event
	if e, ok := p.startedEvent(st, pr, failed, cancelled, running, now); ok {
		events = append(events, e)
	}
	events = append(events, p.failureEvents(ctx, st, pr, append(failed, cancelled...), now)...)
	if e, ok := p.verdictEvent(st, pr, failed, cancelled, running, other, now); ok {
		events = append(events, e)
	}
	return events
}

// startedEvent fires the first time checks are seen for a head commit, so an
// agent that just pushed learns its push registered rather than wondering.
func (p *poller) startedEvent(st *prState, pr model.PR, failed, cancelled []model.JobResult, running []model.RunningJob, now time.Time) (Event, bool) {
	if st.Announced || len(failed)+len(cancelled)+len(running) == 0 {
		return Event{}, false
	}
	st.Announced = true
	if len(running) == 0 {
		// Everything was already terminal by the time we first looked; the
		// verdict says it all and a "checks started" would just be noise.
		return Event{}, false
	}
	return Event{
		Time:   now,
		Kind:   KindCIStarted,
		Target: st.Target,
		Title: fmt.Sprintf("checks running on %s (%s)",
			shortSHA(pr.HeadSHA), jobNames(running)),
		URL: prChecksURL(st.Owner, st.Repo, st.Number),
	}, true
}

// failureEvents drills the jobs that have newly gone red and turns each into an
// event carrying its distilled failing steps. Only new failures are drilled:
// downloading a log is the one genuinely expensive call in a round, and a job
// that failed three polls ago has not changed its mind.
func (p *poller) failureEvents(ctx context.Context, st *prState, pr model.PR, jobs []model.JobResult, now time.Time) []Event {
	reported := newStringSet(st.ReportedJobs)
	var events []Event

	for _, job := range jobs {
		key := fmt.Sprintf("%d/%d", job.ID, job.RunAttempt)
		if reported.has(key) {
			continue
		}
		reported.add(key)
		st.ReportedJobs = reported.slice()
		st.Verdict = "failed"

		body := p.failureBody(ctx, st, job)
		events = append(events, Event{
			Time:   now,
			Kind:   KindCIFailed,
			Target: st.Target,
			Title: fmt.Sprintf("%s %s on %s — %s",
				job.Name, conclusionVerb(job.Conclusion), shortSHA(pr.HeadSHA), pr.Title),
			Body: body,
			URL:  jobURL(st.Owner, st.Repo, job),
		})
	}
	return events
}

// failureBody renders a failed job the way `shuck logs` would: the step
// overview, then each failed step's command and its error excerpt. Sharing the
// renderer matters — an agent should not have to learn a second format for the
// same information depending on whether it asked or was told.
//
// A job whose log cannot be downloaded still produces an event. Knowing a job
// failed matters even when the reason is out of reach.
func (p *poller) failureBody(ctx context.Context, st *prState, job model.JobResult) string {
	raw, err := p.client.JobLog(ctx, st.Owner, st.Repo, job.ID)
	if err != nil {
		return fmt.Sprintf("(logs unavailable: %v)", err)
	}
	res, err := distil.CIFailure(distil.Input{
		JobName:       job.Name,
		JobConclusion: job.Conclusion,
		Steps:         job.Steps,
		RawLog:        raw,
		Options:       distil.Options{Extract: p.extract, MaxCommandLines: logs.DefaultMaxCommandLines},
	})
	if err != nil {
		return fmt.Sprintf("(could not distil the log: %v)", err)
	}

	job.FailedSteps = res.FailedSteps
	var b strings.Builder
	render.Job(&b, job)

	body, _ := distil.CapSummary(strings.TrimSpace(b.String()), summaryLimit,
		fmt.Sprintf("… truncated. Full logs: shuck logs %s/%s %d", st.Owner, st.Repo, st.Number))
	return body
}

// verdictEvent reports the moment every check on a head commit has finished.
// It fires once per commit — the event that closes the push/watch/fix loop.
//
// Knowing a commit is green takes a little care, because nothing in the API
// says so directly: ListJobs returns only failed, cancelled, and running jobs,
// and OtherChecks returns only non-Actions checks that have already gone red.
// So "green" is inferred from having watched checks run and then stop: st.
// Announced records that this commit had jobs in flight, and a round that finds
// none of them left, none failed, and no red external check is the round where
// they all passed. A commit whose checks we never saw run — one that was
// already finished when the watch began, or that has no CI at all — stays
// silent, which is right: it is a fact, not news.
func (p *poller) verdictEvent(st *prState, pr model.PR, failed, cancelled []model.JobResult, running []model.RunningJob, other []model.OtherCheck, now time.Time) (Event, bool) {
	if len(running) > 0 || st.Verdict != "" {
		return Event{}, false
	}
	if len(other) > 0 {
		// A non-Actions check went red. There are no logs to drill for these,
		// but the verdict must still say so.
		st.Verdict = "failed"
		return Event{
			Time:   now,
			Kind:   KindCIFailed,
			Target: st.Target,
			Title: fmt.Sprintf("%s failed on %s — %s",
				count(len(other), "non-Actions check"), shortSHA(pr.HeadSHA), pr.Title),
			Body: renderOtherChecks(other),
			URL:  prChecksURL(st.Owner, st.Repo, st.Number),
		}, true
	}
	if len(failed)+len(cancelled) > 0 || !st.Announced {
		// Either a failure was just reported (which set the verdict, so this
		// is unreachable in practice) or we never saw checks for this commit.
		return Event{}, false
	}

	st.Verdict = "passed"
	return Event{
		Time:   now,
		Kind:   KindCIPassed,
		Target: st.Target,
		Title:  fmt.Sprintf("all checks passed on %s — %s", shortSHA(pr.HeadSHA), pr.Title),
		URL:    prChecksURL(st.Owner, st.Repo, st.Number),
	}, true
}

// reviewEvents is the review half of a round. It leads with the cheap GraphQL
// fingerprint: while that is unchanged nothing about the PR's reviews has
// moved, and the round costs one small query instead of two REST listings.
func (p *poller) reviewEvents(ctx context.Context, st *prState, pr model.PR, now time.Time) []Event {
	fingerprint, err := p.client.ReviewsFingerprint(ctx, st.Owner, st.Repo, st.Number)
	if err != nil {
		p.logf("reviews fingerprint for %s: %v", st.Target, err)
		return nil
	}
	if fingerprint == st.ReviewFingerprint {
		return nil
	}

	if st.ReviewFingerprint == "" {
		// The first sighting of a PR is not a hundred new comments; it is the
		// state of the world. Record the high-water marks and report from here.
		st.ReviewFingerprint = fingerprint
		st.ReviewsSince = now
		st.CommentsSince = now
		return nil
	}

	reviews, reviewsOK := p.submittedReviewEvents(ctx, st, now)
	comments, commentsOK := p.commentEvents(ctx, st, pr, now)

	// The fingerprint is the gate that stops the REST listings from running on
	// every poll, so it may only advance once they have actually run. Advancing
	// it after a failed fetch would close the gate on a review nobody ever
	// heard about.
	if reviewsOK && commentsOK {
		st.ReviewFingerprint = fingerprint
	}
	return append(reviews, comments...)
}

// submittedReviewEvents reports reviews submitted since the last round. A
// review is reported as one event with its inline comments folded in, rather
// than as a verdict plus N comments, because that is how a human would read it.
func (p *poller) submittedReviewEvents(ctx context.Context, st *prState, now time.Time) (events []Event, ok bool) {
	reviews, err := p.client.PRReviewsSince(ctx, st.Owner, st.Repo, st.Number, st.ReviewsSince)
	if err != nil {
		p.logf("reviews for %s: %v", st.Target, err)
		return nil, false
	}
	seen := newInt64Set(st.ReportedReviews)
	for _, rv := range reviews {
		if seen.has(rv.ID) {
			continue
		}
		seen.add(rv.ID)
		if rv.SubmittedAt.After(st.ReviewsSince) {
			st.ReviewsSince = rv.SubmittedAt
		}

		res, err := distil.Review(distil.ReviewInput{
			Reviewer: rv.UserLogin,
			State:    rv.State,
			Body:     rv.Body,
		})
		if err != nil {
			p.logf("distil review %d: %v", rv.ID, err)
			continue
		}
		body, _ := distil.CapSummary(res.Summary, summaryLimit, "… truncated.")
		events = append(events, Event{
			Time:   now,
			Kind:   KindReviewSubmitted,
			Target: st.Target,
			Title:  fmt.Sprintf("%s %s on %s", res.Reviewer, verdictPhrase(res.Verdict), st.Target),
			Body:   body,
			URL:    prURL(st.Owner, st.Repo, st.Number),
		})
	}
	st.ReportedReviews = seen.slice()
	return events, true
}

// commentEvents reports inline review comments left since the last round, each
// with the diff hunk it is anchored to and the surrounding lines of the file at
// the PR head — so acting on a comment does not need a round trip to read the
// code it is about.
func (p *poller) commentEvents(ctx context.Context, st *prState, pr model.PR, now time.Time) (events []Event, ok bool) {
	comments, err := p.client.PRReviewCommentsSince(ctx, st.Owner, st.Repo, st.Number, st.CommentsSince)
	if err != nil {
		p.logf("review comments for %s: %v", st.Target, err)
		return nil, false
	}
	seen := newInt64Set(st.ReportedComments)
	for _, rc := range comments {
		if seen.has(rc.ID) {
			continue
		}
		seen.add(rc.ID)
		if rc.CreatedAt.After(st.CommentsSince) {
			st.CommentsSince = rc.CreatedAt
		}
		if e, ok := p.commentEvent(ctx, st, pr, rc, now); ok {
			events = append(events, e)
		}
	}
	st.ReportedComments = seen.slice()
	return events, true
}

// commentEvent distills one review comment, gathering the thread it replies to
// and the file it points at.
func (p *poller) commentEvent(ctx context.Context, st *prState, pr model.PR, rc gh.PRReviewComment, now time.Time) (Event, bool) {
	in := distil.ReviewCommentInput{
		Reviewer:     rc.UserLogin,
		Path:         rc.Path,
		Line:         rc.Line,
		StartLine:    rc.StartLine,
		Side:         rc.Side,
		Body:         rc.Body,
		DiffHunk:     rc.DiffHunk,
		ContextLines: p.contextLines,
		Thread:       p.thread(ctx, st, rc),
		FileContent:  p.fileAtHead(ctx, st, pr, rc),
	}
	res, err := distil.ReviewComment(in)
	if err != nil {
		p.logf("distil comment %d: %v", rc.ID, err)
		return Event{}, false
	}
	body, _ := distil.CapSummary(res.Summary, summaryLimit, "… truncated.")
	title := fmt.Sprintf("%s commented on %s", res.Reviewer, res.Path)
	if res.Lines != "" {
		title += ":" + res.Lines
	}
	return Event{
		Time:   now,
		Kind:   KindReviewComment,
		Target: st.Target,
		Title:  title,
		Body:   body,
		URL:    fmt.Sprintf("%s#discussion_r%d", prURL(st.Owner, st.Repo, st.Number), rc.ID),
	}, true
}

// thread fetches the earlier comments of a reply's thread. A reply read without
// what it replies to is close to meaningless, and a standalone comment costs
// nothing here.
func (p *poller) thread(ctx context.Context, st *prState, rc gh.PRReviewComment) []distil.ThreadComment {
	if rc.InReplyTo == 0 {
		return nil
	}
	comments, err := p.client.PRCommentThread(ctx, st.Owner, st.Repo, st.Number, rc.InReplyTo)
	if err != nil {
		p.logf("thread for comment %d: %v", rc.ID, err)
		return nil
	}
	var out []distil.ThreadComment
	for _, c := range comments {
		if c.ID == rc.ID {
			continue
		}
		out = append(out, distil.ThreadComment{Author: c.UserLogin, Body: c.Body})
	}
	return out
}

// fileAtHead fetches the commented file at the commit the comment is anchored
// to, for the surrounding-lines window. It degrades to "" — the summary then
// shows the diff hunk alone, which is still useful.
func (p *poller) fileAtHead(ctx context.Context, st *prState, pr model.PR, rc gh.PRReviewComment) string {
	if rc.Path == "" || rc.Line <= 0 || strings.EqualFold(rc.Side, "LEFT") {
		return ""
	}
	ref := rc.CommitID
	if ref == "" {
		ref = pr.HeadSHA
	}
	content, err := p.client.FileContent(ctx, st.Owner, st.Repo, rc.Path, ref)
	if err != nil {
		return ""
	}
	return string(content)
}

func (p *poller) logf(format string, args ...any) {
	if p.log == nil {
		return
	}
	fmt.Fprintf(p.log, "%s "+format+"\n", append([]any{time.Now().Format(time.RFC3339)}, args...)...)
}

// --- small helpers ----------------------------------------------------------

// prURL and prChecksURL build the links an event carries. They are assembled
// rather than taken from the API because the API does not return a checks-tab
// URL and shuck already understands this URL shape.
func prURL(owner, repo string, number int) string {
	return fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, number)
}

func prChecksURL(owner, repo string, number int) string {
	return prURL(owner, repo, number) + "/checks"
}

// jobURL links to a job's log view. model.JobResult carries the run and job
// ids rather than a URL, and this is the shape those ids address.
func jobURL(owner, repo string, job model.JobResult) string {
	return fmt.Sprintf("https://github.com/%s/%s/actions/runs/%d/job/%d", owner, repo, job.RunID, job.ID)
}

// shortSHA abbreviates a commit for a headline.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// conclusionVerb renders a job conclusion as the verb of a headline.
func conclusionVerb(conclusion string) string {
	if model.IsCancelledConclusion(conclusion) {
		return "was cancelled"
	}
	return "failed"
}

// verdictPhrase renders a normalized review verdict for a headline.
func verdictPhrase(verdict string) string {
	switch verdict {
	case "approved":
		return "approved"
	case "changes_requested":
		return "requested changes"
	default:
		return "commented"
	}
}

// jobNames lists the running jobs for a headline, capped so a matrix build does
// not fill the line.
func jobNames(running []model.RunningJob) string {
	const most = 3
	names := make([]string, 0, len(running))
	for _, j := range running {
		names = append(names, j.Name)
	}
	sort.Strings(names)
	if len(names) <= most {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:most], ", "), len(names)-most)
}

// renderOtherChecks lists the non-Actions checks that failed. There is no log
// to drill for these — the check's own details URL is the whole story.
func renderOtherChecks(other []model.OtherCheck) string {
	var b strings.Builder
	for _, c := range other {
		fmt.Fprintf(&b, "%s: %s", c.Name, c.Conclusion)
		if c.URL != "" {
			fmt.Fprintf(&b, "\n  %s", c.URL)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
