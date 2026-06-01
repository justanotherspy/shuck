package model

import "testing"

func TestComplianceReportTallies(t *testing.T) {
	tests := []struct {
		name      string
		checks    []ComplianceCheck
		failures  bool
		compliant bool
	}{
		{
			name:      "no checks is not compliant",
			checks:    nil,
			failures:  false,
			compliant: false,
		},
		{
			name: "all pass",
			checks: []ComplianceCheck{
				{Status: CompliancePass},
				{Status: CompliancePass},
			},
			failures:  false,
			compliant: true,
		},
		{
			name: "skipped checks do not fail",
			checks: []ComplianceCheck{
				{Status: CompliancePass},
				{Status: ComplianceSkipped},
			},
			failures:  false,
			compliant: true,
		},
		{
			name: "any fail is a failure",
			checks: []ComplianceCheck{
				{Status: CompliancePass},
				{Status: ComplianceFail},
			},
			failures:  true,
			compliant: false,
		},
		{
			name: "an errored check is a failure",
			checks: []ComplianceCheck{
				{Status: ComplianceError},
			},
			failures:  true,
			compliant: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &ComplianceReport{Checks: tt.checks}
			if got := r.HasFailures(); got != tt.failures {
				t.Errorf("HasFailures() = %v, want %v", got, tt.failures)
			}
			if got := r.Compliant(); got != tt.compliant {
				t.Errorf("Compliant() = %v, want %v", got, tt.compliant)
			}
		})
	}
}

func TestComplianceReportCount(t *testing.T) {
	r := &ComplianceReport{Checks: []ComplianceCheck{
		{Status: CompliancePass},
		{Status: CompliancePass},
		{Status: ComplianceFail},
		{Status: ComplianceSkipped},
	}}
	for status, want := range map[ComplianceStatus]int{
		CompliancePass:    2,
		ComplianceFail:    1,
		ComplianceSkipped: 1,
		ComplianceError:   0,
	} {
		if got := r.Count(status); got != want {
			t.Errorf("Count(%q) = %d, want %d", status, got, want)
		}
	}
}
