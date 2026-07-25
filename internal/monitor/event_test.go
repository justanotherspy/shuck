package monitor

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestKindSeverity(t *testing.T) {
	// The Stop hook only speaks up for actionable events, so this mapping is
	// what decides whether a passing build can delay an agent from finishing.
	actionable := []Kind{KindCIFailed, KindReviewComment, KindReviewSubmitted}
	// monitor.error and pins.stale are informational on purpose: a failed poll
	// is the monitor's problem and a stale pin is repo hygiene nobody asked
	// about, so neither may hold a finished turn open.
	informational := []Kind{KindCIPassed, KindCIStarted, KindPRState, KindTarget, KindError, KindPinsStale}

	for _, k := range actionable {
		if k.Severity() != SeverityAction {
			t.Errorf("%s should be actionable", k)
		}
	}
	for _, k := range informational {
		if k.Severity() != SeverityInfo {
			t.Errorf("%s should be informational — it must never delay a finish", k)
		}
	}
}

// TestEventSeverityByReviewVerdict is the rule that keeps the Stop hook from
// answering "LGTM" with "address this as part of the current task": the kind is
// the same for every submitted review, so only the verdict can tell an approval
// apart from a request.
func TestEventSeverityByReviewVerdict(t *testing.T) {
	tests := []struct {
		verdict string
		want    Severity
	}{
		{"approved", SeverityInfo},
		{"changes_requested", SeverityAction},
		{"commented", SeverityAction},
		// An unrecognized verdict has to fall to the safe side: silently
		// dropping a review the monitor could not classify is the failure this
		// monitor exists not to have.
		{"dismissed", SeverityAction},
	}

	for _, tt := range tests {
		t.Run(tt.verdict, func(t *testing.T) {
			e := reviewEvent("octocat", tt.verdict, "o/r#7")
			if got := e.Severity(); got != tt.want {
				t.Errorf("Severity() = %q for %q (title %q), want %q", got, tt.verdict, e.Title, tt.want)
			}
		})
	}
}

// TestApprovedHeadline pins the coupling that TestEventSeverityByReviewVerdict
// rests on. approvedHeadline reads a verdict back out of a headline the poller
// wrote, so the two must agree on its shape; building the headline here through
// the poller's own verdictPhrase is what makes a change to that wording fail
// loudly instead of quietly restoring the bug.
func TestApprovedHeadline(t *testing.T) {
	if !approvedHeadline(reviewEvent("octocat", "approved", "o/r#7").Title) {
		t.Error("the poller's approval headline is no longer recognized as one")
	}
	for _, verdict := range []string{"changes_requested", "commented"} {
		if approvedHeadline(reviewEvent("octocat", verdict, "o/r#7").Title) {
			t.Errorf("a %q review reads as an approval", verdict)
		}
	}
	// A login is never empty in practice, but a headline shuck cannot parse
	// must come back actionable rather than be waved through as an approval.
	for _, title := range []string{"", "approved", "approved on o/r#7"} {
		if approvedHeadline(title) {
			t.Errorf("title %q should not read as an approval headline", title)
		}
	}
}

// reviewEvent builds a submitted-review event exactly as poll.go's
// submittedReviewEvents does, verdictPhrase included.
func reviewEvent(login, verdict, target string) Event {
	return Event{
		Kind:   KindReviewSubmitted,
		Target: target,
		Title:  fmt.Sprintf("%s %s on %s", login, verdictPhrase(verdict), target),
	}
}

// TestFormatFeedDoesNotDemandActionOnAnApproval is the same rule at the level
// the agent actually reads: an approval must not be counted into the "needs your
// attention" line, or the feed asks for work that does not exist.
func TestFormatFeedDoesNotDemandActionOnAnApproval(t *testing.T) {
	got := FormatFeed([]Event{reviewEvent("octocat", "approved", "o/r#7")})
	if strings.Contains(got, "your attention") {
		t.Errorf("an approval should not be framed as something to act on:\n%s", got)
	}
}

func TestEventText(t *testing.T) {
	e := Event{
		ID:     1,
		Time:   time.Date(2026, 7, 24, 14, 30, 5, 0, time.UTC),
		Kind:   KindCIFailed,
		Target: "o/r#7",
		Title:  "test failed on abc1234",
		Body:   "line one\nline two",
		URL:    "https://github.com/o/r/actions/runs/1/job/2",
	}

	got := e.Text()
	for _, want := range []string{"ci.failed", "o/r#7", "test failed on abc1234", e.URL, "line one", "line two"} {
		if !strings.Contains(got, want) {
			t.Errorf("Text() is missing %q:\n%s", want, got)
		}
	}
	// The body is indented under its header so a batch stays readable.
	if !strings.Contains(got, "    line one") {
		t.Errorf("body should be indented:\n%s", got)
	}
	if strings.HasSuffix(got, "\n") {
		t.Error("Text() should not end with a newline; the caller spaces the batch")
	}
}

func TestEventTextWithoutBody(t *testing.T) {
	e := Event{Kind: KindCIPassed, Target: "o/r#7", Title: "all checks passed"}
	got := e.Text()
	if !strings.Contains(got, "all checks passed") {
		t.Errorf("Text() = %q", got)
	}
	if strings.Contains(got, "\n\n") {
		t.Errorf("a bodyless event should not leave a blank line:\n%q", got)
	}
}

func TestFormatFeed(t *testing.T) {
	t.Run("empty batches render as nothing", func(t *testing.T) {
		if got := FormatFeed(nil); got != "" {
			t.Errorf("FormatFeed(nil) = %q, want empty so the hook can stay silent", got)
		}
	})

	t.Run("actionable batch says what is expected", func(t *testing.T) {
		got := FormatFeed([]Event{
			{Kind: KindCIFailed, Target: "o/r#7", Title: "test failed", Body: "boom"},
			{Kind: KindCIPassed, Target: "o/r#7", Title: "all checks passed"},
		})

		// The framing matters: this text arrives unbidden mid-conversation, so
		// it has to say where it came from and that it is not the user talking.
		for _, want := range []string{
			"<shuck-monitor>",
			"</shuck-monitor>",
			"2 changes",
			"1 item",
			"not a message from the user",
			"test failed",
			"all checks passed",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("feed is missing %q:\n%s", want, got)
			}
		}
	})

	t.Run("purely informational batch does not demand attention", func(t *testing.T) {
		got := FormatFeed([]Event{{Kind: KindCIPassed, Target: "o/r#7", Title: "all checks passed"}})
		if strings.Contains(got, "need your attention") {
			t.Errorf("a green build should not be framed as something to act on:\n%s", got)
		}
		if !strings.Contains(got, "1 change") {
			t.Errorf("feed = %q, want the singular", got)
		}
	})
}

func TestCount(t *testing.T) {
	if got := count(1, "change"); got != "1 change" {
		t.Errorf("count(1) = %q", got)
	}
	if got := count(3, "change"); got != "3 changes" {
		t.Errorf("count(3) = %q", got)
	}
	if got := count(0, "change"); got != "0 changes" {
		t.Errorf("count(0) = %q", got)
	}
}
