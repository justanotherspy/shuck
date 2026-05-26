package model

import "time"

// SecuritySeverity is the normalized severity scale shuck sorts security alerts
// on. Code scanning's note/warning/error and the GHAS critical/high/medium/low
// levels are both mapped onto it so every source ranks consistently.
type SecuritySeverity string

// Normalized severities, highest first.
const (
	SeverityCritical SecuritySeverity = "critical"
	SeverityHigh     SecuritySeverity = "high"
	SeverityMedium   SecuritySeverity = "medium"
	SeverityLow      SecuritySeverity = "low"
	SeverityWarning  SecuritySeverity = "warning"
	SeverityNote     SecuritySeverity = "note"
	SeverityUnknown  SecuritySeverity = "unknown"
)

// SeverityRank orders severities for sorting (higher is more severe). Unknown
// sorts last.
func SeverityRank(s SecuritySeverity) int {
	switch s {
	case SeverityCritical:
		return 6
	case SeverityHigh:
		return 5
	case SeverityMedium:
		return 4
	case SeverityLow:
		return 3
	case SeverityWarning:
		return 2
	case SeverityNote:
		return 1
	default:
		return 0
	}
}

// SourceStatus is the outcome of querying one security-alert source, so the
// output can distinguish "enabled and clean" from "not enabled" or "no access".
type SourceStatus string

// Per-source fetch outcomes.
const (
	StatusOK        SourceStatus = "ok"        // queried successfully (alerts may be empty)
	StatusDisabled  SourceStatus = "disabled"  // feature not enabled, or state N/A for this source
	StatusForbidden SourceStatus = "forbidden" // token lacks the required access
	StatusError     SourceStatus = "error"     // a genuine error reaching the source
)

// SecuritySource records how a single source responded.
type SecuritySource struct {
	Status  SourceStatus `json:"status"`
	Message string       `json:"message,omitempty"`
}

// CodeScanningAlert is a single code scanning (e.g. CodeQL) finding.
type CodeScanningAlert struct {
	Number      int              `json:"number"`
	State       string           `json:"state"`
	Severity    SecuritySeverity `json:"severity"`
	RuleID      string           `json:"rule_id"`
	Description string           `json:"description,omitempty"`
	Tool        string           `json:"tool,omitempty"`
	Path        string           `json:"path,omitempty"`
	StartLine   int              `json:"start_line,omitempty"`
	EndLine     int              `json:"end_line,omitempty"`
	Message     string           `json:"message,omitempty"`
	HTMLURL     string           `json:"html_url,omitempty"`
}

// SecretLocation is one place a leaked secret was found. Only file locations are
// surfaced (commit/PR-comment locations are skipped).
type SecretLocation struct {
	Path      string `json:"path,omitempty"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
}

// SecretScanningAlert is a single secret scanning finding. It deliberately has
// no field for the raw secret value: shuck never reads it from the API, so the
// secret cannot leak into output, JSON, or the cache.
type SecretScanningAlert struct {
	Number      int              `json:"number"`
	State       string           `json:"state"`
	SecretType  string           `json:"secret_type"`
	DisplayName string           `json:"secret_type_display_name,omitempty"`
	Resolution  string           `json:"resolution,omitempty"`
	Locations   []SecretLocation `json:"locations,omitempty"`
	HTMLURL     string           `json:"html_url,omitempty"`
}

// DependabotAlert is a single vulnerable-dependency finding. GitHub's npm
// "malware" advisories surface here too; there is no separate malware endpoint.
type DependabotAlert struct {
	Number             int              `json:"number"`
	State              string           `json:"state"`
	Severity           SecuritySeverity `json:"severity"`
	Ecosystem          string           `json:"ecosystem,omitempty"`
	Package            string           `json:"package,omitempty"`
	ManifestPath       string           `json:"manifest_path,omitempty"`
	VulnerableVersions string           `json:"vulnerable_version_range,omitempty"`
	FixedVersion       string           `json:"first_patched_version,omitempty"`
	GHSAID             string           `json:"ghsa_id,omitempty"`
	CVEID              string           `json:"cve_id,omitempty"`
	Summary            string           `json:"summary,omitempty"`
	HTMLURL            string           `json:"html_url,omitempty"`
}

// SecurityReport is the assembled security posture for one repository: the
// per-source fetch outcome plus the alerts each returned.
type SecurityReport struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	State string `json:"state"` // the requested state filter (open|all|...)

	CodeScanning   SecuritySource `json:"code_scanning"`
	SecretScanning SecuritySource `json:"secret_scanning"`
	Dependabot     SecuritySource `json:"dependabot"`

	CodeScanningAlerts   []CodeScanningAlert   `json:"code_scanning_alerts"`
	SecretScanningAlerts []SecretScanningAlert `json:"secret_scanning_alerts"`
	DependabotAlerts     []DependabotAlert     `json:"dependabot_alerts"`

	CheckedAt time.Time `json:"checked_at"`
}

// TotalAlerts reports how many alerts were collected across all sources.
func (r *SecurityReport) TotalAlerts() int {
	return len(r.CodeScanningAlerts) + len(r.SecretScanningAlerts) + len(r.DependabotAlerts)
}
