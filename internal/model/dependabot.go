package model

import "time"

// DependabotLevel is the severity of a single Dependabot-config audit finding.
type DependabotLevel string

// Finding severities, in descending order of importance.
const (
	DependabotError   DependabotLevel = "error"   // a problem that breaks or defeats Dependabot
	DependabotWarning DependabotLevel = "warning" // a gap that meaningfully weakens the config
	DependabotInfo    DependabotLevel = "info"    // a best-practice suggestion
)

// Dependabot audit finding categories.
const (
	DependabotCategoryConfig       = "config"        // the file itself (missing, mislocated, version)
	DependabotCategoryCoverage     = "coverage"      // an ecosystem/directory used but not updated
	DependabotCategoryBestPractice = "best-practice" // a recommended-but-absent update setting
)

// DependabotFinding is one observation from auditing a repo's
// .github/dependabot.yml: a coverage gap, a best-practice suggestion, or a
// problem with the file itself.
type DependabotFinding struct {
	Level      DependabotLevel `json:"level"`
	Category   string          `json:"category"`            // config | coverage | best-practice
	Ecosystem  string          `json:"ecosystem,omitempty"` // the package-ecosystem the finding is about
	Directory  string          `json:"directory,omitempty"` // the update directory, when relevant
	Message    string          `json:"message"`
	Suggestion string          `json:"suggestion,omitempty"` // a concrete remedy
}

// DependabotEcosystem records one package ecosystem detected in the repository
// and whether the Dependabot config has an update entry for it.
type DependabotEcosystem struct {
	Ecosystem   string   `json:"ecosystem"`   // the package-ecosystem value, e.g. gomod
	Directories []string `json:"directories"` // repo-relative dirs (leading "/") that hold its manifests
	Covered     bool     `json:"covered"`     // an update entry exists for this ecosystem
}

// DependabotReport is the assembled audit of a repository's Dependabot setup:
// which ecosystems it uses, whether they are covered by the config, and every
// coverage gap and best-practice finding.
type DependabotReport struct {
	Owner        string                `json:"owner"`
	Repo         string                `json:"repo"`
	ConfigSource string                `json:"config_source"` // where the config came from (a path or a github: ref); empty when absent
	HasConfig    bool                  `json:"has_config"`
	Detected     []DependabotEcosystem `json:"detected"`
	Findings     []DependabotFinding   `json:"findings"`
	CheckedAt    time.Time             `json:"checked_at"`
}

// Count tallies the findings at the given level.
func (r *DependabotReport) Count(level DependabotLevel) int {
	n := 0
	for _, f := range r.Findings {
		if f.Level == level {
			n++
		}
	}
	return n
}

// HasErrors reports whether any finding is at error level.
func (r *DependabotReport) HasErrors() bool {
	return r.Count(DependabotError) > 0
}

// OK reports whether the audit found nothing worth flagging.
func (r *DependabotReport) OK() bool {
	return len(r.Findings) == 0
}
