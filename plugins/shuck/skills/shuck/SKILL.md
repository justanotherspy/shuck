---
name: shuck
description: >-
  Show the exact failing CI step logs for a GitHub pull request using the shuck
  MCP tools, and watch a PR's checks until they finish. Use when the user wants
  to know why CI is failing, debug a failed GitHub Actions check, pull the error
  logs for a PR without clicking through GitHub, or wait for CI to complete.
---

# shuck — failing CI logs for a PR

`shuck` drills GitHub Actions failures down to the failing steps and returns only
their error logs. Reach for it instead of paging through the Actions UI or `gh`.

## Getting results: use the MCP tools

This plugin registers a local MCP server (`shuck mcp`) that exposes shuck's
capability as two typed, read-only tools. **Prefer these tools for getting
results** — they return both the rendered report (as text) and the stable JSON
document (as structured output), so you get typed data you can act on without
parsing CLI text.

| Tool | Use it for | How to fill its inputs |
| --- | --- | --- |
| `inspect_pr` | A PR's failing CI (the usual first move). | See target rules below. |
| `inspect_run` | One Actions run or job a CI event points at. | `url` for a run/job URL, **or** `repo` + `run_id` (+ optional `job_id`). |

### Choosing `inspect_pr` inputs

Pick exactly one targeting style; the most specific wins:

- **PR URL** → set `url` (e.g. `https://github.com/owner/repo/pull/42`).
- **Explicit PR** → set `repo` (`owner/repo`) **and** `pr` (the number).
  Setting `repo` without `pr` is an error.
- **PR number, current repo** → set `pr` alone (owner/repo is inferred from the
  local working directory's origin remote).
- **Open PR for the current branch** → pass no target fields at all.

### Choosing `inspect_run` inputs

- **A run or job URL** (e.g. from a CI-failure notification) → set `url`.
  `.../actions/runs/123` inspects the whole run; `.../actions/runs/123/job/456`
  drills just that job.
- **Explicit IDs** → set `repo` (`owner/repo`) and `run_id`; add `job_id` to
  restrict to one job.

Run/job targets skip the PR-wide scan and always re-download logs (no cache).

### Reading the result

Each tool returns the rendered report as text **and** a structured JSON document
(`schema_version`, `pr`/`run`, `summary{failed,cancelled,running,other_failed}`,
`failed_jobs[]{…,failed_steps[]{number,name,kind,command,excerpt}}`,
`cancelled_jobs[]`, `other_checks[]`, `running_jobs[]`). Summarize the failing
steps and their errors for the user; use the structured output when you need to
branch on counts or pull a specific step's `excerpt`/`command`.

If `summary.running > 0`, the snapshot is incomplete — some checks are still
running. To wait for the final verdict, watch the PR (next section).

### Tuning the extraction

Both tools accept the same log-extraction knobs; pass them only when the default
excerpt isn't enough:

- `full` (bool) — return full, untrimmed logs for failed steps.
- `context` (int, default 10) — lines of context kept around each error match.
- `pattern` (string) — override the error-matching regexp when the default
  misses the real error.
- `short_threshold` (int, default 100), `tail` (int, default 100) — sizing knobs
  for when whole logs are shown vs. tailed.

`inspect_pr` also takes the cache knobs: `refresh` (re-download if CI was re-run
and results look stale), `no_cache`, and `offline` (cache only, no network;
requires `repo` + `pr`).

## Watching CI to completion (CLI)

The MCP tools are one-shot snapshots. When you need to **wait until CI finishes**
— so you know when to stop watching — run the CLI watch loop with Bash:

```sh
shuck --watch <target>
```

It re-checks every `--interval` (default 15s) and returns **only when no jobs are
still running** — i.e. every check has reached a terminal state (success,
failure, cancelled, timed out, …). Then it prints the final report. The
**exit code is the verdict and your signal that watching is done**:

- `0` — all checks finished, none failed.
- `1` — checks finished and some failed (read the output for the failing steps).
- `2` — operational error (bad/missing auth, target not found, network).

`<target>` is any form `shuck` accepts (`owner/repo <pr>`, a PR URL, a run/job
URL, a bare PR number, or nothing for the current branch).

How to run it well:

- **CI can take many minutes.** Run the watch command in the background (Bash
  `run_in_background`) or with a generous timeout — don't block the foreground on
  it. You'll be notified when it returns.
- **Bound the wait** with `--watch-timeout <dur>` (e.g. `--watch-timeout 30m`);
  on timeout shuck prints the latest snapshot instead of waiting forever. Default
  is no limit.
- **Want structured final output?** Add `--json`, or once watch reports failures
  (exit `1`) call `inspect_pr`/`inspect_run` for the typed failing-step detail.
- Progress lines ("N running, M failed so far …") go to **stderr**; the final
  report (text or `--json`) is the only thing on **stdout**.

```sh
shuck --watch --watch-timeout 30m justanotherspy/shuck 42
shuck --watch --json https://github.com/justanotherspy/shuck/pull/42
```

Caveat: watch keys off "no jobs still running", so if you start it before CI has
registered any runs for the head commit it reports all-clear immediately. Start
watching once at least one check exists (e.g. right after a CI event), or after
an initial `inspect_pr` shows running jobs.

## Reading logs from the CLI directly

If you just want to read the logs (no typed data needed) you can also run shuck
directly with Bash; the same exit-code meanings above apply.

| Situation | Command |
| --- | --- |
| Explicit PR | `shuck <owner>/<repo> <pr>` |
| PR from its URL | `shuck <pr-url>` |
| A single Actions run | `shuck <run-url>` |
| A single Actions job | `shuck <job-url>` |
| PR number, current repo | `shuck <pr>` |
| Open PR for the current branch | `shuck` |

The same flags map to the MCP knobs above: `--full`, `--context N`,
`--pattern RE`, `--refresh`, `--offline`, `--no-cache`, `--json`.

## Prerequisites

- The `shuck` binary on your PATH (the MCP server runs it). Install it once:

  ```sh
  curl -fsSL https://raw.githubusercontent.com/justanotherspy/shuck/main/install.sh | bash
  # or: go install github.com/justanotherspy/shuck@latest
  ```

  Keep it current with `shuck upgrade` (and check with `shuck version --check`).
- A GitHub token in `GITHUB_TOKEN` or `GH_TOKEN` (the MCP server reads it from
  its environment; the CLI also accepts `--token`).

The plugin's SessionStart hook stays quiet when both are satisfied. It warns
(without blocking) if `shuck` is not on PATH, is too old to run the MCP server
(`shuck upgrade` fixes that), or a token is missing.

## Notes

- Results are cached under `~/.shuck`, keyed by job + run attempt, so repeat runs
  are cheap; pass `refresh`/`--refresh` when a job has been re-run.
- Non-Actions checks (external statuses) are listed by name only — no logs exist
  for them via the API.
- If shuck reports no token, ask the user to set `GITHUB_TOKEN`/`GH_TOKEN` or
  pass `--token`.
