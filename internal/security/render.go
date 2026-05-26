package security

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/justanotherspy/shuck/internal/model"
)

// SchemaVersion is the version of the JSON document `shuck security --json`
// emits. It is bumped only on a breaking change; additive fields keep it.
const SchemaVersion = 1

// Render writes the human-readable security summary for r to w.
func Render(w io.Writer, r *model.SecurityReport) {
	fmt.Fprintf(w, "%s/%s — security alerts (%s)\n", r.Owner, r.Repo, r.State)

	c := Count(r)
	writeSummary(w, r, c)

	writeDependabotSection(w, r)
	writeCodeScanningSection(w, r)
	writeSecretSection(w, r)

	if c.Total == 0 && allOK(r) {
		if r.State == "all" {
			fmt.Fprintln(w, "\n✓ No security alerts.")
		} else {
			fmt.Fprintf(w, "\n✓ No %s security alerts.\n", r.State)
		}
	}
}

func writeSummary(w io.Writer, r *model.SecurityReport, c Counts) {
	if c.Total == 0 {
		if r.State == "all" {
			fmt.Fprintln(w, "\nSummary: no alerts")
		} else {
			fmt.Fprintf(w, "\nSummary: no %s alerts\n", r.State)
		}
		return
	}
	var sev []string
	for _, s := range severityOrder {
		if n := c.BySeverity[s]; n > 0 {
			sev = append(sev, fmt.Sprintf("%d %s", n, s))
		}
	}
	fmt.Fprintf(w, "\nSummary: %d %s — %s\n", c.Total, plural("alert", c.Total), strings.Join(sev, ", "))
}

func writeDependabotSection(w io.Writer, r *model.SecurityReport) {
	if note := sourceStatusNote("Dependabot", r.Dependabot); note != "" {
		fmt.Fprint(w, note)
		return
	}
	if len(r.DependabotAlerts) == 0 {
		return
	}
	fmt.Fprintf(w, "\nDependabot (%d):\n", len(r.DependabotAlerts))
	for _, a := range r.DependabotAlerts {
		writeDependabotAlert(w, a)
	}
}

func writeDependabotAlert(w io.Writer, a model.DependabotAlert) {
	var b strings.Builder
	fmt.Fprintf(&b, "  ● %s", a.Severity)
	if a.Ecosystem != "" {
		fmt.Fprintf(&b, "  %s", a.Ecosystem)
	}
	if a.Package != "" {
		fmt.Fprintf(&b, "  %s", a.Package)
	}
	if a.FixedVersion != "" {
		fmt.Fprintf(&b, " → %s", a.FixedVersion)
	}
	if ids := joinNonEmpty([]string{a.GHSAID, a.CVEID}, "  "); ids != "" {
		fmt.Fprintf(&b, "   %s", ids)
	}
	fmt.Fprintln(w, b.String())
	if a.Summary != "" {
		fmt.Fprintf(w, "      %s\n", firstLine(a.Summary))
	}
	if a.VulnerableVersions != "" {
		fmt.Fprintf(w, "      vulnerable: %s\n", a.VulnerableVersions)
	}
	if a.ManifestPath != "" {
		fmt.Fprintf(w, "      manifest: %s\n", a.ManifestPath)
	}
	if a.HTMLURL != "" {
		fmt.Fprintf(w, "      %s\n", a.HTMLURL)
	}
}

func writeCodeScanningSection(w io.Writer, r *model.SecurityReport) {
	if note := sourceStatusNote("Code scanning", r.CodeScanning); note != "" {
		fmt.Fprint(w, note)
		return
	}
	if len(r.CodeScanningAlerts) == 0 {
		return
	}
	fmt.Fprintf(w, "\nCode scanning (%d):\n", len(r.CodeScanningAlerts))
	for _, a := range r.CodeScanningAlerts {
		writeCodeScanningAlert(w, a)
	}
}

func writeCodeScanningAlert(w io.Writer, a model.CodeScanningAlert) {
	var b strings.Builder
	fmt.Fprintf(&b, "  ● %s  %s", a.Severity, a.RuleID)
	if a.Tool != "" {
		fmt.Fprintf(&b, "   [%s]", a.Tool)
	}
	fmt.Fprintln(w, b.String())
	if loc := locationLabel(a.Path, a.StartLine, a.EndLine); loc != "" {
		fmt.Fprintf(w, "      %s\n", loc)
	}
	if msg := firstLine(a.Message); msg != "" {
		fmt.Fprintf(w, "      %s\n", msg)
	} else if a.Description != "" {
		fmt.Fprintf(w, "      %s\n", firstLine(a.Description))
	}
	if a.HTMLURL != "" {
		fmt.Fprintf(w, "      %s\n", a.HTMLURL)
	}
}

func writeSecretSection(w io.Writer, r *model.SecurityReport) {
	if note := sourceStatusNote("Secret scanning", r.SecretScanning); note != "" {
		fmt.Fprint(w, note)
		return
	}
	if len(r.SecretScanningAlerts) == 0 {
		return
	}
	fmt.Fprintf(w, "\nSecret scanning (%d):\n", len(r.SecretScanningAlerts))
	for _, a := range r.SecretScanningAlerts {
		writeSecretScanningAlert(w, a)
	}
}

func writeSecretScanningAlert(w io.Writer, a model.SecretScanningAlert) {
	name := a.DisplayName
	if name == "" {
		name = a.SecretType
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  ● %s", name)
	if a.SecretType != "" && a.SecretType != name {
		fmt.Fprintf(&b, " (%s)", a.SecretType)
	}
	if a.State != "" {
		fmt.Fprintf(&b, "  [%s]", a.State)
	}
	fmt.Fprintln(w, b.String())
	for _, loc := range a.Locations {
		if l := locationLabel(loc.Path, loc.StartLine, loc.EndLine); l != "" {
			fmt.Fprintf(w, "      %s\n", l)
		}
	}
	if a.Resolution != "" {
		fmt.Fprintf(w, "      resolution: %s\n", a.Resolution)
	}
	if a.HTMLURL != "" {
		fmt.Fprintf(w, "      %s\n", a.HTMLURL)
	}
}

// sourceStatusNote returns a one-line note for a non-OK source (with a leading
// blank line), or "" when the source is OK so the caller renders its alerts.
func sourceStatusNote(name string, src model.SecuritySource) string {
	switch src.Status {
	case model.StatusOK:
		return ""
	case model.StatusDisabled:
		return fmt.Sprintf("\n%s: %s — skipped.\n", name, msgOr(src.Message, "not enabled or no access"))
	case model.StatusForbidden:
		return fmt.Sprintf("\n%s: %s — skipped.\n", name, msgOr(src.Message, "token lacks access"))
	default:
		return fmt.Sprintf("\n%s: error — %s\n", name, msgOr(src.Message, "could not fetch"))
	}
}

func allOK(r *model.SecurityReport) bool {
	return r.CodeScanning.Status == model.StatusOK &&
		r.SecretScanning.Status == model.StatusOK &&
		r.Dependabot.Status == model.StatusOK
}

func locationLabel(path string, start, end int) string {
	if path == "" {
		return ""
	}
	switch {
	case start > 0 && end > start:
		return fmt.Sprintf("%s:%d-%d", path, start, end)
	case start > 0:
		return fmt.Sprintf("%s:%d", path, start)
	default:
		return path
	}
}

func joinNonEmpty(parts []string, sep string) string {
	var keep []string
	for _, p := range parts {
		if p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, sep)
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

func msgOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func plural(word string, n int) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// Document is the stable, machine-readable shape of `shuck security --json`.
type Document struct {
	SchemaVersion        int                         `json:"schema_version"`
	Repo                 Repo                        `json:"repo"`
	State                string                      `json:"state"`
	Summary              SummaryDoc                  `json:"summary"`
	Sources              SourcesDoc                  `json:"sources"`
	CodeScanningAlerts   []model.CodeScanningAlert   `json:"code_scanning_alerts"`
	SecretScanningAlerts []model.SecretScanningAlert `json:"secret_scanning_alerts"`
	DependabotAlerts     []model.DependabotAlert     `json:"dependabot_alerts"`
}

// Repo identifies the inspected repository.
type Repo struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
}

// SummaryDoc is the quick-count view: total, per-severity, and per-source.
type SummaryDoc struct {
	Total      int             `json:"total"`
	BySeverity map[string]int  `json:"by_severity"`
	BySource   SourceCountsDoc `json:"by_source"`
}

// SourceCountsDoc counts the alerts each source returned.
type SourceCountsDoc struct {
	CodeScanning   int `json:"code_scanning"`
	SecretScanning int `json:"secret_scanning"`
	Dependabot     int `json:"dependabot"`
}

// SourcesDoc reports each source's fetch outcome.
type SourcesDoc struct {
	CodeScanning   model.SecuritySource `json:"code_scanning"`
	SecretScanning model.SecuritySource `json:"secret_scanning"`
	Dependabot     model.SecuritySource `json:"dependabot"`
}

// NewDocument projects a report into the stable JSON document. Alert slices are
// always non-nil so they serialize as [] rather than null.
func NewDocument(r *model.SecurityReport) Document {
	c := Count(r)
	bySev := make(map[string]int, len(severityOrder))
	for _, s := range severityOrder {
		bySev[string(s)] = c.BySeverity[s]
	}
	doc := Document{
		SchemaVersion: SchemaVersion,
		Repo:          Repo{Owner: r.Owner, Repo: r.Repo},
		State:         r.State,
		Summary: SummaryDoc{
			Total:      c.Total,
			BySeverity: bySev,
			BySource: SourceCountsDoc{
				CodeScanning:   c.CodeScanning,
				SecretScanning: c.SecretScanning,
				Dependabot:     c.Dependabot,
			},
		},
		Sources: SourcesDoc{
			CodeScanning:   r.CodeScanning,
			SecretScanning: r.SecretScanning,
			Dependabot:     r.Dependabot,
		},
		CodeScanningAlerts:   r.CodeScanningAlerts,
		SecretScanningAlerts: r.SecretScanningAlerts,
		DependabotAlerts:     r.DependabotAlerts,
	}
	if doc.CodeScanningAlerts == nil {
		doc.CodeScanningAlerts = []model.CodeScanningAlert{}
	}
	if doc.SecretScanningAlerts == nil {
		doc.SecretScanningAlerts = []model.SecretScanningAlert{}
	}
	if doc.DependabotAlerts == nil {
		doc.DependabotAlerts = []model.DependabotAlert{}
	}
	return doc
}

// EncodeJSON writes the report as an indented JSON document with a trailing
// newline.
func EncodeJSON(w io.Writer, r *model.SecurityReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(NewDocument(r))
}
