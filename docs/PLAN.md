# shuck improvement plan

Captures two rounds of feedback from agents that used `shuck` to triage CI
failures, the verified status of each request against the current code, the
scope decisions we've locked, and the implementation roadmap. Keep this in sync
as items land.

## Feedback received

### Block 1 — agent dogfooding (priority-ordered)

1. **Structured output (`--json`).** Emit the parsed result (per job: name,
   conclusion, failing step name, extracted error lines) so a program can
   consume it instead of scraping the human text. Keep pretty output the default.
2. **Target a specific job or run.** CI-failure events identify one job. Allow
   `shuck --job <job-url>` or `shuck --run <run-id>` to show just that job's
   failing step, skipping the PR-wide scan.
3. **Discoverability / MCP.** A CLI is invisible to an agent until something
   says it exists. Consider an MCP wrapper so shuck shows up as a typed tool —
   weighed against the cost of running another server; may be subsumed by `--json`.

### Block 2 — second agent (priority-ordered)

1. **`--json` with a stable schema** (`job`, `step`, `conclusion`,
   `error_blocks[]`, failing step's command). Highest value.
2. **Meaningful exit codes** — non-zero when any step failed, for scripts/hooks.
3. **`--version` / `-version`** — reported `flag provided but not defined: -version`.
4. **Multi-job summary** — when a matrix has several failed/cancelled legs,
   print an upfront "N failed, M cancelled" overview before diving into one.
5. **In-progress handling** — a `--watch` (poll-until-complete) mode and/or a
   clear banner when the run is still going and only completed jobs are shown.
6. **GNU-style double-dash flag aliases** alongside the single-dash Go flags.
7. **Discoverability** — frame `shuck <pr>` as the *first move* on a CI failure;
   maybe print a one-line "how to triage CI" hint and improve README wording.

## Status against the current code (verified)

| Item | Status | Notes |
| --- | --- | --- |
| `--json` | **New** | `render` is text-only; `model` already has `json` tags. |
| Exit codes | **Already done** | `Run` returns 0/1/2; `exitFor` keys off `HasFailures()`. Documented in README and the plugin SKILL.md. |
| `--version` | **Already done** | Added in #13 (`cli.go` defines it). Both `-version` and `--version` work; the agent hit a stale binary. |
| Multi-job summary | **New** | No upfront count today. |
| Cancelled legs | **Real gap** | `model.IsFailureConclusion` excludes `cancelled`, so cancelled jobs are silently dropped (neither failed, running, nor counted). |
| In-progress banner | **Partial** | Running jobs are tracked and listed at the bottom; no top banner; `IsTerminal()` exists. |
| `--watch` | **New** | No polling loop. |
| GNU double-dash | **Already works** | Go's `flag` accepts one or two dashes for every flag. No aliases needed. |
| Target job/run | **New** | Pipeline is PR-anchored (`Resolve` → `GetPR`/`FindOpenPR` → `ListJobs(headSHA)`); `gh.Client` has no single-job fetch. |
| MCP server | **Defer** | No server/dep today. A `/shuck` Claude Code plugin already exists (`plugins/shuck/`) providing discovery + auto-install. |

### Cross-cutting footgun: flag ordering

Go's `flag` package stops parsing at the first non-flag argument, so
`shuck owner/repo 42 --json` lands `--json` in the positional args ("too many
arguments"), while `shuck --json owner/repo 42` works. An agent will naturally
write the first form. Fixed with an arg-permutation pre-pass (below).

## Locked decisions

- **JSON schema:** dedicated DTO in a new `internal/jsonout` package with a
  `schema_version` envelope — decoupled from `model` so internal refactors and
  cache-only fields (`inspected`, `checked_at`, `run_id`, `workflow_path`) don't
  leak or break consumers.
- **Flag ordering:** add an arg-permutation pre-pass so flags parse in any
  position (stops at a literal `--`).
- **Delivery:** plan first, then implement `--json` (bundled with the
  permutation pre-pass, since it's what makes `--json` reachable for agents).

## Roadmap (separate PRs unless noted)

1. **`--json` + arg permutation** *(this PR)*
   - New `internal/jsonout`: versioned `Document` DTO + `Encode(w, *model.Report)`.
   - Schema: `schema_version`, `pr`, `summary{failed,running,other_failed}`,
     `failed_jobs[]{id,run_id,name,conclusion,workflow_name,workflow_path,
     failed_steps[]{number,name,kind,command,excerpt}}`, `other_checks[]`,
     `running_jobs[]`. Slices serialize as `[]`, never `null`.
   - `--json` flag wired through both the live and `--offline` render paths;
     exit codes unchanged so it still composes in scripts.
   - `permuteArgs` reorders flags ahead of positionals before `flag.Parse`.
   - Ship `excerpt` as a single string first; add `error_blocks[]` (split on
     the omission markers) only if agents ask.
2. **Cancelled + summary.** Track cancelled jobs in a non-drilled bucket; print
   an upfront `N failed, M cancelled` line. Decide whether cancelled-only exits
   `1` or stays `0`.
3. **Target job/run.** `--job <url>` / `--run <id>`; a job-URL parser; new
   `gh.GetJob`/`ListRunJobs`; a PR-bypass branch in `run()`; a no-PR render
   header. Bypass the PR-keyed cache initially.
4. **In-progress banner.** Top-of-output warning when failures coexist with
   running jobs. `--watch` deferred to a follow-up.
5. **README "first move" wording.** Frame `shuck <pr>` as the first move on a CI
   failure; note the binary may already be on PATH.

### No code needed

- Exit codes (#2), `--version` (#3), GNU double-dash (#6) — already satisfied.

### Pushed back / deferred

- **MCP server.** `--json` plus the existing Claude Code plugin (`/shuck` skill
  + SessionStart auto-install) already deliver structured results and discovery.
  Revisit only if non-Claude agents need typed discovery.
- **Unprompted triage hint** on every run — noise in pipes and `--json`;
  `--help` already serves that role.
