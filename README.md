# 🌽 shuck

[![CI](https://github.com/justanotherspy/shuck/actions/workflows/ci.yml/badge.svg)](https://github.com/justanotherspy/shuck/actions/workflows/ci.yml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/justanotherspy/shuck/badge)](https://scorecard.dev/viewer/?uri=github.com/justanotherspy/shuck)

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
6. Surfaces cancelled jobs — drilling their logs too, so the step that was
   interrupted (and what it was doing) is visible — and any checks still
   running, with an upfront `N failed, M cancelled, …` summary so nothing is
   silently dropped.

A local cache under `~/.cache/shuck` makes repeat runs cheap: it avoids re-downloading
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

No token is required. The script resolves the latest release via the GitHub
REST API, and if that is unavailable — e.g. a shared/CI egress IP hits the
unauthenticated 60/hr limit and gets a `403` — it falls back to the
`github.com` releases redirect, which is not rate-limited. To skip discovery
entirely, set `SHUCK_VERSION`; to lift the API limit, set `GITHUB_TOKEN`
(or `GH_TOKEN`).

Or build from source:

```sh
go install github.com/justanotherspy/shuck@latest
```

Binaries are also available on the
[releases](https://github.com/justanotherspy/shuck/releases) page (built with
GoReleaser).

### Docker

A multi-arch image (linux/amd64, linux/arm64) is published to GHCR on each
release and tagged `:latest`, plus `:edge` for `main`. It runs as a non-root
user on a minimal static base, and images are cosign-signed with SLSA build
provenance:

```sh
docker run --rm -e GITHUB_TOKEN ghcr.io/justanotherspy/shuck:latest <pr>
```

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
shuck <owner>/<repo> <pr>   # CI + reviews + security for an explicit PR (same as `shuck all`)
shuck <pr-url>              # a PR from its GitHub URL
shuck <run-url>             # a single GitHub Actions run (CI only)
shuck <job-url>             # a single GitHub Actions job (CI only)
shuck <pr>                  # owner/repo inferred from the local repo's origin
shuck                       # the open PR for the current branch
shuck --watch [target]      # poll until every check finishes, then print the report
shuck logs [target] [--run <id|url>]  # (l) failing CI step logs for a PR or a single run
shuck reviews [target]      # (r) a PR's reviews and review-comment threads
shuck all [target]          # CI + reviews + security (the default)
shuck action <owner>/<action>[@<version>]  # (a) resolve an Action to its latest tag + SHA for pinning
shuck image [owner | ghcr.io/owner/name[:tag]]  # (i) list GHCR images, or resolve one to its latest digest
shuck security [owner/repo | url]  # (s) summarize a repo's security alerts (code scanning, secrets, Dependabot)
shuck compliance [owner/repo | url]  # (c) check a repo's settings against its .github/compliance.yml
shuck compliance discover [owner/repo]  # snapshot the live settings into .github/compliance.yml
shuck setup                 # install the shuck skill + CLAUDE.md note for Claude Code
shuck version [--check]     # print the installed version; --check looks for an update
shuck upgrade               # download and install the latest release in place
```

Running `shuck` with no subcommand reports a PR's failing CI, its reviews, and
the repo's security alerts together; use `logs` / `reviews` (or their `l` / `r`
shorthands) to focus on one dimension.

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
Cancelled jobs are reported (with the interrupted step's last log output, when
a log exists) but do **not** by themselves set a non-zero exit code —
cancellation is often deliberate (a superseded run, a manual stop), so it stays
`0` unless a real failure is also present.

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

### Pinning GitHub Actions to a SHA

`shuck action <owner>/<action>` resolves an Action to the latest release tag and
the immutable commit SHA it points to, so you can pin a workflow `uses:` line to
a SHA (what GitHub and Dependabot recommend) without hunting through the
Releases page:

```sh
shuck action actions/checkout            # latest stable release
shuck action actions/checkout@v4         # latest v4.x.x
shuck action actions/checkout@4.2        # latest 4.2.x
shuck action actions/checkout 4.2        # version as a separate argument
shuck action github/codeql-action/init   # a subpath action resolves its repo's tags
```

It prints the resolved tag, the SHA, and a ready-to-paste pin line:

```
actions/checkout
  tag: v4.2.2
  sha: 08c6903cd8c0fde910a37f88322edcfb5dd907a8
  pin: actions/checkout@08c6903cd8c0fde910a37f88322edcfb5dd907a8 # v4.2.2
```

Drop the `pin:` value straight after `uses:` in your workflow. The latest
**stable** release wins; a prerelease (e.g. `-rc1`) is chosen only when nothing
stable matches. Add `--json` for a machine-readable document:

```jsonc
{
  "schema_version": 1,
  "action": "actions/checkout", "owner": "actions", "repo": "checkout",
  "requested": "v4", "tag": "v4.2.2",
  "sha": "08c6903cd8c0fde910a37f88322edcfb5dd907a8",
  "ref": "actions/checkout@08c6903cd8c0fde910a37f88322edcfb5dd907a8",
  "pin": "actions/checkout@08c6903cd8c0fde910a37f88322edcfb5dd907a8 # v4.2.2"
}
```

Resolved tags are cached under `~/.cache/shuck/actions/<owner>/<repo>` for an hour
to avoid re-listing; `--refresh` re-fetches immediately. Authentication is optional
for public repos — set `GITHUB_TOKEN`/`GH_TOKEN` (or `--token`) to lift the
unauthenticated rate limit.

### Pinning container images to a digest

`shuck image` does for GHCR container images what `shuck action` does for
Actions: it resolves an image to its latest matching tag and the immutable
manifest digest (`sha256:…`) it points to, so a `FROM` line or a workflow's
`container:` reference can be pinned to a digest:

```sh
shuck image                                      # list every image under the local repo's owner
shuck image chainguard                           # list every image under an owner
shuck image ghcr.io/justanotherspy/shuck         # resolve one image to its latest digest
shuck image ghcr.io/justanotherspy/shuck:v1      # latest v1.x.x
shuck image ghcr.io/justanotherspy/shuck:latest  # an exact (non-semver) tag, e.g. latest
```

Resolving one image prints the tag, the digest, and a ready-to-paste pin line:

```
ghcr.io/justanotherspy/shuck
  tag:    v1.2.3
  digest: sha256:8f4e0ab2…
  pin:    ghcr.io/justanotherspy/shuck@sha256:8f4e0ab2… # v1.2.3
```

For a multi-arch image the digest is the **image-index** digest — the correct
value to pin, since it covers every platform. Tag selection mirrors
`shuck action`: the latest **stable** semver tag wins, a prerelease is chosen
only when nothing stable matches, and an image with no semver tags falls back
to its most recently pushed version (preferring a `latest` tag).

Listing every image under an owner uses the GitHub Packages API and **needs a
token** with the `read:packages` scope (the API has no anonymous enumeration).
Resolving a single **public** `ghcr.io/owner/name` image works without a token
via the anonymous registry API; private images need the token. Listings are
cached under `~/.cache/shuck/images/<owner>` for an hour; `--refresh`
re-fetches. Add `--json` for a machine-readable document:

```jsonc
{
  "schema_version": 1,
  "image": "ghcr.io/justanotherspy/shuck",
  "registry": "ghcr.io", "owner": "justanotherspy", "name": "shuck",
  "requested": "", "tag": "v1.2.3",
  "digest": "sha256:8f4e0ab2…",
  "ref": "ghcr.io/justanotherspy/shuck@sha256:8f4e0ab2…",
  "pin": "ghcr.io/justanotherspy/shuck@sha256:8f4e0ab2… # v1.2.3"
}
```

### Security alerts

`shuck security [owner/repo | url]` pulls a repository's GitHub security alerts
from every available source and summarizes them in one pass — so a human or an
agent can see what to fix without clicking through the Security tab:

```sh
shuck security                         # the repo of the local working directory
shuck security justanotherspy/shuck    # an explicit repository
shuck security https://github.com/owner/repo   # any github.com/<owner>/<repo>[/...] URL
shuck security --state all owner/repo  # include dismissed/fixed/resolved, not just open
shuck security --json owner/repo       # the stable JSON document
shuck security --exit-code owner/repo  # exit 1 when open alerts are found (CI gating)
```

It covers three sources:

- **Code scanning** (e.g. CodeQL) — rule, severity, and `file:line`.
- **Secret scanning** — secret type and the file locations it was found in. The
  **raw secret value is never fetched or shown**, by design.
- **Dependabot** — the vulnerable package, its ecosystem, the fix version, and
  the CVE/GHSA IDs. npm **malware** advisories surface here too (there is no
  separate malware endpoint).

Each source degrades independently: one that is not enabled (or not visible to
your token) is reported and skipped rather than failing the command, so a repo
with only some features enabled still produces output. By default only **open**
alerts are shown; widen with `--state open|all|dismissed|fixed|resolved`.

```
justanotherspy/shuck — security alerts (open)

Summary: 2 alerts — 1 critical, 1 high

Dependabot (2):
  ● critical  npm  lodash → 4.17.21   GHSA-jf85-cpcp-j695  CVE-2019-10744
      Prototype pollution in lodash
      vulnerable: < 4.17.21
      manifest: package-lock.json
      https://github.com/justanotherspy/shuck/security/dependabot/12
  ● high  pip  django → 3.2.4   GHSA-xxxx  CVE-2021-33203
      Potential directory traversal via admindocs
      manifest: requirements.txt
      https://github.com/justanotherspy/shuck/security/dependabot/9

Code scanning: not enabled or no access — skipped.
Secret scanning: not enabled or no access — skipped.
```

Results are cached under `~/.cache/shuck/security/<owner>/<repo>` for an hour;
`--refresh` re-fetches immediately. Security data — especially on private repos —
needs a token (`GITHUB_TOKEN`/`GH_TOKEN`, or `--token`) with the
`security_events` (or `repo`) scope. The exit code is `0` on any successful run
and `2` only on an operational error; pass `--exit-code` to make open findings
exit `1` for CI gating.

### Settings compliance

`shuck compliance [owner/repo | url]` (alias `c`) checks a repository's live
GitHub settings against a `.github/compliance.yml` committed in the repo. That
file is the **definitive statement of the repo's intended settings** — merge
options, features, security, and branch protection — so a CI job can fail when a
setting drifts from policy:

```sh
shuck compliance                       # the local checkout's .github/compliance.yml
shuck compliance justanotherspy/shuck  # fetch the config from the repo and check it
shuck compliance --config policy.yaml owner/repo   # use an explicit config file
shuck compliance --json owner/repo     # the stable JSON document
shuck compliance --exit-zero owner/repo  # report-only (never fail the build)
```

The config is **partial by design**: only the keys it declares are checked, so a
repo can assert just what it cares about. A typo'd key is rejected rather than
silently ignored, and a setting the token cannot read (branch protection and
security need admin/`repo` access) is reported as **skipped**, never a false
pass.

```yaml
# .github/compliance.yml — the intended settings for this repo.
repository:
  visibility: public
  allow_merge_commit: false
  allow_squash_merge: true
  delete_branch_on_merge: true
  has_wiki: false
security:
  secret_scanning: true
  secret_scanning_push_protection: true
  vulnerability_alerts: true
branch_protection:
  main:
    required_approving_review_count: 1
    dismiss_stale_reviews: true
    enforce_admins: true
    required_linear_history: true
    allow_force_pushes: false
    required_status_checks:
      - test
      - lint
```

```
justanotherspy/shuck — compliance
config: .github/compliance.yml

Summary: 12 checked — 11 pass, 1 fail

Repository:
  ✓ allow_merge_commit = false
  ✗ has_wiki: want false, got true
  ...

✗ Not compliant — 1 setting(s) drifted from the config.
```

Config discovery: a bare `shuck compliance` reads the checked-out file (the CI
case); an explicit `owner/repo` fetches `.github/compliance.yml` from the repo
(use `--ref` for a branch/tag/SHA); `--config` overrides both with a local path.
The exit code is `0` when compliant, `1` when a setting drifted (for CI gating),
and `2` on an operational error; `--exit-zero` makes it report-only.

#### Bootstrapping the config: `shuck compliance discover`

Don't write the config by hand — `shuck compliance discover [owner/repo | url]`
reads the repository's live settings (general, security, and the default
branch's protection) and writes them to the local `.github/compliance.yml`:

```sh
shuck compliance discover              # snapshot the local repo's live settings
shuck compliance discover owner/repo   # snapshot an explicit repo's settings
shuck compliance discover --dry-run    # preview without writing
shuck compliance discover --json       # the stable JSON document
```

- **No config yet** → a complete snapshot of every readable setting is created
  (commit it, trim it down to what you care about, and gate CI with
  `shuck compliance`).
- **Config exists** → its declared keys are kept exactly as-is — partial configs
  stay partial — but each declared value that drifted from the live settings is
  updated in place. Comments and key order are preserved.
- **Config up to date** → nothing is written.

Settings the token cannot read (security and branch protection need
admin/`repo` access) are omitted from a new config and left untouched in an
existing one, with a note explaining why. The exit code is `0` on success
(created, updated, or already up to date) and `2` on an operational error.

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

It exposes six read-only tools:

| Tool | Purpose | Key inputs |
| --- | --- | --- |
| `inspect_logs` | Failing CI step logs for a PR, or one Actions run. | `repo` (`owner/repo`), `pr`, `url`; or none → the open PR for the current branch; or `run` (a run/job URL, or a bare run ID with `repo`) |
| `inspect_reviews` | A PR's reviews and review-comment threads. | `repo` (`owner/repo`), `pr`, `url`; or none → the current branch. Optional `review_comment_limit` |
| `inspect_security` | A repo's security alerts (code scanning, secrets, Dependabot). | `repo` (`owner/repo`) or `url`; or none → the local repo. Optional `state`, `refresh` |
| `check_compliance` | Check a repo's settings against its `.github/compliance.yml`. | `repo` (`owner/repo`) or `url`; or none → the local repo. Optional `config`, `ref` |
| `inspect_action` | Resolve a GitHub Action to its latest tag + commit SHA for pinning. | `action` (`owner/action[/subpath][@version]`). Optional `refresh` |
| `inspect_images` | List GHCR images for an owner, or resolve one image to its digest. | `image` (an owner, `owner/repo`, a URL, or `ghcr.io/owner/name[:tag]`); or none → the local repo. Optional `refresh` |

`inspect_logs` accepts the same log-extraction knobs as the CLI (`context`,
`short_threshold`, `tail`, `pattern`, `full`) plus the cache flags (`refresh`,
`no_cache`, `offline`). Each call returns the rendered, human-readable report as
text **and** the matching stable JSON document as typed structured output, so
programmatic consumers get the schema for free. Authentication uses
`GITHUB_TOKEN`/`GH_TOKEN` from the server's environment (`inspect_action` works
unauthenticated against public repos).

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
(the `inspect_logs` / `inspect_reviews` / `inspect_security` / `check_compliance`
/ `inspect_action` / `inspect_images` tools above) that runs the `shuck` binary from
your `PATH`, and a `SessionStart` hook that checks shuck is installed, recent
enough to run the MCP server, and that a GitHub token is present.

The plugin does not install shuck — [install it yourself](#install) and keep it
current with `shuck upgrade`. shuck is published through justanotherspy's central
plugin marketplace, [`justanotherspy/claude-plugins`](https://github.com/justanotherspy/claude-plugins).
Add the marketplace and install the plugin from within Claude Code:

```
/plugin marketplace add justanotherspy/claude-plugins
/plugin install shuck@justanotherspy
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
