package pins

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// sampleReport is a report with one finding in every status, so the renderer's
// branches can be asserted from a single fixture.
func sampleReport() Report {
	return Report{
		Root: "/repo",
		Findings: []Finding{
			{
				Use: Use{
					File: ".github/workflows/ci.yml", Line: 7,
					Raw: "actions/checkout@" + sha422, Slug: "actions/checkout",
					Ref: sha422, Comment: "v4.2.2", Kind: UseRemote,
				},
				Status: StatusPinned,
				Latest: "v4.2.2",
				SHA:    sha422,
			},
			{
				Use: Use{
					File: ".github/workflows/ci.yml", Line: 12,
					Raw: "actions/setup-go@v5", Slug: "actions/setup-go", Ref: "v5", Kind: UseRemote,
				},
				Status:  StatusUnpinned,
				Latest:  "v5.0.0",
				SHA:     sha500,
				PinLine: "actions/setup-go@" + sha500 + " # v5.0.0",
				Note:    `"v5" is a mutable tag — each release re-points it`,
			},
			{
				Use: Use{
					File: ".github/workflows/ci.yml", Line: 20,
					Raw: "actions/cache@" + sha420, Slug: "actions/cache",
					Ref: sha420, Comment: "v4.2.0", Kind: UseRemote,
				},
				Status:  StatusStale,
				Latest:  "v4.2.2",
				SHA:     sha422,
				PinLine: "actions/cache@" + sha422 + " # v4.2.2",
				Note:    "v4.2.0 → v4.2.2",
			},
			{
				Use:    Use{File: "action.yml", Line: 3, Raw: "./nested", Kind: UseLocal},
				Status: StatusSkipped,
				Note:   "local action reference — it ships with this repository, nothing to pin",
			},
		},
		Unpinned:  1,
		Stale:     1,
		Skipped:   1,
		CheckedAt: time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC),
	}
}

func TestRender(t *testing.T) {
	tests := []struct {
		name     string
		rep      Report
		want     []string
		wantNone []string
	}{
		{
			name: "mixed report",
			rep:  sampleReport(),
			want: []string{
				"/repo — action pins",
				"Summary: 4 references — 1 pinned, 1 stale, 1 unpinned, 1 skipped",
				"✗ .github/workflows/ci.yml:12  actions/setup-go@v5",
				"uses: actions/setup-go@" + sha500 + " # v5.0.0",
				"⚠ .github/workflows/ci.yml:20  actions/cache@" + sha420 + " # v4.2.0",
				"uses: actions/cache@" + sha422 + " # v4.2.2",
				"– action.yml:3  ./nested",
				"local action reference",
				"✗ 2 references need attention — 1 unpinned, 1 behind the latest release.",
			},
			// A healthy pin is never printed: the report is a to-do list.
			wantNone: []string{":7", "✓"},
		},
		{
			name: "everything pinned and current",
			rep: Report{
				Root: ".",
				Findings: []Finding{
					{Use: Use{File: "a.yml", Line: 1, Raw: "a/b@" + sha422, Comment: "v4.2.2"}, Status: StatusPinned},
				},
			},
			want: []string{
				"Summary: 1 reference — 1 pinned, 0 stale, 0 unpinned",
				"✓ Every `uses:` reference is pinned to a commit SHA and current.",
			},
			wantNone: []string{"skipped"},
		},
		{
			name: "pinned with skips",
			rep: Report{
				Findings: []Finding{
					{Use: Use{File: "a.yml", Line: 1, Raw: "a/b@" + sha422}, Status: StatusPinned},
					{Use: Use{File: "a.yml", Line: 2, Raw: "./x", Kind: UseLocal}, Status: StatusSkipped, Note: "nothing to pin"},
				},
				Skipped: 1,
			},
			want: []string{
				". — action pins",
				"✓ Every checked reference is pinned to a commit SHA and current (1 skipped).",
			},
		},
		{
			name: "no references at all",
			rep:  Report{Root: "/repo"},
			want: []string{"/repo — action pins", "No `uses:` references found."},
		},
		{
			name: "an unreadable file has no reference text to show",
			rep: Report{
				Findings: []Finding{
					{Use: Use{File: "broken.yml", Line: 1, Kind: UseInvalid}, Status: StatusSkipped, Note: "parse broken.yml: bad"},
				},
				Skipped: 1,
			},
			want: []string{"– broken.yml:1  (unreadable)", "parse broken.yml: bad"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			Render(&buf, tt.rep)
			got := buf.String()
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("output missing %q:\n%s", want, got)
				}
			}
			for _, unwanted := range tt.wantNone {
				if strings.Contains(got, unwanted) {
					t.Errorf("output unexpectedly contains %q:\n%s", unwanted, got)
				}
			}
		})
	}
}

func TestStatusMark(t *testing.T) {
	tests := []struct {
		status Status
		want   string
	}{
		{StatusUnpinned, "✗"},
		{StatusStale, "⚠"},
		{StatusSkipped, "–"},
		{StatusPinned, "✓"},
	}
	for _, tt := range tests {
		t.Run(tt.status.String(), func(t *testing.T) {
			if got := statusMark(tt.status); got != tt.want {
				t.Errorf("statusMark(%v) = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

func TestPlural(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{{0, "references"}, {1, "reference"}, {2, "references"}}
	for _, tt := range tests {
		if got := plural("reference", tt.n); got != tt.want {
			t.Errorf("plural(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestNewDocument(t *testing.T) {
	doc := NewDocument(sampleReport())

	if doc.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", doc.SchemaVersion, SchemaVersion)
	}
	if doc.Root != "/repo" {
		t.Errorf("Root = %q, want /repo", doc.Root)
	}
	want := SummaryDoc{Total: 4, Pinned: 1, Stale: 1, Unpinned: 1, Skipped: 1}
	if doc.Summary != want {
		t.Errorf("Summary = %+v, want %+v", doc.Summary, want)
	}
	if len(doc.Findings) != 4 {
		t.Fatalf("Findings = %d, want 4", len(doc.Findings))
	}

	f := doc.Findings[1]
	if f.Status != "unpinned" || f.Kind != "remote" {
		t.Errorf("finding 1 status/kind = %q/%q, want unpinned/remote", f.Status, f.Kind)
	}
	if f.Ref != "actions/setup-go@v5" || f.Version != "v5" || f.Slug != "actions/setup-go" {
		t.Errorf("finding 1 ref fields = %+v", f)
	}
	if f.Pin != "actions/setup-go@"+sha500+" # v5.0.0" {
		t.Errorf("finding 1 pin = %q", f.Pin)
	}
}

func TestNewDocumentFindingsAreNeverNull(t *testing.T) {
	data, err := json.Marshal(NewDocument(Report{}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(data, []byte(`"findings":[]`)) {
		t.Fatalf("empty findings did not serialize as []: %s", data)
	}
}

func TestEncodeJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeJSON(&buf, sampleReport()); err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	if !bytes.HasSuffix(buf.Bytes(), []byte("\n")) {
		t.Error("EncodeJSON did not end with a newline")
	}

	var doc Document
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	if doc.Summary.Total != 4 || doc.Findings[3].Kind != "local" {
		t.Errorf("round-tripped document = %+v", doc)
	}
	if !doc.CheckedAt.Equal(sampleReport().CheckedAt) {
		t.Errorf("CheckedAt = %v, want %v", doc.CheckedAt, sampleReport().CheckedAt)
	}
	// Healthy findings still appear in JSON — only the text view filters them.
	if doc.Findings[0].Status != "pinned" {
		t.Errorf("finding 0 status = %q, want pinned", doc.Findings[0].Status)
	}
}
