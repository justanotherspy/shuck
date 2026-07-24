package pins

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// SchemaVersion is the version of the JSON document `shuck pins --json` emits.
// It is bumped only on a breaking change; additive fields keep it.
const SchemaVersion = 1

// Render writes the human-readable pin audit for rep to w.
//
// Only the references that need a decision are printed — a repository where
// every pin is current says so in one line rather than listing dozens of
// healthy `uses:` entries. Each printed block leads with file:line so an editor
// (or an agent) can jump straight there, and ends with the exact replacement
// line, so acting on the report is a copy rather than a lookup.
func Render(w io.Writer, rep Report) {
	fmt.Fprintf(w, "%s — action pins\n", noteOr(rep.Root, "."))

	total := len(rep.Findings)
	if total == 0 {
		fmt.Fprintln(w, "\nNo `uses:` references found.")
		return
	}

	fmt.Fprintf(w, "\nSummary: %d %s — %d pinned, %d stale, %d unpinned",
		total, plural("reference", total), rep.Count(StatusPinned), rep.Stale, rep.Unpinned)
	if rep.Skipped > 0 {
		fmt.Fprintf(w, ", %d skipped", rep.Skipped)
	}
	fmt.Fprintln(w)

	for _, f := range rep.Findings {
		if f.Status == StatusPinned {
			continue
		}
		writeFinding(w, f)
	}

	switch {
	case rep.HasIssues():
		issues := rep.Unpinned + rep.Stale
		fmt.Fprintf(w, "\n✗ %d %s %s attention — %d unpinned, %d behind the latest release.\n",
			issues, plural("reference", issues), verb(issues, "needs", "need"), rep.Unpinned, rep.Stale)
	case rep.Skipped > 0:
		fmt.Fprintf(w, "\n✓ Every checked reference is pinned to a commit SHA and current (%d skipped).\n", rep.Skipped)
	default:
		fmt.Fprintln(w, "\n✓ Every `uses:` reference is pinned to a commit SHA and current.")
	}
}

// writeFinding prints one non-pinned finding: where it is, what it says today,
// why it was flagged, and the line to paste in its place.
func writeFinding(w io.Writer, f Finding) {
	fmt.Fprintf(w, "\n%s %s:%d  %s\n", statusMark(f.Status), f.File, f.Line, refLabel(f))
	if f.Note != "" {
		fmt.Fprintf(w, "    %s\n", f.Note)
	}
	if f.PinLine != "" {
		fmt.Fprintf(w, "    uses: %s\n", f.PinLine)
	}
}

// statusMark is the leading glyph for a finding, matching the vocabulary the
// other shuck reports use: ✗ for something wrong, – for something skipped.
func statusMark(s Status) string {
	switch s {
	case StatusUnpinned:
		return "✗"
	case StatusStale:
		return "⚠"
	case StatusSkipped:
		return "–"
	default:
		return "✓"
	}
}

// refLabel renders a reference the way it appears in the file, comment and all,
// so the printed block can be matched against the source by eye.
func refLabel(f Finding) string {
	if f.Raw == "" {
		return "(unreadable)"
	}
	if f.Comment != "" {
		return f.Raw + " # " + f.Comment
	}
	return f.Raw
}

// plural appends an "s" to word unless n is exactly 1.
func plural(word string, n int) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// verb agrees a verb with a count, for the summary line.
func verb(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

// Document is the stable, machine-readable shape of `shuck pins --json`. Like
// the other shuck JSON views its types are separate from the domain types, so
// refactoring Finding or Report cannot silently reshape what consumers parse.
type Document struct {
	SchemaVersion int          `json:"schema_version"`
	Root          string       `json:"root"`
	Summary       SummaryDoc   `json:"summary"`
	CheckedAt     time.Time    `json:"checked_at"`
	Findings      []FindingDoc `json:"findings"`
}

// SummaryDoc is the quick tally of a pin audit.
type SummaryDoc struct {
	Total    int `json:"total"`
	Pinned   int `json:"pinned"`
	Stale    int `json:"stale"`
	Unpinned int `json:"unpinned"`
	Skipped  int `json:"skipped"`
}

// FindingDoc is one audited reference in the JSON view. Status and Kind are
// emitted as their lowercase names rather than their Go integer values, so the
// schema stays readable and reordering the constants cannot change it.
type FindingDoc struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Ref     string `json:"ref"`
	Slug    string `json:"slug,omitempty"`
	Version string `json:"version,omitempty"`
	Comment string `json:"comment,omitempty"`
	Kind    string `json:"kind"`
	Status  string `json:"status"`
	Latest  string `json:"latest,omitempty"`
	SHA     string `json:"sha,omitempty"`
	Pin     string `json:"pin,omitempty"`
	Note    string `json:"note,omitempty"`
}

// NewDocument projects a report onto the stable, versioned JSON view. The
// findings slice is always non-nil so it serializes as [] rather than null.
func NewDocument(rep Report) Document {
	findings := make([]FindingDoc, 0, len(rep.Findings))
	for _, f := range rep.Findings {
		findings = append(findings, FindingDoc{
			File:    f.File,
			Line:    f.Line,
			Ref:     f.Raw,
			Slug:    f.Slug,
			Version: f.Ref,
			Comment: f.Comment,
			Kind:    f.Kind.String(),
			Status:  f.Status.String(),
			Latest:  f.Latest,
			SHA:     f.SHA,
			Pin:     f.PinLine,
			Note:    f.Note,
		})
	}
	return Document{
		SchemaVersion: SchemaVersion,
		Root:          rep.Root,
		Summary: SummaryDoc{
			Total:    len(rep.Findings),
			Pinned:   rep.Count(StatusPinned),
			Stale:    rep.Stale,
			Unpinned: rep.Unpinned,
			Skipped:  rep.Skipped,
		},
		CheckedAt: rep.CheckedAt,
		Findings:  findings,
	}
}

// EncodeJSON writes the report as an indented JSON document with a trailing
// newline.
func EncodeJSON(w io.Writer, rep Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(NewDocument(rep))
}
