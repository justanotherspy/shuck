package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-github/v88/github"

	"github.com/justanotherspy/shuck/internal/image"
	"github.com/justanotherspy/shuck/internal/model"
)

// stubImageLister records calls and returns canned data so the image command can
// be tested without hitting the network.
type stubImageLister struct {
	packages     []string
	versions     map[string][]model.ImageVersion // image name -> versions (Packages API)
	registryTags []string                        // anonymous registry tags
	registryDig  string                          // digest returned for any tag
	pkgCalls     int
	versionCalls int
	regTagCalls  int
	regDigCalls  int
	sha          string
	shaErr       error
	shaCalls     int
	packagesErr  error
	versionsErr  error
}

func (s *stubImageLister) ListContainerPackages(_ context.Context, _ string) ([]string, error) {
	s.pkgCalls++
	return s.packages, s.packagesErr
}

func (s *stubImageLister) ListImageVersions(_ context.Context, _, name string) ([]model.ImageVersion, error) {
	s.versionCalls++
	return s.versions[name], s.versionsErr
}

func (s *stubImageLister) RegistryTags(_ context.Context, _, _ string) ([]string, error) {
	s.regTagCalls++
	return s.registryTags, nil
}

func (s *stubImageLister) RegistryDigest(_ context.Context, _, _, _ string) (string, error) {
	s.regDigCalls++
	return s.registryDig, nil
}

func (s *stubImageLister) DefaultBranchSHA(_ context.Context, _, _ string) (string, error) {
	s.shaCalls++
	return s.sha, s.shaErr
}

func withStubImageLister(t *testing.T, s *stubImageLister) {
	t.Helper()
	t.Setenv("SHUCK_HOME", t.TempDir())
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	prev := NewImageLister
	NewImageLister = func(string) ImageLister { return s }
	t.Cleanup(func() { NewImageLister = prev })
}

func TestRunImageResolveAuthed(t *testing.T) {
	s := &stubImageLister{
		versions: map[string][]model.ImageVersion{
			"api": {
				{Tags: []string{"v1.2.0", "latest"}, Digest: "sha256:aaa"},
				{Tags: []string{"v1.1.0"}, Digest: "sha256:bbb"},
			},
		},
	}
	withStubImageLister(t, s)
	t.Setenv("GITHUB_TOKEN", "x") // authed → Packages API path

	var out, errOut bytes.Buffer
	code := runImage([]string{"ghcr.io/acme/api"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "sha256:aaa") || !strings.Contains(out.String(), "v1.2.0") {
		t.Errorf("output = %q", out.String())
	}
	if s.versionCalls == 0 {
		t.Errorf("expected ListImageVersions to be called")
	}
	if s.regTagCalls != 0 {
		t.Errorf("registry path should not be used when authed")
	}
}

func TestRunImageResolveAnonymous(t *testing.T) {
	s := &stubImageLister{
		registryTags: []string{"v2.0.0", "v1.0.0", "latest"},
		registryDig:  "sha256:zzz",
	}
	withStubImageLister(t, s) // no token → anonymous registry path

	var out, errOut bytes.Buffer
	code := runImage([]string{"ghcr.io/acme/api", "--json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	var doc image.Document
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if doc.Digest != "sha256:zzz" || doc.Tag != "v2.0.0" {
		t.Errorf("doc = %+v", doc)
	}
	if s.regTagCalls == 0 || s.regDigCalls == 0 {
		t.Errorf("expected registry tag+digest calls, got %d/%d", s.regTagCalls, s.regDigCalls)
	}
}

// forbiddenErr mimics the Packages API rejecting a token that lacks the
// read:packages scope (or any fine-grained token, which the API never accepts).
func forbiddenErr() error {
	return &github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusForbidden}}
}

func TestRunImageResolveScopelessTokenFallsBackToRegistry(t *testing.T) {
	// A token without read:packages 403s on the Packages API; a public image
	// must still resolve via the anonymous registry path.
	s := &stubImageLister{
		packagesErr:  forbiddenErr(),
		registryTags: []string{"v1.0.0", "edge"},
		registryDig:  "sha256:fff",
	}
	withStubImageLister(t, s)
	t.Setenv("GITHUB_TOKEN", "x") // authed, but the token cannot read packages

	var out, errOut bytes.Buffer
	code := runImage([]string{"ghcr.io/acme/api"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "sha256:fff") || !strings.Contains(out.String(), "v1.0.0") {
		t.Errorf("output = %q", out.String())
	}
	if s.regTagCalls == 0 || s.regDigCalls == 0 {
		t.Errorf("expected anonymous registry fallback, got tag/digest calls %d/%d", s.regTagCalls, s.regDigCalls)
	}
}

func TestRunImageResolveVersionsAuthErrFallsBackToRegistry(t *testing.T) {
	// The owner listing succeeds but the per-image versions read is forbidden
	// (e.g. a package the token cannot see); fall back to the registry.
	s := &stubImageLister{
		packages:     []string{"other"},
		versionsErr:  forbiddenErr(),
		registryTags: []string{"v3.0.0"},
		registryDig:  "sha256:ddd",
	}
	withStubImageLister(t, s)
	t.Setenv("GITHUB_TOKEN", "x")

	var out, errOut bytes.Buffer
	code := runImage([]string{"ghcr.io/acme/api"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "sha256:ddd") {
		t.Errorf("output = %q", out.String())
	}
	if s.regTagCalls == 0 || s.regDigCalls == 0 {
		t.Errorf("expected anonymous registry fallback, got tag/digest calls %d/%d", s.regTagCalls, s.regDigCalls)
	}
}

func TestRunImageResolveNonAuthErrIsFatal(t *testing.T) {
	// A non-auth Packages API failure (e.g. 500) is an operational error, not a
	// cue to silently fall back to the registry.
	s := &stubImageLister{
		packagesErr: &github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusInternalServerError}},
	}
	withStubImageLister(t, s)
	t.Setenv("GITHUB_TOKEN", "x")

	var out, errOut bytes.Buffer
	code := runImage([]string{"ghcr.io/acme/api"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit=%d, want 2 (stderr=%s)", code, errOut.String())
	}
	if s.regTagCalls != 0 {
		t.Errorf("registry should not be consulted on a non-auth error")
	}
}

func TestRunImageListScopelessTokenGuidance(t *testing.T) {
	// Listing has no anonymous fallback; a 403 must explain that the Packages
	// API needs a classic read:packages token (fine-grained tokens never work).
	s := &stubImageLister{packagesErr: forbiddenErr()}
	withStubImageLister(t, s)
	t.Setenv("GITHUB_TOKEN", "x")

	var out, errOut bytes.Buffer
	code := runImage([]string{"acme"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit=%d, want 2", code)
	}
	errText := errOut.String()
	if !strings.Contains(errText, "classic") || !strings.Contains(errText, "read:packages") || !strings.Contains(errText, "fine-grained") {
		t.Errorf("stderr = %q, want classic/read:packages/fine-grained guidance", errText)
	}
}

func TestRunImageListAuthed(t *testing.T) {
	s := &stubImageLister{
		packages: []string{"api", "worker"},
		versions: map[string][]model.ImageVersion{
			"api":    {{Tags: []string{"v1.0.0"}, Digest: "sha256:a"}},
			"worker": {{Tags: []string{"v2.0.0"}, Digest: "sha256:w"}},
		},
	}
	withStubImageLister(t, s)
	t.Setenv("GITHUB_TOKEN", "x")

	var out, errOut bytes.Buffer
	code := runImage([]string{"acme", "--json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	var doc image.ListDocument
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if doc.Owner != "acme" || len(doc.Images) != 2 {
		t.Fatalf("doc = %+v", doc)
	}
	if s.pkgCalls != 1 {
		t.Errorf("ListContainerPackages calls = %d, want 1", s.pkgCalls)
	}
}

func TestRunImageListRequiresToken(t *testing.T) {
	s := &stubImageLister{}
	withStubImageLister(t, s) // no token

	var out, errOut bytes.Buffer
	code := runImage([]string{"acme"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit=%d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "read:packages") {
		t.Errorf("stderr = %q, want read:packages guidance", errOut.String())
	}
	if s.pkgCalls != 0 {
		t.Errorf("should not call the API without a token")
	}
}

func TestImagesCacheHitAndRefresh(t *testing.T) {
	s := &stubImageLister{
		packages: []string{"api"},
		versions: map[string][]model.ImageVersion{"api": {{Tags: []string{"v1.0.0"}, Digest: "sha256:a"}}},
		sha:      "samesha",
	}
	withStubImageLister(t, s)

	ctx := context.Background()
	opts := ImageOptions{Token: "x"}

	// First call populates the cache.
	if _, err := Images(ctx, "acme", opts); err != nil {
		t.Fatalf("Images #1: %v", err)
	}
	firstPkgCalls := s.pkgCalls

	// Second call within the TTL with an unchanged default-branch SHA is served
	// from cache (cheap SHA check, no re-enumeration).
	if _, err := Images(ctx, "acme", opts); err != nil {
		t.Fatalf("Images #2: %v", err)
	}
	if s.pkgCalls != firstPkgCalls {
		t.Errorf("cache not reused: pkgCalls went %d -> %d", firstPkgCalls, s.pkgCalls)
	}

	// --refresh forces a re-fetch.
	if _, err := Images(ctx, "acme", ImageOptions{Token: "x", Refresh: true}); err != nil {
		t.Fatalf("Images refresh: %v", err)
	}
	if s.pkgCalls <= firstPkgCalls {
		t.Errorf("refresh did not re-fetch: pkgCalls = %d", s.pkgCalls)
	}
}
