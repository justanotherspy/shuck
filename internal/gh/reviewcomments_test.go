package gh

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPRReviewComment(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widgets/pulls/comments/9001" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"id": 9001,
			"pull_request_review_id": 77,
			"in_reply_to_id": 42,
			"path": "internal/foo/foo.go",
			"line": 12,
			"start_line": 10,
			"side": "RIGHT",
			"body": "This swallows the error.",
			"diff_hunk": "@@ -8,7 +8,9 @@\n-old\n+new",
			"commit_id": "abc123",
			"user": {"id": 583231, "login": "octocat"}
		}`))
	}))
	defer srv.Close()

	got, err := testClient(t, srv).PRReviewComment(context.Background(), "acme", "widgets", 9001)
	if err != nil {
		t.Fatalf("PRReviewComment: %v", err)
	}
	want := PRReviewComment{
		ID: 9001, ReviewID: 77, InReplyTo: 42,
		Path: "internal/foo/foo.go", Line: 12, StartLine: 10, Side: "RIGHT",
		Body: "This swallows the error.", DiffHunk: "@@ -8,7 +8,9 @@\n-old\n+new",
		CommitID: "abc123", UserID: 583231, UserLogin: "octocat",
	}
	if got != want {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestPRReview(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widgets/pulls/5/reviews/77" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"id": 77,
			"state": "CHANGES_REQUESTED",
			"body": "Needs work.",
			"user": {"id": 583231, "login": "octocat"}
		}`))
	}))
	defer srv.Close()

	got, err := testClient(t, srv).PRReview(context.Background(), "acme", "widgets", 5, 77)
	if err != nil {
		t.Fatalf("PRReview: %v", err)
	}
	want := PRReview{ID: 77, State: "CHANGES_REQUESTED", Body: "Needs work.", UserID: 583231, UserLogin: "octocat"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestPRReviewCommentsPaginates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widgets/pulls/5/reviews/77/comments" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		switch r.URL.Query().Get("page") {
		case "", "1":
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
			_, _ = w.Write([]byte(`[{"id": 1, "body": "a", "user": {"id": 7, "login": "alice"}}]`))
		case "2":
			_, _ = w.Write([]byte(`[{"id": 2, "body": "b", "user": {"id": 7, "login": "alice"}}]`))
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
		}
	}))
	defer srv.Close()

	got, err := testClient(t, srv).PRReviewComments(context.Background(), "acme", "widgets", 5, 77)
	if err != nil {
		t.Fatalf("PRReviewComments: %v", err)
	}
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Errorf("got %+v, want ids [1 2]", got)
	}
}

func TestPRCommentThread(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widgets/pulls/5/comments" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		// Root 42, a reply, and two comments of an unrelated thread.
		_, _ = w.Write([]byte(`[
			{"id": 42, "body": "root", "user": {"id": 1, "login": "alice"}},
			{"id": 50, "body": "other root", "user": {"id": 2, "login": "bob"}},
			{"id": 51, "in_reply_to_id": 50, "body": "other reply", "user": {"id": 1, "login": "alice"}},
			{"id": 60, "in_reply_to_id": 42, "body": "reply", "user": {"id": 2, "login": "bob"}}
		]`))
	}))
	defer srv.Close()

	got, err := testClient(t, srv).PRCommentThread(context.Background(), "acme", "widgets", 5, 42)
	if err != nil {
		t.Fatalf("PRCommentThread: %v", err)
	}
	if len(got) != 2 || got[0].ID != 42 || got[1].ID != 60 {
		t.Errorf("got %+v, want thread ids [42 60]", got)
	}
}

func TestReviewCommentErrorsPropagate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := testClient(t, srv)
	ctx := context.Background()

	if _, err := c.PRReviewComment(ctx, "a", "b", 1); err == nil || !strings.Contains(err.Error(), "review comment 1") {
		t.Errorf("PRReviewComment error = %v, want named comment", err)
	}
	if _, err := c.PRReview(ctx, "a", "b", 1, 2); err == nil || !strings.Contains(err.Error(), "review 2") {
		t.Errorf("PRReview error = %v, want named review", err)
	}
	if _, err := c.PRReviewComments(ctx, "a", "b", 1, 2); err == nil {
		t.Error("PRReviewComments: want error on 500")
	}
	if _, err := c.PRCommentThread(ctx, "a", "b", 1, 2); err == nil {
		t.Error("PRCommentThread: want error on 500")
	}
}

func TestIsNotFoundSeesReviewCommentGone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := testClient(t, srv).PRReviewComment(context.Background(), "a", "b", 1)
	if !IsNotFound(err) {
		t.Errorf("IsNotFound(%v) = false, want true", err)
	}
}
