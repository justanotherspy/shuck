package monitor

import (
	"strings"
	"testing"
	"time"
)

func TestKindSeverity(t *testing.T) {
	// The Stop hook only speaks up for actionable events, so this mapping is
	// what decides whether a passing build can delay an agent from finishing.
	actionable := []Kind{KindCIFailed, KindReviewComment, KindReviewSubmitted, KindPinsStale}
	// monitor.error is informational on purpose: a failed poll is the
	// monitor's problem, and making it actionable would let a network blip
	// hold a finished turn open.
	informational := []Kind{KindCIPassed, KindCIStarted, KindPRState, KindTarget, KindError}

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
