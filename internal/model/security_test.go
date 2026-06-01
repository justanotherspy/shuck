package model

import "testing"

func TestSeverityRank(t *testing.T) {
	tests := []struct {
		sev  SecuritySeverity
		want int
	}{
		{SeverityCritical, 6},
		{SeverityHigh, 5},
		{SeverityMedium, 4},
		{SeverityLow, 3},
		{SeverityWarning, 2},
		{SeverityNote, 1},
		{SeverityUnknown, 0},
		{SecuritySeverity("bogus"), 0},
		{"", 0},
	}
	for _, tt := range tests {
		if got := SeverityRank(tt.sev); got != tt.want {
			t.Errorf("SeverityRank(%q) = %d, want %d", tt.sev, got, tt.want)
		}
	}
	// Ranks must be strictly ordered, highest first.
	ordered := []SecuritySeverity{
		SeverityCritical, SeverityHigh, SeverityMedium,
		SeverityLow, SeverityWarning, SeverityNote, SeverityUnknown,
	}
	for i := 1; i < len(ordered); i++ {
		if SeverityRank(ordered[i-1]) <= SeverityRank(ordered[i]) {
			t.Errorf("rank(%q) should exceed rank(%q)", ordered[i-1], ordered[i])
		}
	}
}

func TestSecurityReportTotalAlerts(t *testing.T) {
	tests := []struct {
		name string
		r    SecurityReport
		want int
	}{
		{"empty", SecurityReport{}, 0},
		{
			"all sources",
			SecurityReport{
				CodeScanningAlerts:   []CodeScanningAlert{{Number: 1}, {Number: 2}},
				SecretScanningAlerts: []SecretScanningAlert{{Number: 3}},
				DependabotAlerts:     []DependabotAlert{{Number: 4}, {Number: 5}, {Number: 6}},
			},
			6,
		},
		{
			"only dependabot",
			SecurityReport{DependabotAlerts: []DependabotAlert{{Number: 1}}},
			1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.r.TotalAlerts(); got != tt.want {
				t.Errorf("TotalAlerts() = %d, want %d", got, tt.want)
			}
		})
	}
}
