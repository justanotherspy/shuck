package dependabot

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/justanotherspy/shuck/internal/model"
)

// SchemaVersion is the version of the JSON document `shuck dependabot --json`
// emits. It is bumped only on a breaking change; additive fields keep it.
const SchemaVersion = 1

// categoryTitle labels each finding category in the text output.
var categoryTitle = map[string]string{
	model.DependabotCategoryConfig:       "Config",
	model.DependabotCategoryCoverage:     "Coverage",
	model.DependabotCategoryBestPractice: "Best practices",
}

// categoryOrder fixes the section order in the text output.
var categoryOrder = []string{
	model.DependabotCategoryConfig,
	model.DependabotCategoryCoverage,
	model.DependabotCategoryBestPractice,
}

// Render writes the human-readable audit summary for r to w.
func Render(w io.Writer, r *model.DependabotReport) {
	fmt.Fprintf(w, "%s/%s — dependabot audit\n", r.Owner, r.Repo)
	if r.ConfigSource != "" {
		fmt.Fprintf(w, "config: %s\n", r.ConfigSource)
	} else {
		fmt.Fprintln(w, "config: (none)")
	}

	fmt.Fprintln(w, "\nDetected ecosystems:")
	if len(r.Detected) == 0 {
		fmt.Fprintln(w, "  (none found)")
	}
	for _, e := range r.Detected {
		mark := "✓"
		if !e.Covered {
			mark = "✗"
		}
		fmt.Fprintf(w, "  %s %s (%s)\n", mark, e.Ecosystem, strings.Join(e.Directories, ", "))
	}

	errs := r.Count(model.DependabotError)
	warns := r.Count(model.DependabotWarning)
	infos := r.Count(model.DependabotInfo)
	fmt.Fprintf(w, "\nSummary: %d finding(s) — %d error, %d warning, %d info\n", len(r.Findings), errs, warns, infos)

	for _, cat := range categoryOrder {
		fs := findingsFor(r, cat)
		if len(fs) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s:\n", categoryTitle[cat])
		for _, f := range fs {
			writeFinding(w, f)
		}
	}

	if r.OK() {
		fmt.Fprintln(w, "\n✓ Dependabot config looks good — no findings.")
		return
	}
	if errs > 0 {
		fmt.Fprintf(w, "\n✗ %d error(s), %d warning(s) — see above.\n", errs, warns)
	} else {
		fmt.Fprintf(w, "\n%d suggestion(s) to improve the Dependabot config.\n", len(r.Findings))
	}
}

func writeFinding(w io.Writer, f model.DependabotFinding) {
	mark := levelMark(f.Level)
	scope := f.Ecosystem
	if f.Ecosystem != "" && f.Directory != "" {
		scope = fmt.Sprintf("%s (%s)", f.Ecosystem, f.Directory)
	}
	if scope != "" {
		fmt.Fprintf(w, "  %s %s: %s\n", mark, scope, f.Message)
	} else {
		fmt.Fprintf(w, "  %s %s\n", mark, f.Message)
	}
	if f.Suggestion != "" {
		fmt.Fprintf(w, "      → %s\n", f.Suggestion)
	}
}

func levelMark(l model.DependabotLevel) string {
	switch l {
	case model.DependabotError:
		return "✗"
	case model.DependabotWarning:
		return "!"
	default:
		return "–"
	}
}

func findingsFor(r *model.DependabotReport, category string) []model.DependabotFinding {
	var out []model.DependabotFinding
	for _, f := range r.Findings {
		if f.Category == category {
			out = append(out, f)
		}
	}
	return out
}

// Document is the stable, machine-readable shape of `shuck dependabot --json`.
type Document struct {
	SchemaVersion int                         `json:"schema_version"`
	Repo          Repo                        `json:"repo"`
	ConfigSource  string                      `json:"config_source"`
	HasConfig     bool                        `json:"has_config"`
	OK            bool                        `json:"ok"`
	Summary       SummaryDoc                  `json:"summary"`
	Ecosystems    []model.DependabotEcosystem `json:"ecosystems"`
	Findings      []model.DependabotFinding   `json:"findings"`
}

// Repo identifies the inspected repository.
type Repo struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
}

// SummaryDoc is the quick tally of an audit report.
type SummaryDoc struct {
	Total   int `json:"total"`
	Error   int `json:"error"`
	Warning int `json:"warning"`
	Info    int `json:"info"`
}

// NewDocument projects a report into the stable JSON document. The slices are
// always non-nil so they serialize as [] rather than null.
func NewDocument(r *model.DependabotReport) Document {
	ecos := r.Detected
	if ecos == nil {
		ecos = []model.DependabotEcosystem{}
	}
	findings := r.Findings
	if findings == nil {
		findings = []model.DependabotFinding{}
	}
	return Document{
		SchemaVersion: SchemaVersion,
		Repo:          Repo{Owner: r.Owner, Repo: r.Repo},
		ConfigSource:  r.ConfigSource,
		HasConfig:     r.HasConfig,
		OK:            r.OK(),
		Summary: SummaryDoc{
			Total:   len(r.Findings),
			Error:   r.Count(model.DependabotError),
			Warning: r.Count(model.DependabotWarning),
			Info:    r.Count(model.DependabotInfo),
		},
		Ecosystems: ecos,
		Findings:   findings,
	}
}

// EncodeJSON writes the report as an indented JSON document with a trailing
// newline.
func EncodeJSON(w io.Writer, r *model.DependabotReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(NewDocument(r))
}
