# 🌽 shuck

[![CI](https://github.com/justanotherspy/shuck/actions/workflows/ci.yml/badge.svg)](https://github.com/justanotherspy/shuck/actions/workflows/ci.yml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/justanotherspy/shuck/badge)](https://scorecard.dev/viewer/?uri=github.com/justanotherspy/shuck)

**shuck the husk, keep the kernel.**

`shuck` is a Go CLI for GitHub PR triage, built for developers and agents who
want the signal without the fluff. Everything it does answers one question:
**what is wrong with the branch I am on right now?** `shuck monitor` answers it
without being asked — it follows your working tree and tells you when CI goes
red or a reviewer comments — and the report commands answer it on demand.

Either way you get the same thing: a red build drilled down to the failing
**steps**, with just their error logs — no tab-hopping, no log scrolling. Each
failed step is tagged with a coarse class
(`lint`/`test`/`build`/`timeout`/`oom`/`infra`) and shown alongside the job's
check-run **annotations** — the `file:line` pointers problem matchers emit — so
you land on the offending line, not in a wall of log.

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

In [Claude Code](#claude-code-plugin) you do not run any of that. The plugin's
own monitor registers the session's working tree and delivers each new CI
failure, review comment, and stale action pin as a notification when it happens
— with the failing step's logs, or the comment's diff hunk, already in the
event. Anything the tree cannot imply — another checkout's PR, one you are
waiting on — goes on the list with `shuck monitor watch <target>`. See
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
| `shuck action` | Resolve a GitHub Action to its latest tag + commit SHA for pinning. |

It is one binary and one command surface. Add `--json` to any report for stable,
machine-readable output, and install the
[Claude Code plugin](#claude-code-plugin) to have the monitor wired into a
session automatically.

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
whose cursor advances, so two consumers each see every event once. `--follow`
and `--wait` are mutually exclusive: a follow already blocks until you interrupt
it, so the combination exits `2` rather than letting you believe it will stop.

### What it notices

| Event | What it carries |
| --- | --- |
| `ci.failed` | One job went red. Body: the log (whole if short, distilled if long) plus where the raw log is cached. |
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

How much log you get depends on how much log there is. Under 8 KiB the whole
thing goes in verbatim — there is nothing to gain by excerpting it, and nothing
to wonder about having been cut. Above that you get the distilled failing steps
instead. Either way the event ends with the path the raw log was cached at, so
the full text is a file read away rather than another round trip to GitHub.

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

Watches expire after 12 hours with no client asking the monitor how things
stand — a `status`, `events`, or `poke` call refreshes every watch, not just the
one it named, so one live session keeps them all alive — and a daemon that was
started on demand exits once its last watch goes, so nothing keeps polling
GitHub after your sessions end. One you start yourself
(`shuck monitor run`) stays up regardless.

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
shuck security (s) [owner/repo | url]           # security alerts
shuck setup                                     # install the Claude Code skill + CLAUDE.md note
shuck version [--check] | shuck upgrade         # version / self-update
```

Authentication uses `GITHUB_TOKEN` (or `GH_TOKEN`), `--token`, or — last — a
logged-in `gh`, whose token is read with `gh auth token --hostname github.com`
(shuck talks to github.com and nothing else, so the host is named rather than
left to gh's default — an enterprise credential is not one to send there). That
fallback exists
because an environment variable only reaches a process that was started with it,
so a long-running session (and the monitor it starts) can never pick up a token
exported after the fact. A local
cache under `~/.cache/shuck` makes repeat runs cheap — on the same commit, logs
already downloaded are re-parsed locally instead of re-fetched.

**Exit codes are operational, gating is opt-in**: `0` means the report was
produced (even if it shows failures), `2` means an operational error. Pass
`--exit-code` to exit `1` on the verdict of the command you ran — and each gate
belongs to the command that owns it. On the default/`all` path (and on `logs`)
that verdict is the CI one alone: security findings never flip the exit code
there. Gate on those with `shuck security --exit-code`, and on action pins with
`shuck pins --exit-code`. `shuck reviews` has no verdict of its own — reviews
are not pass/fail, and it fetches no CI half for a gate to read — so
`--exit-code` is accepted there for parity but can never reach `1`.

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
| `--exit-code` | false | Exit `1` on the command's own verdict (CI gating): failing checks here; open alerts on `security`, unpinned/stale refs on `pins`; nothing on `reviews`, which has no verdict. |
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

This is for a shell script or a CI job — something with nobody to notify. Any
loop you would otherwise sit in front of belongs to the
[background monitor](#background-monitor), which tells you instead of making you
hold a terminal open.

### JSON output

`--json` emits a stable, versioned document for every report command (`shuck` /
`all`, `logs`, `reviews`, `pins`, `security`, `action`), so agents and scripts
can consume results deterministically:

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
(`[]`, never `null`). The monitor's `--json` is a different, narrower shape —
one status or watch document, or one object per event, line-delimited — and does
not follow the report schema, though each of those carries a `schema_version` of
its own.

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
monitor runs the same audit against watched trees on a 10-minute cadence — it
does not watch for edits, so a change to a workflow surfaces at the next scan,
and a pin that goes stale because the action cut a release surfaces without
anyone touching the repo at all.

## Pinning actions

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

It prefers the latest **stable** semver tag, falling back to a prerelease only
when nothing stable matches, and keeps you on the major you already chose (`@v4`
resolves within 4.x.x; a newer major is mentioned in a note rather than applied
silently). Results are cached for an hour; `--refresh` re-fetches.

For a whole checkout at once, use [`shuck pins`](#action-pin-audit) — it finds
every `uses:` that is unpinned or stale and prints the corrected line for each.

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

## Claude Code plugin

shuck ships as a [Claude Code](https://claude.com/claude-code) plugin: a
`/shuck` skill, and a plugin monitor that streams the background monitor's
events into the session. Install the `shuck` binary first (the plugin runs it
from your `PATH`), then:

```
/plugin marketplace add justanotherspy/claude-plugins
/plugin install shuck@justanotherspy
```

**The plugin monitor is the channel.** Claude Code starts `shuck monitor stream`
for the lifetime of the session; it registers the session's working tree and
prints each new event, and every line it prints reaches the session as a
notification. There is no polling and no tool call. A source the working tree
cannot imply is added from the session with `shuck monitor watch <target>` and
retired with `shuck monitor unwatch`, and its events arrive on the same stream.

One hook sits beside the stream, and it does not deliver. It exists only for
what a monitor process structurally cannot do:

| Hook | What it does | Why a monitor cannot |
| --- | --- | --- |
| `PostToolUse` (Bash) | Re-registers the session's working directory on every tool call, so entering a worktree retargets the monitor; and after a `git push` (or `gh pr create` / `gh run rerun` / …) asks it to re-check now instead of at the next interval. | A separate process is spawned once, in the directory the session opened in — it can see neither a later `cd` nor the session's tool calls. |

**No hook gates a finish.** There was a `Stop` hook that blocked a turn while
the monitor held a red build. It is retired: it decided on the last *known*
actionable event with no notion of a superseded head, so in practice it re-handed
sessions the failure they had just fixed and pushed, one poll interval before the
pass landed. Delivery without enforcement is the trade — the stream says what
happened, and what you do about it is yours. The entry point still returns
success and does nothing, so an installed copy of an older `hooks.json` cannot
resurrect the gate.

All the logic lives in the binary (`shuck monitor stream` and
`shuck monitor hook <event>`); the shell shims only exist so a session without
shuck installed degrades to one line saying so. Every path exits 0 — a
background convenience must never be why a prompt is rejected, and a stack trace
in a notification is worse than no notification. Opt out with
`SHUCK_MONITOR_DISABLE=1`. The stream reads it once, when it starts, so
exporting it mid-session stops the hook but not a stream that is already
running.

Prefer not to use the marketplace? `shuck setup` installs the same skill into
`~/.claude/skills/shuck` and adds a managed note to your `~/.claude/CLAUDE.md`.
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
