# 🌽 shuck

[![CI](https://github.com/justanotherspy/shuck/actions/workflows/ci.yml/badge.svg)](https://github.com/justanotherspy/shuck/actions/workflows/ci.yml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/justanotherspy/shuck/badge)](https://scorecard.dev/viewer/?uri=github.com/justanotherspy/shuck)

**shuck the husk, keep the kernel.**

`shuck` is a Go CLI for GitHub PR triage, built for developers and agents who
want the signal without the fluff. Its core trick: when CI goes red, `shuck <pr>`
drills GitHub Actions failures down to the failing **steps** and prints just
their error logs — no tab-hopping, no log scrolling. Each failed step is tagged
with a coarse class (`lint`/`test`/`build`/`timeout`/`oom`/`infra`) and shown
alongside the job's check-run **annotations** — the `file:line` pointers problem
matchers emit — so you land on the offending line, not in a wall of log.

It is one portable binary and a GitHub token. Nothing to deploy, no webhook, no
account: a standing CI gate fails the build if the binary ever links a cloud
SDK, a serverless runtime, or a server framework.

## Don't poll CI — be told

`shuck monitor` is a **local background daemon**. Point it at a working tree and
it follows that tree: it reads the branch out of `.git/HEAD`, finds the open PR
for it, and re-checks on a cadence that tightens while CI is running. Switch
branches or worktrees and it retargets itself — you never tell it a PR number.

```sh
shuck monitor          # what it is watching, and where those PRs stand
shuck monitor watch    # follow this working tree (starts the daemon if needed)
shuck monitor events   # hand over what has happened since you last looked
```

In [Claude Code](#claude-code-plugin) you do not run any of that. The plugin
registers the session's working tree on start and delivers each new CI failure,
review comment, and stale action pin into the conversation as it happens — with
the failing step's logs, or the comment's diff hunk, already in the event. See
[Background monitor](#background-monitor).

The command set — the monitor above, and everything else one shot, no daemon:

| Command | What it does |
| --- | --- |
| `shuck` / `shuck all` | One report: failing CI logs + reviews + security alerts for a PR. |
| `shuck monitor` | The background monitor: CI, reviews, and pin drift as they happen. |
| `shuck logs` | Just the failing CI step logs (for a PR or a single Actions run/job). |
| `shuck reviews` | A PR's reviews and review-comment threads. |
| `shuck pins` | Workflow actions that are not SHA-pinned, or whose pin has gone stale. |
| `shuck security` | A repo's security alerts (code scanning, secrets, Dependabot). |
| `shuck compliance` | Check a repo's live settings against a committed `.github/compliance.yml`. |
| `shuck dependabot` | Audit `.github/dependabot.yml` against the ecosystems the repo uses. |
| `shuck action` | Resolve a GitHub Action to its latest tag + commit SHA for pinning. |
| `shuck image` | Resolve a GHCR container image to its latest tag + digest for pinning. |

Everything is available three ways: the CLI (with `--json` for stable,
machine-readable output), a local [MCP server](#mcp-server) (`shuck mcp`), and a
[Claude Code plugin](#claude-code-plugin).

## Install

### Homebrew (macOS and Linux)

```sh
brew install --cask justanotherspy/tap/shuck
```

### Install script

Downloads a prebuilt binary for your OS/arch, verifies its checksum, and
installs it into an on-PATH directory (no Go toolchain or token needed):

```sh
curl -fsSL https://raw.githubusercontent.com/justanotherspy/shuck/main/install.sh | bash
```

Pin a version or directory with `SHUCK_VERSION=v0.2.0` / `SHUCK_INSTALL_DIR=/usr/local/bin`.

### Other options

```sh
go install github.com/justanotherspy/shuck@latest                      # build from source
docker run --rm -e GITHUB_TOKEN ghcr.io/justanotherspy/shuck:latest <pr>  # multi-arch GHCR image
```

Binaries are also on the [releases](https://github.com/justanotherspy/shuck/releases)
page; release artifacts are cosign-signed with SLSA provenance and SBOMs.

### Staying up to date

```sh
shuck version --check   # query GitHub for the latest release
shuck upgrade           # download, verify, and replace this binary in place
```

## Background monitor

The monitor is the subscription half of shuck: instead of running a command and
reading a report, you register a working tree once and it tells you what
changed. Everything is local — no webhook, no server, and no credential beyond
the `GITHUB_TOKEN` the rest of shuck uses.

```sh
shuck monitor                    # what is being watched, and where it stands
shuck monitor watch [target]     # follow something (default: this working tree)
shuck monitor unwatch [target]   # stop following it
shuck monitor events             # hand over what has happened since you last looked
shuck monitor poke [target]      # re-check now, without waiting for the interval
shuck monitor stop               # shut the monitor down
shuck monitor log                # print the monitor's own log
```

A target is a directory, or a pull request — `owner/repo#42`, a PR URL,
`owner/repo 42`, or a bare number for the local repository. `shuck m` is the
shorthand. `shuck monitor events` takes `--follow` (keep printing as they
arrive), `--all` (the whole retained journal), `--wait D` (block for up to `D`
when nothing is pending), `--limit N`, and `--consumer NAME` — the identity
whose cursor advances, so two consumers each see every event once.

### What it notices

| Event | What it carries |
| --- | --- |
| `ci.failed` | One job went red. Body: the distilled failing-step logs, same as `shuck logs`. |
| `ci.passed` | Every check on a head commit finished green. Once per commit. |
| `ci.started` | First sighting of checks for a new head commit — your push registered. |
| `review.comment` | A new inline comment, with its diff hunk, ±10 lines of the file at the commit it is anchored to, and the thread it replies to. |
| `review.submitted` | An approval, changes-requested, or comment review, inline comments folded in. |
| `pr.state` | The PR merged, closed, or moved out of draft. |
| `pins.stale` | A workflow action that is unpinned or behind its release. Body: the corrected line. |
| `watch.target` | The watch retargeted itself: a branch switch, a PR found, a PR closed. |
| `monitor.error` | A poll failed. Reported once, then counted into the backoff. |

Nothing in the GitHub API says a commit is green — the jobs listing returns only
what failed, was cancelled, or is still running — so `ci.passed` is inferred
from having watched checks run and then stop. A commit whose checks had already
finished when the watch began stays silent: that is a fact about the past, not
news.

Events land in a durable append-only journal under `~/.cache/shuck/monitor/`
with a cursor per consumer, so restarting the daemon neither replays history
nor loses a failure that arrived while nobody was looking. Each consumer is
handed each event once.

### How it paces itself

Every polled pull request carries its own deadline: **12s** while a run is in
flight, **90s** for an open PR whose checks are all terminal, **5m** once it is
merged or closed. Poll errors back off exponentially to 15 minutes, and when the
token's REST quota drops below 500 remaining every interval doubles. Two watches
that resolve to the same pull request are polled once between them. A branch
with no open PR is not polled at all — the monitor just re-asks once a minute
whether one has appeared.

Watches expire after 12 hours nobody has asked about them, and a daemon that
was started on demand exits once its last watch goes — so nothing keeps polling
GitHub after your sessions end. One you start yourself (`shuck monitor run`)
stays up regardless.

Clients are short-lived: one JSON line over a unix socket in a `0700`
directory (loopback plus a random token where a unix socket is not available),
one line back, connection closed. A client that finds no daemon starts one,
detached.

## Usage

```sh
shuck [target]              # CI + reviews + security for a PR (same as `shuck all`)
shuck <owner>/<repo> <pr>   # an explicit PR; also <pr-url>, a bare <pr>, or nothing
                            # (owner/repo/PR inferred from the local checkout)
shuck <run-url> | <job-url> # a single GitHub Actions run / job (CI only)
shuck --watch [target]      # poll until every check finishes, then print the report

# Subcommands (single-letter shorthands in parentheses)
shuck monitor (m) [watch|events|status|…]       # the background monitor
shuck logs (l) [target] [--run <id|url>]        # failing CI step logs only
shuck reviews (r) [target]                      # reviews only
shuck pins (p) [dir]                            # unpinned / stale workflow actions
shuck action (a) <owner>/<action>[@<version>]   # SHA-pin a GitHub Action
shuck image (i) [owner | ghcr.io/owner/name[:tag]]  # list / digest-pin GHCR images
shuck security (s) [owner/repo | url]           # security alerts
shuck compliance (c) [owner/repo | url]         # settings vs .github/compliance.yml
shuck compliance discover [owner/repo]          # snapshot live settings into the config
shuck dependabot (d) [owner/repo | url]         # audit .github/dependabot.yml vs the repo's ecosystems
shuck dependabot discover [owner/repo]          # scaffold/extend .github/dependabot.yml
shuck dependabot fix [owner/repo]               # fill best-practice gaps in existing entries
shuck mcp                                       # run as a local MCP (stdio) server
shuck setup                                     # install the Claude Code skill (+ MCP)
shuck version [--check] | shuck upgrade         # version / self-update
```

Authentication uses `GITHUB_TOKEN` (or `GH_TOKEN`), or pass `--token`. A local
cache under `~/.cache/shuck` makes repeat runs cheap — on the same commit, logs
already downloaded are re-parsed locally instead of re-fetched.

**Exit codes are operational, gating is opt-in**: `0` means the report was
produced (even if it shows failures), `2` means an operational error. Pass
`--exit-code` to make failing checks (or open security alerts, or unpinned
actions) exit `1` for CI gating. `shuck compliance` is the exception: drift
exits `1` by default (suppress with `--exit-zero`).

### Flags

| Flag | Default | Description |
| --- | --- | --- |
| `--context N` | 10 | Lines of context kept around each error match. |
| `--short-threshold N` | 100 | Logs with at most this many lines are shown whole. |
| `--tail N` | 100 | Lines tailed when a long log has no error match. |
| `--pattern RE` | — | Override the error-matching regexp. |
| `--full` | false | Show full, untrimmed logs for failed steps. |
| `--max-command-lines N` | 30 | Max lines of a failed step's command to show (`0` = no limit). |
| `--review-comment-limit N` | 5 | Max comments shown per active review thread. |
| `--download-artifacts DIR` | — | Download a run's uploaded artifacts, each extracted to `DIR/<name>/` (run targets only). |
| `--state S` | open | Security alert states to include: `open\|all\|dismissed\|fixed\|resolved`. |
| `--token T` | — | GitHub token (overrides `GITHUB_TOKEN`/`GH_TOKEN`). |
| `--refresh` | false | Ignore and rebuild the cache. |
| `--no-cache` | false | Do not read or write the cache. |
| `--offline` | false | Render only from cache, without network access. |
| `--json` | false | Emit machine-readable JSON (stable schema) instead of text. |
| `--exit-code` | false | Exit `1` when failing checks are found (CI gating). |
| `--watch` | false | Poll until every check reaches a terminal state, then report. |
| `--interval D` | 15s | Poll interval for `--watch`. |
| `--watch-timeout D` | 0 | Give up watching after this long (`0` = no limit). |

Flags may appear before or after the target, and `-json` / `--json` are
equivalent. Run `shuck --help` (or `shuck <subcommand> --help`) for the full
usage.

## Failing CI logs

For each failed (or cancelled) GitHub Actions job, shuck identifies the failed
steps, downloads the job log, and extracts the relevant error lines:

- **Short logs** (≤ `--short-threshold` lines) are shown whole.
- **Long logs** are grepped for error tokens; `±--context` lines around each
  match are kept.
- **No match** falls back to the last `--tail` lines.

Each failed step also shows the command it ran (the `run:` script or the
`owner/action@ref` + inputs), taken from the log itself. Non-Actions checks
(external apps, commit statuses) are listed by name — the API exposes no logs
for them. Cancelled jobs are drilled too, so the step that was interrupted is
visible.

```
justanotherspy/shuck PR #42 — fix flaky parser   (commit a1b2c3d)

Summary: 1 failed

Workflow: CI (.github/workflows/ci.yml)
Job: build  [failure]

  ▸ Step 3 — Run tests (failed)
    Step command:
      * bash run:  go test ./...
    error logs:
      --- FAIL: TestParse (0.00s)
          parse_test.go:42: expected 1, got 2
      ##[error]Process completed with exit code 1.
```

Pass an Actions run/job URL to skip the PR-wide scan and inspect just that run —
handy when a failure notification already points at the job. A run URL can name
a specific attempt (`.../actions/runs/<id>/attempts/<n>`) to inspect a re-run's
earlier attempt instead of the latest. A PR "Checks" tab link
(`.../pull/<n>/checks?check_run_id=<id>`) is resolved straight to the Actions
job behind that check, so you can paste the URL you're already looking at.

### Watching until CI finishes

`--watch` polls the target every `--interval` until no jobs are still running,
then prints the final report. Progress goes to stderr; the report is the only
thing on stdout. Combine with `--exit-code` for a scriptable verdict
(`0` clean, `1` failures, `2` error) and `--watch-timeout` to bound the wait.

```sh
shuck --watch --watch-timeout 30m --json <pr-url>
```

For a loop you don't have to sit in front of, use the
[background monitor](#background-monitor) instead.

### JSON output

`--json` emits a stable, versioned document for every command, so agents and
scripts can consume results deterministically:

```jsonc
{
  "schema_version": 1,
  "pr": { "owner": "…", "repo": "…", "number": 42, "head_sha": "…" },
  "summary": { "failed": 1, "cancelled": 0, "running": 0, "other_failed": 0 },
  "failed_jobs": [
    {
      "name": "build", "workflow_name": "CI",
      "failed_steps": [
        { "name": "Run tests", "command": "go test ./...",
          "excerpt": "--- FAIL: TestParse …" }
      ]
    }
  ]
}
```

`schema_version` is bumped only on breaking changes; lists are always present
(`[]`, never `null`).

## Action pin audit

`shuck pins [dir]` audits a checkout for `uses:` references that are not pinned
to a commit SHA — `actions/checkout@v4` runs whatever commit that tag points at
today — and for pins that have fallen behind: a `# v4.2.2` comment naming a
release that has since been superseded. Both halves matter, and every finding
comes with the line to paste.

```
$ shuck pins
. — action pins

Summary: 12 references — 10 pinned, 1 stale, 1 unpinned

✗ .github/workflows/ci.yml:31  actions/checkout@v4
    "v4" is a mutable tag — each release re-points it
    uses: actions/checkout@08c6903cd8c0fde910a37f88322edcfb5dd907a8 # v4.2.2

✗ 2 references need attention — 1 unpinned, 1 behind the latest release.
```

It reads `.github/workflows/*.y{a,}ml`, the repository's own `action.yml`, and
the manifests under `.github/actions/*/` — so job steps, composite actions, and
reusable-workflow `jobs.<id>.uses` are all covered. A suggested pin stays on the
major you chose (`@v4` resolves within 4.x.x); a newer major is mentioned in the
note, never silently proposed as the fix. Local (`./…`) and `docker://`
references are reported as skipped, with what to do about them instead.

```sh
shuck pins                      # audit the current directory
shuck pins ../other-checkout    # audit somewhere else
shuck pins --offline            # list the references without resolving releases
shuck pins --exit-code          # exit 1 on an unpinned or stale reference (CI gating)
```

Auth is optional for public actions; a token only lifts the unauthenticated rate
limit. Tag lists are cached for an hour (`--refresh` re-fetches). The background
monitor runs the same audit against watched trees, re-auditing when the workflow
files change.

## Pinning actions and images

`shuck action` resolves a GitHub Action to its latest matching release tag and
the immutable commit SHA it points to — a ready-to-paste SHA pin for `uses:`
lines (what GitHub and Dependabot recommend):

```sh
$ shuck action actions/checkout@v4
actions/checkout
  tag: v4.2.2
  sha: 08c6903cd8c0fde910a37f88322edcfb5dd907a8
  pin: actions/checkout@08c6903cd8c0fde910a37f88322edcfb5dd907a8 # v4.2.2
```

`shuck image` does the same for GHCR container images, resolving to the latest
matching tag and its manifest digest (for multi-arch images, the image-index
digest — the correct pin target):

```sh
shuck image chainguard                       # list every image under an owner
shuck image ghcr.io/justanotherspy/shuck:v1  # resolve the latest v1.x.x to its digest
```

Both prefer the latest **stable** semver tag (prereleases only when nothing
stable matches); cosign signature/attestation referrer tags are never selected.
Resolving a single public image works without a token; **listing** an owner's
images uses the GitHub Packages API, which requires a classic token with
`read:packages`. Results are cached for an hour; `--refresh` re-fetches.

## Security alerts

`shuck security [owner/repo | url]` summarizes a repo's open alerts across code
scanning, secret scanning, and Dependabot in one pass. Each source degrades
independently — one that is disabled or invisible to the token is reported and
skipped, never a failure. The raw secret values are never fetched, by design.

```sh
shuck security justanotherspy/shuck      # open alerts for a repo
shuck security --state all owner/repo    # include dismissed/fixed/resolved
shuck security --exit-code owner/repo    # exit 1 on open findings (CI gating)
```

Needs a token with `security_events` (or `repo`) scope for most sources.

## Settings compliance

`shuck compliance` checks a repo's live GitHub settings — merge options,
features, security, Actions policies, and branch protection (classic rules
**and** rulesets) — against a `.github/compliance.yml` committed in the repo:

```yaml
# .github/compliance.yml — the intended settings for this repo.
repository:
  allow_merge_commit: false
  delete_branch_on_merge: true
security:
  secret_scanning: true
  vulnerability_alerts: true
actions:
  default_workflow_permissions: read
branch_protection:
  main:
    required_approving_review_count: 1
    required_status_checks: [test, lint]
```

The config is **partial by design** — only declared keys are checked — and
strict: unknown keys and invalid values are rejected, never silently skipped.
Settings the token cannot read (branch protection, security, and Actions
policies need admin access; merge settings need a classic token) are reported
as **skipped**, never a false pass. Exit is `0` when compliant, `1` on drift
(for CI gating; `--exit-zero` makes it report-only), `2` on error.

Don't write the config by hand — bootstrap or refresh it from the live settings:

```sh
shuck compliance discover            # snapshot live settings into .github/compliance.yml
shuck compliance discover --dry-run  # preview without writing
```

A missing config gets a complete snapshot; an existing one keeps only its
declared keys and has drifted values patched in place (comments preserved).

## Dependabot audit

`shuck dependabot` checks a repo's `.github/dependabot.yml` against the package
ecosystems the repo **actually uses** — detected from its manifest files
(`go.mod`, `package.json`, `Dockerfile`, `*.tf`, `*.csproj`, Actions workflows,
…). It flags ecosystems that are used but have no update entry, and best-practice
gaps in each entry (missing `groups`, `assignees`, `labels`, a `cooldown`,
`open-pull-requests-limit`, or a `commit-message` prefix):

```sh
shuck dependabot                         # audit the local checkout
shuck dependabot owner/repo              # detect ecosystems from the repo's file tree
shuck dependabot --json owner/repo       # stable JSON document
shuck dependabot --exit-code --error-on-missing-ecosystem owner/repo  # gate CI on coverage
```

Findings are `error` / `warning` / `info`. Exit is `0` whenever a report is
produced and `2` on an operational error; `--exit-code` gates on errors,
`--error-on-missing-ecosystem` makes an uncovered ecosystem an error, and
`--strict` makes warnings gate too.

Don't write the config by hand — scaffold or extend it from the detected
ecosystems:

```sh
shuck dependabot discover            # scaffold/extend .github/dependabot.yml
shuck dependabot discover --dry-run  # preview without writing
```

A missing config is scaffolded in full (weekly schedule, a minor/patch group, a
label, a cooldown, an open-PR limit, a commit-message prefix per ecosystem); an
existing one gets an entry appended for each uncovered ecosystem, comments
preserved. Add assignees yourself — shuck can't know who should own the PRs.

`discover` only closes coverage gaps; it never edits the entries already in the
config. To clear the best-practice findings on existing entries, use `fix`,
which fills in each entry's missing `groups`, `labels`, `cooldown`,
`open-pull-requests-limit`, and `commit-message` prefix in place (comments and
key order preserved, present fields untouched, no network):

```sh
shuck dependabot fix             # patch .github/dependabot.yml in place
shuck dependabot fix --dry-run   # preview without writing
```

## MCP server

`shuck mcp` runs a local [Model Context Protocol](https://modelcontextprotocol.io)
stdio server, so any MCP-aware agent can use shuck as typed tool calls:

| Tool | Purpose |
| --- | --- |
| `inspect_logs` | Failing CI step logs for a PR or a single Actions run. |
| `inspect_reviews` | A PR's reviews and review-comment threads. |
| `inspect_security` | A repo's security alerts. |
| `check_compliance` | Check a repo's settings against its `.github/compliance.yml`. |
| `audit_dependabot` | Audit a repo's `.github/dependabot.yml` against the ecosystems it uses. |
| `check_pins` | Find workflow actions that are unpinned or whose SHA pin is stale. |
| `inspect_action` | Resolve an Action to its latest tag + SHA for pinning. |
| `inspect_images` | List GHCR images, or resolve one to its digest. |
| `monitor_status` | What the background monitor is watching, and where those PRs stand. |
| `monitor_events` | Collect what the monitor noticed; `wait_seconds` blocks until something happens. |
| `monitor_watch` | Follow a working tree (default) or pin one pull request. |
| `monitor_unwatch` | Stop following something. |

`monitor_events` with `wait_seconds` is how an agent waits for CI without
polling: watch the tree, push, then block until the monitor has something to
say.

Each tool returns the rendered text report **and** the matching stable JSON
document as structured output. Register it with any MCP client, e.g. in
`.mcp.json` for Claude Code:

```jsonc
{ "mcpServers": { "shuck": { "command": "shuck", "args": ["mcp"] } } }
```

## Claude Code plugin

shuck ships as a [Claude Code](https://claude.com/claude-code) plugin: a
`/shuck` skill, the MCP server above, and the hooks that wire the background
monitor into a session. Install the `shuck` binary first (the plugin runs it
from your `PATH`), then:

```
/plugin marketplace add justanotherspy/claude-plugins
/plugin install shuck@justanotherspy
```

The hooks are the whole integration — no polling, no tool call:

| Hook | What it does |
| --- | --- |
| `SessionStart` | Checks the binary and token, registers the session's working tree, and fast-forwards the session's cursor so it hears what happens next, not the last hour of history. |
| `UserPromptSubmit` | Delivers what is new as a `<shuck-monitor>` block. |
| `PostToolUse` (Bash) | After a `git push` (or `gh pr create` / `gh run rerun` / …) asks the monitor to re-check now instead of at the next interval. |
| `Stop` | Hands over anything actionable and asks for one more turn rather than finishing on a red build. |
| `SessionEnd` | Retires the session's cursor. |

All the logic lives in the binary (`shuck monitor hook <event>`); the shell shim
only exists so a session without shuck installed degrades to silence. Every path
exits 0 — a background convenience must never be why a prompt is rejected. Opt
out with `SHUCK_MONITOR_DISABLE=1`, or just the `Stop` hook with
`SHUCK_MONITOR_NO_STOP=1`.

Prefer not to use the marketplace? `shuck setup` installs the same skill into
`~/.claude/skills/shuck`, adds a managed note to your `~/.claude/CLAUDE.md`, and
optionally registers the MCP server at user scope (`--mcp` / `--no-mcp`).
Re-running is safe; `--dry-run` previews.

## Development

```sh
make tools   # install pinned dev tools (golangci-lint, goreleaser, …)
make build   # build ./bin/shuck
make test    # go test -race with coverage
make lint    # golangci-lint
make ci      # what CI runs: deps + lint + modernize-check + test + cover-check + build
```

Run `make help` for the full target list. How it all fits together:
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md). What's next and what's missing:
[docs/PLAN.md](docs/PLAN.md). See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT — see [LICENSE](LICENSE).
