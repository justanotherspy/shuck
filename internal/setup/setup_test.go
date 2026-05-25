package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fakeSkill = "---\nname: shuck\n---\n# skill body\n"

// useConfigDir points setup at a temp Claude config dir and returns the dir.
func useConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	return dir
}

func TestRunInstallsSkillAndNote(t *testing.T) {
	dir := useConfigDir(t)
	var stdout, stderr strings.Builder

	if code := Run([]string{"--no-mcp"}, fakeSkill, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, stderr.String())
	}

	skillPath := filepath.Join(dir, "skills", "shuck", "SKILL.md")
	got, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read skill: %v", err)
	}
	if string(got) != fakeSkill {
		t.Errorf("skill content = %q, want %q", got, fakeSkill)
	}

	mdPath := filepath.Join(dir, "CLAUDE.md")
	md, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}
	if !strings.Contains(string(md), claudeBegin) || !strings.Contains(string(md), claudeEnd) {
		t.Errorf("CLAUDE.md missing markers:\n%s", md)
	}
	if !strings.Contains(string(md), "shuck` skill") {
		t.Errorf("CLAUDE.md missing skill mention:\n%s", md)
	}
	if !strings.Contains(stdout.String(), "skipping MCP server registration (--no-mcp)") {
		t.Errorf("expected --no-mcp note, got %q", stdout.String())
	}
}

func TestRunIsIdempotent(t *testing.T) {
	dir := useConfigDir(t)
	mdPath := filepath.Join(dir, "CLAUDE.md")

	// Seed CLAUDE.md with pre-existing user content so we verify it is preserved.
	const preamble = "# My notes\n\nsome existing guidance\n"
	if err := os.WriteFile(mdPath, []byte(preamble), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func() string {
		var out, errOut strings.Builder
		if code := Run([]string{"--no-mcp"}, fakeSkill, strings.NewReader(""), &out, &errOut); code != 0 {
			t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut.String())
		}
		return out.String()
	}

	run()
	first, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	out2 := run()
	second, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}

	if string(first) != string(second) {
		t.Errorf("CLAUDE.md changed on second run:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if !strings.HasPrefix(string(second), preamble) {
		t.Errorf("existing content not preserved:\n%s", second)
	}
	if n := strings.Count(string(second), claudeBegin); n != 1 {
		t.Errorf("begin marker appears %d times, want 1:\n%s", n, second)
	}
	if !strings.Contains(out2, "already up to date") {
		t.Errorf("second run should report up to date, got %q", out2)
	}
}

func TestRunUpdatesStaleBlock(t *testing.T) {
	dir := useConfigDir(t)
	mdPath := filepath.Join(dir, "CLAUDE.md")

	stale := "intro\n\n" + claudeBegin + "\nOLD CONTENT\n" + claudeEnd + "\n\ntrailer\n"
	if err := os.WriteFile(mdPath, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errOut strings.Builder
	if code := Run([]string{"--no-mcp"}, fakeSkill, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut.String())
	}

	md, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(md), "OLD CONTENT") {
		t.Errorf("stale content not replaced:\n%s", md)
	}
	if !strings.HasPrefix(string(md), "intro\n") || !strings.HasSuffix(string(md), "trailer\n") {
		t.Errorf("surrounding content not preserved:\n%s", md)
	}
	if !strings.Contains(out.String(), "updated CLAUDE.md") {
		t.Errorf("expected updated note, got %q", out.String())
	}
}

func TestRunDryRunWritesNothing(t *testing.T) {
	dir := useConfigDir(t)
	var out, errOut strings.Builder
	if code := Run([]string{"--dry-run", "--mcp"}, fakeSkill, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut.String())
	}

	if _, err := os.Stat(filepath.Join(dir, "skills", "shuck", "SKILL.md")); !os.IsNotExist(err) {
		t.Errorf("dry-run created the skill file (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Errorf("dry-run created CLAUDE.md (err=%v)", err)
	}
	o := out.String()
	for _, want := range []string{"[dry-run] would write skill", "[dry-run] would write CLAUDE.md note", "[dry-run] would register the shuck MCP server"} {
		if !strings.Contains(o, want) {
			t.Errorf("dry-run output missing %q; got:\n%s", want, o)
		}
	}
}

func TestRunMCPViaClaudeCLI(t *testing.T) {
	useConfigDir(t)
	stubClaude(t, []byte("Added shuck MCP server\n"), nil)

	var out, errOut strings.Builder
	if code := Run([]string{"--mcp"}, fakeSkill, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "registered the shuck MCP server at user scope") {
		t.Errorf("expected success message, got %q", out.String())
	}
}

func TestRunMCPClaudeMissingPrintsInstructions(t *testing.T) {
	useConfigDir(t)
	orig := lookPath
	lookPath = func(string) (string, error) { return "", fmt.Errorf("not found") }
	t.Cleanup(func() { lookPath = orig })

	var out, errOut strings.Builder
	if code := Run([]string{"--mcp"}, fakeSkill, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "claude mcp add --scope user shuck -- shuck mcp") {
		t.Errorf("expected manual instructions, got %q", out.String())
	}
}

func TestRunMCPClaudeFailureFallsBack(t *testing.T) {
	useConfigDir(t)
	stubClaude(t, []byte("boom"), fmt.Errorf("exit status 1"))

	var out, errOut strings.Builder
	if code := Run([]string{"--mcp"}, fakeSkill, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "`claude mcp add` failed") {
		t.Errorf("expected failure note on stderr, got %q", errOut.String())
	}
	if !strings.Contains(out.String(), "register the MCP server manually") {
		t.Errorf("expected manual fallback, got %q", out.String())
	}
}

func TestRunPromptYes(t *testing.T) {
	useConfigDir(t)
	var gotArgs []string
	stubClaudeFunc(t, func(_ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte("ok"), nil
	})

	// A bytes-backed stdin is not a terminal, so without a flag the MCP step is
	// skipped; exercise the prompt parser directly instead.
	var w strings.Builder
	if !promptYesNo(strings.NewReader("yes\n"), &w, "q? ") {
		t.Error("promptYesNo(yes) = false, want true")
	}
	if promptYesNo(strings.NewReader("\n"), &w, "q? ") {
		t.Error("promptYesNo(empty) = true, want false")
	}
	if promptYesNo(strings.NewReader(""), &w, "q? ") {
		t.Error("promptYesNo(EOF) = true, want false")
	}

	// Sanity-check the claude invocation shape via the --mcp path.
	var out, errOut strings.Builder
	if code := Run([]string{"--mcp"}, fakeSkill, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut.String())
	}
	want := []string{"mcp", "add", "--scope", "user", "shuck", "--", "shuck", "mcp"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("claude args = %v, want %v", gotArgs, want)
	}
}

func TestRunNonInteractiveDefaultSkips(t *testing.T) {
	useConfigDir(t)
	// No --mcp/--no-mcp and a non-terminal stdin: the MCP step is skipped and
	// the manual instructions are printed.
	var out, errOut strings.Builder
	if code := Run(nil, fakeSkill, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "no TTY; re-run with --mcp") {
		t.Errorf("expected non-interactive skip note, got %q", out.String())
	}
}

func TestRunMutuallyExclusiveMCPFlags(t *testing.T) {
	useConfigDir(t)
	var out, errOut strings.Builder
	if code := Run([]string{"--mcp", "--no-mcp"}, fakeSkill, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "mutually exclusive") {
		t.Errorf("expected mutually-exclusive error, got %q", errOut.String())
	}
}

func TestRunRejectsPositionalArg(t *testing.T) {
	useConfigDir(t)
	var out, errOut strings.Builder
	if code := Run([]string{"oops"}, fakeSkill, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "takes no positional arguments") {
		t.Errorf("expected positional-arg error, got %q", errOut.String())
	}
}

// stubClaude points lookPath at a fake claude and makes runCommand return out/err.
func stubClaude(t *testing.T, out []byte, err error) {
	t.Helper()
	stubClaudeFunc(t, func(string, ...string) ([]byte, error) { return out, err })
}

func stubClaudeFunc(t *testing.T, fn func(string, ...string) ([]byte, error)) {
	t.Helper()
	origLook, origRun := lookPath, runCommand
	lookPath = func(string) (string, error) { return "/usr/bin/claude", nil }
	runCommand = fn
	t.Cleanup(func() { lookPath, runCommand = origLook, origRun })
}
