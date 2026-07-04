package distil

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

// Mirrors of unexported markers the fuzz invariants must recognize.
var (
	fuzzTSPrefix = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z `)
	fuzzEllipsis = regexp.MustCompile(`^… \(\d+ lines omitted\) …$`)
)

var fuzzConclusions = []string{"failure", "cancelled", "timed_out", "success", ""}

// FuzzDistilCIFailure asserts CIFailure's semantic contract on arbitrary log
// bytes (CI logs are attacker-influenceable via PR contents): it never
// panics, is deterministic, always yields at least one step within the
// pairing bound, only ever excerpts lines that exist in the input (or its
// own omission markers), and summarizes every non-empty result.
func FuzzDistilCIFailure(f *testing.F) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		f.Fatalf("read testdata: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("testdata", e.Name(), "log.txt"))
		if os.IsNotExist(err) {
			// Not a CI-failure corpus case (e.g. testdata/review, testdata/fuzz).
			continue
		}
		if err != nil {
			f.Fatalf("read seed log: %v", err)
		}
		f.Add(string(raw), byte(0), 3, 100, 10, 100, 30)
	}
	f.Add("##[group]Run go test ./...\n##[endgroup]\n--- FAIL: TestX\n##[error]exit 1\n", byte(1), 4, 1, 0, 1, 1)
	f.Add("no groups at all\njust noise\n", byte(2), 0, 0, 0, 0, 0)

	f.Fuzz(func(t *testing.T, raw string, conclusionSel byte, nSteps, shortThreshold, context, tail, maxCmd int) {
		// Clamp the knobs to sane ranges: negatives are rejected by design and
		// huge values only slow the fuzzer down without exploring new logic.
		clamp := func(v, lo, hi int) int { return min(max(v, lo), hi) }
		nSteps = clamp(nSteps, 0, 64)
		opts := DefaultOptions()
		opts.Extract.ShortThreshold = clamp(shortThreshold, 0, 4096)
		opts.Extract.Context = clamp(context, 0, 4096)
		opts.Extract.Tail = clamp(tail, 0, 4096)
		opts.MaxCommandLines = clamp(maxCmd, 0, 4096)

		conclusion := fuzzConclusions[int(conclusionSel)%len(fuzzConclusions)]
		steps := make([]model.StepOverview, 0, nSteps)
		drillable := 0
		for i := range nSteps {
			c := "success"
			if i%2 == 0 {
				c = "failure"
				drillable++
			}
			steps = append(steps, model.StepOverview{Number: i + 1, Name: fmt.Sprintf("step-%d", i+1), Conclusion: c})
		}

		in := Input{JobName: "fuzz-job", JobConclusion: conclusion, Steps: steps, RawLog: raw, Options: opts}
		res, err := CIFailure(in)
		if err != nil {
			t.Fatalf("valid options must not error: %v", err)
		}
		res2, err := CIFailure(in)
		if err != nil || !reflect.DeepEqual(res, res2) {
			t.Fatalf("CIFailure is not deterministic")
		}

		// Always at least one step; never more than the pairing bound.
		errMarkers := strings.Count(raw, "##[error]")
		bound := max(max(drillable, errMarkers), 1)
		if len(res.FailedSteps) < 1 || len(res.FailedSteps) > bound {
			t.Fatalf("got %d failed steps, want within [1, %d]", len(res.FailedSteps), bound)
		}

		// Excerpt provenance: every line either exists in the timestamp-stripped
		// input, is an omission marker, or is the no-match fallback. (Command is
		// exempt: clamping appends its own "… (N more lines) …" marker.)
		input := make(map[string]bool)
		for line := range strings.SplitSeq(raw, "\n") {
			input[fuzzTSPrefix.ReplaceAllString(line, "")] = true
		}
		for _, fs := range res.FailedSteps {
			if fs.Excerpt == "(no matching error log section found)" || fs.Excerpt == "" {
				continue
			}
			for line := range strings.SplitSeq(fs.Excerpt, "\n") {
				if input[line] || fuzzEllipsis.MatchString(line) {
					continue
				}
				t.Fatalf("excerpt line not derived from input: %q", line)
			}
		}

		if res.Summary == "" {
			t.Fatalf("summary must be non-empty when steps exist")
		}
	})
}

var fuzzSides = []string{"RIGHT", "LEFT", ""}

// FuzzDistilReviewComment asserts ReviewComment's contract on arbitrary
// comment material (review bodies, hunks, and file contents are
// attacker-authored): it never panics, is deterministic, always yields a
// summary that carries the comment body, and only ever renders context
// lines that exist in the file at their stated line numbers, within the
// requested window.
func FuzzDistilReviewComment(f *testing.F) {
	f.Add("alice", "a/b.go", "looks wrong", "@@ -1,3 +1,3 @@\n-x\n+y", "l1\nl2\nl3\nl4\nl5\n", 3, 0, 2, byte(0))
	f.Add("", "", "", "", "", 0, 0, 0, byte(1))
	f.Add("bob", "f", "multi\nline\nbody", "", "only line", 1, 1, 100, byte(2))

	f.Fuzz(func(t *testing.T, reviewer, path, body, hunk, file string, line, start, ctxLines int, sideSel byte) {
		clamp := func(v, lo, hi int) int { return min(max(v, lo), hi) }
		line = clamp(line, 0, 1<<20)
		start = clamp(start, 0, 1<<20)
		ctxLines = clamp(ctxLines, 0, 512)

		in := ReviewCommentInput{
			Reviewer: reviewer, Path: path, Line: line, StartLine: start,
			Side: fuzzSides[int(sideSel)%len(fuzzSides)],
			Body: body, DiffHunk: hunk, FileContent: file, ContextLines: ctxLines,
		}
		res, err := ReviewComment(in)
		if err != nil {
			t.Fatalf("valid input must not error: %v", err)
		}
		res2, err := ReviewComment(in)
		if err != nil || !reflect.DeepEqual(res, res2) {
			t.Fatalf("ReviewComment is not deterministic")
		}
		if res.Summary == "" {
			t.Fatalf("summary must be non-empty")
		}
		if b := strings.TrimSpace(body); b != "" && !strings.Contains(res.Summary, b) {
			t.Fatalf("summary must carry the comment body")
		}

		// Context provenance and bounds, on the pure core directly: every
		// rendered line exists in the file at its stated number, and the
		// window never exceeds the commented range plus ±ContextLines.
		ctx := contextWindow(in)
		if ctx == "" {
			return
		}
		if !strings.Contains(res.Summary, ctx) {
			t.Fatalf("summary must carry the context window")
		}
		fileLines := strings.Split(file, "\n")
		n := ctxLines
		if n == 0 {
			n = DefaultContextLines
		}
		lo := max(min(start, line), 1)
		got := strings.Split(ctx, "\n")
		if len(got) > (line-lo)+2*n+1 {
			t.Fatalf("context window has %d lines, want at most %d", len(got), (line-lo)+2*n+1)
		}
		for _, cl := range got {
			numStr, text, ok := strings.Cut(cl, " | ")
			if !ok {
				t.Fatalf("context line %q not in 'N | text' form", cl)
			}
			num, err := strconv.Atoi(strings.TrimSpace(numStr))
			if err != nil || num < 1 || num > len(fileLines) {
				t.Fatalf("context line %q has bad line number", cl)
			}
			if fileLines[num-1] != text {
				t.Fatalf("context line %d = %q, file has %q", num, text, fileLines[num-1])
			}
		}
	})
}

// FuzzDistilReview asserts Review's contract on arbitrary review material:
// never panics, deterministic, one non-empty summary carrying the verdict
// and every comment body — one coherent event regardless of input shape.
func FuzzDistilReview(f *testing.F) {
	f.Add("bob", "APPROVED", "ship it", "a.go", "nit", "@@ -1 +1 @@", 3, 2)
	f.Add("", "", "", "", "", "", 0, 0)
	f.Add("carol", "changes_requested", "no", "b.go", "fix\nthis", "", 9, 5)

	f.Fuzz(func(t *testing.T, reviewer, state, body, cPath, cBody, cHunk string, cLine, nComments int) {
		clamp := func(v, lo, hi int) int { return min(max(v, lo), hi) }
		cLine = clamp(cLine, 0, 1<<20)
		nComments = clamp(nComments, 0, 32)

		comments := make([]ReviewCommentInput, 0, nComments)
		for i := range nComments {
			comments = append(comments, ReviewCommentInput{
				Path: cPath, Line: cLine + i, Body: cBody, DiffHunk: cHunk,
			})
		}
		in := ReviewInput{Reviewer: reviewer, State: state, Body: body, Comments: comments}
		res, err := Review(in)
		if err != nil {
			t.Fatalf("valid input must not error: %v", err)
		}
		res2, err := Review(in)
		if err != nil || !reflect.DeepEqual(res, res2) {
			t.Fatalf("Review is not deterministic")
		}
		if res.Summary == "" {
			t.Fatalf("summary must be non-empty")
		}
		if res.Comments != nComments {
			t.Fatalf("Comments = %d, want %d", res.Comments, nComments)
		}
		if res.Verdict == "" || res.Verdict != strings.ToLower(res.Verdict) {
			t.Fatalf("verdict %q must be non-empty and lowercase", res.Verdict)
		}
		if b := strings.TrimSpace(body); b != "" && !strings.Contains(res.Summary, b) {
			t.Fatalf("summary must carry the review body")
		}
		if b := strings.TrimSpace(cBody); b != "" && nComments > 0 && !strings.Contains(res.Summary, b) {
			t.Fatalf("summary must carry the comment bodies")
		}
	})
}
