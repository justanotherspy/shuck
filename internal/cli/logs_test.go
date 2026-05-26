package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/cache"
	"github.com/justanotherspy/shuck/internal/jsonout"
	"github.com/justanotherspy/shuck/internal/model"
)

func TestRunTarget(t *testing.T) {
	cases := []struct {
		name       string
		run        string
		positional []string
		wantOwner  string
		wantRepo   string
		wantRun    int64
		wantJob    int64
		wantErr    bool
	}{
		{"run url", "https://github.com/o/r/actions/runs/123", nil, "o", "r", 123, 0, false},
		{"job url", "https://github.com/o/r/actions/runs/123/job/456", nil, "o", "r", 123, 456, false},
		{"bare id with repo", "9", []string{"o/r"}, "o", "r", 9, 0, false},
		{"bare id with a pr positional errors", "9", []string{"42"}, "", "", 0, 0, true},
		{"bare id with too many positionals errors", "9", []string{"o/r", "42"}, "", "", 0, 0, true},
		{"pr url is not a run", "https://github.com/o/r/pull/1", nil, "", "", 0, 0, true},
		{"garbage", "nope", nil, "", "", 0, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tgt, err := runTarget(c.run, c.positional)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if err != nil {
				return
			}
			if tgt.Owner != c.wantOwner || tgt.Repo != c.wantRepo || tgt.RunID != c.wantRun || tgt.JobID != c.wantJob {
				t.Errorf("tgt = %+v, want %s/%s run=%d job=%d", tgt, c.wantOwner, c.wantRepo, c.wantRun, c.wantJob)
			}
		})
	}
}

// TestRunLogsOfflineJSON proves `shuck logs` renders a plain single document
// (not the combined envelope the default/all path uses).
func TestRunLogsOfflineJSON(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	report := &model.Report{
		PR: model.PR{Owner: "o", Repo: "r", Number: 42, Title: "fix", HeadSHA: "abc1234"},
		FailedJobs: []model.JobResult{{
			ID: 1, Name: "build", Conclusion: "failure", Inspected: true,
			FailedSteps: []model.FailedStep{{Number: 2, Name: "Run tests", Excerpt: "boom"}},
		}},
	}
	if err := cache.Save(report); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	var out, errb strings.Builder
	code := runLogs([]string{"o/r", "42", "--offline", "--json"}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%q", code, errb.String())
	}
	var doc jsonout.Document
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("not a plain jsonout.Document: %v\n%s", err, out.String())
	}
	if doc.Summary.Failed != 1 || len(doc.FailedJobs) != 1 {
		t.Errorf("unexpected doc: %+v", doc)
	}
}
