package distil

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reviewCorpusInput mirrors the input.json fixture shape of the review
// corpus cases: exactly one of Comment or Review is set.
type reviewCorpusInput struct {
	Comment *ReviewCommentInput `json:"comment,omitempty"`
	Review  *ReviewInput        `json:"review,omitempty"`
}

// TestReviewGolden distills every corpus case under testdata/review/ and
// compares the JSON-encoded result against the case's committed golden.
// Regenerate (only when output is meant to change) with:
//
//	go test ./internal/distil -run Golden -update
func TestReviewGolden(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("testdata", "review"))
	if err != nil {
		t.Fatalf("read testdata/review: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			dir := filepath.Join("testdata", "review", e.Name())
			raw, err := os.ReadFile(filepath.Join(dir, "input.json"))
			if err != nil {
				t.Fatalf("read input fixture: %v", err)
			}
			var in reviewCorpusInput
			if err := json.Unmarshal(raw, &in); err != nil {
				t.Fatalf("unmarshal input fixture: %v", err)
			}

			var res any
			switch {
			case in.Comment != nil && in.Review == nil:
				res, err = ReviewComment(*in.Comment)
			case in.Review != nil && in.Comment == nil:
				res, err = Review(*in.Review)
			default:
				t.Fatalf("fixture must set exactly one of comment/review")
			}
			if err != nil {
				t.Fatalf("distil: %v", err)
			}
			got, err := json.MarshalIndent(res, "", "  ")
			if err != nil {
				t.Fatalf("marshal result: %v", err)
			}
			got = append(got, '\n')

			goldenPath := filepath.Join(dir, "result.golden.json")
			if *update {
				if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden (run with -update to generate): %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("result differs from golden %s\n--- got ---\n%s\n--- want ---\n%s",
					goldenPath, got, want)
			}
		})
	}
}

func TestReviewCommentHeader(t *testing.T) {
	tests := []struct {
		name string
		in   ReviewCommentInput
		want string
	}{
		{
			name: "single line",
			in:   ReviewCommentInput{Reviewer: "alice", Path: "a/b.go", Line: 12, Body: "x"},
			want: "Reviewer alice commented on a/b.go:12:",
		},
		{
			name: "multi-line range",
			in:   ReviewCommentInput{Reviewer: "alice", Path: "a/b.go", StartLine: 10, Line: 14, Body: "x"},
			want: "Reviewer alice commented on a/b.go:10–14:",
		},
		{
			name: "file-level comment",
			in:   ReviewCommentInput{Reviewer: "alice", Path: "a/b.go", Body: "x"},
			want: "Reviewer alice commented on a/b.go:",
		},
		{
			name: "missing login and path",
			in:   ReviewCommentInput{Line: 3, Body: "x"},
			want: "Reviewer reviewer commented on (unknown file):3:",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := ReviewComment(tt.in)
			if err != nil {
				t.Fatalf("ReviewComment: %v", err)
			}
			first, _, _ := strings.Cut(res.Summary, "\n")
			if first != tt.want {
				t.Errorf("header = %q, want %q", first, tt.want)
			}
		})
	}
}

func TestReviewCommentContextWindow(t *testing.T) {
	file := "l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\nl10\n"
	base := ReviewCommentInput{
		Reviewer: "alice", Path: "f.go", Body: "b",
		Side: "RIGHT", FileContent: file, ContextLines: 2,
	}
	tests := []struct {
		name      string
		mutate    func(*ReviewCommentInput)
		wantCtx   bool
		wantLines []string // exact numbered lines when wantCtx
	}{
		{
			name:      "window clamps at start of file",
			mutate:    func(in *ReviewCommentInput) { in.Line = 1 },
			wantCtx:   true,
			wantLines: []string{"1 | l1", "2 | l2", "3 | l3"},
		},
		{
			name:      "window clamps at end of file",
			mutate:    func(in *ReviewCommentInput) { in.Line = 10 },
			wantCtx:   true,
			wantLines: []string{" 8 | l8", " 9 | l9", "10 | l10"},
		},
		{
			name:      "range widens the window",
			mutate:    func(in *ReviewCommentInput) { in.StartLine = 4; in.Line = 6 },
			wantCtx:   true,
			wantLines: []string{"2 | l2", "3 | l3", "4 | l4", "5 | l5", "6 | l6", "7 | l7", "8 | l8"},
		},
		{
			name:    "no line anchor means no context",
			mutate:  func(in *ReviewCommentInput) { in.Line = 0 },
			wantCtx: false,
		},
		{
			name:    "line past EOF means no context",
			mutate:  func(in *ReviewCommentInput) { in.Line = 11 },
			wantCtx: false,
		},
		{
			name:    "start past EOF means no context",
			mutate:  func(in *ReviewCommentInput) { in.StartLine = 11; in.Line = 12 },
			wantCtx: false,
		},
		{
			name:    "left side means no context",
			mutate:  func(in *ReviewCommentInput) { in.Line = 5; in.Side = "LEFT" },
			wantCtx: false,
		},
		{
			name:    "no file content means no context",
			mutate:  func(in *ReviewCommentInput) { in.Line = 5; in.FileContent = "" },
			wantCtx: false,
		},
		{
			name: "end anchor past EOF clamps when start is inside",
			mutate: func(in *ReviewCommentInput) {
				in.StartLine = 9
				in.Line = 15
			},
			wantCtx:   true,
			wantLines: []string{" 7 | l7", " 8 | l8", " 9 | l9", "10 | l10"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := base
			tt.mutate(&in)
			ctx := contextWindow(in)
			if !tt.wantCtx {
				if ctx != "" {
					t.Fatalf("contextWindow = %q, want none", ctx)
				}
				res, err := ReviewComment(in)
				if err != nil {
					t.Fatalf("ReviewComment: %v", err)
				}
				if strings.Contains(res.Summary, "File context at head") {
					t.Errorf("summary has a context block, want none:\n%s", res.Summary)
				}
				return
			}
			if got, want := strings.Split(ctx, "\n"), tt.wantLines; !equalStrings(got, want) {
				t.Errorf("contextWindow = %#v, want %#v", got, want)
			}
			res, err := ReviewComment(in)
			if err != nil {
				t.Fatalf("ReviewComment: %v", err)
			}
			if !strings.Contains(res.Summary, ctx) {
				t.Errorf("summary missing context window:\n%s", res.Summary)
			}
		})
	}
}

func TestReviewCommentNegativeContextLines(t *testing.T) {
	_, err := ReviewComment(ReviewCommentInput{ContextLines: -1})
	if err == nil {
		t.Fatal("negative ContextLines must error")
	}
	_, err = Review(ReviewInput{Comments: []ReviewCommentInput{{ContextLines: -1}}})
	if err == nil {
		t.Fatal("negative ContextLines on a review comment must error")
	}
}

func TestReviewCommentThreadQuoting(t *testing.T) {
	res, err := ReviewComment(ReviewCommentInput{
		Reviewer: "carol", Path: "f.go", Line: 3, Body: "agreed",
		Thread: []ThreadComment{
			{Author: "alice", Body: "first\nsecond"},
			{Author: "", Body: "reply"},
		},
	})
	if err != nil {
		t.Fatalf("ReviewComment: %v", err)
	}
	for _, want := range []string{
		"In reply to 2 earlier comment(s) in this thread:",
		"> alice: first\n> second",
		"> reviewer: reply",
	} {
		if !strings.Contains(res.Summary, want) {
			t.Errorf("summary missing %q:\n%s", want, res.Summary)
		}
	}
}

func TestReviewVerdicts(t *testing.T) {
	tests := []struct {
		state       string
		wantVerdict string
		wantPhrase  string
	}{
		{"APPROVED", "approved", "approved the pull request"},
		{"changes_requested", "changes_requested", "requested changes"},
		{"commented", "commented", "commented on the pull request"},
		{"", "commented", "commented on the pull request"},
		{"weird", "weird", `submitted a "weird" review`},
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			res, err := Review(ReviewInput{Reviewer: "bob", State: tt.state})
			if err != nil {
				t.Fatalf("Review: %v", err)
			}
			if res.Verdict != tt.wantVerdict {
				t.Errorf("Verdict = %q, want %q", res.Verdict, tt.wantVerdict)
			}
			if !strings.Contains(res.Summary, "Reviewer bob "+tt.wantPhrase) {
				t.Errorf("summary missing %q:\n%s", tt.wantPhrase, res.Summary)
			}
		})
	}
}

func TestReviewCoherentEvent(t *testing.T) {
	res, err := Review(ReviewInput{
		Reviewer: "bob",
		State:    "changes_requested",
		Body:     "Overall direction is fine.",
		Comments: []ReviewCommentInput{
			{Path: "a.go", Line: 3, Body: "rename this", DiffHunk: "@@ -1,3 +1,3 @@\n-x\n+y"},
			{Path: "b.go", StartLine: 7, Line: 9, Body: "extract a helper"},
			{Path: "c.go", Body: "file-level note"},
		},
	})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if res.Comments != 3 {
		t.Errorf("Comments = %d, want 3", res.Comments)
	}
	for _, want := range []string{
		"Reviewer bob requested changes — 3 inline comment(s)",
		"Overall direction is fine.",
		"a.go:3:", "b.go:7–9:", "c.go:",
		"Diff hunk:\n@@ -1,3 +1,3 @@",
	} {
		if !strings.Contains(res.Summary, want) {
			t.Errorf("summary missing %q:\n%s", want, res.Summary)
		}
	}
}

func TestReviewCommentEmptyBodyMarker(t *testing.T) {
	res, err := ReviewComment(ReviewCommentInput{Reviewer: "a", Path: "f", Line: 1, Body: " \n "})
	if err != nil {
		t.Fatalf("ReviewComment: %v", err)
	}
	if !strings.Contains(res.Summary, "(no comment text)") {
		t.Errorf("summary missing empty-body marker:\n%s", res.Summary)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
