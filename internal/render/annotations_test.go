package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

func TestReportAnnotationsAndClass(t *testing.T) {
	job := model.JobResult{
		Name: "build", Conclusion: "failure", WorkflowName: "CI", WorkflowPath: ".github/workflows/ci.yml",
		Steps: []model.StepOverview{{Number: 2, Name: "Run tests", Conclusion: "failure"}},
		Annotations: []model.Annotation{
			{Path: "main_test.go", StartLine: 12, StartColumn: 5, Level: "failure", Message: "TestFoo failed"},
			{Path: "x.go", StartLine: 3, Level: "warning", Title: "vet", Message: "shadowed var"},
			// notice level is dropped from the text view.
			{Path: "y.go", StartLine: 1, Level: "notice", Message: "fyi"},
		},
		FailedSteps: []model.FailedStep{{
			Number: 2, Name: "Run tests", Command: "go test ./...", Kind: model.KindBash,
			Class: model.ClassTest, Excerpt: "--- FAIL: TestFoo",
		}},
	}
	r := &model.Report{PR: model.PR{Owner: "o", Repo: "r", Number: 1}, FailedJobs: []model.JobResult{job}}

	var buf bytes.Buffer
	Report(&buf, r)
	out := buf.String()

	for _, want := range []string{
		"Annotations:",
		"✗ main_test.go:12:5 — TestFoo failed",
		"⚠ x.go:3 — vet: shadowed var",
		"▸ Step 2 — Run tests (failed) [test]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "fyi") {
		t.Errorf("notice-level annotation should be omitted from text output:\n%s", out)
	}
}

func TestReportAnnotationsCap(t *testing.T) {
	var anns []model.Annotation
	for i := range maxRenderedAnnotations + 5 {
		anns = append(anns, model.Annotation{Path: "f.go", StartLine: i + 1, Level: "failure", Message: "boom"})
	}
	r := &model.Report{
		PR:         model.PR{Owner: "o", Repo: "r", Number: 1},
		FailedJobs: []model.JobResult{{Name: "j", Conclusion: "failure", Annotations: anns}},
	}
	var buf bytes.Buffer
	Report(&buf, r)
	if got := buf.String(); !strings.Contains(got, "… 5 more annotations") {
		t.Errorf("expected truncation note for %d annotations:\n%s", len(anns), got)
	}
}

// No failure/warning annotations means no Annotations block at all.
func TestReportNoAnnotationsBlock(t *testing.T) {
	r := &model.Report{
		PR: model.PR{Owner: "o", Repo: "r", Number: 1},
		FailedJobs: []model.JobResult{{
			Name: "j", Conclusion: "failure",
			Annotations: []model.Annotation{{Path: "y.go", StartLine: 1, Level: "notice", Message: "fyi"}},
			FailedSteps: []model.FailedStep{{Number: 1, Name: "s", Excerpt: "boom"}},
		}},
	}
	var buf bytes.Buffer
	Report(&buf, r)
	if strings.Contains(buf.String(), "Annotations:") {
		t.Errorf("notice-only job should not print an Annotations block:\n%s", buf.String())
	}
}
