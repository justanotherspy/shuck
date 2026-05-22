---
name: shuck
description: >-
  Show the exact failing CI step logs for a GitHub pull request using the shuck
  CLI. Use when the user wants to know why CI is failing, debug a failed GitHub
  Actions check, or pull the error logs for a PR without clicking through GitHub.
---

# shuck — failing CI logs for a PR

`shuck` drills GitHub Actions failures down to the failing steps and prints only
their error logs. Reach for it instead of paging through the Actions UI or `gh`.

## Prerequisites

- The `shuck` binary on PATH (`go install github.com/justanotherspy/shuck@latest`).
- A GitHub token in `GITHUB_TOKEN` or `GH_TOKEN` (or pass `--token`).

The plugin's SessionStart hook warns at session start if either is missing.

## How to run

Pick the invocation that matches what the user gave you, and run it with Bash:

| Situation | Command |
| --- | --- |
| Explicit PR | `shuck <owner>/<repo> <pr>` |
| PR number, current repo | `shuck <pr>` |
| Open PR for the current branch | `shuck` |

```sh
shuck justanotherspy/shuck 42
```

## Reading the exit code

shuck's exit code is meaningful — do not treat a non-zero exit as a tool
failure on its own:

- `0` — no failing checks.
- `1` — failing checks were reported. This is the normal "found failures" case:
  read the output and summarize the failing steps and their errors for the user.
- `2` — operational error (bad/missing auth, target not found, network). Read
  stderr and surface the problem.

## Useful flags

- `--full` — show full, untrimmed logs for failed steps (use when the trimmed
  excerpt cut off the relevant error).
- `--context N` — lines of context kept around each error match (default 10).
- `--pattern RE` — override the error-matching regexp when the default misses
  the real error.
- `--refresh` — ignore the cache and re-download logs (use if CI was re-run and
  results look stale).
- `--offline` — render only from the local cache, no network.
- `--no-cache` — do not read or write the cache.

## Notes

- Results are cached under `~/.shuck`, keyed by job + run attempt, so repeat runs
  are cheap; add `--refresh` when a job has been re-run.
- Non-Actions checks (external statuses) are listed by name only — no logs exist
  for them via the API.
- If shuck reports no token, ask the user to set `GITHUB_TOKEN`/`GH_TOKEN` or
  pass `--token`.
