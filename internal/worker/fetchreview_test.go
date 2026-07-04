package worker

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// reviewAPI serves the GitHub REST surface the review fetches touch. The
// knobs script per-endpoint failures.
type reviewAPI struct {
	srv          *httptest.Server
	commentGone  bool
	reviewGone   bool
	reviewErr    bool
	threadErr    bool
	contentsErr  bool
	contentsHits []string // "path?ref" of each contents request
}

func newReviewAPI(t *testing.T) *reviewAPI {
	t.Helper()
	api := &reviewAPI{}
	mux := http.NewServeMux()
	api.srv = httptest.NewServer(mux)
	t.Cleanup(api.srv.Close)

	mux.HandleFunc("GET /api/v3/repos/o/r/pulls/comments/9001", func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer ghs_inst" {
			t.Errorf("comment request auth = %q, want the installation token", auth)
		}
		if api.commentGone {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{
			"id": 9001, "pull_request_review_id": 77, "in_reply_to_id": 8000,
			"path": "a.go", "line": 3, "side": "RIGHT",
			"body": "rename this", "diff_hunk": "@@ -1 +1 @@",
			"commit_id": "commit9",
			"user": {"id": 555, "login": "octocat"}
		}`)
	})
	mux.HandleFunc("GET /api/v3/repos/o/r/pulls/7/reviews/77", func(w http.ResponseWriter, _ *http.Request) {
		switch {
		case api.reviewGone:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		case api.reviewErr:
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
		default:
			fmt.Fprint(w, `{"id": 77, "state": "COMMENTED", "body": "", "user": {"id": 555, "login": "octocat"}}`)
		}
	})
	mux.HandleFunc("GET /api/v3/repos/o/r/pulls/7/reviews/77/comments", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"id": 9001, "body": "rename this", "user": {"id": 555, "login": "octocat"}}]`)
	})
	mux.HandleFunc("GET /api/v3/repos/o/r/pulls/7/comments", func(w http.ResponseWriter, _ *http.Request) {
		if api.threadErr {
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, `[
			{"id": 8000, "body": "root", "user": {"id": 1, "login": "alice"}},
			{"id": 9001, "in_reply_to_id": 8000, "body": "rename this", "user": {"id": 555, "login": "octocat"}},
			{"id": 5555, "body": "unrelated", "user": {"id": 2, "login": "bob"}}
		]`)
	})
	mux.HandleFunc("GET /api/v3/repos/o/r/contents/a.go", func(w http.ResponseWriter, r *http.Request) {
		api.contentsHits = append(api.contentsHits, "a.go?"+r.URL.Query().Get("ref"))
		if api.contentsErr {
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
			return
		}
		content := base64.StdEncoding.EncodeToString([]byte("l1\nl2\nl3\n"))
		fmt.Fprintf(w, `{"type": "file", "encoding": "base64", "content": %q}`, content)
	})
	return api
}

func TestGHFetcherFetchReviewComment(t *testing.T) {
	api := newReviewAPI(t)
	f := &GHFetcher{APIBase: api.srv.URL}

	data, err := f.FetchReviewComment(context.Background(), "ghs_inst", rcEnvelope())
	if err != nil {
		t.Fatalf("FetchReviewComment: %v", err)
	}
	if data.Comment.ID != 9001 || data.Comment.Body != "rename this" || data.Comment.UserID != 555 {
		t.Errorf("comment = %+v", data.Comment)
	}
	if !data.Parent.Found || data.Parent.State != "COMMENTED" || data.Parent.Comments != 1 {
		t.Errorf("parent = %+v, want the standalone wrapper's shape", data.Parent)
	}
	if len(data.Thread) != 2 || data.Thread[0].ID != 8000 || data.Thread[1].ID != 9001 {
		t.Errorf("thread = %+v, want root + reply only", data.Thread)
	}
	if data.FileContent != "l1\nl2\nl3\n" {
		t.Errorf("file content = %q", data.FileContent)
	}
	// The envelope's head SHA wins over the comment's commit_id.
	if len(api.contentsHits) != 1 || api.contentsHits[0] != "a.go?head1" {
		t.Errorf("contents fetched at %v, want [a.go?head1]", api.contentsHits)
	}
}

func TestGHFetcherFetchReviewCommentGone(t *testing.T) {
	api := newReviewAPI(t)
	api.commentGone = true
	f := &GHFetcher{APIBase: api.srv.URL}

	_, err := f.FetchReviewComment(context.Background(), "ghs_inst", rcEnvelope())
	if !errors.Is(err, ErrGone) {
		t.Fatalf("err = %v, want ErrGone", err)
	}
}

func TestGHFetcherFetchReviewCommentParentGoneDegrades(t *testing.T) {
	api := newReviewAPI(t)
	api.reviewGone = true
	f := &GHFetcher{APIBase: api.srv.URL}

	data, err := f.FetchReviewComment(context.Background(), "ghs_inst", rcEnvelope())
	if err != nil {
		t.Fatalf("a deleted parent must degrade to standalone, got: %v", err)
	}
	if data.Parent.Found {
		t.Errorf("parent = %+v, want not found", data.Parent)
	}
}

func TestGHFetcherFetchReviewCommentParentErrorFails(t *testing.T) {
	api := newReviewAPI(t)
	api.reviewErr = true
	f := &GHFetcher{APIBase: api.srv.URL}

	if _, err := f.FetchReviewComment(context.Background(), "ghs_inst", rcEnvelope()); err == nil {
		t.Fatal("an unreadable parent must fail the fetch (both arms need the same verdict)")
	}
}

func TestGHFetcherFetchReviewCommentThreadFailureDegrades(t *testing.T) {
	api := newReviewAPI(t)
	api.threadErr = true
	f := &GHFetcher{APIBase: api.srv.URL}

	data, err := f.FetchReviewComment(context.Background(), "ghs_inst", rcEnvelope())
	if err != nil {
		t.Fatalf("a thread fetch failure must degrade, got: %v", err)
	}
	if data.Thread != nil {
		t.Errorf("thread = %+v, want none", data.Thread)
	}
}

func TestGHFetcherFetchReviewCommentFileFailureDegrades(t *testing.T) {
	api := newReviewAPI(t)
	api.contentsErr = true
	f := &GHFetcher{APIBase: api.srv.URL}

	data, err := f.FetchReviewComment(context.Background(), "ghs_inst", rcEnvelope())
	if err != nil {
		t.Fatalf("a file fetch failure must degrade to hunk-only, got: %v", err)
	}
	if data.FileContent != "" {
		t.Errorf("file content = %q, want empty", data.FileContent)
	}
}

func TestGHFetcherFetchReviewCommentHeadSHAFallback(t *testing.T) {
	api := newReviewAPI(t)
	f := &GHFetcher{APIBase: api.srv.URL}

	env := rcEnvelope()
	env.HeadSHA = ""
	if _, err := f.FetchReviewComment(context.Background(), "ghs_inst", env); err != nil {
		t.Fatalf("FetchReviewComment: %v", err)
	}
	if len(api.contentsHits) != 1 || api.contentsHits[0] != "a.go?commit9" {
		t.Errorf("contents fetched at %v, want the comment's commit_id", api.contentsHits)
	}
}

func TestGHFetcherFetchReview(t *testing.T) {
	api := newReviewAPI(t)
	f := &GHFetcher{APIBase: api.srv.URL}

	data, err := f.FetchReview(context.Background(), "ghs_inst", rvEnvelope())
	if err != nil {
		t.Fatalf("FetchReview: %v", err)
	}
	if data.Review.ID != 77 || data.Review.State != "COMMENTED" {
		t.Errorf("review = %+v", data.Review)
	}
	if len(data.Comments) != 1 || data.Comments[0].ID != 9001 {
		t.Errorf("comments = %+v", data.Comments)
	}
	// No file contents on the review arm: hunk-only by design.
	if len(api.contentsHits) != 0 {
		t.Errorf("review arm fetched file contents: %v", api.contentsHits)
	}
}

func TestGHFetcherFetchReviewGone(t *testing.T) {
	api := newReviewAPI(t)
	api.reviewGone = true
	f := &GHFetcher{APIBase: api.srv.URL}

	_, err := f.FetchReview(context.Background(), "ghs_inst", rvEnvelope())
	if !errors.Is(err, ErrGone) {
		t.Fatalf("err = %v, want ErrGone", err)
	}
}

func TestGHFetcherReviewBadRepo(t *testing.T) {
	f := &GHFetcher{}
	env := rcEnvelope()
	env.Repo = "not-a-repo"
	if _, err := f.FetchReviewComment(context.Background(), "tok", env); err == nil {
		t.Error("FetchReviewComment: want error for a bad repo")
	}
	env = rvEnvelope()
	env.Repo = "not-a-repo"
	if _, err := f.FetchReview(context.Background(), "tok", env); err == nil {
		t.Error("FetchReview: want error for a bad repo")
	}
}
