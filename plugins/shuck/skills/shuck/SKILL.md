---
name: shuck
description: >-
  Show the exact failing CI step logs for a GitHub pull request, and watch a
  PR's checks until they finish. Works two ways — the `shuck` CLI (with `--json`
  for structured output) or the shuck MCP tools — use either or both. Use when
  the user wants to know why CI is failing, debug a failed GitHub Actions check,
  pull the error logs for a PR without clicking through GitHub, or wait for CI to
  complete.
---

# shuck — failing CI logs for a PR

`shuck` drills GitHub Actions failures down to the failing steps and returns only
their error logs. Reach for it instead of paging through the Actions UI or `gh`.

## Two ways in — use either or both

shuck exposes the exact same capability through two front-ends. They share one
engine, so they return the same failing-step data; pick whichever is wired up.

| Front-end | How you call it | Best when |
| --- | --- | --- |
| **CLI** (`shuck …`, Bash) | run the binary; add `--json` for structured data | the binary is on PATH; you want to **watch** CI to completion, script exit codes, or pipe `--json` |
| **MCP tools** (`inspect_pr` / `inspect_run`) | call the tools | the shuck MCP server is registered; you want typed structured output with no parsing |

Both return the same stable JSON document (see [The JSON document](#the-json-document)),
so you can branch on counts or pull a specific step's error excerpt without
parsing free text. If only one is available, use it; if both are, the CLI with
`--json` and the MCP tools are interchangeable for one-shot inspection — the CLI
additionally does `--watch`.

## Picking a target

Every entry point accepts the same target forms. Pick the one matching what you
have:

| You have | CLI | MCP `inspect_pr` | MCP `inspect_run` |
| --- | --- | --- | --- |
| owner/repo + PR number | `shuck owner/repo 42` | `repo`+`pr` | — |
| a PR URL | `shuck <pr-url>` | `url` | — |
| a PR number, current repo | `shuck 42` | `pr` alone | — |
| the current branch's open PR | `shuck` | (no fields) | — |
| an Actions run URL | `shuck <run-url>` | — | `url` |
| an Actions job URL | `shuck <job-url>` | — | `url` |
| explicit run/job IDs | `shuck <run-url>` | — | `repo`+`run_id`(+`job_id`) |

Rules that bite:

- For `inspect_pr`, setting `repo` **without** `pr` is an error; owner/repo is
  inferred from the local origin remote only when you pass `pr` alone or nothing.
- Run/job targets (URLs ending `/actions/runs/123` or `.../job/456`) skip the
  PR-wide scan and **always re-download logs** (no cache).

## Using the CLI

```sh
shuck [flags] [target]          # inspect once, print the report
shuck --json [flags] [target]   # same, but emit the stable JSON document
shuck --watch [flags] [target]  # poll until every check finishes, then report
```

### Subcommands

| Command | What it does |
| --- | --- |
| `shuck [target]` | inspect a PR / run / job once and print failing steps |
| `shuck --watch [target]` | poll until CI is terminal, then print the final report |
| `shuck version [--check]` | print the installed version; `--check` looks for a newer release |
| `shuck upgrade` | download + install the latest release in place (and refresh the installed skill) |
| `shuck setup` | install this skill + a CLAUDE.md note (and, optionally, the MCP) |
| `shuck mcp` | run as a local MCP (stdio) server — used by the MCP front-end |

### Flags

Extraction (tune only when the default excerpt isn't enough):

- `--full` — print full, untrimmed logs for failed steps.
- `--context N` (default 10) — lines of context kept around each error match.
- `--pattern RE` — override the error-matching regexp when the default misses
  the real error.
- `--short-threshold N` (default 100) — logs at most this many lines are shown
  whole.
- `--tail N` (default 100) — lines tailed when a long log has no error match.

Output, cache, auth:

- `--json` — emit the stable JSON document instead of text.
- `--refresh` — ignore and rebuild the cache (use when a job was re-run).
- `--no-cache` — do not read or write the cache.
- `--offline` — render only from cache, no network (requires an explicit PR;
  not valid with `--watch`).
- `--token T` — GitHub token, overriding `GITHUB_TOKEN` / `GH_TOKEN`.

Watch-only:

- `--interval D` (default 15s) — poll interval.
- `--watch-timeout D` (default 0 = no limit) — give up after this long and print
  the latest snapshot instead of waiting forever.

### Exit codes

The exit code is the verdict — use it to branch without parsing output:

- `0` — checks finished, none failed.
- `1` — checks finished and some failed (read the output / JSON for the steps).
- `2` — operational error (bad/missing auth, target not found, network).

### Examples

```sh
shuck                                             # current branch's open PR
shuck justanotherspy/shuck 42                     # explicit PR
shuck --json https://github.com/owner/repo/pull/42  # structured output
shuck --full --context 30 owner/repo 42           # more log context
shuck https://github.com/owner/repo/actions/runs/123/job/456  # one job
```

## The JSON document

`--json` (CLI) and the MCP tools' structured output return the same shape.
**Prefer it when you need to act on results programmatically** — branch on
counts, then read a specific step's `excerpt` / `command`. Top-level fields:

- `schema_version` (int) — bumped only on breaking changes; additive fields keep
  the version.
- `pr` `{owner, repo, number, title, head_sha, head_branch}` — present for PR
  targets.
- `run` `{owner, repo, run_id, job_id?, title, head_sha, head_branch, workflow_name}`
  — present **instead of** PR context for run/job URL targets.
- `summary` `{failed, cancelled, running, other_failed}` — quick counts so you
  can branch without walking the lists.
- `failed_jobs[]` `{id, run_id, name, conclusion, workflow_name, workflow_path,
  failed_steps[]}` where each step is
  `{number, name, kind, command, excerpt}` — `command` is the action ref or
  shell line, `excerpt` is the extracted error.
- `cancelled_jobs[]` `{name, conclusion, workflow_name}`.
- `other_checks[]` `{name, conclusion, url}` — non-Actions checks; no logs.
- `running_jobs[]` `{name, status, workflow_name}`.

If `summary.running > 0` the snapshot is **incomplete** — some checks are still
running. To wait for the final verdict, watch the PR (next section).

## Using the MCP tools

The MCP server (`shuck mcp`) exposes two read-only tools. Each returns the
rendered report as text **and** the JSON document above as structured output, so
you get typed data without parsing CLI text.

| Tool | Use it for | Inputs |
| --- | --- | --- |
| `inspect_pr` | a PR's failing CI (the usual first move) | target fields per the table above |
| `inspect_run` | one Actions run or job a CI event points at | `url`, **or** `repo`+`run_id`(+`job_id`) |

Both accept the same extraction knobs as the CLI flags: `full`, `context`,
`pattern`, `short_threshold`, `tail`. `inspect_pr` additionally takes the cache
knobs `refresh`, `no_cache`, and `offline` (cache only; requires `repo`+`pr`).
The MCP tools are one-shot snapshots — to **wait** for CI, use the CLI watch loop.

## Watching CI to completion (CLI)

One-shot inspection (CLI without `--watch`, or the MCP tools) is a point-in-time
snapshot. When you need to **wait until CI finishes** — so you know when to stop
watching — run the CLI watch loop with Bash:

```sh
shuck --watch <target>
```

It re-checks every `--interval` and returns **only when no jobs are still
running** — every check has reached a terminal state (success, failure,
cancelled, timed out, …). Then it prints the final report and exits with the
verdict code above (`0` clean, `1` failures, `2` error).

How to run it well:

- **CI can take many minutes.** Run the watch command in the background (Bash
  `run_in_background`) or with a generous timeout — don't block the foreground on
  it. You'll be notified when it returns.
- **Bound the wait** with `--watch-timeout <dur>` (e.g. `--watch-timeout 30m`);
  on timeout shuck prints the latest snapshot instead of waiting forever.
- **Want structured final output?** Add `--json`, or once watch reports failures
  (exit `1`) call `inspect_pr` / `inspect_run` for the typed failing-step detail.
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
  its environment; the CLI also accepts `--token`).

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
