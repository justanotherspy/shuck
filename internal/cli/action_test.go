package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/action"
	"github.com/justanotherspy/shuck/internal/model"
)

func TestActionCore(t *testing.T) {
	s := &stubLister{tags: []model.ActionTag{
		{Name: "v4.1.0", SHA: "sha410"},
		{Name: "v4.2.2", SHA: "sha422"},
	}}
	withStubLister(t, s)

	ref, err := action.ParseRef("actions/checkout@v4")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	resolved, err := Action(context.Background(), ref, ActionOptions{})
	if err != nil {
		t.Fatalf("Action: %v", err)
	}
	if resolved.Tag != "v4.2.2" || resolved.SHA != "sha422" {
		t.Errorf("resolved = %+v, want tag v4.2.2 sha422", resolved)
	}
}

// stubLister records how many times the network was hit and returns a fixed
// tag list, so the action command's caching and selection can be tested
// without GitHub.
type stubLister struct {
	tags  []model.ActionTag
	calls int
	err   error
}

func (s *stubLister) ListActionTags(_ context.Context, _, _ string) ([]model.ActionTag, error) {
	s.calls++
	return s.tags, s.err
}

// withStubLister swaps in a stub tag lister for the duration of a test and
// isolates the cache under a temp SHUCK_HOME.
func withStubLister(t *testing.T, s *stubLister) {
	t.Helper()
	t.Setenv("SHUCK_HOME", t.TempDir())
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	prev := newTagLister
	newTagLister = func(string) tagLister { return s }
	t.Cleanup(func() { newTagLister = prev })
}

func TestRunActionTextAndDefaultSelection(t *testing.T) {
	s := &stubLister{tags: []model.ActionTag{
		{Name: "v3.6.0", SHA: "sha360"},
		{Name: "v4", SHA: "sha4float"},
		{Name: "v4.2.2", SHA: "sha422"},
	}}
	withStubLister(t, s)

	var out, errb bytes.Buffer
	if code := runAction([]string{"actions/checkout"}, &out, &errb); code != 0 {
		t.Fatalf("runAction exit = %d, stderr=%s", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "tag: v4.2.2") || !strings.Contains(got, "pin: actions/checkout@sha422 # v4.2.2") {
		t.Errorf("unexpected output:\n%s", got)
	}
}

func TestRunActionConstraintAfterPositional(t *testing.T) {
	s := &stubLister{tags: []model.ActionTag{
		{Name: "v3.6.0", SHA: "sha360"},
		{Name: "v4.2.2", SHA: "sha422"},
	}}
	withStubLister(t, s)

	// Flag after the positional must be permuted, and the @-constraint honored.
	var out, errb bytes.Buffer
	if code := runAction([]string{"actions/checkout@v3", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("runAction exit = %d, stderr=%s", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, `"tag": "v3.6.0"`) || !strings.Contains(got, `"sha": "sha360"`) {
		t.Errorf("unexpected JSON:\n%s", got)
	}
}

func TestRunActionSeparateVersionArg(t *testing.T) {
	s := &stubLister{tags: []model.ActionTag{{Name: "v3.6.0", SHA: "x"}, {Name: "v4.2.2", SHA: "y"}}}
	withStubLister(t, s)

	var out, errb bytes.Buffer
	if code := runAction([]string{"actions/checkout", "v3"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "tag: v3.6.0") {
		t.Errorf("separate version arg not honored:\n%s", out.String())
	}
}

func TestRunActionCachesAcrossRuns(t *testing.T) {
	s := &stubLister{tags: []model.ActionTag{{Name: "v1.0.0", SHA: "a"}}}
	withStubLister(t, s)

	var out, errb bytes.Buffer
	runAction([]string{"actions/checkout"}, &out, &errb)
	runAction([]string{"actions/checkout"}, &out, &errb)
	if s.calls != 1 {
		t.Errorf("expected 1 network fetch with a warm cache, got %d", s.calls)
	}

	// --refresh forces a re-fetch even when the cache is fresh.
	runAction([]string{"actions/checkout", "--refresh"}, &out, &errb)
	if s.calls != 2 {
		t.Errorf("--refresh should re-fetch: calls = %d, want 2", s.calls)
	}
}

func TestRunActionErrors(t *testing.T) {
	withStubLister(t, &stubLister{tags: []model.ActionTag{{Name: "v1.0.0", SHA: "a"}}})

	cases := [][]string{
		{},                            // missing action
		{"not-a-slug"},                // bad ref
		{"actions/checkout@v9"},       // no matching tag
		{"actions/checkout@v3", "v4"}, // version given twice
	}
	for _, args := range cases {
		var out, errb bytes.Buffer
		if code := runAction(args, &out, &errb); code != 2 {
			t.Errorf("runAction(%v) exit = %d, want 2 (stderr=%q)", args, code, errb.String())
		}
	}
}
