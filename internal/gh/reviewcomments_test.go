package gh

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}

func reviewIDs(reviews []PRReview) []int64 {
	ids := make([]int64, 0, len(reviews))
	for _, rv := range reviews {
		ids = append(ids, rv.ID)
	}
	return ids
}

func commentIDs(comments []PRReviewComment) []int64 {
	ids := make([]int64, 0, len(comments))
	for _, rc := range comments {
		ids = append(ids, rc.ID)
	}
	return ids
}

func sameIDs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

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

// reviewsPage is the reviews listing every PRReviewsSince test is filtered
// against: two submitted reviews five seconds apart, plus a PENDING draft that
// carries no submitted_at (the author's own unsubmitted review, which is not an
// event for anyone).
const reviewsPage = `[
	{"id": 1, "state": "APPROVED", "body": "lgtm", "submitted_at": "2026-05-01T10:00:00Z", "user": {"id": 7, "login": "alice"}},
	{"id": 2, "state": "CHANGES_REQUESTED", "body": "needs work", "submitted_at": "2026-05-01T10:00:05Z", "user": {"id": 8, "login": "bob"}},
	{"id": 3, "state": "PENDING", "body": "draft", "user": {"id": 9, "login": "carol"}}
]`

func TestPRReviewsSinceFilters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widgets/pulls/5/reviews" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(reviewsPage))
	}))
	defer srv.Close()
	c := testClient(t, srv)

	tests := []struct {
		name  string
		since time.Time
		want  []int64
	}{
		// A first sighting has no watermark: everything that was actually
		// submitted is new, but a pending draft still is not.
		{"zero since keeps every submitted review", time.Time{}, []int64{1, 2}},
		{"before both", mustTime(t, "2026-05-01T09:59:59Z"), []int64{1, 2}},
		// The boundary is strictly-after: a watermark set from review 1's own
		// timestamp must not hand review 1 back on the next poll, or the
		// monitor repeats a verdict the session already acted on.
		{"exactly review 1", mustTime(t, "2026-05-01T10:00:00Z"), []int64{2}},
		{"one second inside the gap", mustTime(t, "2026-05-01T10:00:01Z"), []int64{2}},
		{"exactly the newest review", mustTime(t, "2026-05-01T10:00:05Z"), nil},
		{"after everything", mustTime(t, "2026-05-01T10:00:06Z"), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.PRReviewsSince(context.Background(), "acme", "widgets", 5, tc.since)
			if err != nil {
				t.Fatalf("PRReviewsSince: %v", err)
			}
			if !sameIDs(reviewIDs(got), tc.want) {
				t.Errorf("ids = %v, want %v", reviewIDs(got), tc.want)
			}
		})
	}
}

func TestPRReviewsSinceMapsFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(reviewsPage))
	}))
	defer srv.Close()

	got, err := testClient(t, srv).PRReviewsSince(context.Background(), "acme", "widgets", 5, time.Time{})
	if err != nil {
		t.Fatalf("PRReviewsSince: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d reviews, want 2", len(got))
	}
	want := PRReview{
		ID: 2, State: "CHANGES_REQUESTED", Body: "needs work",
		UserID: 8, UserLogin: "bob",
		SubmittedAt: mustTime(t, "2026-05-01T10:00:05Z"),
	}
	// SubmittedAt is what the caller advances its watermark from, so it has to
	// survive the round trip intact — a dropped verdict or author would also
	// leave the event body unattributable.
	if !got[1].SubmittedAt.Equal(want.SubmittedAt) {
		t.Errorf("SubmittedAt = %v, want %v", got[1].SubmittedAt, want.SubmittedAt)
	}
	got[1].SubmittedAt = want.SubmittedAt
	if got[1] != want {
		t.Errorf("got %+v\nwant %+v", got[1], want)
	}
}

func TestPRReviewsSincePaginates(t *testing.T) {
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want 100", got)
		}
		switch page {
		case "", "1":
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
			_, _ = w.Write([]byte(`[
				{"id": 1, "state": "COMMENTED", "submitted_at": "2026-05-01T09:00:00Z", "user": {"login": "alice"}},
				{"id": 2, "state": "APPROVED", "submitted_at": "2026-05-01T11:00:00Z", "user": {"login": "alice"}}
			]`))
		case "2":
			_, _ = w.Write([]byte(`[
				{"id": 3, "state": "COMMENTED", "submitted_at": "2026-05-01T09:30:00Z", "user": {"login": "bob"}},
				{"id": 4, "state": "CHANGES_REQUESTED", "submitted_at": "2026-05-01T11:30:00Z", "user": {"login": "bob"}}
			]`))
		default:
			t.Errorf("unexpected page %q", page)
		}
	}))
	defer srv.Close()

	// Each page holds one stale and one new review, so a filter applied only to
	// the first page — or a loop that stops after it — changes the answer.
	got, err := testClient(t, srv).PRReviewsSince(context.Background(), "acme", "widgets", 5, mustTime(t, "2026-05-01T10:00:00Z"))
	if err != nil {
		t.Fatalf("PRReviewsSince: %v", err)
	}
	if !sameIDs(reviewIDs(got), []int64{2, 4}) {
		t.Errorf("ids = %v, want [2 4]", reviewIDs(got))
	}
	if len(pages) != 2 || pages[1] != "2" {
		t.Errorf("requested pages = %v, want the second page to be followed", pages)
	}
}

func TestPRReviewsSinceErrorPropagates(t *testing.T) {
	tests := []struct {
		name    string
		failing string // the page number that 500s
	}{
		{"first page", "1"},
		// A failure on a later page must not surface as the first page's
		// reviews with a nil error: the caller would advance its watermark past
		// reviews it never saw.
		{"second page", "2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				page := r.URL.Query().Get("page")
				if page == "" {
					page = "1"
				}
				if page == tc.failing {
					http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
					return
				}
				w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
				_, _ = w.Write([]byte(`[{"id": 1, "state": "APPROVED", "submitted_at": "2026-05-01T10:00:00Z", "user": {"login": "alice"}}]`))
			}))
			defer srv.Close()

			got, err := testClient(t, srv).PRReviewsSince(context.Background(), "acme", "widgets", 5, time.Time{})
			if err == nil {
				t.Fatalf("PRReviewsSince = %+v, want an error on 500", got)
			}
			if !strings.Contains(err.Error(), "list reviews on acme/widgets#5") {
				t.Errorf("error = %v, want it to name the PR", err)
			}
			if got != nil {
				t.Errorf("got %+v alongside an error, want nil", got)
			}
		})
	}
}

func TestPRReviewCommentsSinceRequest(t *testing.T) {
	// PRReviewCommentsSince does no client-side filtering: the query it builds
	// *is* the filter, so the request itself is the contract under test.
	tests := []struct {
		name  string
		since time.Time
	}{
		{"utc watermark", mustTime(t, "2026-05-01T10:00:00Z")},
		// A watermark carried in a non-UTC location must still name the same
		// instant on the wire.
		{"offset watermark", mustTime(t, "2026-05-01T12:00:00+02:00")},
		{"zero since", time.Time{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/acme/widgets/pulls/5/comments" {
					t.Errorf("unexpected path %q", r.URL.Path)
				}
				q := r.URL.Query()
				// Newest last: the caller renders comments in the order it gets
				// them and folds replies onto their root.
				if q.Get("sort") != "created" || q.Get("direction") != "asc" {
					t.Errorf("sort/direction = %q/%q, want created/asc", q.Get("sort"), q.Get("direction"))
				}
				if q.Get("per_page") != "100" {
					t.Errorf("per_page = %q, want 100", q.Get("per_page"))
				}
				switch {
				case tc.since.IsZero():
					// A zero watermark must be omitted, not serialized as year
					// 1: the first sighting asks for the whole history.
					if q.Has("since") {
						t.Errorf("since = %q on a first sighting, want it omitted", q.Get("since"))
					}
				default:
					got, err := time.Parse(time.RFC3339, q.Get("since"))
					if err != nil {
						t.Fatalf("since = %q, unparseable: %v", q.Get("since"), err)
					}
					if !got.Equal(tc.since) {
						t.Errorf("since = %v, want %v", got, tc.since)
					}
				}
				_, _ = w.Write([]byte(`[]`))
			}))
			defer srv.Close()

			if _, err := testClient(t, srv).PRReviewCommentsSince(context.Background(), "acme", "widgets", 5, tc.since); err != nil {
				t.Fatalf("PRReviewCommentsSince: %v", err)
			}
		})
	}
}

func TestPRReviewCommentsSinceMapsFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{
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
			"user": {"id": 583231, "login": "octocat"},
			"created_at": "2026-05-01T10:00:00Z",
			"updated_at": "2026-05-01T10:30:00Z"
		}]`))
	}))
	defer srv.Close()

	got, err := testClient(t, srv).PRReviewCommentsSince(context.Background(), "acme", "widgets", 5, time.Time{})
	if err != nil {
		t.Fatalf("PRReviewCommentsSince: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d comments, want 1", len(got))
	}
	// CreatedAt is the watermark the caller advances; the anchor fields
	// (path/line/side/diff hunk) are the whole reason this poll is REST rather
	// than the GraphQL reviews query.
	want := PRReviewComment{
		ID: 9001, ReviewID: 77, InReplyTo: 42,
		Path: "internal/foo/foo.go", Line: 12, StartLine: 10, Side: "RIGHT",
		Body: "This swallows the error.", DiffHunk: "@@ -8,7 +8,9 @@\n-old\n+new",
		CommitID: "abc123", UserID: 583231, UserLogin: "octocat",
		CreatedAt: mustTime(t, "2026-05-01T10:00:00Z"),
		UpdatedAt: mustTime(t, "2026-05-01T10:30:00Z"),
	}
	if !got[0].CreatedAt.Equal(want.CreatedAt) || !got[0].UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("timestamps = %v/%v, want %v/%v", got[0].CreatedAt, got[0].UpdatedAt, want.CreatedAt, want.UpdatedAt)
	}
	got[0].CreatedAt, got[0].UpdatedAt = want.CreatedAt, want.UpdatedAt
	if got[0] != want {
		t.Errorf("got %+v\nwant %+v", got[0], want)
	}
}

func TestPRReviewCommentsSincePaginates(t *testing.T) {
	since := mustTime(t, "2026-05-01T10:00:00Z")
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		// Every page must carry the filter: rebuilding the options inside the
		// loop would turn page 2 into the PR's whole comment history.
		if got := r.URL.Query().Get("since"); got == "" {
			t.Errorf("page %q dropped the since filter", page)
		} else if ts, err := time.Parse(time.RFC3339, got); err != nil || !ts.Equal(since) {
			t.Errorf("page %q since = %q, want %v", page, got, since)
		}
		switch page {
		case "", "1":
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
			_, _ = w.Write([]byte(`[{"id": 1, "body": "a", "user": {"login": "alice"}}]`))
		case "2":
			_, _ = w.Write([]byte(`[{"id": 2, "body": "b", "user": {"login": "bob"}}]`))
		default:
			t.Errorf("unexpected page %q", page)
		}
	}))
	defer srv.Close()

	got, err := testClient(t, srv).PRReviewCommentsSince(context.Background(), "acme", "widgets", 5, since)
	if err != nil {
		t.Fatalf("PRReviewCommentsSince: %v", err)
	}
	if !sameIDs(commentIDs(got), []int64{1, 2}) {
		t.Errorf("ids = %v, want [1 2] in page order", commentIDs(got))
	}
	if len(pages) != 2 || pages[1] != "2" {
		t.Errorf("requested pages = %v, want the second page to be followed", pages)
	}
}

func TestPRReviewCommentsSinceErrorPropagates(t *testing.T) {
	tests := []struct {
		name    string
		failing string
	}{
		{"first page", "1"},
		// The failure mode this guards: returning page 1 with a nil error tells
		// the monitor it saw everything, and the reviewer's later comments are
		// filtered out forever by the advanced watermark.
		{"second page", "2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				page := r.URL.Query().Get("page")
				if page == "" {
					page = "1"
				}
				if page == tc.failing {
					http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
					return
				}
				w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
				_, _ = w.Write([]byte(`[{"id": 1, "body": "a", "user": {"login": "alice"}}]`))
			}))
			defer srv.Close()

			got, err := testClient(t, srv).PRReviewCommentsSince(context.Background(), "acme", "widgets", 5, time.Time{})
			if err == nil {
				t.Fatalf("PRReviewCommentsSince = %+v, want an error on 500", got)
			}
			if !strings.Contains(err.Error(), "list review comments on acme/widgets#5") {
				t.Errorf("error = %v, want it to name the PR", err)
			}
			if got != nil {
				t.Errorf("got %+v alongside an error, want nil", got)
			}
		})
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
