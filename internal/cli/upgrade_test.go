package cli

import (
	"errors"
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

func TestRefreshInstalledSkillSuccess(t *testing.T) {
	var gotExe string
	orig := refreshSkillCmd
	refreshSkillCmd = func(exe string) ([]byte, error) {
		gotExe = exe
		return []byte("refreshed installed skill: /home/u/.claude/skills/shuck/SKILL.md\n"), nil
	}
	t.Cleanup(func() { refreshSkillCmd = orig })

	var stdout, stderr strings.Builder
	refreshInstalledSkill("/usr/local/bin/shuck", &stdout, &stderr)

	if gotExe != "/usr/local/bin/shuck" {
		t.Errorf("exec'd %q, want the upgraded binary path", gotExe)
	}
	if !strings.Contains(stdout.String(), "refreshed installed skill") {
		t.Errorf("expected the refresh note on stdout, got %q", stdout.String())
	}
	if stderr.String() != "" {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}
}

func TestRefreshInstalledSkillFailureIsNonFatal(t *testing.T) {
	orig := refreshSkillCmd
	refreshSkillCmd = func(string) ([]byte, error) {
		return []byte("boom"), errors.New("exit status 1")
	}
	t.Cleanup(func() { refreshSkillCmd = orig })

	var stdout, stderr strings.Builder
	// Must not panic or otherwise propagate; the upgrade already succeeded.
	refreshInstalledSkill("/usr/local/bin/shuck", &stdout, &stderr)

	if !strings.Contains(stderr.String(), "could not refresh the installed skill") {
		t.Errorf("expected a warning on stderr, got %q", stderr.String())
	}
}

func TestUpgradeRejectsArgs(t *testing.T) {
	t.Setenv("GOBIN", t.TempDir())
	var stdout, stderr strings.Builder
	if code := runUpgrade([]string{"v1.2.3"}, &stdout, &stderr); code != 2 {
		t.Errorf("exit = %d, want 2 for stray arg", code)
	}
}
