package model

import "testing"

func TestConclusionPredicates(t *testing.T) {
	tests := []struct {
		conclusion string
		failure    bool
		cancelled  bool
		drillable  bool
	}{
		{"failure", true, false, true},
		{"timed_out", true, false, true},
		{"startup_failure", true, false, true},
		{"action_required", true, false, true},
		{"cancelled", false, true, true},
		{"success", false, false, false},
		{"skipped", false, false, false},
		{"neutral", false, false, false},
		{"", false, false, false},
	}
	for _, tt := range tests {
		if got := IsFailureConclusion(tt.conclusion); got != tt.failure {
			t.Errorf("IsFailureConclusion(%q) = %v, want %v", tt.conclusion, got, tt.failure)
		}
		if got := IsCancelledConclusion(tt.conclusion); got != tt.cancelled {
			t.Errorf("IsCancelledConclusion(%q) = %v, want %v", tt.conclusion, got, tt.cancelled)
		}
		if got := IsDrillableConclusion(tt.conclusion); got != tt.drillable {
			t.Errorf("IsDrillableConclusion(%q) = %v, want %v", tt.conclusion, got, tt.drillable)
		}
	}
}
