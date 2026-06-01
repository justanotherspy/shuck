package gh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNextLink(t *testing.T) {
	tests := []struct {
		name string
		link string
		want string
	}{
		{"absolute next", `<https://ghcr.io/v2/o/n/tags/list?last=z>; rel="next"`, "https://ghcr.io/v2/o/n/tags/list?last=z"},
		{"relative next", `</v2/o/n/tags/list?last=z>; rel="next"`, "https://ghcr.io/v2/o/n/tags/list?last=z"},
		{"no next rel", `<https://x>; rel="prev"`, ""},
		{"empty", "", ""},
		{"malformed no semicolon", `<https://x>`, ""},
		{"empty url", `<>; rel="next"`, ""},
		{"next among many", `<https://a>; rel="prev", </v2/n>; rel="next"`, "https://ghcr.io/v2/n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextLink(tc.link, "https://ghcr.io"); got != tc.want {
				t.Errorf("nextLink(%q) = %q, want %q", tc.link, got, tc.want)
			}
		})
	}
}

func TestRegistryTags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_, _ = w.Write([]byte(`{"token":"anon-token"}`))
		case "/v2/o/n/tags/list":
			if got := r.Header.Get("Authorization"); got != "Bearer anon-token" {
				t.Errorf("missing bearer token, got %q", got)
			}
			if r.URL.Query().Get("last") == "" {
				// Page one points at page two via a relative Link header.
				w.Header().Set("Link", `</v2/o/n/tags/list?last=v1>; rel="next"`)
				_, _ = w.Write([]byte(`{"tags":["v1"]}`))
				return
			}
			_, _ = w.Write([]byte(`{"tags":["v2"]}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	tags, err := testClient(t, srv).RegistryTags(context.Background(), "o", "n")
	if err != nil {
		t.Fatalf("RegistryTags: %v", err)
	}
	if len(tags) != 2 || tags[0] != "v1" || tags[1] != "v2" {
		t.Errorf("tags = %v", tags)
	}
}

func TestRegistryTagsTokenError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no token", http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).RegistryTags(context.Background(), "o", "n"); err == nil {
		t.Fatal("expected error when the token exchange fails")
	}
}

func TestRegistryTagsListError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"token":"t"}`))
			return
		}
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).RegistryTags(context.Background(), "o", "n"); err == nil {
		t.Fatal("expected error on non-200 tags listing")
	}
}

func TestRegistryDigest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			// The PAT-carrying client exchanges via Basic auth.
			if _, _, ok := r.BasicAuth(); !ok {
				t.Error("expected Basic auth on token exchange with a PAT")
			}
			_, _ = w.Write([]byte(`{"token":"t"}`))
		case "/v2/o/n/manifests/v1":
			if !strings.Contains(r.Header.Get("Accept"), "manifest") {
				t.Errorf("missing manifest Accept header: %q", r.Header.Get("Accept"))
			}
			w.Header().Set("Docker-Content-Digest", "sha256:deadbeef")
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	digest, err := testClient(t, srv).RegistryDigest(context.Background(), "o", "n", "v1")
	if err != nil {
		t.Fatalf("RegistryDigest: %v", err)
	}
	if digest != "sha256:deadbeef" {
		t.Errorf("digest = %q", digest)
	}
}

func TestRegistryDigestNoHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"token":"t"}`))
			return
		}
		// 200 but no Docker-Content-Digest header.
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).RegistryDigest(context.Background(), "o", "n", "v1"); err == nil {
		t.Fatal("expected error when no digest header is present")
	}
}

func TestRegistryDigestManifestError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"token":"t"}`))
			return
		}
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).RegistryDigest(context.Background(), "o", "n", "v1"); err == nil {
		t.Fatal("expected error on non-200 manifest")
	}
}

func TestGhcrTokenBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{not json`))
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).ghcrToken(context.Background(), "o", "n"); err == nil {
		t.Fatal("expected decode error")
	}
}
