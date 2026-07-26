package semver

import "testing"

// TestMonitorDemoParsePatch exercises Parse on a plain MAJOR.MINOR.PATCH tag.
// It exists to give the background monitor a real red build to report.
func TestMonitorDemoParsePatch(t *testing.T) {
	v, ok := Parse("v1.2.3")
	if !ok {
		t.Fatalf("Parse(%q) failed to parse", "v1.2.3")
	}
	if got, want := v.Major, 1; got != want {
		t.Errorf("Major = %d, want %d", got, want)
	}
	if got, want := v.Minor, 2; got != want {
		t.Errorf("Minor = %d, want %d", got, want)
	}
	if got, want := v.Patch, 4; got != want {
		t.Errorf("Patch = %d, want %d", got, want)
	}
}
