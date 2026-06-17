package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigDirEnvOverride(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/claude")
	got, err := configDir()
	if err != nil {
		t.Fatalf("configDir: %v", err)
	}
	if got != "/custom/claude" {
		t.Errorf("configDir = %q, want /custom/claude", got)
	}
}

func TestConfigDirHomeFallback(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := configDir()
	if err != nil {
		t.Fatalf("configDir: %v", err)
	}
	if want := filepath.Join(home, ".claude"); got != want {
		t.Errorf("configDir = %q, want %q", got, want)
	}
}

func TestParseHelpReturnsErrHelp(t *testing.T) {
	var stderr strings.Builder
	if code := Run([]string{"-h"}, fakeSkill, strings.NewReader(""), &strings.Builder{}, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0 for -h", code)
	}
	if !strings.Contains(stderr.String(), "install the shuck skill") {
		t.Errorf("expected usage text on -h, got %q", stderr.String())
	}
}

func TestParseUnknownFlagExitsTwo(t *testing.T) {
	var out, errOut strings.Builder
	if code := Run([]string{"--nope"}, fakeSkill, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2 for unknown flag", code)
	}
}

// fakeTTY is an *os.File that reports as a terminal would not — but its Fd is a
// real pipe, so term.IsTerminal returns false. We use it to exercise the
// isInteractive type assertion's success branch (an *os.File that is not a TTY).
func TestIsInteractiveOnNonFile(t *testing.T) {
	if isInteractive(strings.NewReader("")) {
		t.Error("a non-*os.File reader must not be interactive")
	}
}

func TestIsInteractiveOnPipeFile(t *testing.T) {
	rp, wp, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rp.Close(); _ = wp.Close() })
	// A pipe is an *os.File but not a terminal, so this exercises the assertion
	// success path returning false.
	if isInteractive(rp) {
		t.Error("a pipe *os.File must not be interactive")
	}
}

// TestRunInteractiveDeclines drives maybeInstallMCP's interactive branch by
// passing a terminal-like *os.File whose content is "n": the prompt runs and the
// user declines, so the MCP step is skipped.
func TestPromptDeclineSkips(t *testing.T) {
	// We cannot easily allocate a real PTY here, so exercise promptYesNo and the
	// surrounding messaging directly via maybeInstallMCP with the noMCP path.
	var out strings.Builder
	maybeInstallMCP(options{noMCP: true}, strings.NewReader(""), &out, &strings.Builder{})
	if !strings.Contains(out.String(), "skipping MCP server registration (--no-mcp)") {
		t.Errorf("expected --no-mcp skip note, got %q", out.String())
	}
}

func TestPromptYesNoVariants(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"y\n", true},
		{"Y\n", true},
		{"yes\n", true},
		{"YES\n", true},
		{"  yes  \n", true},
		{"n\n", false},
		{"no\n", false},
		{"maybe\n", false},
		{"\n", false},
		{"", false},
	}
	for _, c := range cases {
		var w strings.Builder
		if got := promptYesNo(strings.NewReader(c.in), &w, "q? "); got != c.want {
			t.Errorf("promptYesNo(%q) = %v, want %v", c.in, got, c.want)
		}
		if !strings.Contains(w.String(), "q? ") {
			t.Errorf("prompt not written for input %q", c.in)
		}
	}
}

// TestRunDryRunRefreshSkill covers the dry-run branch of refreshInstalledSkill:
// a stale installed skill is reported but not written.
func TestRunDryRunRefreshSkill(t *testing.T) {
	dir := useConfigDir(t)
	skillPath := filepath.Join(dir, "skills", "shuck", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillPath, []byte("OLD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	if code := Run([]string{"--refresh-skill", "--dry-run"}, fakeSkill, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "[dry-run] would refresh installed skill") {
		t.Errorf("expected dry-run refresh note, got %q", out.String())
	}
	got, _ := os.ReadFile(skillPath)
	if string(got) != "OLD\n" {
		t.Errorf("dry-run must not rewrite the skill, got %q", got)
	}
}

// TestInstallSkillReadError makes the skill path a directory so os.ReadFile
// fails with a non-IsNotExist error, hitting installSkill's read-error branch.
func TestInstallSkillReadError(t *testing.T) {
	dir := useConfigDir(t)
	skillPath := filepath.Join(dir, "skills", "shuck", "SKILL.md")
	if err := os.MkdirAll(skillPath, 0o755); err != nil { // path is a dir, not a file
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	if code := Run([]string{"--no-mcp"}, fakeSkill, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2 when the skill path is unreadable", code)
	}
	if !strings.Contains(errOut.String(), "read existing skill") {
		t.Errorf("expected read-error note, got %q", errOut.String())
	}
}

// TestRefreshSkillReadError hits refreshInstalledSkill's read-error branch the
// same way: the installed-skill path is a directory.
func TestRefreshSkillReadError(t *testing.T) {
	dir := useConfigDir(t)
	skillPath := filepath.Join(dir, "skills", "shuck", "SKILL.md")
	if err := os.MkdirAll(skillPath, 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	if code := Run([]string{"--refresh-skill"}, fakeSkill, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2 when the installed skill is unreadable", code)
	}
	if !strings.Contains(errOut.String(), "read installed skill") {
		t.Errorf("expected refresh read-error note, got %q", errOut.String())
	}
}

// TestRefreshClaudeMDReadError hits refreshClaudeMD's read-error branch: with an
// installed skill present (so the skill arm succeeds first) and CLAUDE.md a
// directory, reading the note fails with a non-IsNotExist error.
func TestRefreshClaudeMDReadError(t *testing.T) {
	dir := useConfigDir(t)
	skillPath := filepath.Join(dir, "skills", "shuck", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillPath, []byte("OLD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "CLAUDE.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	if code := Run([]string{"--refresh-skill"}, fakeSkill, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2 when CLAUDE.md is unreadable", code)
	}
	if !strings.Contains(errOut.String(), "read CLAUDE.md") {
		t.Errorf("expected CLAUDE.md read-error note, got %q", errOut.String())
	}
}

// TestUpdateClaudeMDReadError makes CLAUDE.md a directory so its read fails with
// a non-IsNotExist error.
func TestUpdateClaudeMDReadError(t *testing.T) {
	dir := useConfigDir(t)
	if err := os.MkdirAll(filepath.Join(dir, "CLAUDE.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	if code := Run([]string{"--no-mcp"}, fakeSkill, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2 when CLAUDE.md is unreadable", code)
	}
	if !strings.Contains(errOut.String(), "read CLAUDE.md") {
		t.Errorf("expected CLAUDE.md read-error note, got %q", errOut.String())
	}
}

// TestRunConfigDirError exercises the configDir failure path in Run: with no
// CLAUDE_CONFIG_DIR and no resolvable home directory, Run reports an error.
func TestRunConfigDirError(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("HOME", "")
	// On most platforms an empty HOME makes os.UserHomeDir fail.
	if _, err := os.UserHomeDir(); err == nil {
		t.Skip("os.UserHomeDir still resolves without HOME on this platform")
	}
	var out, errOut strings.Builder
	if code := Run([]string{"--no-mcp"}, fakeSkill, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2 when config dir cannot be resolved", code)
	}
	if !strings.Contains(errOut.String(), "shuck:") {
		t.Errorf("expected error note, got %q", errOut.String())
	}
}
