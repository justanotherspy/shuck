package cli

import (
	"os"
	"testing"
)

// TestMain takes the `gh auth token` fallback out of the package's tests.
//
// A developer's machine is usually logged into gh and a CI runner is not, so
// leaving the real one in place would make every "no token" assertion depend on
// whose machine ran it — and the failure is not a polite one: a test that
// expects `shuck monitor run` to refuse to start would instead start a real
// daemon with a real credential and poll GitHub. Tests that are about the
// fallback install their own stub with stubGhAuthToken.
func TestMain(m *testing.M) {
	ghAuthToken = func() string { return "" }
	os.Exit(m.Run())
}

// stubGhAuthToken makes `gh auth token` answer with token for one test.
func stubGhAuthToken(t *testing.T, token string) {
	t.Helper()
	original := ghAuthToken
	ghAuthToken = func() string { return token }
	t.Cleanup(func() { ghAuthToken = original })
}
