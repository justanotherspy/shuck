package distil

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCapSummary(t *testing.T) {
	long := "job: failure — 3 failed step(s)\n- build (step 1) [compile]: error: x\n- test (step 2) [test]: FAIL TestY\n- lint (step 3): issue found"
	tests := []struct {
		name      string
		summary   string
		limit     int
		note      string
		want      string
		truncated bool
	}{
		{
			name:    "fits unchanged",
			summary: "short summary",
			limit:   100,
			note:    "[truncated]",
			want:    "short summary",
		},
		{
			name:    "limit zero means unlimited",
			summary: long,
			limit:   0,
			note:    "[truncated]",
			want:    long,
		},
		{
			name:    "limit negative means unlimited",
			summary: long,
			limit:   -1,
			want:    long,
		},
		{
			name:    "exact fit unchanged",
			summary: "abc",
			limit:   3,
			note:    "[truncated]",
			want:    "abc",
		},
		{
			name:      "cuts at line boundary keeping header and first steps",
			summary:   long,
			limit:     90,
			note:      "[cut]",
			want:      "job: failure — 3 failed step(s)\n- build (step 1) [compile]: error: x\n[cut]",
			truncated: true,
		},
		{
			name:      "single long line cut mid-line",
			summary:   strings.Repeat("x", 200),
			limit:     50,
			note:      "[cut]",
			want:      strings.Repeat("x", 44) + "\n[cut]",
			truncated: true,
		},
		{
			name:      "mid-line cut is rune safe",
			summary:   strings.Repeat("é", 100), // 2 bytes each
			limit:     11,
			note:      "",
			want:      strings.Repeat("é", 5), // 10 bytes, not 11
			truncated: true,
		},
		{
			name:      "empty note appends nothing",
			summary:   long,
			limit:     40,
			note:      "",
			want:      "job: failure — 3 failed step(s)",
			truncated: true,
		},
		{
			name:      "note alone overflows budget",
			summary:   long,
			limit:     10,
			note:      "[summary truncated — full logs archived]",
			want:      "[summary t",
			truncated: true,
		},
		{
			name:      "note exactly consumes budget",
			summary:   long,
			limit:     6,
			note:      "[cut]",
			want:      "[cut]",
			truncated: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, truncated := CapSummary(tt.summary, tt.limit, tt.note)
			if got != tt.want {
				t.Errorf("CapSummary() = %q, want %q", got, tt.want)
			}
			if truncated != tt.truncated {
				t.Errorf("truncated = %v, want %v", truncated, tt.truncated)
			}
			if tt.limit > 0 && len(got) > tt.limit {
				t.Errorf("result %d bytes exceeds limit %d", len(got), tt.limit)
			}
		})
	}
}

// FuzzDistilCapSummary asserts CapSummary's contract on arbitrary inputs:
// the result never exceeds a positive limit, valid UTF-8 in yields valid
// UTF-8 out, a summary within budget is returned identically, and a
// truncated result carries (a prefix of) the caller's note.
func FuzzDistilCapSummary(f *testing.F) {
	f.Add("job: failure — 1 failed step(s)\n- build: error", 24, "[truncated]")
	f.Add(strings.Repeat("é", 100), 11, "")
	f.Add("", 1, "note")
	f.Add("line1\nline2\nline3", 8, "…")
	f.Fuzz(func(t *testing.T, summary string, limit int, note string) {
		got, truncated := CapSummary(summary, limit, note)

		if limit > 0 && len(got) > limit {
			t.Fatalf("result %d bytes exceeds limit %d", len(got), limit)
		}
		if limit <= 0 || len(summary) <= limit {
			if truncated || got != summary {
				t.Fatalf("within-budget summary must be identity, got truncated=%v", truncated)
			}
			return
		}
		if !truncated {
			t.Fatalf("over-budget summary must report truncated")
		}
		if utf8.ValidString(summary) && utf8.ValidString(note) && !utf8.ValidString(got) {
			t.Fatalf("valid UTF-8 in, invalid out: %q", got)
		}
		if note != "" && !strings.HasSuffix(got, note) && !strings.HasPrefix(note, got) {
			// The note survives whole (as the suffix) or, when it alone
			// overflows the budget, the whole result is a prefix of it.
			t.Fatalf("truncated result does not carry the note: %q", got)
		}
	})
}
