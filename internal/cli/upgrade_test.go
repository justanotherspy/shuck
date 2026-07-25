package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/monitor"
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

// fakeReleaseServer serves one downloadable release of tag: the "latest"
// endpoint, a tar.gz whose single file is the shuck binary, and the
// checksums.txt that archive has to verify against.
func fakeReleaseServer(t *testing.T, tag string, binary []byte) *httptest.Server {
	t.Helper()

	var tgz bytes.Buffer
	gz := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "shuck", Mode: 0o755, Size: int64(len(binary))}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(binary); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	archive := fmt.Sprintf("shuck_%s_%s_%s.tar.gz", strings.TrimPrefix(tag, "v"), runtime.GOOS, runtime.GOARCH)
	sums := fmt.Sprintf("%x  %s\n", sha256.Sum256(tgz.Bytes()), archive)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			fmt.Fprintf(w, `{"tag_name":%q}`, tag)
		case strings.HasSuffix(r.URL.Path, "/"+archive):
			_, _ = w.Write(tgz.Bytes())
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			_, _ = w.Write([]byte(sums))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestUpgradeStopsTheStaleMonitor covers what a successful upgrade leaves
// behind. The monitor daemon and the clients that talk to it only promise to
// understand each other within one build (internal/monitor/protocol.go), so a
// daemon still running the replaced binary is exactly the mismatch that promise
// assumes away — and nothing else ever stops it, because the daemon outlives
// every session that asked for it. Stopping it here is enough: the next hook
// finds none running and spawns one from the binary just installed.
func TestUpgradeStopsTheStaleMonitor(t *testing.T) {
	d := startFakeDaemon(t, func(monitor.Request) monitor.Response {
		return monitor.Response{OK: true}
	})
	// Keep the test binary out of any go bin dir so it counts as upgradable.
	t.Setenv("GOBIN", t.TempDir())
	withVersion(t, "v0.0.1")

	srv := fakeReleaseServer(t, "v9.9.9", []byte("the new shuck"))
	origClient := newReleaseClient
	newReleaseClient = func(token string) *release.Client {
		c := release.New(token)
		c.APIBase = srv.URL
		c.DownloadBase = srv.URL
		return c
	}
	t.Cleanup(func() { newReleaseClient = origClient })

	// The test binary must not overwrite itself, and the refresh must not
	// exec it.
	var installed []byte
	origReplace := replaceRunning
	replaceRunning = func(_ string, bin []byte) error { installed = bin; return nil }
	t.Cleanup(func() { replaceRunning = origReplace })
	origRefresh := refreshSkillCmd
	refreshSkillCmd = func(string) ([]byte, error) { return nil, nil }
	t.Cleanup(func() { refreshSkillCmd = origRefresh })

	var stdout, stderr strings.Builder
	if code := runUpgrade(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, stderr.String())
	}
	if string(installed) != "the new shuck" {
		t.Fatalf("installed %q, want the downloaded binary — the upgrade did not get far enough to prove anything", installed)
	}
	stopped := false
	for _, req := range d.seen() {
		if req.Op == monitor.OpStop {
			stopped = true
		}
	}
	if !stopped {
		t.Errorf("the old daemon is still running after the upgrade; requests seen = %+v", d.seen())
	}
	if !strings.Contains(stdout.String(), "monitor") {
		t.Errorf("stdout = %q, want the stopped monitor mentioned so nobody wonders where it went", stdout.String())
	}
}

// TestUpgradeSurvivesAMonitorThatWillNotStop keeps the stop best-effort. The
// binary is already replaced by then, so a daemon that refuses must not turn a
// finished upgrade into a failed command.
func TestUpgradeSurvivesAMonitorThatWillNotStop(t *testing.T) {
	startFakeDaemon(t, func(monitor.Request) monitor.Response {
		return monitor.Response{Error: "the monitor is wedged"}
	})
	t.Setenv("GOBIN", t.TempDir())
	withVersion(t, "v0.0.1")

	srv := fakeReleaseServer(t, "v9.9.9", []byte("the new shuck"))
	origClient := newReleaseClient
	newReleaseClient = func(token string) *release.Client {
		c := release.New(token)
		c.APIBase = srv.URL
		c.DownloadBase = srv.URL
		return c
	}
	t.Cleanup(func() { newReleaseClient = origClient })

	origReplace := replaceRunning
	replaceRunning = func(string, []byte) error { return nil }
	t.Cleanup(func() { replaceRunning = origReplace })
	origRefresh := refreshSkillCmd
	refreshSkillCmd = func(string) ([]byte, error) { return nil, nil }
	t.Cleanup(func() { refreshSkillCmd = origRefresh })

	var stdout, stderr strings.Builder
	if code := runUpgrade(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0 — the binary is already in place; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "upgraded shuck") {
		t.Errorf("stdout = %q, want the upgrade still reported as done", stdout.String())
	}
}
