package promexpo

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteGroupsHelpAndType(t *testing.T) {
	var b strings.Builder
	err := Write(&b, []Sample{
		{Name: "shuck_x_total", Help: "an x", Type: Counter, Value: 3},
		{Name: "shuck_y", Help: "a gauge", Type: Gauge, Value: -1},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	want := "# HELP shuck_x_total an x\n" +
		"# TYPE shuck_x_total counter\n" +
		"shuck_x_total 3\n" +
		"# HELP shuck_y a gauge\n" +
		"# TYPE shuck_y gauge\n" +
		"shuck_y -1\n"
	if got := b.String(); got != want {
		t.Fatalf("output mismatch\n got:\n%q\nwant:\n%q", got, want)
	}
}

func TestWriteRepeatedNameEmitsHeaderOnce(t *testing.T) {
	var b strings.Builder
	if err := Write(&b, []Sample{
		{Name: "fam_total", Help: "h", Type: Counter, Value: 1},
		{Name: "fam_total", Help: "h", Type: Counter, Value: 2},
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := b.String()
	if n := strings.Count(out, "# TYPE fam_total"); n != 1 {
		t.Fatalf("want one TYPE line, got %d in:\n%s", n, out)
	}
	if n := strings.Count(out, "# HELP fam_total"); n != 1 {
		t.Fatalf("want one HELP line, got %d in:\n%s", n, out)
	}
	if !strings.Contains(out, "fam_total 1\n") || !strings.Contains(out, "fam_total 2\n") {
		t.Fatalf("missing sample lines in:\n%s", out)
	}
}

func TestWriteDefaultsTypeToCounterAndSkipsEmptyHelp(t *testing.T) {
	var b strings.Builder
	if err := Write(&b, []Sample{{Name: "n_total", Value: 5}}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := b.String()
	if strings.Contains(got, "# HELP") {
		t.Fatalf("empty help should emit no HELP line: %q", got)
	}
	if !strings.Contains(got, "# TYPE n_total counter\n") {
		t.Fatalf("missing defaulted TYPE line: %q", got)
	}
}

func TestEscapeHelp(t *testing.T) {
	var b strings.Builder
	if err := Write(&b, []Sample{{Name: "n", Help: "line\none\\two", Type: Gauge}}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := b.String(); !strings.Contains(got, `# HELP n line\none\\two`+"\n") {
		t.Fatalf("help not escaped: %q", got)
	}
}

func TestHandlerServesExposition(t *testing.T) {
	h := Handler(func() []Sample {
		return []Sample{{Name: "shuck_hits_total", Help: "hits", Type: Counter, Value: 7}}
	})
	req := httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != ContentType {
		t.Fatalf("content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "shuck_hits_total 7\n") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestHandlerHeadHasNoBody(t *testing.T) {
	called := false
	h := Handler(func() []Sample { called = true; return nil })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/metrics", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD should have empty body, got %q", rec.Body.String())
	}
	if called {
		t.Fatalf("collect should not run for HEAD")
	}
}

func TestHandlerRejectsPost(t *testing.T) {
	h := Handler(nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/metrics", http.NoBody))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if rec.Header().Get("Allow") == "" {
		t.Fatalf("missing Allow header")
	}
}

func TestHandlerNilCollect(t *testing.T) {
	h := Handler(nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("nil collect: status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestMergeAndSortedNames(t *testing.T) {
	merged := Merge(
		[]Sample{{Name: "b_total", Value: 1}},
		[]Sample{{Name: "a_total", Value: 2}, {Name: "b_total", Value: 3}},
	)
	if len(merged) != 3 {
		t.Fatalf("merged len = %d", len(merged))
	}
	names := SortedNames(merged)
	if len(names) != 2 || names[0] != "a_total" || names[1] != "b_total" {
		t.Fatalf("sorted names = %v", names)
	}
}
