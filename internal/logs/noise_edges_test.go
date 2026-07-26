package logs

import (
	"strings"
	"testing"
)

// A run of exactly one passing line still costs a marker line, so the marker
// has to read like English or the excerpt looks broken to whoever reads it.
func TestExtractCollapsesASingleLineWithSingularWording(t *testing.T) {
	got := Extract([]string{
		"ok  	example.com/pkg	0.01s",
		"--- FAIL: TestWidget (0.00s)",
	}, DefaultOptions())

	want := "… (1 passing/progress line omitted) …\n--- FAIL: TestWidget (0.00s)"
	if got != want {
		t.Fatalf("Extract() =\n%s\n\nwant:\n%s", got, want)
	}
}

// The noise tokens are anchored at the head of a line on purpose: `ok` and
// `PASS` mean "this unit succeeded" only in that position, and matching them
// mid-line would start eating the prose an agent needs.
func TestExtractKeepsNoiseTokensThatAreNotAtLineStart(t *testing.T) {
	lines := []string{
		"the build was ok until it wasn't",
		"waiting for PASS from the runner",
		"##[error]build failed",
	}
	got := Extract(lines, DefaultOptions())

	for _, want := range lines {
		if !strings.Contains(got, want) {
			t.Errorf("Extract() dropped %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "omitted") {
		t.Errorf("Extract() collapsed a mid-line token:\n%s", got)
	}
}
