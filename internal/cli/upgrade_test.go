package cli

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/release"
)

func TestUpgradeManagedExternally(t *testing.T) {
	// Make the running test binary look go-installed by pointing GOBIN at its
	// directory. upgrade must refuse and not touch the network.
	exe, err := release.ExecutablePath()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOBIN", filepath.Dir(exe))

	hit := false
	orig := newReleaseClient
	newReleaseClient = func(token string) *release.Client {
		hit = true
		return release.New(token)
	}
	t.Cleanup(func() { newReleaseClient = orig })

	var stdout, stderr strings.Builder
	if code := runUpgrade(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, stderr.String())
	}
	if hit {
		t.Error("upgrade reached the network despite a go-installed binary")
	}
	if !strings.Contains(stdout.String(), "go install github.com/justanotherspy/shuck@latest") {
		t.Errorf("expected go install guidance, got %q", stdout.String())
	}
}

func TestUpgradeAlreadyUpToDate(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	// Keep the test binary out of any go bin dir so it is treated as upgradable.
	t.Setenv("GOBIN", t.TempDir())
	withVersion(t, "v9.9.9")

	// A download attempt would 404 against this api-only server; "already up to
	// date" must short-circuit before any download.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9"}`))
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

	var stdout, stderr strings.Builder
	if code := runUpgrade(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "already up to date") {
		t.Errorf("expected already-up-to-date, got %q", stdout.String())
	}
}

func TestUpgradeRejectsArgs(t *testing.T) {
	t.Setenv("GOBIN", t.TempDir())
	var stdout, stderr strings.Builder
	if code := runUpgrade([]string{"v1.2.3"}, &stdout, &stderr); code != 2 {
		t.Errorf("exit = %d, want 2 for stray arg", code)
	}
}
