package logs

import "testing"

// TestMonitorSmokeDeliberateFailure is a deliberate CI failure used to exercise
// the shuck monitor's delivery path end to end. It is not a real test and must
// be deleted before this branch is merged.
func TestMonitorSmokeDeliberateFailure(t *testing.T) {
	got := Parse("##[group]Run make test\nboom\n##[endgroup]\n")
	if len(got) != 99 {
		t.Fatalf("monitor smoke test: deliberate failure, sections = %d, want 99", len(got))
	}
}
