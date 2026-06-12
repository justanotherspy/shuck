package render

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/justanotherspy/shuck/internal/model"
)

func TestReportListsArtifacts(t *testing.T) {
	r := &model.Report{
		Run: &model.RunInfo{Owner: "o", Repo: "r", RunID: 123, Title: "CI", HeadSHA: "abcdef1234567"},
		FailedJobs: []model.JobResult{{
			Name: "build", Conclusion: "failure", WorkflowName: "CI",
			FailedSteps: []model.FailedStep{{Number: 2, Name: "Run tests", Excerpt: "--- FAIL"}},
		}},
		Artifacts: []model.Artifact{
			{Name: "coverage", SizeBytes: 2 * 1024 * 1024, ExpiresAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Path: "/tmp/arts/coverage"},
			{Name: "stale-bundle", SizeBytes: 512, Expired: true},
		},
	}

	var buf bytes.Buffer
	Report(&buf, r)
	out := buf.String()

	for _, want := range []string{
		"Artifacts:",
		"📦 coverage (2.0 MiB, expires 2026-09-01) — saved to /tmp/arts/coverage",
		"📦 stale-bundle (512 B, expired)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

// Artifacts must render on the all-clear path too: a green run can still carry
// the artifacts the user came for.
func TestReportListsArtifactsAllClear(t *testing.T) {
	r := &model.Report{
		Run:       &model.RunInfo{Owner: "o", Repo: "r", RunID: 123, Title: "CI", HeadSHA: "abcdef1234567"},
		Artifacts: []model.Artifact{{Name: "dist", SizeBytes: 4096}},
	}

	var buf bytes.Buffer
	Report(&buf, r)
	out := buf.String()

	if !strings.Contains(out, "✓ no failures in run 123") {
		t.Errorf("missing all-clear line:\n%s", out)
	}
	if !strings.Contains(out, "📦 dist (4.0 KiB)") {
		t.Errorf("missing artifact on all-clear path:\n%s", out)
	}
}

func TestFormatSize(t *testing.T) {
	for _, tt := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{3 * 1024 * 1024 * 1024, "3.0 GiB"},
	} {
		if got := formatSize(tt.in); got != tt.want {
			t.Errorf("formatSize(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
