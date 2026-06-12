package gh

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunArtifacts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/actions/runs/123/artifacts" {
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		// Two pages, to exercise pagination.
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`{"total_count":3,"artifacts":[
				{"id":3,"name":"stale","size_in_bytes":7,"expired":true}]}`))
			return
		}
		w.Header().Set("Link", `<`+linkBase(r)+`?page=2>; rel="next"`)
		_, _ = w.Write([]byte(`{"total_count":3,"artifacts":[
			{"id":1,"name":"coverage","size_in_bytes":2048,"expired":false,
			 "created_at":"2026-06-01T10:00:00Z","expires_at":"2026-09-01T10:00:00Z"},
			{"id":2,"name":"bin","size_in_bytes":5}]}`))
	}))
	defer srv.Close()

	arts, err := testClient(t, srv).RunArtifacts(context.Background(), "o", "r", 123)
	if err != nil {
		t.Fatalf("RunArtifacts: %v", err)
	}
	if len(arts) != 3 {
		t.Fatalf("artifacts = %d, want 3 (paginated)", len(arts))
	}
	a := arts[0]
	if a.ID != 1 || a.RunID != 123 || a.Name != "coverage" || a.SizeBytes != 2048 || a.Expired {
		t.Errorf("artifact[0] = %+v", a)
	}
	if a.CreatedAt.IsZero() || a.ExpiresAt.IsZero() {
		t.Errorf("timestamps not parsed: %+v", a)
	}
	if !arts[2].Expired {
		t.Errorf("artifact[2] should be expired: %+v", arts[2])
	}
}

func TestRunArtifactsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).RunArtifacts(context.Background(), "o", "r", 123); err == nil {
		t.Fatal("expected error")
	}
}

func TestArtifactArchive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/r/actions/artifacts/9/zip" {
			http.Redirect(w, r, linkBase(r)+"/blob", http.StatusFound)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/blob") {
			_, _ = w.Write([]byte("zip bytes"))
			return
		}
		t.Errorf("unexpected path %q", r.URL.Path)
	}))
	defer srv.Close()

	rc, err := testClient(t, srv).ArtifactArchive(context.Background(), "o", "r", 9)
	if err != nil {
		t.Fatalf("ArtifactArchive: %v", err)
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if string(body) != "zip bytes" {
		t.Errorf("archive = %q", body)
	}
}

func TestArtifactArchiveURLError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).ArtifactArchive(context.Background(), "o", "r", 9); err == nil {
		t.Fatal("expected error when the archive URL cannot be resolved")
	}
}

func TestArtifactArchiveRedirectTargetError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/r/actions/artifacts/9/zip" {
			http.Redirect(w, r, linkBase(r)+"/blob", http.StatusFound)
			return
		}
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).ArtifactArchive(context.Background(), "o", "r", 9); err == nil {
		t.Fatal("expected error when the redirect target returns non-200")
	}
}
