// Package security assembles, sorts, and renders a repository's GitHub security
// alerts (code scanning, secret scanning, and Dependabot) into shuck's text and
// JSON output. It does no network I/O: the gh layer fetches, this package shapes
// the result for humans and agents.
package security

import (
	"sort"

	"github.com/justanotherspy/shuck/internal/model"
)

// severityOrder lists severities from most to least severe, for stable display
// and rank-ordered breakdowns.
var severityOrder = []model.SecuritySeverity{
	model.SeverityCritical,
	model.SeverityHigh,
	model.SeverityMedium,
	model.SeverityLow,
	model.SeverityWarning,
	model.SeverityNote,
	model.SeverityUnknown,
}

// Sort orders each alert slice by severity (most severe first), then by alert
// number ascending, so output is deterministic.
func Sort(r *model.SecurityReport) {
	sort.SliceStable(r.CodeScanningAlerts, func(i, j int) bool {
		a, b := r.CodeScanningAlerts[i], r.CodeScanningAlerts[j]
		if ra, rb := model.SeverityRank(a.Severity), model.SeverityRank(b.Severity); ra != rb {
			return ra > rb
		}
		return a.Number < b.Number
	})
	sort.SliceStable(r.DependabotAlerts, func(i, j int) bool {
		a, b := r.DependabotAlerts[i], r.DependabotAlerts[j]
		if ra, rb := model.SeverityRank(a.Severity), model.SeverityRank(b.Severity); ra != rb {
			return ra > rb
		}
		return a.Number < b.Number
	})
	sort.SliceStable(r.SecretScanningAlerts, func(i, j int) bool {
		return r.SecretScanningAlerts[i].Number < r.SecretScanningAlerts[j].Number
	})
}

// Counts is a quick tally of a report's alerts, by severity and by source.
type Counts struct {
	Total          int
	BySeverity     map[model.SecuritySeverity]int
	CodeScanning   int
	SecretScanning int
	Dependabot     int
}

// Count tallies a report. BySeverity always contains every severity key (zero
// when absent) so callers can emit a stable breakdown. Secret scanning alerts
// carry no severity and are counted as unknown.
func Count(r *model.SecurityReport) Counts {
	c := Counts{
		BySeverity:     make(map[model.SecuritySeverity]int, len(severityOrder)),
		CodeScanning:   len(r.CodeScanningAlerts),
		SecretScanning: len(r.SecretScanningAlerts),
		Dependabot:     len(r.DependabotAlerts),
	}
	for _, s := range severityOrder {
		c.BySeverity[s] = 0
	}
	for _, a := range r.CodeScanningAlerts {
		c.BySeverity[a.Severity]++
	}
	for _, a := range r.DependabotAlerts {
		c.BySeverity[a.Severity]++
	}
	c.BySeverity[model.SeverityUnknown] += len(r.SecretScanningAlerts)
	c.Total = c.CodeScanning + c.SecretScanning + c.Dependabot
	return c
}
