package model

import "testing"

func TestReportHasFailures(t *testing.T) {
	tests := []struct {
		name string
		r    Report
		want bool
	}{
		{"empty", Report{}, false},
		{"failed job", Report{FailedJobs: []JobResult{{ID: 1}}}, true},
		{"other check", Report{OtherChecks: []OtherCheck{{Name: "lint"}}}, true},
		{"both", Report{FailedJobs: []JobResult{{ID: 1}}, OtherChecks: []OtherCheck{{Name: "lint"}}}, true},
		{"cancelled only", Report{CancelledJobs: []JobResult{{ID: 1}}}, false},
		{"running only", Report{RunningJobs: []RunningJob{{Name: "build"}}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.r.HasFailures(); got != tt.want {
				t.Errorf("HasFailures() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReportIsTerminal(t *testing.T) {
	tests := []struct {
		name string
		r    Report
		want bool
	}{
		{"empty", Report{}, true},
		{"no running jobs", Report{FailedJobs: []JobResult{{ID: 1}}}, true},
		{"a running job", Report{RunningJobs: []RunningJob{{Name: "build"}}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.r.IsTerminal(); got != tt.want {
				t.Errorf("IsTerminal() = %v, want %v", got, tt.want)
			}
		})
	}
}
