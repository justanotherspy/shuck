// Package setup installs shuck's Claude Code integration for users who do not use
// the plugin marketplace. `shuck setup` writes the shuck skill into the Claude
// config directory and adds a managed note to the user's CLAUDE.md. Re-running
// refreshes the skill and the note in place when either has drifted from this
// binary's copies.
package setup

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// claudeBegin/claudeEnd delimit shuck's managed section in the user's CLAUDE.md
// so re-running setup rewrites that block in place instead of appending a copy.
const (
	claudeBegin = "<!-- BEGIN shuck (managed by `shuck setup`) -->"
	claudeEnd   = "<!-- END shuck -->"
)

// claudeNote is the body shuck keeps between the markers in the user's CLAUDE.md.
// It names the `shuck` skill so the agent reaches for it, and enumerates the
// scenarios shuck covers so the agent knows when to. It leads with the monitor,
// because the loop that matters most is the one the agent does not have to
// remember to start: push, then get told.
const claudeNote = "## shuck — GitHub PR & CI triage (skill + CLI)\n" +
	"\n" +
	"`shuck` shucks the husk and keeps the kernel: it returns the **exact failing\n" +
	"CI step logs** for a GitHub PR — and covers the rest of PR/repo hygiene —\n" +
	"instead of paging through the Actions UI or hand-rolling `gh`/GitHub API calls.\n" +
	"**Whenever a GitHub Actions run, a PR, or a repo-hygiene question is in play,\n" +
	"reach for the `shuck` skill first.**\n" +
	"\n" +
	"Invoke the **`shuck` skill** for the full playbook. It drives the `shuck` CLI\n" +
	"(Bash; add `--json` to any report for structured output).\n" +
	"\n" +
	"Reach for shuck to:\n" +
	"\n" +
	"- **Find out when your PR breaks, without asking.** `shuck monitor watch`\n" +
	"  registers this working tree with a local background daemon; it follows the\n" +
	"  branch you are on, finds its open PR, and reports new CI failures, review\n" +
	"  comments, and stale action pins as they happen. `shuck monitor status`\n" +
	"  answers \"is it green?\"; `shuck monitor events --wait 10m` blocks until\n" +
	"  there is something to know, instead of sleeping and re-checking.\n" +
	"- **Debug why CI is red.** `shuck logs <pr>` returns each failed step's\n" +
	"  command and error excerpt — for a PR, a single run/job URL, or a specific\n" +
	"  re-run attempt.\n" +
	"- **Grab a run's archived artifacts.** `shuck logs --run <id|url>\n" +
	"  --download-artifacts <dir>` lists what a run uploaded and extracts each\n" +
	"  artifact zip to `<dir>/<name>/`.\n" +
	"- **See what reviewers asked for.** `shuck reviews <pr>`.\n" +
	"- **Triage security alerts.** `shuck security` — code scanning, secret\n" +
	"  scanning, Dependabot.\n" +
	"- **Keep Actions SHA-pinned.** `shuck pins` audits a checkout's workflows\n" +
	"  for unpinned or stale `uses:` references and prints the corrected line;\n" +
	"  `shuck action actions/checkout@v4` resolves one Action's latest tag +\n" +
	"  commit SHA.\n" +
	"\n" +
	"Install/keep current with `shuck upgrade`; manage this note and the skill with\n" +
	"`shuck setup`."

type options struct {
	dryRun       bool
	refreshSkill bool
}

// Run executes `shuck setup`. skill is the embedded SKILL.md content. It returns
// a process exit code: 0 on success, 2 on a usage or write error.
func Run(args []string, skill string, stdout, stderr io.Writer) int {
	o, err := parse(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	dir, err := configDir()
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}

	// --refresh-skill is the in-place refresh path `shuck upgrade` invokes on the
	// freshly-installed binary: bring the *already-installed* skill and managed
	// CLAUDE.md note up to date with this binary's embedded copies, but create
	// nothing new. A user who never ran `shuck setup` has neither artifact, so
	// both steps are quiet no-ops for them.
	if o.refreshSkill {
		if err := refreshInstalledSkill(dir, skill, o.dryRun, stdout); err != nil {
			fmt.Fprintln(stderr, "shuck:", err)
			return 2
		}
		if err := refreshClaudeMD(dir, o.dryRun, stdout); err != nil {
			fmt.Fprintln(stderr, "shuck:", err)
			return 2
		}
		return 0
	}

	if err := installSkill(dir, skill, o.dryRun, stdout); err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	if err := updateClaudeMD(dir, o.dryRun, stdout); err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	return 0
}

func parse(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("shuck setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "shuck setup — install the shuck skill into your Claude config and add a note to")
		fmt.Fprintln(stderr, "your CLAUDE.md.")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Writes under $CLAUDE_CONFIG_DIR (default ~/.claude). Re-running is safe: the skill")
		fmt.Fprintln(stderr, "and the CLAUDE.md block are refreshed in place.")
		fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}
	var o options
	fs.BoolVar(&o.dryRun, "dry-run", false, "report what would change without writing anything")
	fs.BoolVar(&o.refreshSkill, "refresh-skill", false, "refresh the already-installed skill and CLAUDE.md note in place (used by `shuck upgrade`); creates nothing new")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "shuck: setup takes no positional arguments, got %q\n", fs.Arg(0))
		return options{}, errors.New("unexpected argument")
	}
	return o, nil
}

// configDir returns the Claude Code config directory: $CLAUDE_CONFIG_DIR if set,
// otherwise ~/.claude.
func configDir() (string, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".claude"), nil
}

// installSkill writes the embedded SKILL.md to <dir>/skills/shuck/SKILL.md,
// reporting whether it installed, updated, or left the file unchanged.
func installSkill(dir, skill string, dryRun bool, stdout io.Writer) error {
	path := filepath.Join(dir, "skills", "shuck", "SKILL.md")
	existing, err := os.ReadFile(path)
	switch {
	case err == nil && string(existing) == skill:
		fmt.Fprintf(stdout, "skill already up to date: %s\n", path)
		return nil
	case err != nil && !os.IsNotExist(err):
		return fmt.Errorf("read existing skill: %w", err)
	}

	verb := "installed"
	if err == nil {
		verb = "updated"
	}
	if dryRun {
		fmt.Fprintf(stdout, "[dry-run] would write skill: %s\n", path)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create skill directory: %w", err)
	}
	// The skill is a documentation file the user (and Claude) reads; 0644 keeps
	// it world-readable on purpose.
	if err := os.WriteFile(path, []byte(skill), 0o644); err != nil { //nolint:gosec // user-readable doc file
		return fmt.Errorf("write skill: %w", err)
	}
	fmt.Fprintf(stdout, "%s skill: %s\n", verb, path)
	return nil
}

// refreshInstalledSkill rewrites the installed skill to match skill, but only
// when it already exists. It backs `shuck setup --refresh-skill`, which
// `shuck upgrade` runs on the new binary so the on-disk skill tracks the binary.
// A user who never ran `shuck setup` has no skill to refresh, so this is a quiet
// no-op for them rather than creating config files behind their back.
func refreshInstalledSkill(dir, skill string, dryRun bool, stdout io.Writer) error {
	path := filepath.Join(dir, "skills", "shuck", "SKILL.md")
	existing, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return nil
	case err != nil:
		return fmt.Errorf("read installed skill: %w", err)
	case string(existing) == skill:
		fmt.Fprintf(stdout, "installed skill already up to date: %s\n", path)
		return nil
	}
	if dryRun {
		fmt.Fprintf(stdout, "[dry-run] would refresh installed skill: %s\n", path)
		return nil
	}
	// The skill is a documentation file the user (and Claude) reads; 0644 keeps
	// it world-readable on purpose.
	if err := os.WriteFile(path, []byte(skill), 0o644); err != nil { //nolint:gosec // user-readable doc file
		return fmt.Errorf("write skill: %w", err)
	}
	fmt.Fprintf(stdout, "refreshed installed skill: %s\n", path)
	return nil
}

// refreshClaudeMD rewrites shuck's managed block in <dir>/CLAUDE.md to the
// current claudeNote, but only when the file already exists and already contains
// the markers — i.e. the user previously ran `shuck setup`. Like
// refreshInstalledSkill it is the in-place arm of `shuck upgrade`: it never
// creates CLAUDE.md and never injects the block where it isn't already, so an
// upgrade keeps an opted-in note current without writing config behind the back
// of a user who never asked for it.
func refreshClaudeMD(dir string, dryRun bool, stdout io.Writer) error {
	path := filepath.Join(dir, "CLAUDE.md")
	existing, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return nil
	case err != nil:
		return fmt.Errorf("read CLAUDE.md: %w", err)
	}
	// No managed block means the user never installed the note (or removed it on
	// purpose); refresh must not inject one — that is `shuck setup`'s job.
	if !strings.Contains(string(existing), claudeBegin) {
		return nil
	}
	block := claudeBegin + "\n" + claudeNote + "\n" + claudeEnd + "\n"
	updated, verb := spliceSection(string(existing), block)
	if verb == "unchanged" {
		fmt.Fprintf(stdout, "installed CLAUDE.md note already up to date: %s\n", path)
		return nil
	}
	if dryRun {
		fmt.Fprintf(stdout, "[dry-run] would refresh CLAUDE.md note: %s\n", path)
		return nil
	}
	// CLAUDE.md is a documentation file meant to be read; 0644 is intentional.
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil { //nolint:gosec // user-readable doc file
		return fmt.Errorf("write CLAUDE.md: %w", err)
	}
	fmt.Fprintf(stdout, "refreshed CLAUDE.md note: %s\n", path)
	return nil
}

// updateClaudeMD inserts or refreshes shuck's managed section in
// <dir>/CLAUDE.md, delimited by claudeBegin/claudeEnd.
func updateClaudeMD(dir string, dryRun bool, stdout io.Writer) error {
	path := filepath.Join(dir, "CLAUDE.md")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read CLAUDE.md: %w", err)
	}

	block := claudeBegin + "\n" + claudeNote + "\n" + claudeEnd + "\n"
	updated, verb := spliceSection(string(existing), block)
	if verb == "unchanged" {
		fmt.Fprintf(stdout, "CLAUDE.md note already up to date: %s\n", path)
		return nil
	}
	if dryRun {
		fmt.Fprintf(stdout, "[dry-run] would write CLAUDE.md note: %s\n", path)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	// CLAUDE.md is a documentation file meant to be read; 0644 is intentional.
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil { //nolint:gosec // user-readable doc file
		return fmt.Errorf("write CLAUDE.md: %w", err)
	}
	fmt.Fprintf(stdout, "%s CLAUDE.md note: %s\n", verb, path)
	return nil
}

// spliceSection returns content with shuck's managed block inserted or replaced,
// plus a verb describing the change ("added", "updated", or "unchanged"). When
// the markers are absent the block is appended after a blank line; when present,
// the span between them (inclusive) is replaced.
func spliceSection(content, block string) (result, verb string) {
	begin := strings.Index(content, claudeBegin)
	if begin >= 0 {
		if rel := strings.Index(content[begin:], claudeEnd); rel >= 0 {
			end := begin + rel + len(claudeEnd)
			// Absorb a single trailing newline after the end marker so the block's
			// own trailing newline doesn't accumulate blank lines across re-runs.
			if end < len(content) && content[end] == '\n' {
				end++
			}
			replaced := content[:begin] + block + content[end:]
			if replaced == content {
				return content, "unchanged"
			}
			return replaced, "updated"
		}
	}
	if trimmed := strings.TrimRight(content, "\n"); trimmed != "" {
		return trimmed + "\n\n" + block, "added"
	}
	return block, "added"
}
