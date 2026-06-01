package logs

import (
	"fmt"
	"strings"
	"testing"
)

func TestExtractEmptyAndBlankOnly(t *testing.T) {
	if got := Extract(nil, DefaultOptions()); got != "" {
		t.Errorf("Extract(nil) = %q, want empty", got)
	}
	if got := Extract([]string{"", "  ", "\t"}, DefaultOptions()); got != "" {
		t.Errorf("Extract(blank-only) = %q, want empty", got)
	}
}

func TestExtractTrimsBlankEdges(t *testing.T) {
	lines := []string{"", "  ", "keep me", "and me", ""}
	got := Extract(lines, DefaultOptions())
	if got != "keep me\nand me" {
		t.Errorf("Extract trimmed = %q", got)
	}
}

// TestExtractLongNoMatchTailCoversWhole exercises the no-match tail branch where
// Tail >= len(lines), so nothing is omitted (omitted == 0) and the whole tail is
// returned without an ellipsis.
func TestExtractLongNoMatchTailCoversWhole(t *testing.T) {
	var lines []string
	for i := range 150 {
		lines = append(lines, fmt.Sprintf("clean line %d", i))
	}
	// ShortThreshold below the length forces the long path; a generous Tail means
	// the tail window covers every line.
	opts := Options{ShortThreshold: 100, Context: 5, Tail: 1000, Pattern: DefaultPattern()}
	got := Extract(lines, opts)
	if strings.Contains(got, "lines omitted") {
		t.Errorf("no lines should be omitted when Tail exceeds length:\n%s", got)
	}
	if !strings.Contains(got, "clean line 0") || !strings.Contains(got, "clean line 149") {
		t.Errorf("tail should include the whole log: %q", got)
	}
}

// TestExtractMatchAtStartNoLeadingEllipsis covers renderWindows' branch where the
// first window starts at 0 (no leading ellipsis emitted).
func TestExtractMatchAtStartNoLeadingEllipsis(t *testing.T) {
	var lines []string
	for i := range 200 {
		lines = append(lines, fmt.Sprintf("clean line %d", i))
	}
	lines[0] = "error at the very top"
	opts := Options{ShortThreshold: 50, Context: 2, Tail: 50, Pattern: DefaultPattern()}
	got := Extract(lines, opts)
	if !strings.HasPrefix(got, "error at the very top") {
		t.Errorf("expected no leading ellipsis, got:\n%s", got)
	}
	// There is still a trailing ellipsis for the omitted tail.
	if !strings.Contains(got, "lines omitted") {
		t.Errorf("expected a trailing omission marker:\n%s", got)
	}
}

// TestExtractMatchAtEndNoTrailingEllipsis covers renderWindows where the final
// window ends at len(lines) (no trailing ellipsis).
func TestExtractMatchAtEndNoTrailingEllipsis(t *testing.T) {
	var lines []string
	for i := range 200 {
		lines = append(lines, fmt.Sprintf("clean line %d", i))
	}
	lines[199] = "error at the very end"
	opts := Options{ShortThreshold: 50, Context: 2, Tail: 50, Pattern: DefaultPattern()}
	got := Extract(lines, opts)
	if !strings.HasSuffix(got, "error at the very end") {
		t.Errorf("expected no trailing ellipsis, got:\n%s", got)
	}
	// A leading ellipsis is present for the omitted head.
	if !strings.HasPrefix(got, "… (") {
		t.Errorf("expected a leading omission marker:\n%s", got)
	}
}
