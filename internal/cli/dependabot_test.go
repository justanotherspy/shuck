package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

// stubDependabot serves canned file contents and a canned file tree so the
// command can be exercised without GitHub. A path absent from files yields a
// 404-like error that gh.FileNotFound recognizes.
type stubDependabot struct {
	files   map[string][]byte
	tree    []string
	treeErr error
	fileErr error
}

func (s *stubDependabot) FileContent(_ context.Context, _, _, path, _ string) ([]byte, error) {
	if s.fileErr != nil {
		return nil, s.fileErr
	}
	if b, ok := s.files[path]; ok {
		return b, nil
	}
	return nil, errors.New("404 not found")
}

func (s *stubDependabot) RepoTree(_ context.Context, _, _, _ string) ([]string, error) {
	return s.tree, s.treeErr
}

func withStubDependabot(t *testing.T, s *stubDependabot) {
	t.Helper()
	t.Setenv("SHUCK_HOME", t.TempDir())
	t.Setenv("GITHUB_TOKEN", "x") // non-empty so the pipeline does not error on auth
	t.Setenv("GH_TOKEN", "")
	prev := newDependabotLister
	newDependabotLister = func(string) dependabotLister { return s }
	t.Cleanup(func() { newDependabotLister = prev })
}

const goodDependabot = `version: 2
updates:
  - package-ecosystem: gomod
    directory: /
    schedule:
      interval: weekly
    assignees: [alice]
    labels: [dependencies]
    open-pull-requests-limit: 10
    cooldown:
      default-days: 5
    commit-message:
      prefix: chore
    groups:
      all:
        patterns: ["*"]
`

func TestRunDependabotText(t *testing.T) {
	withStubDependabot(t, &stubDependabot{
		files: map[string][]byte{defaultDependabotConfig: []byte(goodDependabot)},
		tree:  []string{"go.mod", "main.go"},
	})
	var out, errb bytes.Buffer
	code := runDependabot([]string{"o/r"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{"o/r — dependabot audit", "✓ gomod (/)", "looks good"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestRunDependabotMissingConfig(t *testing.T) {
	withStubDependabot(t, &stubDependabot{tree: []string{"go.mod"}})
	var out, errb bytes.Buffer
	// Report-only: a missing config produces output but exit 0 without --exit-code.
	if code := runDependabot([]string{"o/r"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out.String(), "no .github/dependabot.yml") {
		t.Errorf("missing no-config finding:\n%s", out.String())
	}
	// With --exit-code the error-level finding gates.
	out.Reset()
	if code := runDependabot([]string{"o/r", "--exit-code"}, &out, &errb); code != 1 {
		t.Fatalf("--exit-code with missing config should exit 1, got %d", code)
	}
}

func TestRunDependabotErrorOnMissingEcosystem(t *testing.T) {
	withStubDependabot(t, &stubDependabot{
		files: map[string][]byte{defaultDependabotConfig: []byte(goodDependabot)},
		tree:  []string{"go.mod", "web/package.json"},
	})
	var out, errb bytes.Buffer
	code := runDependabot([]string{"o/r", "--error-on-missing-ecosystem", "--exit-code"}, &out, &errb)
	if code != 1 {
		t.Fatalf("uncovered npm should gate to 1, got %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "npm") {
		t.Errorf("expected npm in output:\n%s", out.String())
	}
}

func TestRunDependabotStrict(t *testing.T) {
	// A config covering gomod but with no groups yields a warning; --strict gates.
	bare := "version: 2\nupdates:\n  - package-ecosystem: gomod\n    directory: /\n    schedule:\n      interval: weekly\n"
	withStubDependabot(t, &stubDependabot{
		files: map[string][]byte{defaultDependabotConfig: []byte(bare)},
		tree:  []string{"go.mod"},
	})
	var out, errb bytes.Buffer
	if code := runDependabot([]string{"o/r", "--exit-code"}, &out, &errb); code != 0 {
		t.Fatalf("warnings alone should not gate without --strict, got %d", code)
	}
	out.Reset()
	if code := runDependabot([]string{"o/r", "--exit-code", "--strict"}, &out, &errb); code != 1 {
		t.Fatalf("--strict should gate on warnings, got %d", code)
	}
}

func TestRunDependabotJSON(t *testing.T) {
	withStubDependabot(t, &stubDependabot{
		files: map[string][]byte{defaultDependabotConfig: []byte(goodDependabot)},
		tree:  []string{"go.mod"},
	})
	var out, errb bytes.Buffer
	if code := runDependabot([]string{"o/r", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var doc struct {
		SchemaVersion int  `json:"schema_version"`
		HasConfig     bool `json:"has_config"`
		OK            bool `json:"ok"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if doc.SchemaVersion != 1 || !doc.HasConfig || !doc.OK {
		t.Errorf("doc = %+v", doc)
	}
}

func TestRunDependabotMisnamedYaml(t *testing.T) {
	withStubDependabot(t, &stubDependabot{
		files: map[string][]byte{altDependabotConfig: []byte(goodDependabot)},
		tree:  []string{"go.mod"},
	})
	var out, errb bytes.Buffer
	if code := runDependabot([]string{"o/r"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out.String(), "GitHub ignores") {
		t.Errorf("expected misnamed warning:\n%s", out.String())
	}
}

func TestRunDependabotConfigFlag(t *testing.T) {
	withStubDependabot(t, &stubDependabot{tree: []string{"go.mod"}})
	dir := t.TempDir()
	path := filepath.Join(dir, "db.yml")
	if err := os.WriteFile(path, []byte(goodDependabot), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := runDependabot([]string{"o/r", "--config", path}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "config: "+path) {
		t.Errorf("expected config source %q:\n%s", path, out.String())
	}
}

func TestRunDependabotInvalidConfig(t *testing.T) {
	withStubDependabot(t, &stubDependabot{
		files: map[string][]byte{defaultDependabotConfig: []byte("version: 1\n")},
		tree:  []string{"go.mod"},
	})
	var out, errb bytes.Buffer
	if code := runDependabot([]string{"o/r"}, &out, &errb); code != 2 {
		t.Fatalf("invalid config should exit 2, got %d", code)
	}
}

func TestRunDependabotTreeError(t *testing.T) {
	withStubDependabot(t, &stubDependabot{
		files:   map[string][]byte{defaultDependabotConfig: []byte(goodDependabot)},
		treeErr: errors.New("network down"),
	})
	var out, errb bytes.Buffer
	if code := runDependabot([]string{"o/r"}, &out, &errb); code != 2 {
		t.Fatalf("tree error should exit 2, got %d", code)
	}
}

func TestRunDependabotBadRepo(t *testing.T) {
	withStubDependabot(t, &stubDependabot{})
	var out, errb bytes.Buffer
	if code := runDependabot([]string{"not a repo", "extra", "args"}, &out, &errb); code != 2 {
		t.Fatalf("bad target should exit 2, got %d", code)
	}
}

func TestRunDependabotDiscoverCreate(t *testing.T) {
	withStubDependabot(t, &stubDependabot{tree: []string{"go.mod", "web/package.json"}})
	dir := t.TempDir()
	path := filepath.Join(dir, ".github", "dependabot.yml")
	var out, errb bytes.Buffer
	code := runDependabot([]string{"discover", "o/r", "--config", path}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "package-ecosystem: gomod") || !strings.Contains(body, "package-ecosystem: npm") {
		t.Errorf("generated config missing entries:\n%s", body)
	}
	if !strings.Contains(out.String(), "Created") {
		t.Errorf("expected Created message:\n%s", out.String())
	}
}

func TestRunDependabotDiscoverDryRun(t *testing.T) {
	withStubDependabot(t, &stubDependabot{tree: []string{"go.mod"}})
	dir := t.TempDir()
	path := filepath.Join(dir, "out.yml")
	var out, errb bytes.Buffer
	code := runDependabot([]string{"discover", "o/r", "--config", path, "--dry-run"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("--dry-run must not write the file")
	}
	if !strings.Contains(out.String(), "Would create") {
		t.Errorf("expected dry-run message:\n%s", out.String())
	}
}

func TestRunDependabotDiscoverJSON(t *testing.T) {
	withStubDependabot(t, &stubDependabot{tree: []string{"go.mod"}})
	dir := t.TempDir()
	path := filepath.Join(dir, "out.yml")
	var out, errb bytes.Buffer
	code := runDependabot([]string{"discover", "o/r", "--config", path, "--json"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var doc struct {
		Created bool     `json:"created"`
		Added   []string `json:"added"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if !doc.Created || len(doc.Added) != 1 {
		t.Errorf("doc = %+v", doc)
	}
}

func TestDependabotExit(t *testing.T) {
	errReport := &model.DependabotReport{Findings: []model.DependabotFinding{{Level: model.DependabotError}}}
	warnReport := &model.DependabotReport{Findings: []model.DependabotFinding{{Level: model.DependabotWarning}}}
	if dependabotExit(errReport, false, false) != 0 {
		t.Error("no --exit-code should be 0")
	}
	if dependabotExit(errReport, true, false) != 1 {
		t.Error("errors with --exit-code should be 1")
	}
	if dependabotExit(warnReport, true, false) != 0 {
		t.Error("warnings without --strict should be 0")
	}
	if dependabotExit(warnReport, true, true) != 1 {
		t.Error("warnings with --strict should be 1")
	}
}

func TestScanLocalFiles(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "go.mod"), "module x")
	mustWrite(t, filepath.Join(root, "sub", "package.json"), "{}")
	mustWrite(t, filepath.Join(root, "node_modules", "dep", "package.json"), "{}")
	mustWrite(t, filepath.Join(root, ".git", "config"), "x")

	paths, err := scanLocalFiles(root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	got := strings.Join(paths, ",")
	if !strings.Contains(got, "go.mod") || !strings.Contains(got, "sub/package.json") {
		t.Errorf("expected manifests, got %v", paths)
	}
	if strings.Contains(got, "node_modules") || strings.Contains(got, ".git") {
		t.Errorf("skipped dirs leaked: %v", paths)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
