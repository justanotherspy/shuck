package logs

import (
	"fmt"
	"strings"
	"testing"
)

func TestExtractShortReturnsWhole(t *testing.T) {
	lines := []string{"line 1", "line 2", "all good"}
	got := Extract(lines, DefaultOptions())
	if got != "line 1\nline 2\nall good" {
		t.Errorf("got %q", got)
	}
}

func TestExtractLongTailsWhenNoMatch(t *testing.T) {
	var lines []string
	for i := range 300 {
		lines = append(lines, fmt.Sprintf("clean line %d", i))
	}
	opts := Options{ShortThreshold: 100, Context: 10, Tail: 50, Pattern: DefaultPattern()}
	got := Extract(lines, opts)
	if !strings.Contains(got, "clean line 299") {
		t.Errorf("tail should include the last line: %q", lastLine(got))
	}
	if !strings.Contains(got, "lines omitted") {
		t.Errorf("tail should note omitted lines")
	}
	if strings.Contains(got, "clean line 100") {
		t.Errorf("tail should not include line 100")
	}
}

func TestExtractLongGrepsWithContext(t *testing.T) {
	var lines []string
	for i := range 200 {
		lines = append(lines, fmt.Sprintf("clean line %d", i))
	}
	lines[150] = "fatal: something exploded"
	opts := Options{ShortThreshold: 100, Context: 2, Tail: 50, Pattern: DefaultPattern()}
	got := Extract(lines, opts)

	if !strings.Contains(got, "fatal: something exploded") {
		t.Errorf("missing the matched line: %q", got)
	}
	if !strings.Contains(got, "clean line 148") || !strings.Contains(got, "clean line 152") {
		t.Errorf("missing ±context lines: %q", got)
	}
	if strings.Contains(got, "clean line 100") {
		t.Errorf("should not include far-away lines: %q", got)
	}
	if !strings.Contains(got, "lines omitted") {
		t.Errorf("expected omission markers")
	}
}

func TestExtractMergesAdjacentWindows(t *testing.T) {
	var lines []string
	for i := range 200 {
		lines = append(lines, fmt.Sprintf("clean line %d", i))
	}
	lines[100] = "error one"
	lines[103] = "error two"
	opts := Options{ShortThreshold: 50, Context: 3, Tail: 50, Pattern: DefaultPattern()}
	got := Extract(lines, opts)
	// The two matches are 3 apart with ±3 context, so the windows merge into one
	// block: line 101/102 between them must be present (no omission marker between).
	if !strings.Contains(got, "clean line 101") || !strings.Contains(got, "clean line 102") {
		t.Errorf("adjacent windows should merge, keeping the lines between: %q", got)
	}
	if strings.Count(got, "lines omitted") != 2 {
		t.Errorf("expected exactly leading+trailing omission markers, got %d: %q", strings.Count(got, "lines omitted"), got)
	}
}

func lastLine(s string) string {
	parts := strings.Split(s, "\n")
	return parts[len(parts)-1]
}
