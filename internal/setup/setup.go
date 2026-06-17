// Package setup installs shuck's Claude Code integration for users who do not use
// the plugin marketplace. `shuck setup` writes the shuck skill into the Claude
// config directory, adds a managed note to the user's CLAUDE.md, and optionally
// registers the local MCP server at user scope. Re-running refreshes the skill
// and the note in place when either has drifted from this binary's copies.
package setup

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/term"
)

// claudeBegin/claudeEnd delimit shuck's managed section in the user's CLAUDE.md
// so re-running setup rewrites that block in place instead of appending a copy.
const (
	claudeBegin = "<!-- BEGIN shuck (managed by `shuck setup`) -->"
	claudeEnd   = "<!-- END shuck -->"
)

// claudeNote is the body shuck keeps between the markers in the user's CLAUDE.md.
// It names the `shuck` skill so the agent reaches for it, and enumerates the
// scenarios shuck covers so the agent knows when to. shuck is reachable through
// the skill (which drives the CLI) or the MCP tools, so both are mentioned.
const claudeNote = "## shuck — GitHub PR & CI triage (skill + CLI + MCP)\n" +
	"\n" +
	"`shuck` shucks the husk and keeps the kernel: it returns the **exact failing\n" +
	"CI step logs** for a GitHub PR — and covers the rest of PR/repo hygiene —\n" +
	"instead of paging through the Actions UI or hand-rolling `gh`/GitHub API calls.\n" +
	"**Whenever a GitHub Actions run, a PR, or a repo-hygiene question is in play,\n" +
	"reach for the `shuck` skill first.**\n" +
	"\n" +
	"Invoke the **`shuck` skill** for the full playbook. It drives shuck either way —\n" +
	"the `shuck` CLI (Bash; add `--json` for structured output) or the shuck **MCP\n" +
	"tools** — same engine, same data, so use whichever is wired up. Only the CLI\n" +
	"can `--watch` CI to completion; the MCP tools are one-shot snapshots.\n" +
	"\n" +
	"Reach for shuck to:\n" +
	"\n" +
	"- **Monitor a PR to a verdict.** Every time you open a PR or push new commits,\n" +
	"  start `shuck --watch --exit-code <pr>` in the background and act on the\n" +
	"  result — green, or the exact failing-step logs. Close the loop on every push.\n" +
	"- **Debug why CI is red.** `shuck logs <pr>` / `inspect_logs` returns each\n" +
	"  failed step's command and error excerpt — for a PR, a single run/job URL, or\n" +
	"  a specific re-run attempt.\n" +
	"- **Grab a run's archived artifacts.** `shuck logs --run <id|url>\n" +
	"  --download-artifacts <dir>` (MCP: `download_artifacts`) lists what a run\n" +
	"  uploaded and extracts each artifact zip to `<dir>/<name>/`.\n" +
	"- **See what reviewers asked for.** `shuck reviews <pr>` / `inspect_reviews`.\n" +
	"- **Triage security alerts.** `shuck security` / `inspect_security` — code\n" +
	"  scanning, secret scanning, Dependabot.\n" +
	"- **Check settings against policy.** `shuck compliance` / `check_compliance`\n" +
	"  vs a committed `.github/compliance.yml`.\n" +
	"- **Audit Dependabot coverage.** `shuck dependabot` / `audit_dependabot`.\n" +
	"- **Pin Actions and images.** `shuck action actions/checkout@v4` /\n" +
	"  `inspect_action` resolves an Action's latest tag + commit SHA; `shuck image`\n" +
	"  / `inspect_images` resolves a GHCR image's latest digest.\n" +
	"\n" +
	"MCP tools: `inspect_logs`, `inspect_reviews`, `inspect_security`,\n" +
	"`check_compliance`, `audit_dependabot`, `inspect_action`, `inspect_images`.\n" +
	"\n" +
	"Install/keep current with `shuck upgrade`; manage this note and the skill with\n" +
	"`shuck setup`."

const (
	mcpName    = "shuck"
	mcpCommand = "shuck"
)

// lookPath and runCommand are indirected so tests can stub the claude CLI.
var (
	lookPath   = exec.LookPath
	runCommand = func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).CombinedOutput()
	}
)

type options struct {
	mcp          bool
	noMCP        bool
	dryRun       bool
	refreshSkill bool
}

// Run executes `shuck setup`. skill is the embedded SKILL.md content; stdin is
// used to prompt about the optional MCP step when it is a terminal. It returns a
// process exit code: 0 on success, 2 on a usage or write error.
func Run(args []string, skill string, stdin io.Reader, stdout, stderr io.Writer) int {
	o, err := parse(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if o.mcp && o.noMCP {
		fmt.Fprintln(stderr, "shuck: --mcp and --no-mcp are mutually exclusive")
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
	// nothing new and never touch the MCP or prompt. A user who never ran
	// `shuck setup` has neither artifact, so both steps are quiet no-ops for them.
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
	maybeInstallMCP(o, stdin, stdout, stderr)
	return 0
}

func parse(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("shuck setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "shuck setup — install the shuck skill into your Claude config, add a note to your")
		fmt.Fprintln(stderr, "CLAUDE.md, and optionally register the shuck MCP server at user scope.")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Writes under $CLAUDE_CONFIG_DIR (default ~/.claude). Re-running is safe: the skill")
		fmt.Fprintln(stderr, "and the CLAUDE.md block are refreshed in place.")
		fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}
	var o options
	fs.BoolVar(&o.mcp, "mcp", false, "register the shuck MCP server at user scope without prompting")
	fs.BoolVar(&o.noMCP, "no-mcp", false, "skip the MCP server step without prompting")
	fs.BoolVar(&o.dryRun, "dry-run", false, "report what would change without writing anything")
	fs.BoolVar(&o.refreshSkill, "refresh-skill", false, "refresh the already-installed skill and CLAUDE.md note in place (used by `shuck upgrade`); creates nothing new and skips the MCP step")
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

// maybeInstallMCP decides whether to register the user-scope MCP server (per the
// flags or an interactive prompt) and acts on it.
func maybeInstallMCP(o options, stdin io.Reader, stdout, stderr io.Writer) {
	want := false
	switch {
	case o.noMCP:
		fmt.Fprintln(stdout, "skipping MCP server registration (--no-mcp)")
	case o.mcp:
		want = true
	case isInteractive(stdin):
		want = promptYesNo(stdin, stdout, "Register the shuck MCP server at user scope? [y/N] ")
		if !want {
			fmt.Fprintln(stdout, "skipping MCP server registration")
		}
	default:
		fmt.Fprintln(stdout, "not registering the MCP server (no TTY; re-run with --mcp to install it).")
		printMCPInstructions(stdout)
	}
	if !want {
		return
	}
	if o.dryRun {
		fmt.Fprintln(stdout, "[dry-run] would register the shuck MCP server at user scope")
		return
	}
	installMCP(stdout, stderr)
}

// installMCP registers the shuck MCP server at user scope. It prefers the claude
// CLI (`claude mcp add --scope user shuck -- shuck mcp`); if claude is not on
// PATH or the command fails, it prints manual instructions. It never fails the
// overall setup — the skill and CLAUDE.md note are already in place.
func installMCP(stdout, stderr io.Writer) {
	claude, err := lookPath("claude")
	if err != nil {
		fmt.Fprintln(stdout, "claude CLI not found on PATH; register the MCP server manually:")
		printMCPInstructions(stdout)
		return
	}
	out, err := runCommand(claude, "mcp", "add", "--scope", "user", mcpName, "--", mcpCommand, "mcp")
	if err != nil {
		fmt.Fprintf(stderr, "shuck: `claude mcp add` failed: %v\n", err)
		if trimmed := strings.TrimRight(string(out), "\n"); trimmed != "" {
			fmt.Fprintln(stderr, trimmed)
		}
		fmt.Fprintln(stdout, "register the MCP server manually:")
		printMCPInstructions(stdout)
		return
	}
	fmt.Fprintln(stdout, "registered the shuck MCP server at user scope (claude mcp add --scope user)")
}

func printMCPInstructions(stdout io.Writer) {
	fmt.Fprintln(stdout, "  claude mcp add --scope user shuck -- shuck mcp")
	fmt.Fprintln(stdout, `or add this to your MCP config (e.g. the "mcpServers" map in ~/.claude.json):`)
	fmt.Fprintln(stdout, `  "shuck": { "command": "shuck", "args": ["mcp"] }`)
}

// isInteractive reports whether r is a real terminal, used to decide whether to
// prompt about the optional MCP step. A pipe, file, or /dev/null is not a
// terminal, so non-interactive runs skip the prompt and stay scriptable.
func isInteractive(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// promptYesNo writes prompt to w and reads a line from r, returning true only for
// an explicit yes (y/yes, case-insensitive). EOF or anything else is a no.
func promptYesNo(r io.Reader, w io.Writer, prompt string) bool {
	fmt.Fprint(w, prompt)
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}
