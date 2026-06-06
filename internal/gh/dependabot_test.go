package gh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRepoTreeDefaultBranch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/commits/HEAD":
			_, _ = w.Write([]byte("deadbeef")) // GetCommitSHA1 reads the body as the SHA
		case "/repos/o/r/git/trees/deadbeef":
			if r.URL.Query().Get("recursive") != "1" {
				t.Errorf("want recursive=1, got %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"sha":"deadbeef","tree":[
				{"path":"go.mod","type":"blob"},
				{"path":"internal","type":"tree"},
				{"path":"internal/x.go","type":"blob"}
			],"truncated":false}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	paths, err := testClient(t, srv).RepoTree(context.Background(), "o", "r", "")
	if err != nil {
		t.Fatalf("RepoTree: %v", err)
	}
	if len(paths) != 2 || paths[0] != "go.mod" || paths[1] != "internal/x.go" {
		t.Errorf("paths = %v, want [go.mod internal/x.go] (no tree entries)", paths)
	}
}

func TestRepoTreeExplicitRef(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/git/trees/main" {
			t.Errorf("unexpected path %q (ref should be used directly)", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"tree":[{"path":"package.json","type":"blob"}]}`))
	}))
	defer srv.Close()

	paths, err := testClient(t, srv).RepoTree(context.Background(), "o", "r", "main")
	if err != nil {
		t.Fatalf("RepoTree: %v", err)
	}
	if len(paths) != 1 || paths[0] != "package.json" {
		t.Errorf("paths = %v", paths)
	}
}

func TestRepoTreeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).RepoTree(context.Background(), "o", "r", "main"); err == nil {
		t.Fatal("expected error")
	}
}

func TestRepoTreeDefaultBranchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"no"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).RepoTree(context.Background(), "o", "r", ""); err == nil {
		t.Fatal("expected error when default branch is unreadable")
	}
}
