package setup

import (
	"bytes"
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

	if code := Run(nil, fakeSkill, &stdout, &stderr); code != 0 {
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
	if !strings.Contains(stdout.String(), "installed skill:") || !strings.Contains(stdout.String(), "added CLAUDE.md note:") {
		t.Errorf("expected a note naming both files written, got %q", stdout.String())
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
		if code := Run(nil, fakeSkill, &out, &errOut); code != 0 {
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

	if !bytes.Equal(first, second) {
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
	if code := Run(nil, fakeSkill, &out, &errOut); code != 0 {
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
	if code := Run([]string{"--dry-run"}, fakeSkill, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut.String())
	}

	if _, err := os.Stat(filepath.Join(dir, "skills", "shuck", "SKILL.md")); !os.IsNotExist(err) {
		t.Errorf("dry-run created the skill file (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Errorf("dry-run created CLAUDE.md (err=%v)", err)
	}
	o := out.String()
	for _, want := range []string{"[dry-run] would write skill", "[dry-run] would write CLAUDE.md note"} {
		if !strings.Contains(o, want) {
			t.Errorf("dry-run output missing %q; got:\n%s", want, o)
		}
	}
}

func TestRunRejectsPositionalArg(t *testing.T) {
	useConfigDir(t)
	var out, errOut strings.Builder
	if code := Run([]string{"oops"}, fakeSkill, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "takes no positional arguments") {
		t.Errorf("expected positional-arg error, got %q", errOut.String())
	}
}

func TestRefreshSkillNoopWhenNotInstalled(t *testing.T) {
	dir := useConfigDir(t)
	var out, errOut strings.Builder
	if code := Run([]string{"--refresh-skill"}, fakeSkill, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut.String())
	}
	// A user who never ran setup has no skill: refresh must not create one,
	// nor touch CLAUDE.md.
	if _, err := os.Stat(filepath.Join(dir, "skills", "shuck", "SKILL.md")); !os.IsNotExist(err) {
		t.Errorf("refresh-skill created the skill file (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Errorf("refresh-skill created CLAUDE.md (err=%v)", err)
	}
	if out.String() != "" {
		t.Errorf("expected no output for a no-op refresh, got %q", out.String())
	}
}

func TestRefreshSkillUpdatesStaleAndLeavesRest(t *testing.T) {
	dir := useConfigDir(t)
	skillPath := filepath.Join(dir, "skills", "shuck", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillPath, []byte("OLD SKILL\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errOut strings.Builder
	if code := Run([]string{"--refresh-skill"}, fakeSkill, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut.String())
	}
	got, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != fakeSkill {
		t.Errorf("skill not refreshed: got %q, want %q", got, fakeSkill)
	}
	if !strings.Contains(out.String(), "refreshed installed skill") {
		t.Errorf("expected refresh note, got %q", out.String())
	}
	// CLAUDE.md must not be created by the skill-only path.
	if _, err := os.Stat(filepath.Join(dir, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Errorf("refresh-skill wrote CLAUDE.md (err=%v)", err)
	}

	// Re-running is a no-op that reports up to date.
	out.Reset()
	if code := Run([]string{"--refresh-skill"}, fakeSkill, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "already up to date") {
		t.Errorf("expected up-to-date note on second run, got %q", out.String())
	}
}

// TestRefreshUpdatesStaleNoteWhenPresent verifies the upgrade refresh path keeps
// an already-installed CLAUDE.md note current: a stale managed block is rewritten
// in place to the real claudeNote while the user's surrounding content survives.
func TestRefreshUpdatesStaleNoteWhenPresent(t *testing.T) {
	dir := useConfigDir(t)

	// An installed skill so the skill arm has something to refresh.
	skillPath := filepath.Join(dir, "skills", "shuck", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillPath, []byte("OLD SKILL\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A CLAUDE.md that already carries shuck's managed block, but stale.
	mdPath := filepath.Join(dir, "CLAUDE.md")
	stale := "# My notes\n\n" + claudeBegin + "\nOLD NOTE\n" + claudeEnd + "\n\ntrailer\n"
	if err := os.WriteFile(mdPath, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errOut strings.Builder
	if code := Run([]string{"--refresh-skill"}, fakeSkill, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut.String())
	}

	md, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(md), "OLD NOTE") {
		t.Errorf("stale note not replaced:\n%s", md)
	}
	if !strings.Contains(string(md), "shuck` skill") {
		t.Errorf("refreshed note missing skill mention:\n%s", md)
	}
	if !strings.HasPrefix(string(md), "# My notes\n") || !strings.HasSuffix(string(md), "trailer\n") {
		t.Errorf("surrounding content not preserved:\n%s", md)
	}
	if !strings.Contains(out.String(), "refreshed CLAUDE.md note") {
		t.Errorf("expected CLAUDE.md refresh note, got %q", out.String())
	}
}

// TestRefreshLeavesUnmanagedClaudeMDAlone verifies the refresh path does not
// inject the block into a CLAUDE.md that has no markers — only `shuck setup`
// adds the note; refresh never writes config the user did not opt into.
func TestRefreshLeavesUnmanagedClaudeMDAlone(t *testing.T) {
	dir := useConfigDir(t)
	skillPath := filepath.Join(dir, "skills", "shuck", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillPath, []byte("OLD SKILL\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mdPath := filepath.Join(dir, "CLAUDE.md")
	const userOnly = "# Just my notes\n\nno shuck block here\n"
	if err := os.WriteFile(mdPath, []byte(userOnly), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errOut strings.Builder
	if code := Run([]string{"--refresh-skill"}, fakeSkill, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut.String())
	}

	md, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(md) != userOnly {
		t.Errorf("unmanaged CLAUDE.md was modified:\n%s", md)
	}
	if strings.Contains(string(md), claudeBegin) {
		t.Errorf("refresh injected a managed block into an unmanaged CLAUDE.md:\n%s", md)
	}
}

// TestRefreshNoteDryRun covers refreshClaudeMD's dry-run branch: a stale managed
// note is reported but not written.
func TestRefreshNoteDryRun(t *testing.T) {
	dir := useConfigDir(t)
	skillPath := filepath.Join(dir, "skills", "shuck", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillPath, []byte("OLD SKILL\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mdPath := filepath.Join(dir, "CLAUDE.md")
	stale := claudeBegin + "\nOLD NOTE\n" + claudeEnd + "\n"
	if err := os.WriteFile(mdPath, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errOut strings.Builder
	if code := Run([]string{"--refresh-skill", "--dry-run"}, fakeSkill, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "[dry-run] would refresh CLAUDE.md note") {
		t.Errorf("expected dry-run refresh note, got %q", out.String())
	}
	got, _ := os.ReadFile(mdPath)
	if string(got) != stale {
		t.Errorf("dry-run must not rewrite CLAUDE.md, got %q", got)
	}
}

// TestRefreshNoteUpToDate covers the unchanged branch: when the installed note
// already matches claudeNote, refresh reports it and writes nothing.
func TestRefreshNoteUpToDate(t *testing.T) {
	dir := useConfigDir(t)
	skillPath := filepath.Join(dir, "skills", "shuck", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillPath, []byte(fakeSkill), 0o644); err != nil {
		t.Fatal(err)
	}
	mdPath := filepath.Join(dir, "CLAUDE.md")
	current := claudeBegin + "\n" + claudeNote + "\n" + claudeEnd + "\n"
	if err := os.WriteFile(mdPath, []byte(current), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errOut strings.Builder
	if code := Run([]string{"--refresh-skill"}, fakeSkill, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "installed CLAUDE.md note already up to date") {
		t.Errorf("expected up-to-date note, got %q", out.String())
	}
}
