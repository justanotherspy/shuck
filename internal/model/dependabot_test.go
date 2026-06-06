package model

import "testing"

func TestDependabotReportCounts(t *testing.T) {
	r := &DependabotReport{Findings: []DependabotFinding{
		{Level: DependabotError},
		{Level: DependabotWarning},
		{Level: DependabotWarning},
		{Level: DependabotInfo},
	}}
	if got := r.Count(DependabotError); got != 1 {
		t.Errorf("errors = %d", got)
	}
	if got := r.Count(DependabotWarning); got != 2 {
		t.Errorf("warnings = %d", got)
	}
	if got := r.Count(DependabotInfo); got != 1 {
		t.Errorf("infos = %d", got)
	}
	if !r.HasErrors() {
		t.Error("HasErrors should be true")
	}
	if r.OK() {
		t.Error("OK should be false")
	}
}

func TestDependabotReportOK(t *testing.T) {
	r := &DependabotReport{}
	if r.HasErrors() {
		t.Error("empty report has no errors")
	}
	if !r.OK() {
		t.Error("empty report is OK")
	}
}
