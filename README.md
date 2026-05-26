# 🌽 shuck

**shuck the husk, keep the kernel.**

`shuck` is a Go CLI that returns the *exact* failing CI step logs for a pull
request. Instead of clicking through GitHub, the `gh` CLI, or MCP calls to reach
the one error that matters, `shuck` drills GitHub Actions failures down to the
failing **steps** and prints just their error logs. It's built for devs and
agents who want the signal without the fluff.

**When CI goes red on a PR, `shuck <pr>` is the first move.** One command takes
you from "a check failed" to the precise error lines — no tab-hopping, no log
scrolling. The [Claude Code plugin](#claude-code-plugin) wires the same
capability in as a skill and an MCP server.

## What it does

Given a PR, `shuck`:

1. Resolves the target PR and its head commit.
2. Reads the PR's checks via the GitHub API using your `GITHUB_TOKEN`.
3. Finds the **failed** GitHub Actions jobs and, within each, the failed **steps**.
4. Downloads only those jobs' logs and extracts the relevant error lines.
5. Lists non-Actions failures (external checks / commit statuses) by name — no
   logs are available for those.
6. Surfaces cancelled jobs and any checks still running, with an upfront
   `N failed, M cancelled, …` summary so nothing is silently dropped.

A local cache under `~/.shuck` makes repeat runs cheap: it avoids re-downloading
logs for job attempts it has already inspected on the same commit.

## Install

### Homebrew (macOS and Linux)

```sh
brew install --cask justanotherspy/tap/shuck
```

Or tap once, then install by short name:

```sh
brew tap justanotherspy/tap
brew install --cask shuck
```

Upgrade with `brew upgrade --cask shuck`. The cask is republished to
[`justanotherspy/homebrew-tap`](https://github.com/justanotherspy/homebrew-tap)
automatically on every release.

### Install script

Download a prebuilt binary (no Go toolchain needed). The script picks the
archive for your OS/arch, verifies its checksum, and installs `shuck` into an
on-PATH directory:

```sh
curl -fsSL https://raw.githubusercontent.com/justanotherspy/shuck/main/install.sh | bash
```

Pin a version or target directory with environment variables:

```sh
curl -fsSL https://raw.githubusercontent.com/justanotherspy/shuck/main/install.sh \
  | SHUCK_VERSION=v0.2.0 SHUCK_INSTALL_DIR=/usr/local/bin bash
```

Or build from source:

```sh
go install github.com/justanotherspy/shuck@latest
```

Binaries are also available on the
[releases](https://github.com/justanotherspy/shuck/releases) page (built with
GoReleaser).

### Keeping shuck up to date

Check whether a newer release exists, then upgrade in place:

```sh
shuck version --check   # query GitHub for the latest release
shuck upgrade           # download + verify the latest and replace this binary
```

`shuck upgrade` replaces the binary wherever it currently lives (the same place
`install.sh` put it), verifying the download against `checksums.txt` first. If
shuck was installed with `go install`, it says so and leaves the upgrade to the
Go toolchain (`go install …@latest`). Plain `shuck version` is offline; it only
surfaces an "update available" hint from the last `--check`.

## Usage

```sh
shuck <owner>/<repo> <pr>   # inspect an explicit PR
shuck <pr-url>              # inspect a PR from its GitHub URL
shuck <run-url>             # inspect a single GitHub Actions run
shuck <job-url>             # inspect a single GitHub Actions job
shuck <pr>                  # owner/repo inferred from the local repo's origin
shuck                       # inspect the open PR for the current branch
shuck --watch [target]      # poll until every check finishes, then print the report
shuck setup                 # install the shuck skill + CLAUDE.md note for Claude Code
shuck version [--check]     # print the installed version; --check looks for an update
shuck upgrade               # download and install the latest release in place
```

Pass a GitHub Actions URL to skip the PR-wide scan and look at just one run or
job — handy when a CI-failure notification already points at the failing job:

```sh
shuck https://github.com/justanotherspy/shuck/actions/runs/123          # whole run
shuck https://github.com/justanotherspy/shuck/actions/runs/123/job/456  # one job
```

A run/job target reports only that run's Actions jobs (no PR-wide non-Actions
checks) and bypasses the cache, so its logs are always freshly downloaded.

Authentication uses `GITHUB_TOKEN` (or `GH_TOKEN`), or pass `--token`.

```sh
export GITHUB_TOKEN=ghp_...
shuck justanotherspy/shuck 42
```

### Flags

| Flag | Default | Description |
| --- | --- | --- |
| `--context N` | 10 | Lines of context kept around each error match. |
| `--short-threshold N` | 100 | Logs with at most this many lines are shown whole. |
| `--tail N` | 100 | Lines tailed when a long log has no error match. |
| `--pattern RE` | — | Override the error-matching regexp. |
| `--full` | false | Show full, untrimmed logs for failed steps. |
| `--max-command-lines N` | 30 | Max lines of a failed step's command to show; longer commands are truncated (`0` = no limit). |
| `--token T` | — | GitHub token (overrides `GITHUB_TOKEN`/`GH_TOKEN`). |
| `--refresh` | false | Ignore and rebuild the cache. |
| `--no-cache` | false | Do not read or write the cache. |
| `--offline` | false | Render only from cache, without network access. |
| `--json` | false | Emit machine-readable JSON (stable schema) instead of text. |
| `--version` | false | Print the shuck version and exit. |
| `--watch` | false | Poll until every check reaches a terminal state, then print the report. |
| `--interval D` | 15s | Poll interval for `--watch`. |
| `--watch-timeout D` | 0 | Give up watching after this long (`0` = no limit). |

Run `shuck --help` to print this usage and the full flag list. Flags may appear
before or after the target (`shuck owner/repo 42 --json` works), and accept one
or two dashes (`-json` and `--json` are equivalent). A leading Unicode dash is
tolerated too, so a flag mangled by macOS "smart dashes" or a rich-text
copy-paste (`shuck 42 —full`) still works.

Exit codes: `0` no failing checks · `1` failing checks reported · `2` error.
Cancelled jobs are reported in the summary but do **not** by themselves set a
non-zero exit code — cancellation is often deliberate (a superseded run, a
manual stop), so it stays `0` unless a real failure is also present.

### Watching until CI finishes

`--watch` turns shuck into a poll-until-complete loop: it re-checks the target
every `--interval` (default 15s) and returns **only when no jobs are still
running** — every check has reached a terminal state (success, failure,
cancelled, timed out, …) — then prints the final report. The exit code is the
verdict (`0` clean, `1` failures, `2` error), so it composes in scripts and
gives an agent a clear "watching is done" signal.

```sh
shuck --watch justanotherspy/shuck 42                 # wait, then print
shuck --watch --watch-timeout 30m --json <pr-url>     # bounded, machine-readable
```

Progress lines go to stderr; the final report (text or `--json`) is the only
thing on stdout. Bound an open-ended wait with `--watch-timeout D` (on timeout,
shuck prints the latest snapshot instead of blocking forever). `--watch` works
with any target (PR, run, or job) and cannot be combined with `--offline`, since
the cache does not change while you wait.

Watch keys off "no jobs still running", so if you start it before CI has
registered any runs for the head commit it reports all-clear immediately — start
watching once at least one check exists.

### JSON output

`--json` emits a stable, versioned document instead of the pretty text, so an
agent or script can consume results deterministically. The exit code is
unchanged, so `--json` still composes in pipelines.

```jsonc
{
  "schema_version": 1,
  "pr": { "owner": "…", "repo": "…", "number": 42, "title": "…",
          "head_sha": "…", "head_branch": "…" },
  "summary": { "failed": 1, "cancelled": 0, "running": 0, "other_failed": 0 },
  "failed_jobs": [
    {
      "id": 7, "run_id": 9, "name": "build", "conclusion": "failure",
      "workflow_name": "CI", "workflow_path": ".github/workflows/ci.yml",
      "failed_steps": [
        { "number": 3, "name": "Run tests", "kind": "bash",
          "command": "go test ./...", "excerpt": "--- FAIL: TestParse …" }
      ]
    }
  ],
  "cancelled_jobs": [],
  "other_checks": [],
  "running_jobs": []
}
```

For a run/job URL target the `pr` object is left zero-valued and a `run` object
carries the head context instead:

```jsonc
{
  "schema_version": 1,
  "pr": { "owner": "", "repo": "", "number": 0, "title": "", "head_sha": "", "head_branch": "" },
  "run": { "owner": "…", "repo": "…", "run_id": 123, "job_id": 456,
           "title": "…", "head_sha": "…", "head_branch": "…", "workflow_name": "CI" },
  "summary": { "failed": 1, "cancelled": 0, "running": 0, "other_failed": 0 },
  "failed_jobs": [ /* … */ ]
}
```

`schema_version` is bumped only on a breaking change; new fields (like `run`)
are added without a bump. Lists are always present (`[]`, never `null`).

### How log extraction works

For each failed step:

- **Short logs** (≤ `--short-threshold` lines) are shown whole.
- **Long logs** are grepped for error/failure tokens; `±--context` lines around
  each match are kept, with omitted spans marked.
- **Long logs with no match** are tailed to the last `--tail` lines (the
  "error only at the very end" case).

Each failed step also shows the command it ran: the full (multi-line) shell
script for a `run:` step, or the `owner/action@ref` plus the echoed `with:`
inputs and `env:` for an action step. Commands longer than `--max-command-lines`
(default 30) are truncated with a `… (N more lines) …` marker; pass
`--max-command-lines 0` for no limit.

### Example output

```
justanotherspy/shuck PR #42 — fix flaky parser   (commit a1b2c3d)

Summary: 1 failed

Workflow: CI (.github/workflows/ci.yml)
Job: build  [failure]
Steps:
  1. Set up job (success)
  2. Checkout (success)
  3. Run tests (failure)
  4. Upload coverage (skipped)

  ▸ Step 3 — Run tests (failed)
    Step command:
      * bash run:
        ```
        go test ./...
        ```
    error logs:
      ```
      --- FAIL: TestParse (0.00s)
          parse_test.go:42: expected 1, got 2
      FAIL
      ##[error]Process completed with exit code 1.
      ```
```

## MCP server

`shuck` doubles as a local [Model Context Protocol](https://modelcontextprotocol.io)
server so any MCP-aware agent can pull failing CI logs as typed tool calls
instead of scraping CLI text. Start it over stdio with:

```sh
shuck mcp
```

It exposes two read-only tools:

| Tool | Purpose | Key inputs |
| --- | --- | --- |
| `inspect_pr` | Failing CI step logs for a PR. | `repo` (`owner/repo`), `pr`, `url`; or none → the open PR for the current branch |
| `inspect_run` | Failing steps for a single Actions run or job. | `url`; or `repo` + `run_id` (+ optional `job_id`) |

Both accept the same log-extraction knobs as the CLI (`context`,
`short_threshold`, `tail`, `pattern`, `full`); `inspect_pr` also takes the cache
flags (`refresh`, `no_cache`, `offline`). Each call returns the rendered,
human-readable report as text **and** the same stable [`--json`](#json-output)
document as typed structured output, so programmatic consumers get the schema
for free. Authentication uses `GITHUB_TOKEN`/`GH_TOKEN` from the server's
environment.

Register it with any MCP client. For Claude Code, add it to `.mcp.json`:

```jsonc
{
  "mcpServers": {
    "shuck": { "command": "shuck", "args": ["mcp"] }
  }
}
```

The [Claude Code plugin](#claude-code-plugin) registers this server for you; it
runs the `shuck` on your `PATH`, so install shuck first (see [Install](#install)).

## Claude Code plugin

`shuck` also ships as a [Claude Code](https://claude.com/claude-code) plugin so
agents can pull failing CI logs for you. It adds a `/shuck` skill, an MCP server
(the `inspect_pr` / `inspect_run` tools above) that runs the `shuck` binary from
your `PATH`, and a `SessionStart` hook that checks shuck is installed, recent
enough to run the MCP server, and that a GitHub token is present.

The plugin does not install shuck — [install it yourself](#install) and keep it
current with `shuck upgrade`. Then add the marketplace and install the plugin
from within Claude Code:

```
/plugin marketplace add justanotherspy/shuck
/plugin install shuck@shuck-marketplace
```

### Without the marketplace: `shuck setup`

Prefer not to use the plugin marketplace? `shuck setup` wires the same skill in
at the user level:

```sh
shuck setup
```

It:

- installs the `shuck` skill into `~/.claude/skills/shuck/SKILL.md` (the same
  skill the plugin ships, embedded in the binary);
- adds a short, managed note to your `~/.claude/CLAUDE.md` saying you can reach
  shuck through either the skill (CLI) or the MCP; and
- offers to register the local MCP server at user scope — in an interactive
  terminal it prompts; otherwise pass `--mcp` to install it (via
  `claude mcp add --scope user shuck -- shuck mcp`) or `--no-mcp` to skip.

Re-running is safe: the skill and the CLAUDE.md block are refreshed in place, not
duplicated. Writes go under `$CLAUDE_CONFIG_DIR` (default `~/.claude`); use
`--dry-run` to preview. As with the plugin, install the `shuck` binary first.

## Development

```sh
make build   # build ./shuck
make test    # go test -race ./...
make lint    # golangci-lint (run `make lint-install` first)
make cover   # coverage report
```

## License

MIT — see [LICENSE](LICENSE).
