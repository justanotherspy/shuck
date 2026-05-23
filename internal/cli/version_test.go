package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/justanotherspy/shuck/internal/release"
)

// withVersion pins the package version var for a test and restores it after.
func withVersion(t *testing.T, v string) {
	t.Helper()
	orig := version
	version = v
	t.Cleanup(func() { version = orig })
}

// stubLatest points newReleaseClient at a local server returning tag as the
// latest release, restoring the real constructor after the test.
func stubLatest(t *testing.T, tag string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"tag_name":%q}`, tag)
	}))
	t.Cleanup(srv.Close)

	orig := newReleaseClient
	newReleaseClient = func(token string) *release.Client {
		c := release.New(token)
		c.APIBase = srv.URL
		c.DownloadBase = srv.URL
		return c
	}
	t.Cleanup(func() { newReleaseClient = orig })
}

func TestRunVersionPrintsCurrent(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	withVersion(t, "v1.0.0")

	var stdout, stderr strings.Builder
	if code := runVersion(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, stderr.String())
	}
	if got := stdout.String(); got != "shuck v1.0.0\n" {
		t.Errorf("output = %q, want %q", got, "shuck v1.0.0\n")
	}
}

func TestRunVersionHintFromFreshCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SHUCK_HOME", home)
	withVersion(t, "v1.0.0")
	writeVersionCheck(t, home, versionCheck{Latest: "v1.2.0", CheckedAt: time.Now()})

	var stdout, stderr strings.Builder
	runVersion(nil, &stdout, &stderr)
	if !strings.Contains(stdout.String(), "update available: v1.2.0") {
		t.Errorf("expected update hint, got %q", stdout.String())
	}
}

func TestRunVersionNoHintWhenStale(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SHUCK_HOME", home)
	withVersion(t, "v1.0.0")
	writeVersionCheck(t, home, versionCheck{Latest: "v1.2.0", CheckedAt: time.Now().Add(-versionCheckTTL - time.Hour)})

	var stdout, stderr strings.Builder
	runVersion(nil, &stdout, &stderr)
	if strings.Contains(stdout.String(), "update available") {
		t.Errorf("stale cache should not produce a hint, got %q", stdout.String())
	}
}

func TestRunVersionCheckFindsUpdate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SHUCK_HOME", home)
	withVersion(t, "v1.0.0")
	stubLatest(t, "v1.5.0")

	var stdout, stderr strings.Builder
	if code := runVersion([]string{"--check"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "update available: v1.5.0") {
		t.Errorf("expected update available, got %q", stdout.String())
	}
	// --check must persist the result so a later bare `version` can hint offline.
	if vc, ok := loadVersionCheck(); !ok || vc.Latest != "v1.5.0" {
		t.Errorf("version-check not cached: %+v ok=%v", vc, ok)
	}
}

func TestRunVersionCheckUpToDate(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	withVersion(t, "v1.5.0")
	stubLatest(t, "v1.5.0")

	var stdout, stderr strings.Builder
	runVersion([]string{"--check"}, &stdout, &stderr)
	if !strings.Contains(stdout.String(), "up to date") {
		t.Errorf("expected up to date, got %q", stdout.String())
	}
}

func writeVersionCheck(t *testing.T, home string, vc versionCheck) {
	t.Helper()
	data, err := json.Marshal(vc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, versionCheckFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
}
