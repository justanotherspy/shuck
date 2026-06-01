package gh

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCursor(t *testing.T) {
	if cursor("") != nil {
		t.Error("empty cursor should be nil so $after is sent as null")
	}
	if cursor("abc") != "abc" {
		t.Error("non-empty cursor should pass through")
	}
}

func TestActor(t *testing.T) {
	if a := actor(nil); a.Login != "" || a.Typename != "" {
		t.Errorf("nil actor = %+v, want zero", a)
	}
	in := &gqlActor{Login: "bob", Typename: "User"}
	if a := actor(in); a.Login != "bob" {
		t.Errorf("actor = %+v", a)
	}
}

func TestParseTime(t *testing.T) {
	if got := parseTime("2026-01-02T03:04:05Z"); got.IsZero() {
		t.Error("valid RFC3339 should parse")
	}
	if got := parseTime("not a time"); !got.IsZero() {
		t.Errorf("invalid input should yield zero time, got %v", got)
	}
	if !parseTime("").Equal(time.Time{}) {
		t.Error("empty input should yield zero time")
	}
}

func TestNormalizeReviewState(t *testing.T) {
	tests := map[string]string{
		"APPROVED":          "approved",
		"approved":          "approved", // case-insensitive
		"CHANGES_REQUESTED": "changes_requested",
		"COMMENTED":         "commented",
		"DISMISSED":         "dismissed",
		"PENDING":           "",
		"WHATEVER":          "",
	}
	for in, want := range tests {
		if got := normalizeReviewState(in); got != want {
			t.Errorf("normalizeReviewState(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGraphQLErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	if err := testClient(t, srv).graphQL(context.Background(), "q", nil, nil); err == nil {
		t.Fatal("expected error on non-200 status")
	}
}

func TestGraphQLErrorsArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"errors":[{"message":"field not found"}]}`))
	}))
	defer srv.Close()

	err := testClient(t, srv).graphQL(context.Background(), "q", nil, nil)
	if err == nil {
		t.Fatal("expected error from the GraphQL errors array")
	}
}

func TestGraphQLBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{not json`))
	}))
	defer srv.Close()

	if err := testClient(t, srv).graphQL(context.Background(), "q", nil, nil); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestGraphQLSuccessNilOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "bearer "+token {
			t.Errorf("auth header = %q", got)
		}
		_, _ = w.Write([]byte(`{"data":{"x":1}}`))
	}))
	defer srv.Close()

	if err := testClient(t, srv).graphQL(context.Background(), "q", nil, nil); err != nil {
		t.Fatalf("graphQL: %v", err)
	}
}

func TestReviewsFingerprint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{
			"updatedAt":"2026-01-01T00:00:00Z",
			"reviews":{"totalCount":3},
			"reviewThreads":{"totalCount":2,"nodes":[
				{"isResolved":true,"isOutdated":false},
				{"isResolved":false,"isOutdated":true}
			]}
		}}}}`))
	}))
	defer srv.Close()

	fp, err := testClient(t, srv).ReviewsFingerprint(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatalf("ReviewsFingerprint: %v", err)
	}
	want := "2026-01-01T00:00:00Z|r3|t2|res1|out1"
	if fp != want {
		t.Errorf("fingerprint = %q, want %q", fp, want)
	}
}

func TestReviewsFingerprintError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).ReviewsFingerprint(context.Background(), "o", "r", 1); err == nil {
		t.Fatal("expected error")
	}
}

// reviewsResponses serves the two paginated GraphQL queries PRReviews issues
// (reviews then threads), distinguishing them by the request body. fetchReviews
// is paged across two responses to exercise the cursor loop.
func TestPRReviews(t *testing.T) {
	reviewCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		body := string(buf)
		switch {
		case strings.Contains(body, "reviews(first:100"):
			reviewCalls++
			if reviewCalls == 1 {
				_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviews":{
					"pageInfo":{"hasNextPage":true,"endCursor":"C1"},
					"nodes":[{"id":"R1","state":"CHANGES_REQUESTED","body":"fix","author":{"login":"bob","__typename":"User"}}]
				}}}}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviews":{
				"pageInfo":{"hasNextPage":false,"endCursor":""},
				"nodes":[{"id":"R2","state":"APPROVED","body":"lgtm","author":{"login":"alice","__typename":"User"}}]
			}}}}}`))
		case strings.Contains(body, "reviewThreads(first:100"):
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{
				"pageInfo":{"hasNextPage":false,"endCursor":""},
				"nodes":[{"isResolved":false,"isOutdated":false,"path":"main.go","line":10,
					"comments":{"nodes":[{"body":"nit","author":{"login":"bob","__typename":"User"},"pullRequestReview":{"id":"R1"}}]}}]
			}}}}}`))
		default:
			t.Errorf("unexpected query body: %s", body)
		}
	}))
	defer srv.Close()

	reviews, err := testClient(t, srv).PRReviews(context.Background(), "o", "r", 1, 5)
	if err != nil {
		t.Fatalf("PRReviews: %v", err)
	}
	if len(reviews) != 2 {
		t.Fatalf("reviews = %+v, want 2", reviews)
	}
	if reviews[0].State != "changes_requested" || len(reviews[0].Threads) != 1 {
		t.Errorf("review[0] = %+v", reviews[0])
	}
	if reviews[0].Threads[0].Path != "main.go" {
		t.Errorf("thread = %+v", reviews[0].Threads[0])
	}
	if reviews[1].State != "approved" {
		t.Errorf("review[1] = %+v", reviews[1])
	}
}

func TestPRReviewsReviewsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).PRReviews(context.Background(), "o", "r", 1, 5); err == nil {
		t.Fatal("expected error from fetchReviews")
	}
}

func TestPRReviewsThreadsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		if strings.Contains(string(buf), "reviews(first:100") {
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviews":{
				"pageInfo":{"hasNextPage":false},"nodes":[]
			}}}}}`))
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).PRReviews(context.Background(), "o", "r", 1, 5); err == nil {
		t.Fatal("expected error from fetchThreads")
	}
}
