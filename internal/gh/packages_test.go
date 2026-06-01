package gh

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v88/github"
)

func TestIsNotFound(t *testing.T) {
	if !isNotFound(&github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}}) {
		t.Error("404 should be reported as not found")
	}
	if isNotFound(&github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusForbidden}}) {
		t.Error("403 is not a not-found")
	}
	if isNotFound(errors.New("plain")) {
		t.Error("plain error is not a not-found")
	}
	if isNotFound(nil) {
		t.Error("nil is not a not-found")
	}
}

func TestContainerTags(t *testing.T) {
	// Valid container metadata.
	v := &github.PackageVersion{Metadata: []byte(`{"package_type":"container","container":{"tags":["v1","latest"]}}`)}
	if got := containerTags(v); len(got) != 2 || got[0] != "v1" {
		t.Errorf("valid tags = %v", got)
	}
	// Empty metadata.
	if got := containerTags(&github.PackageVersion{}); got != nil {
		t.Errorf("empty metadata = %v, want nil", got)
	}
	// Invalid JSON.
	if got := containerTags(&github.PackageVersion{Metadata: []byte(`{not json`)}); got != nil {
		t.Errorf("invalid JSON = %v, want nil", got)
	}
	// Non-container package (no container field).
	if got := containerTags(&github.PackageVersion{Metadata: []byte(`{"package_type":"npm"}`)}); got != nil {
		t.Errorf("non-container = %v, want nil", got)
	}
}

func TestListContainerPackagesOrg(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orgs/acme/packages" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[{"name":"web"},{"name":"api"},{"name":""}]`))
	}))
	defer srv.Close()

	names, err := testClient(t, srv).ListContainerPackages(context.Background(), "acme")
	if err != nil {
		t.Fatalf("ListContainerPackages: %v", err)
	}
	if len(names) != 2 || names[0] != "web" || names[1] != "api" {
		t.Errorf("names = %v", names)
	}
}

func TestListContainerPackagesUserFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs/joe/packages":
			http.Error(w, `{"message":"not an org"}`, http.StatusNotFound)
		case "/users/joe/packages":
			_, _ = w.Write([]byte(`[{"name":"tool"}]`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	names, err := testClient(t, srv).ListContainerPackages(context.Background(), "joe")
	if err != nil {
		t.Fatalf("ListContainerPackages: %v", err)
	}
	if len(names) != 1 || names[0] != "tool" {
		t.Errorf("names = %v", names)
	}
}

func TestListContainerPackagesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).ListContainerPackages(context.Background(), "acme"); err == nil {
		t.Fatal("expected error on 403")
	}
}

func TestListContainerPackagesUserError(t *testing.T) {
	// Org 404 falls through to the user endpoint, which itself errors.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/orgs/joe/packages" {
			http.Error(w, `{"message":"nf"}`, http.StatusNotFound)
			return
		}
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).ListContainerPackages(context.Background(), "joe"); err == nil {
		t.Fatal("expected error from user endpoint")
	}
}

func TestListImageVersionsOrg(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orgs/acme/packages/container/web/versions" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[
			{"name":"sha256:aaa","updated_at":"2026-01-01T00:00:00Z","metadata":{"package_type":"container","container":{"tags":["v1"]}}},
			{"name":"","metadata":{}}
		]`))
	}))
	defer srv.Close()

	vers, err := testClient(t, srv).ListImageVersions(context.Background(), "acme", "web")
	if err != nil {
		t.Fatalf("ListImageVersions: %v", err)
	}
	if len(vers) != 1 || vers[0].Digest != "sha256:aaa" || len(vers[0].Tags) != 1 {
		t.Errorf("versions = %+v", vers)
	}
}

func TestListImageVersionsUserFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs/joe/packages/container/tool/versions":
			http.Error(w, `{"message":"not an org"}`, http.StatusNotFound)
		case "/users/joe/packages/container/tool/versions":
			_, _ = w.Write([]byte(`[{"name":"sha256:bbb","metadata":{"container":{"tags":["latest"]}}}]`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	vers, err := testClient(t, srv).ListImageVersions(context.Background(), "joe", "tool")
	if err != nil {
		t.Fatalf("ListImageVersions: %v", err)
	}
	if len(vers) != 1 || vers[0].Digest != "sha256:bbb" {
		t.Errorf("versions = %+v", vers)
	}
}

func TestListImageVersionsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).ListImageVersions(context.Background(), "acme", "web"); err == nil {
		t.Fatal("expected error")
	}
}

func TestListImageVersionsUserError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/orgs/joe/packages/container/tool/versions" {
			http.Error(w, `{"message":"nf"}`, http.StatusNotFound)
			return
		}
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).ListImageVersions(context.Background(), "joe", "tool"); err == nil {
		t.Fatal("expected error from user endpoint")
	}
}
