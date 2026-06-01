package compliance

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/justanotherspy/shuck/internal/model"
)

// SchemaVersion is the version of the JSON document `shuck compliance --json`
// emits. It is bumped only on a breaking change; additive fields keep it.
const SchemaVersion = 1

// categoryOrder fixes the section order in the text output.
var categoryOrder = []string{"repository", "security", "branch_protection"}

var categoryTitle = map[string]string{
	"repository":        "Repository",
	"security":          "Security",
	"branch_protection": "Branch protection",
}

// Render writes the human-readable compliance summary for r to w.
func Render(w io.Writer, r *model.ComplianceReport) {
	fmt.Fprintf(w, "%s/%s — compliance\n", r.Owner, r.Repo)
	if r.ConfigSource != "" {
		fmt.Fprintf(w, "config: %s\n", r.ConfigSource)
	}

	pass := r.Count(model.CompliancePass)
	fail := r.Count(model.ComplianceFail)
	skip := r.Count(model.ComplianceSkipped)

	fmt.Fprintf(w, "\nSummary: %d checked — %d pass, %d fail", len(r.Checks), pass, fail)
	if skip > 0 {
		fmt.Fprintf(w, ", %d skipped", skip)
	}
	fmt.Fprintln(w)

	for _, cat := range categoryOrder {
		checks := checksFor(r, cat)
		if len(checks) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s:\n", categoryTitle[cat])
		for _, c := range checks {
			writeCheck(w, c)
		}
	}

	if len(r.Checks) == 0 {
		fmt.Fprintln(w, "\nNo checks declared in the config.")
		return
	}
	if fail == 0 {
		if skip > 0 {
			fmt.Fprintf(w, "\n✓ Compliant (%d setting(s) skipped — not readable with this token).\n", skip)
		} else {
			fmt.Fprintln(w, "\n✓ Compliant — all settings match the config.")
		}
	} else {
		fmt.Fprintf(w, "\n✗ Not compliant — %d setting(s) drifted from the config.\n", fail)
	}
}

func writeCheck(w io.Writer, c model.ComplianceCheck) {
	switch c.Status {
	case model.CompliancePass:
		fmt.Fprintf(w, "  ✓ %s = %s\n", c.Setting, c.Expected)
	case model.ComplianceFail:
		fmt.Fprintf(w, "  ✗ %s: want %s, got %s\n", c.Setting, c.Expected, valueOr(c.Actual, "(unset)"))
	case model.ComplianceSkipped:
		fmt.Fprintf(w, "  – %s: want %s — skipped (%s)\n", c.Setting, c.Expected, valueOr(c.Message, "not readable"))
	default:
		fmt.Fprintf(w, "  ! %s: want %s — error (%s)\n", c.Setting, c.Expected, valueOr(c.Message, "evaluation failed"))
	}
}

func checksFor(r *model.ComplianceReport, category string) []model.ComplianceCheck {
	var out []model.ComplianceCheck
	for _, c := range r.Checks {
		if c.Category == category {
			out = append(out, c)
		}
	}
	return out
}

func valueOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// Document is the stable, machine-readable shape of `shuck compliance --json`.
type Document struct {
	SchemaVersion int                     `json:"schema_version"`
	Repo          Repo                    `json:"repo"`
	ConfigSource  string                  `json:"config_source"`
	Compliant     bool                    `json:"compliant"`
	Summary       SummaryDoc              `json:"summary"`
	Checks        []model.ComplianceCheck `json:"checks"`
}

// Repo identifies the inspected repository.
type Repo struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
}

// SummaryDoc is the quick tally of a compliance report.
type SummaryDoc struct {
	Total   int `json:"total"`
	Pass    int `json:"pass"`
	Fail    int `json:"fail"`
	Skipped int `json:"skipped"`
}

// NewDocument projects a report into the stable JSON document. The checks slice
// is always non-nil so it serializes as [] rather than null.
func NewDocument(r *model.ComplianceReport) Document {
	checks := r.Checks
	if checks == nil {
		checks = []model.ComplianceCheck{}
	}
	return Document{
		SchemaVersion: SchemaVersion,
		Repo:          Repo{Owner: r.Owner, Repo: r.Repo},
		ConfigSource:  r.ConfigSource,
		Compliant:     r.Compliant(),
		Summary: SummaryDoc{
			Total:   len(r.Checks),
			Pass:    r.Count(model.CompliancePass),
			Fail:    r.Count(model.ComplianceFail),
			Skipped: r.Count(model.ComplianceSkipped),
		},
		Checks: checks,
	}
}

// EncodeJSON writes the report as an indented JSON document with a trailing
// newline.
func EncodeJSON(w io.Writer, r *model.ComplianceReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(NewDocument(r))
}

// RenderDiscovery writes the human-readable summary of a `shuck compliance
// discover` run to w. dryRun switches the verbs to the conditional and prints
// the would-be file contents instead of claiming they were written.
func RenderDiscovery(w io.Writer, d *Discovery, dryRun bool) {
	fmt.Fprintf(w, "%s/%s — compliance discover\n", d.Owner, d.Repo)
	fmt.Fprintf(w, "config: %s\n", d.Path)

	switch {
	case d.Created:
		verb := "Created"
		if dryRun {
			verb = "Would create"
		}
		fmt.Fprintf(w, "\n%s %s from the live settings:\n", verb, d.Path)
	case len(d.Changes) > 0:
		verb := "Updated"
		if dryRun {
			verb = "Would update"
		}
		fmt.Fprintf(w, "\n%s %s — %d declared setting(s) synced to the live values:\n", verb, d.Path, len(d.Changes))
		for _, c := range d.Changes {
			fmt.Fprintf(w, "  ~ %s.%s: %s → %s\n", c.Category, c.Setting, valueOr(c.From, "(unset)"), valueOr(c.To, "(unset)"))
		}
	default:
		fmt.Fprintf(w, "\n✓ %s already matches the live settings — nothing to update.\n", d.Path)
	}

	for _, n := range d.Notes {
		fmt.Fprintf(w, "  – %s\n", n)
	}

	// Show the resulting file when it is new (so the user sees what they got)
	// or when nothing was written (--dry-run preview of an update).
	if d.Created || (dryRun && len(d.Changes) > 0) {
		fmt.Fprintf(w, "\n%s", d.Data)
	}
}

// DiscoveryDocument is the stable, machine-readable shape of
// `shuck compliance discover --json`.
type DiscoveryDocument struct {
	SchemaVersion int      `json:"schema_version"`
	Repo          Repo     `json:"repo"`
	Path          string   `json:"path"`
	Created       bool     `json:"created"`
	Updated       bool     `json:"updated"`
	UpToDate      bool     `json:"up_to_date"`
	Changes       []Change `json:"changes"`
	Notes         []string `json:"notes"`
	Config        string   `json:"config"` // the resulting config file contents
}

// NewDiscoveryDocument projects a discovery into the stable JSON document. The
// changes and notes slices are always non-nil so they serialize as [] rather
// than null.
func NewDiscoveryDocument(d *Discovery) DiscoveryDocument {
	changes := d.Changes
	if changes == nil {
		changes = []Change{}
	}
	notes := d.Notes
	if notes == nil {
		notes = []string{}
	}
	return DiscoveryDocument{
		SchemaVersion: SchemaVersion,
		Repo:          Repo{Owner: d.Owner, Repo: d.Repo},
		Path:          d.Path,
		Created:       d.Created,
		Updated:       !d.Created && len(d.Changes) > 0,
		UpToDate:      !d.Changed,
		Changes:       changes,
		Notes:         notes,
		Config:        string(d.Data),
	}
}

// EncodeDiscoveryJSON writes the discovery as an indented JSON document with a
// trailing newline.
func EncodeDiscoveryJSON(w io.Writer, d *Discovery) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(NewDiscoveryDocument(d))
}
