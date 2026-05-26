---
name: shuck
description: >-
  Show the exact failing CI step logs for a GitHub pull request, summarize its
  reviews, list a repo's security alerts, and pin GitHub Actions to SHAs — plus
  watch a PR's checks until they finish. Works two ways: the `shuck` CLI (with
  `--json` for structured output) or the shuck MCP tools (`inspect_logs`,
  `inspect_reviews`, `inspect_security`, `inspect_action`) — use either or both.
  Use when the user wants to know why CI is failing, debug a failed GitHub
  Actions check, pull the error logs for a PR, see what reviewers asked for,
  triage a repo's security findings, or SHA-pin an action.
---

# shuck — failing CI logs, reviews, and security for a PR

`shuck` drills GitHub Actions failures down to the failing steps and returns only
their error logs, summarizes a PR's reviews, lists a repo's security alerts, and
resolves an Action to a SHA pin. Reach for it instead of paging through the
GitHub UI or `gh`.

## Two ways in — use either or both

shuck exposes the same capabilities through two front-ends that share one engine,
so they return the same data; pick whichever is wired up.

| Front-end | How you call it | Best when |
| --- | --- | --- |
| **CLI** (`shuck …`, Bash) | run the binary; add `--json` for structured data | the binary is on PATH; you want to **watch** CI to completion, script exit codes, or pipe `--json` |
| **MCP tools** | call `inspect_logs` / `inspect_reviews` / `inspect_security` / `inspect_action` | the shuck MCP server is registered; you want typed structured output with no parsing |

For one-shot inspection the two are interchangeable; only the CLI does `--watch`.

## The commands at a glance

| What you want | CLI | MCP tool |
| --- | --- | --- |
| Everything on a PR (CI + reviews + security) | `shuck [target]` / `shuck all [target]` | (call the three below) |
| Failing CI step logs | `shuck logs [target]` (alias `l`) | `inspect_logs` |
| Logs for a single Actions run | `shuck logs --run <id\|url>` | `inspect_logs` with `run` |
| A PR's reviews | `shuck reviews [target]` (alias `r`) | `inspect_reviews` |
| A repo's security alerts | `shuck security [repo]` (alias `s`) | `inspect_security` |
| Resolve an Action to a SHA pin | `shuck action <ref>` (alias `a`) | `inspect_action` |

Running `shuck` with **no subcommand** is the same as `shuck all`: CI + reviews +
security in one report. Use `logs` / `reviews` to focus on one dimension.

## Picking a target

The PR-oriented entry points accept the same target forms:

| You have | CLI | MCP (`inspect_logs` / `inspect_reviews`) |
| --- | --- | --- |
| owner/repo + PR number | `shuck owner/repo 42` | `repo` + `pr` |
| a PR URL | `shuck <pr-url>` | `url` |
| a PR number, current repo | `shuck 42` | `pr` alone |
| the current branch's open PR | `shuck` | (no fields) |
| an Actions run/job URL (logs only) | `shuck logs <run-url>` | `inspect_logs` `run` = the URL |
| a run ID + repo (logs only) | `shuck logs --run 123 owner/repo` | `inspect_logs` `run` = `"123"`, `repo` |

Rules that bite:

- For the MCP PR tools, setting `repo` **without** `pr` is an error; owner/repo is
  inferred from the local origin remote only when you pass `pr` alone or nothing.
- Run/job targets (URLs ending `/actions/runs/123` or `.../job/456`, or
  `logs --run`) skip the PR-wide scan and **always re-download logs** (no cache);
  they carry no reviews or security half.

## Using the CLI

```sh
shuck [flags] [target]          # CI + reviews + security, once
shuck --json [flags] [target]   # same, but emit the stable JSON document
shuck --watch [flags] [target]  # poll until every check finishes, then report
```

### Subcommands

| Command (alias) | What it does |
| --- | --- |
| `shuck [target]` / `shuck all [target]` | CI + reviews + repo security in one report (the default) |
| `shuck logs [target] [--run <id\|url>]` (`l`) | failing CI step logs for a PR or a single run |
| `shuck reviews [target]` (`r`) | a PR's reviews and review-comment threads |
| `shuck security [owner/repo \| url]` (`s`) | a repo's security alerts (code scanning, secrets, Dependabot) |
| `shuck action <owner>/<action>[@<ver>]` (`a`) | resolve an Action to its latest tag + commit SHA for pinning |
| `shuck version [--check]` | print the installed version; `--check` looks for a newer release |
| `shuck upgrade` | download + install the latest release in place (and refresh the installed skill) |
| `shuck setup` | install this skill + a CLAUDE.md note (and, optionally, the MCP) |
| `shuck mcp` | run as a local MCP (stdio) server — used by the MCP front-end |

### Flags

Extraction (tune only when the default excerpt isn't enough), on the default
path and on `logs`:

- `--full` — print full, untrimmed logs for failed steps.
- `--context N` (default 10) — lines of context kept around each error match.
- `--pattern RE` — override the error-matching regexp when the default misses
  the real error.
- `--short-threshold N` (default 100) — logs at most this many lines are shown whole.
- `--tail N` (default 100) — lines tailed when a long log has no error match.

Focusing and selection:

- `--run <id|url>` (on `logs`) — inspect one workflow run instead of a PR.
- `--state open|all|dismissed|fixed|resolved` (default `open`) — which security
  alerts to include in the default/`all` view (and on `shuck security`).

Output, cache, auth (default path and the focus subcommands):

- `--json` — emit the stable JSON document instead of text.
- `--refresh` — ignore and rebuild the cache (use when a job was re-run).
- `--no-cache` — do not read or write the cache.
- `--offline` — render only from cache, no network (requires an explicit PR;
  not valid with `--watch`; skips the security half).
- `--token T` — GitHub token, overriding `GITHUB_TOKEN` / `GH_TOKEN`.

Watch-only (default/`all` path):

- `--watch` — poll until every check reaches a terminal state, then print.
- `--interval D` (default 15s) — poll interval.
- `--watch-timeout D` (default 0 = no limit) — give up after this long and print
  the latest snapshot instead of waiting forever.

### Exit codes

The exit code is the verdict — use it to branch without parsing output:

- `0` — checks finished, none failed.
- `1` — checks finished and some failed (read the output / JSON for the steps).
  Security findings do **not** flip the exit code on the default/`all` path; use
  `shuck security --exit-code` to gate on open alerts.
- `2` — operational error (bad/missing auth, target not found, network).

### Examples

```sh
shuck                                             # current branch's open PR: CI + reviews + security
shuck logs justanotherspy/shuck 42                # just the failing CI logs
shuck reviews 42                                  # just the reviews
shuck logs --run https://github.com/owner/repo/actions/runs/123  # one run
shuck logs --run 123 owner/repo                   # one run, by ID
shuck --json https://github.com/owner/repo/pull/42  # combined structured output
shuck security owner/repo                         # a repo's open security alerts
shuck action actions/checkout@v4                  # resolve to a SHA pin
```

## The JSON document

`--json` returns a stable, versioned shape. **Prefer it when you need to act on
results programmatically.**

- `shuck logs --json` and `shuck reviews --json` (and the `inspect_logs` /
  `inspect_reviews` MCP tools' structured output) return the **inspection
  document**: `schema_version`, `pr` `{owner, repo, number, title, head_sha,
  head_branch}` (or `run` instead, for run/job targets), `summary`
  `{failed, cancelled, running, other_failed}`, `failed_jobs[]`
  `{id, run_id, name, conclusion, workflow_name, workflow_path, failed_steps[]}`
  where each step is `{number, name, kind, command, excerpt}`, plus
  `cancelled_jobs[]`, `other_checks[]`, `running_jobs[]`, and `reviews[]`.
- `shuck` / `shuck all --json` (a PR target) wrap that in a **combined envelope**:
  `{schema_version, inspection: <the document above>, security?: <the security
  document>, security_error?}`. The `security` half is omitted (and
  `security_error` set) if the security fetch failed; it is absent entirely for
  run/offline targets, which emit the plain inspection document instead.
- `shuck security --json` returns the **security document** (see below);
  `shuck action --json` returns the resolved-pin document
  (`{schema_version, action, owner, repo, tag, sha, ref, pin}`).

If `summary.running > 0` the snapshot is **incomplete** — some checks are still
running. To wait for the final verdict, watch the PR (below).

## Using the MCP tools

The MCP server (`shuck mcp`) exposes four read-only tools. Each returns the
rendered report as text **and** the matching JSON document as structured output.

| Tool | Use it for | Inputs |
| --- | --- | --- |
| `inspect_logs` | a PR's failing CI, or one run | PR target fields per the table above; **or** `run` (a run/job URL, or a bare run ID with `repo`) |
| `inspect_reviews` | a PR's reviews and comment threads | PR target fields; optional `review_comment_limit` |
| `inspect_security` | a repo's security alerts | `repo` (`owner/repo`) **or** `url`, or none → the local repo; optional `state`, `refresh` |
| `inspect_action` | resolve an Action to a SHA pin | `action` (`owner/action[/subpath][@version]`); optional `refresh` |

`inspect_logs` also accepts the extraction knobs (`full`, `context`, `pattern`,
`short_threshold`, `tail`) and the cache knobs (`refresh`, `no_cache`, `offline`).
The MCP tools are one-shot snapshots — to **wait** for CI, use the CLI watch loop.
There is no combined `all` MCP tool: call `inspect_logs` + `inspect_reviews` +
`inspect_security` for the full picture.

## Security alerts

`shuck security` (CLI) and `inspect_security` (MCP) summarize a repository's
GitHub security alerts in one shot, so you can triage what to fix without paging
through the Security tab. Three sources:

- **Code scanning** (e.g. CodeQL) — rule, severity, `file:line`.
- **Secret scanning** — secret type and file locations. The **raw secret value
  is never fetched or shown** — only its type, location, and state.
- **Dependabot** — vulnerable package, ecosystem, fix version, CVE/GHSA IDs. npm
  **malware** advisories surface here (no separate malware endpoint).

```sh
shuck security                       # the local working directory's repo
shuck security owner/repo            # an explicit repo (or a github.com URL)
shuck security --state all owner/repo  # include dismissed/fixed/resolved
shuck security --json owner/repo     # the stable JSON document
shuck security --exit-code owner/repo  # exit 1 when open alerts are found
```

Each source degrades independently: one that is **not enabled** or **not visible
to the token** is reported and skipped, not failed — so a repo with only some
features on still produces output. By default only **open** alerts show; widen
with `--state open|all|dismissed|fixed|resolved`.

The security JSON document (also `inspect_security`'s structured output):

- `schema_version` (int), `repo` `{owner, repo}`, `state`.
- `summary` `{total, by_severity{critical…unknown}, by_source{code_scanning, secret_scanning, dependabot}}`.
- `sources` — each of `code_scanning` / `secret_scanning` / `dependabot` with a
  `{status, message?}` where status is `ok` | `disabled` | `forbidden` | `error`.
- `code_scanning_alerts[]`, `secret_scanning_alerts[]`, `dependabot_alerts[]` —
  per-alert detail (severity, location, package → `first_patched_version`, IDs,
  `html_url`). No raw secret value is ever present.

Exit code (CLI): `0` on any successful run, `2` only on an operational error;
`--exit-code` makes open findings exit `1` for CI gating. Results are cached
under `~/.shuck/security/<owner>/<repo>` for an hour; `--refresh` re-fetches.
Security data — especially private repos — needs a token with the
`security_events` (or `repo`) scope.

## Pinning actions to SHAs

`shuck action <owner>/<action>[@<version>]` (alias `a`) and the `inspect_action`
tool resolve an Action to its latest matching tag and commit SHA, and print a pin
line ready to drop after `uses:`:

```sh
shuck action actions/checkout          # latest stable
shuck action actions/checkout@v4       # latest matching v4
shuck action --json github/codeql-action/init@v3
```

Auth is optional for public repos; a token lifts the unauthenticated rate limit.
Tags are cached for a day; `--refresh` re-fetches.

## Watching CI to completion (CLI)

One-shot inspection (CLI without `--watch`, or the MCP tools) is a point-in-time
snapshot. When you need to **wait until CI finishes**, run the CLI watch loop on
the default/`all` path with Bash:

```sh
shuck --watch <target>
```

It re-checks every `--interval` and returns **only when no jobs are still
running** — every check has reached a terminal state (success, failure,
cancelled, timed out, …). Then it prints the final report (CI + reviews +
security) and exits with the verdict code above (`0` clean, `1` failures, `2`
error).

How to run it well:

- **CI can take many minutes.** Run the watch command in the background (Bash
  `run_in_background`) or with a generous timeout — don't block the foreground on
  it. You'll be notified when it returns.
- **Bound the wait** with `--watch-timeout <dur>` (e.g. `--watch-timeout 30m`);
  on timeout shuck prints the latest snapshot instead of waiting forever.
- **Want structured final output?** Add `--json`, or once watch reports failures
  (exit `1`) call `inspect_logs` for the typed failing-step detail.
- Progress lines ("N running, M failed so far …") go to **stderr**; the final
  report (text or `--json`) is the only thing on **stdout**.

```sh
shuck --watch --watch-timeout 30m justanotherspy/shuck 42
shuck --watch --json https://github.com/justanotherspy/shuck/pull/42
```

Caveat: watch keys off "no jobs still running", so if you start it before CI has
registered any runs for the head commit it reports all-clear immediately. Start
watching once at least one check exists (e.g. right after a CI event), or after
an initial inspection shows running jobs.

## Prerequisites

- The `shuck` binary on your PATH (the MCP server also runs it). Install it once:

  ```sh
  curl -fsSL https://raw.githubusercontent.com/justanotherspy/shuck/main/install.sh | bash
  # or: go install github.com/justanotherspy/shuck@latest
  ```

  Keep it current with `shuck upgrade` (and check with `shuck version --check`).
  `shuck upgrade` also refreshes this skill in your Claude config if you
  installed it with `shuck setup`.
- A GitHub token in `GITHUB_TOKEN` or `GH_TOKEN` (the MCP server reads it from
  its environment; the CLI also accepts `--token`). `shuck action` works
  unauthenticated against public repos, but a token lifts the rate limit.

The plugin's SessionStart hook stays quiet when both are satisfied. It warns
(without blocking) if `shuck` is not on PATH, is too old to run the MCP server
(`shuck upgrade` fixes that), or a token is missing.

## Notes

- Results are cached under `~/.shuck`, keyed by job + run attempt, so repeat runs
  are cheap; pass `--refresh` / `refresh` when a job has been re-run.
- Non-Actions checks (external statuses) are listed by name only — no logs exist
  for them via the API.
- If shuck reports no token, ask the user to set `GITHUB_TOKEN` / `GH_TOKEN` or
  pass `--token`.
