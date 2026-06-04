package gh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckRunID(t *testing.T) {
	tests := []struct {
		url  string
		want int64
	}{
		{"https://api.github.com/repos/o/r/check-runs/123456", 123456},
		{"https://api.github.com/repos/o/r/check-runs/0", 0},
		{"", 0},
		{"not-a-url", 0},
		{"https://api.github.com/repos/o/r/check-runs/", 0},
		{"https://api.github.com/repos/o/r/check-runs/abc", 0},
	}
	for _, tt := range tests {
		if got := checkRunID(tt.url); got != tt.want {
			t.Errorf("checkRunID(%q) = %d, want %d", tt.url, got, tt.want)
		}
	}
}

func TestJobAnnotations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/check-runs/55/annotations" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[
			{"path":"main.go","start_line":10,"end_line":10,"start_column":2,"annotation_level":"failure","title":"vet","message":"undefined: foo"},
			{"path":"x.go","start_line":3,"end_line":3,"annotation_level":"warning","message":"shadow"}
		]`))
	}))
	defer srv.Close()

	anns, err := testClient(t, srv).JobAnnotations(context.Background(), "o", "r", 55)
	if err != nil {
		t.Fatalf("JobAnnotations: %v", err)
	}
	if len(anns) != 2 {
		t.Fatalf("got %d annotations, want 2: %+v", len(anns), anns)
	}
	if anns[0].Path != "main.go" || anns[0].StartLine != 10 || anns[0].StartColumn != 2 ||
		anns[0].Level != "failure" || anns[0].Title != "vet" || anns[0].Message != "undefined: foo" {
		t.Errorf("annotation[0] = %+v", anns[0])
	}
	if anns[1].Level != "warning" || anns[1].Message != "shadow" {
		t.Errorf("annotation[1] = %+v", anns[1])
	}
}

// A zero check-run ID short-circuits without any HTTP call.
func TestJobAnnotationsZeroID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("JobAnnotations(0) must not make a request")
	}))
	defer srv.Close()

	anns, err := testClient(t, srv).JobAnnotations(context.Background(), "o", "r", 0)
	if err != nil || anns != nil {
		t.Fatalf("JobAnnotations(0) = %+v, %v; want nil, nil", anns, err)
	}
}

func TestJobAnnotationsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).JobAnnotations(context.Background(), "o", "r", 55); err == nil {
		t.Fatal("expected error from a failing annotations request")
	}
}
