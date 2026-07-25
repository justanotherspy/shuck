package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/action"
	"github.com/justanotherspy/shuck/internal/model"
)

// pinsRepo writes a checkout whose .github/workflows holds one file.
func pinsRepo(t *testing.T, workflow string) string {
	t.Helper()
	dir := t.TempDir()
	wf := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(wf, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wf, "ci.yml"), []byte(workflow), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

const pinsWorkflow = `name: CI
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: make ci
`

// stubTagLister answers tag lookups without a network, through the same
// package var `shuck action` uses.
type stubTagLister struct {
	tags map[string][]model.ActionTag
}

func (s *stubTagLister) ListActionTags(_ context.Context, owner, repo string) ([]model.ActionTag, error) {
	return s.tags[owner+"/"+repo], nil
}

func (s *stubTagLister) DefaultBranchSHA(context.Context, string, string) (string, error) {
	return "branch-sha", nil
}

// withStubTags points the pin resolver at canned tags and shuck's cache at a
// temp directory, so the audit runs end to end without the network.
func withStubTags(t *testing.T) {
	t.Helper()
	t.Setenv("SHUCK_HOME", t.TempDir())
	t.Setenv("GITHUB_TOKEN", "test-token")

	original := NewTagLister
	NewTagLister = func(string) TagLister {
		return &stubTagLister{tags: map[string][]model.ActionTag{
			"actions/checkout": {
				{Name: "v4.2.2", SHA: "3d3c42e5aac5ba805825da76410c181273ba90b1"},
				{Name: "v3.6.0", SHA: "1111111111111111111111111111111111111111"},
			},
		}}
	}
	t.Cleanup(func() { NewTagLister = original })
}

func TestRunPins(t *testing.T) {
	withStubTags(t)
	dir := pinsRepo(t, pinsWorkflow)

	code, stdout, _ := runCLI("pins", dir)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — producing a report is success", code)
	}
	for _, want := range []string{"actions/checkout@v4", "mutable tag", "3d3c42e5aac5ba805825da76410c181273ba90b1", "v4.2.2"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("report is missing %q:\n%s", want, stdout)
		}
	}
}

func TestRunPinsAlias(t *testing.T) {
	withStubTags(t)
	dir := pinsRepo(t, pinsWorkflow)

	if code, stdout, _ := runCLI("p", dir); code != 0 || !strings.Contains(stdout, "actions/checkout") {
		t.Errorf("`shuck p` did not reach the pin audit: exit %d\n%s", code, stdout)
	}
}

func TestRunPinsExitCode(t *testing.T) {
	withStubTags(t)

	// Gating is opt-in, exactly as it is for the other report commands.
	dir := pinsRepo(t, pinsWorkflow)
	if code, _, _ := runCLI("pins", dir); code != 0 {
		t.Errorf("exit = %d without --exit-code, want 0", code)
	}
	if code, _, _ := runCLI("pins", "--exit-code", dir); code != 1 {
		t.Errorf("exit = %d with --exit-code and an unpinned action, want 1", code)
	}

	clean := pinsRepo(t, `name: CI
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v4.2.2
`)
	if code, _, _ := runCLI("pins", "--exit-code", clean); code != 0 {
		t.Errorf("exit = %d for a fully pinned, current checkout, want 0", code)
	}
}

func TestRunPinsJSON(t *testing.T) {
	withStubTags(t)
	dir := pinsRepo(t, pinsWorkflow)

	code, stdout, _ := runCLI("pins", "--json", dir)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var doc struct {
		SchemaVersion int `json:"schema_version"`
		Summary       struct {
			Unpinned int `json:"unpinned"`
		} `json:"summary"`
		Findings []struct {
			File    string `json:"file"`
			Status  string `json:"status"`
			PinLine string `json:"pin_line"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("--json is not valid JSON: %v\n%s", err, stdout)
	}
	if doc.SchemaVersion == 0 {
		t.Error("the JSON view should carry a schema version")
	}
	if doc.Summary.Unpinned != 1 {
		t.Errorf("summary.unpinned = %d, want 1", doc.Summary.Unpinned)
	}
	if len(doc.Findings) == 0 || doc.Findings[0].File != ".github/workflows/ci.yml" {
		t.Errorf("findings = %+v, want the workflow's repo-relative path", doc.Findings)
	}
}

func TestRunPinsOffline(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	dir := pinsRepo(t, pinsWorkflow)

	// Offline still reports what is unpinned — it just cannot suggest the fix.
	code, stdout, _ := runCLI("pins", "--offline", dir)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "actions/checkout@v4") {
		t.Errorf("offline should still list the reference:\n%s", stdout)
	}
	if strings.Contains(stdout, "3d3c42e5aac5ba805825da76410c181273ba90b1") {
		t.Errorf("offline cannot resolve a fix, so it must not suggest a pin:\n%s", stdout)
	}
}

func TestRunPinsArgumentErrors(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())

	if code, _, stderr := runCLI("pins", "a", "b"); code != 2 || !strings.Contains(stderr, "too many arguments") {
		t.Errorf("exit = %d, stderr = %q", code, stderr)
	}
	missing := filepath.Join(t.TempDir(), "nope")
	if code, _, stderr := runCLI("pins", missing); code != 2 || stderr == "" {
		t.Errorf("a missing directory should be reported: exit %d, stderr %q", code, stderr)
	}
	if code, _, _ := runCLI("pins", "--nonsense"); code != 2 {
		t.Errorf("exit = %d for an unknown flag, want 2", code)
	}
}

func TestCachedTagResolverMemoizes(t *testing.T) {
	withStubTags(t)

	calls := 0
	original := NewTagLister
	NewTagLister = func(string) TagLister {
		calls++
		return &stubTagLister{tags: map[string][]model.ActionTag{
			"actions/checkout": {{Name: "v4.2.2", SHA: "abc"}},
		}}
	}
	t.Cleanup(func() { NewTagLister = original })

	r := newPinResolver("token", false)
	ref, err := action.ParseRef("actions/checkout@v4")
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tag != "v4.2.2" || got.SHA != "abc" {
		t.Errorf("resolved to %s/%s, want v4.2.2/abc", got.Tag, got.SHA)
	}

	// A repository's workflows name the same actions over and over; one audit
	// must not fetch the same tag list twice.
	before := calls
	if _, err := r.Resolve(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if calls != before {
		t.Errorf("the resolver fetched %d more times for a repeated reference, want 0", calls-before)
	}
}
