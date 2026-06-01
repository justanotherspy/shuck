package gh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListActionTags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/tags" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.URL.Query().Get("page") == "" {
			// First page links to a second to exercise the pagination loop.
			w.Header().Set("Link", `<`+linkBase(r)+`?page=2>; rel="next"`)
			_, _ = w.Write([]byte(`[
				{"name":"v1.0.0","commit":{"sha":"aaa"}},
				{"name":"","commit":{"sha":"skip"}},
				{"name":"v0.9.0","commit":{"sha":""}}
			]`))
			return
		}
		_, _ = w.Write([]byte(`[{"name":"v2.0.0","commit":{"sha":"bbb"}}]`))
	}))
	defer srv.Close()

	tags, err := testClient(t, srv).ListActionTags(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("ListActionTags: %v", err)
	}
	// Tags missing a name or SHA are dropped; the two valid ones span both pages.
	if len(tags) != 2 {
		t.Fatalf("tags = %+v, want 2", tags)
	}
	if tags[0].Name != "v1.0.0" || tags[0].SHA != "aaa" {
		t.Errorf("tag[0] = %+v", tags[0])
	}
	if tags[1].Name != "v2.0.0" || tags[1].SHA != "bbb" {
		t.Errorf("tag[1] = %+v", tags[1])
	}
}

func TestListActionTagsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).ListActionTags(context.Background(), "o", "r"); err == nil {
		t.Fatal("expected error")
	}
}
