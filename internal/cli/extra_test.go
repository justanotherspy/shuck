package cli

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
	"github.com/justanotherspy/shuck/internal/release"
	"github.com/justanotherspy/shuck/internal/target"
)

func TestFindPackage(t *testing.T) {
	pkgs := []model.ImagePackage{
		{Name: "api", Versions: []model.ImageVersion{{Digest: "sha256:a"}}},
		{Name: "web", Versions: []model.ImageVersion{{Digest: "sha256:b"}}},
	}
	if v, ok := findPackage(pkgs, "web"); !ok || len(v) != 1 || v[0].Digest != "sha256:b" {
		t.Errorf("findPackage(web) = %v, %v", v, ok)
	}
	if _, ok := findPackage(pkgs, "missing"); ok {
		t.Errorf("findPackage(missing) should report not found")
	}
}

func TestResolveOwner(t *testing.T) {
	if o, err := resolveOwner([]string{"acme"}); err != nil || o != "acme" {
		t.Errorf("bare owner = %q, %v, want acme", o, err)
	}
	if o, err := resolveOwner([]string{"acme/widget"}); err != nil || o != "acme" {
		t.Errorf("owner/repo = %q, %v, want acme", o, err)
	}
	if o, err := resolveOwner([]string{"https://github.com/acme/widget"}); err != nil || o != "acme" {
		t.Errorf("url = %q, %v, want acme", o, err)
	}
}

func TestLooksLikeImageRef(t *testing.T) {
	cases := map[string]bool{
		"ghcr.io/acme/api":         true,
		"ghcr.io/acme/api:v1":      true,
		"https://ghcr.io/acme/api": true,
		"acme":                     false,
		"acme/widget":              false,
		"https://github.com/acme":  false,
		"docker.io/acme/api":       false,
	}
	for in, want := range cases {
		if got := looksLikeImageRef(in); got != want {
			t.Errorf("looksLikeImageRef(%q) = %v, want %v", in, got, want)
		}
	}
}

// stubLatestErr points newReleaseClient at a server that always 500s, so the
// latest-release lookup fails.
func stubLatestErr(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
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

func TestHasOpenAlerts(t *testing.T) {
	if hasOpenAlerts(&model.SecurityReport{}) {
		t.Error("empty report has no open alerts")
	}
	if !hasOpenAlerts(&model.SecurityReport{
		CodeScanningAlerts: []model.CodeScanningAlert{{State: "open"}},
	}) {
		t.Error("open code-scanning alert should count")
	}
	if !hasOpenAlerts(&model.SecurityReport{
		DependabotAlerts: []model.DependabotAlert{{State: "open"}},
	}) {
		t.Error("open dependabot alert should count")
	}
	if !hasOpenAlerts(&model.SecurityReport{
		SecretScanningAlerts: []model.SecretScanningAlert{{State: "open"}},
	}) {
		t.Error("open secret-scanning alert should count")
	}
	if hasOpenAlerts(&model.SecurityReport{
		CodeScanningAlerts:   []model.CodeScanningAlert{{State: "dismissed"}},
		DependabotAlerts:     []model.DependabotAlert{{State: "fixed"}},
		SecretScanningAlerts: []model.SecretScanningAlert{{State: "resolved"}},
	}) {
		t.Error("non-open alerts should not count as open")
	}
}

// TestRunVersionCheckError exercises the --check error branch when the release
// lookup fails.
func TestRunVersionCheckError(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	withVersion(t, "v1.0.0")
	stubLatestErr(t)

	var out, errb strings.Builder
	if code := runVersion([]string{"--check"}, &out, &errb); code != 2 {
		t.Fatalf("exit = %d, want 2 on release error", code)
	}
	if !strings.Contains(errb.String(), "could not check for updates") {
		t.Errorf("expected error note, got %q", errb.String())
	}
}

// TestRunDefaultWatch drives the bare `shuck --watch` path end to end against a
// terminal stubbed report, proving runDefault wires up watch mode.
func TestRunDefaultWatch(t *testing.T) {
	s := ciStub()
	withStubInspect(t, s)
	withStubSecurity(t, okStub())
	t.Setenv("GITHUB_TOKEN", "test-token")

	var out, errb strings.Builder
	code := Run([]string{"--watch", "o/r", "42"}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "build") {
		t.Errorf("watch should print the terminal report:\n%s", out.String())
	}
}

// TestRunDefaultInspectError proves runDefault maps an inspection error to
// exit 2 and a "shuck:" note.
func TestRunDefaultInspectError(t *testing.T) {
	s := ciStub()
	s.prErr = errors.New("boom")
	withStubInspect(t, s)

	var out, errb strings.Builder
	code := Run([]string{"o/r", "42"}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "shuck:") {
		t.Errorf("expected a shuck: error on stderr, got %q", errb.String())
	}
}

// TestRunDefaultVersionFlag proves `shuck --version` short-circuits before any
// network or target resolution.
func TestRunDefaultVersionFlag(t *testing.T) {
	withVersion(t, "v3.2.1")
	var out, errb strings.Builder
	if code := Run([]string{"--version"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "v3.2.1") {
		t.Errorf("output = %q, want the injected version", out.String())
	}
}

// TestRunDefaultSecurityErrorDegrades proves the combined path keeps reporting
// CI when the security half fails (emitAll's security-error branch).
func TestRunDefaultSecurityErrorDegrades(t *testing.T) {
	s := ciStub()
	withStubInspect(t, s)
	withStubSecurity(t, &stubSecurity{
		csSrc:     model.SecuritySource{Status: model.StatusError, Message: "x"},
		secretSrc: model.SecuritySource{Status: model.StatusError, Message: "y"},
		depSrc:    model.SecuritySource{Status: model.StatusError, Message: "z"},
	})
	t.Setenv("GITHUB_TOKEN", "test-token")

	var out, errb strings.Builder
	code := Run([]string{"o/r", "42"}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (CI verdict); stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "security alerts: unavailable") {
		t.Errorf("expected the degraded security note:\n%s", out.String())
	}
}

// TestEmitInvalidPatternError reaches the buildExtractOptions error branch
// through inspectWith (an invalid --pattern).
func TestInspectWithBadPattern(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	tgt := target.Target{Owner: "o", Repo: "r", Number: 1}
	if _, err := inspectWith(t.Context(), tgt, options{
		pattern: "(", reviewCommentLimit: 5, context: 10, shortThreshold: 100, tail: 100,
	}); err == nil {
		t.Error("invalid --pattern should error before any network call")
	}
}

// TestInspectWithNegativeMaxCommandLines covers that validation branch.
func TestInspectWithNegativeMaxCommandLines(t *testing.T) {
	tgt := target.Target{Owner: "o", Repo: "r", Number: 1}
	if _, err := inspectWith(t.Context(), tgt, options{
		maxCommandLines: -1, reviewCommentLimit: 5, context: 10, shortThreshold: 100, tail: 100,
	}); err == nil {
		t.Error("negative --max-command-lines should error")
	}
}
