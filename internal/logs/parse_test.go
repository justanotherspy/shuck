package logs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

func TestParseJobFailure(t *testing.T) {
	raw := readFixture(t, "job_failure.log")
	secs := Parse(raw)

	if len(secs) != 3 {
		t.Fatalf("got %d sections, want 3: %+v", len(secs), secs)
	}

	if secs[0].Header != "Run actions/checkout@v4" {
		t.Errorf("section 0 header = %q", secs[0].Header)
	}
	if secs[0].HasError {
		t.Errorf("section 0 should not have an error")
	}
	if secs[0].Kind() != model.KindAction {
		t.Errorf("section 0 kind = %q, want action", secs[0].Kind())
	}

	test := secs[1]
	if test.Header != "Run go test ./..." {
		t.Errorf("section 1 header = %q", test.Header)
	}
	if !test.HasError {
		t.Errorf("section 1 should have an error")
	}
	if test.Kind() != model.KindBash {
		t.Errorf("section 1 kind = %q, want bash", test.Kind())
	}
	if test.Command() != "go test ./..." {
		t.Errorf("section 1 command = %q", test.Command())
	}
	body := strings.Join(test.Body, "\n")
	if strings.Contains(body, "2024-05-01") {
		t.Errorf("timestamps not stripped from body: %q", body)
	}
	if !strings.Contains(body, "--- FAIL: TestThing") {
		t.Errorf("body missing failure line: %q", body)
	}
	if !strings.Contains(body, "##[error]Process completed") {
		t.Errorf("body missing error marker: %q", body)
	}
}

func TestErrorSections(t *testing.T) {
	secs := Parse(readFixture(t, "job_failure.log"))
	errs := ErrorSections(secs)
	if len(errs) != 1 {
		t.Fatalf("got %d error sections, want 1", len(errs))
	}
	if errs[0].Header != "Run go test ./..." {
		t.Errorf("error section header = %q", errs[0].Header)
	}
}

func TestParseLeadingSection(t *testing.T) {
	raw := "2024-05-01T10:00:00.0000000Z preamble line\n2024-05-01T10:00:01.0000000Z ##[group]Run echo hi\n2024-05-01T10:00:01.5000000Z ##[endgroup]\n2024-05-01T10:00:02.0000000Z hi\n"
	secs := Parse(raw)
	if len(secs) != 2 {
		t.Fatalf("got %d sections, want 2", len(secs))
	}
	if secs[0].Header != "" || len(secs[0].Body) == 0 || secs[0].Body[0] != "preamble line" {
		t.Errorf("leading section = %+v", secs[0])
	}
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}
