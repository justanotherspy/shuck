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
| `--json` | **Done (#20)** | `internal/jsonout` emits a versioned `Document`; wired through live + `--offline`. |
| Exit codes | **Already done** | `Run` returns 0/1/2; `exitFor` keys off `HasFailures()`. Documented in README and the plugin SKILL.md. |
| `--version` | **Already done** | Added in #13 (`cli.go` defines it). Both `-version` and `--version` work; the agent hit a stale binary. |
| Multi-job summary | **Done** | `render.writeSummary` prints an upfront `N failed, M cancelled, …` line; `jsonout.Summary` carries the counts. |
| Cancelled legs | **Done** | `model.IsCancelledConclusion` + a `CancelledJobs` bucket of full `JobResult`s; drilled best-effort so the interrupted step's last output is shown. Stays exit `0` on its own (cancellation is often deliberate). |
| In-progress banner | **Done** | `writeSummary` prints a top `⚠ N still running — failures shown may be incomplete` banner when failures coexist with running jobs. |
| `--watch` | **Done** | `cli.watch` polls `inspectWith` every `--interval` (default 15s) until `Report.IsTerminal()` (no running jobs), then emits with the failure-aware exit code. `--watch-timeout` bounds the wait; Ctrl-C (SIGINT) stops it and prints the latest snapshot. Rejected with `--offline`. |
| GNU double-dash | **Already works** | Go's `flag` accepts one or two dashes for every flag. No aliases needed. |
| Target job/run | **Done** | `shuck <run-url>` / `shuck <job-url>` (positional, no flags). `target.parseActionsURL` + `Target.RunID/JobID`; `gh.RunReport` fetches run metadata and classifies its jobs (or one job); `cli.inspectRun` drills and renders with a run-aware header. Bypasses the PR-keyed cache; JSON gains an additive `run` object. |
| MCP server | **Done** | `internal/mcp` serves `inspect_pr` / `inspect_run` over stdio (`shuck mcp`), reusing `cli.Inspect`. Typed input/output schemas via the official Go SDK; returns rendered text + the `jsonout` document. Auto-registered by the Claude Code plugin (`plugins/shuck/.mcp.json`). |

### Cross-cutting footgun: flag ordering

Go's `flag` package stops parsing at the first non-flag argument, so
`shuck owner/repo 42 --json` lands `--json` in the positional args ("too many
arguments"), while `shuck --json owner/repo 42` works. An agent will naturally
write the first form. Fixed with an arg-permutation pre-pass (below).

### Cross-cutting footgun: Unicode dashes — **DONE**

macOS "smart dashes" and rich-text copy-paste (Slack, docs, the web) silently
turn `--` into an em-dash, so a pasted `shuck 42 —full` arrived with a `—full`
token. `permuteArgs` keyed flag detection off a leading ASCII `-`, so `—full`
fell through to the positionals and `shuck 42 —full` failed with the misleading
`invalid repo "42"` (it was read as `owner/repo` + a stray token). Fixed by
`canonicalDashes`, a pre-pass inside `permuteArgs` that rewrites a leading run of
Unicode dash runes (en/em dash and horizontal bar → `--`; hyphen-width variants
and the minus sign → `-`) back to ASCII before flag classification. Positionals
(numbers, `owner/repo`, URLs) never start with a dash, so they pass through
untouched, and a lone wide dash still maps to the `--` positional separator.
Covered by a flag×target×ordering×dash-style matrix in `cli_test.go`.

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

1. **`--json` + arg permutation** — **DONE (#20).**
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
2. **Cancelled + summary.** — **DONE.**
   - `Report.CancelledJobs` is a bucket of full `JobResult`s classified via
     `model.IsCancelledConclusion`; their logs are drilled best-effort so the
     interrupted step (and its last output) shows like a failed step.
   - `render.writeSummary` prints an upfront `N failed, M cancelled, …` line; a
     cancelled-only run is no longer mislabelled "all checks passing".
   - JSON gains `summary.cancelled` and a `cancelled_jobs[]` array (additive;
     `schema_version` unchanged).
   - **Decision:** cancelled-only stays exit `0` (cancellation is often
     deliberate); only `HasFailures()` drives exit `1`.
3. **Target job/run.** — **DONE.** Shipped as a single positional URL
   (`shuck <run-url>` / `shuck <job-url>`) rather than `--job`/`--run` flags, to
   keep one simple interface alongside the existing `shuck <pr-url>`.
   - `target.parseActionsURL` recognizes `.../actions/runs/<run>` and
     `.../actions/runs/<run>/job/<job>`; `Target` gains `RunID`/`JobID`.
   - `gh.RunReport` fetches the run's head context and classifies its jobs
     (whole run) or a single job (job URL); `classifyJobs` is shared with
     `ListJobs`.
   - `cli.inspectRun` is a PR-bypass branch in `run()` that drills the failed
     jobs and renders. Run targets bypass the PR-keyed cache (always re-download).
   - `render` shows a run/job header and all-clear message in place of the PR
     line; `jsonout` adds an additive optional `run` object (no `schema_version`
     bump). `--offline` is rejected for run/job URLs (nothing is cached).
   - **Deferred:** the `/pull/<n>/checks?check_run_id=` URL form and run-attempt
     selection (the attempt segment is currently ignored — latest is used).
4. **In-progress banner.** — **DONE.** `writeSummary` prints a top-of-output
   `⚠ N still running — failures shown may be incomplete` banner when failures
   coexist with running jobs.
   - **`--watch` follow-up — DONE.** `cli.watch` is a poll-until-complete loop:
     it re-runs `inspectWith` every `--interval` (default 15s) and returns only
     once `Report.IsTerminal()` holds (the running bucket is empty, i.e. every
     check is success/failure/cancelled/…), then emits with the normal 0/1 exit
     code so callers get a clear "CI finished" signal. `--watch-timeout` bounds
     the wait (0 = unlimited; on timeout it prints the latest snapshot); a
     SIGINT-aware context lets Ctrl-C stop the loop and still print the latest
     result. Rejected with `--offline` (the cache can't change while waiting).
     The loop takes injected `inspect`/`sleep` funcs so its termination logic is
     unit-tested without network or real waiting. Stays CLI-only — the one-shot
     MCP tools are unchanged.
5. **README "first move" wording.** — **DONE.** README frames `shuck <pr>` as
   the first move on a CI failure and notes the plugin may already put the
   binary on `PATH`.

### No code needed

- Exit codes (#2), `--version` (#3), GNU double-dash (#6) — already satisfied.

### MCP server — DONE

Shipped after the deferral once typed discovery for non-Claude agents became the
explicit goal.

- New `internal/mcp` package hosts a stdio server (the official
  `github.com/modelcontextprotocol/go-sdk`). Launched via a `shuck mcp`
  subcommand dispatched from `main.go`.
- Two typed tools — `inspect_pr` (PR target: `repo`/`pr`/`url`, or local branch)
  and `inspect_run` (run/job `url`, or `repo`+`run_id`+`job_id`) — both taking
  the CLI's extraction knobs (and, for PRs, the cache flags). Input/output JSON
  schemas are inferred from Go structs by the SDK, so agents get typed discovery.
- The pipeline is shared, not duplicated: `cli.run` was refactored into
  `inspectWith` + an exported `cli.Inspect` that returns a `*model.Report`; the
  CLI emits it as text/JSON and the MCP handlers wrap it as a tool result.
- Each tool returns the rendered report as text content **and** the stable
  `jsonout.Document` as structured output (`jsonout.NewDocument` exported for
  this). Token comes from `GITHUB_TOKEN`/`GH_TOKEN` in the server env (no secret
  in tool args).
- The Claude Code plugin bundles it via `plugins/shuck/.mcp.json`
  (`${CLAUDE_PLUGIN_ROOT}/bin/shuck mcp`), auto-registered when the plugin loads.

## Round 3 — sharper failure signal

Two additive features that make a failure self-describing, so an agent can jump
straight to the offending line and route the failure (fix vs. re-run) without
re-parsing the excerpt. Both are additive to the `--json` schema (no
`schema_version` bump) and flow through the CLI, the MCP tools, and the cache.

1. **Check-run annotations.** — **DONE.** shuck previously only scraped
   `##[error]` log text; it now also surfaces a job's GitHub check-run
   annotations — the structured `path:line:message` records that problem
   matchers (golangci-lint, `go test`, `tsc`, eslint, compilers) emit — which
   point straight at the offending location.
   - `gh.JobAnnotations` paginates the check-runs annotations API. The check-run
     ID is parsed from each job's `check_run_url` (`gh.checkRunID`); an Actions
     job's ID and its check-run ID differ, and only the latter keys the API.
   - Annotations are **cheap metadata**, so they are re-fetched every run (even
     on the same-commit log-reuse path) and not cached — consistent with how the
     checks list is always re-validated. Fetching is best-effort: an error is a
     warning, never a failed inspection (like reviews).
   - Attached at the **job** level (`model.JobResult.Annotations`), not per
     failed step: the API does not associate annotations with steps. JSON adds
     `failed_jobs[]/cancelled_jobs[].annotations[]{path,start_line,end_line,
     start_column,end_column,level,title,message}` (always `[]`, never null).
   - Text render lists `failure`/`warning` annotations under each job (capped at
     20 with a "… N more" note; `notice` level is dropped from text but kept in
     JSON).
2. **Failure classification.** — **DONE.** Each failed step is tagged with a
   coarse `model.FailureClass` — `lint | test | build | timeout | oom | infra` —
   so consumers can route without re-reading the excerpt.
   - New dependency-free `internal/classify`: a heuristic over the step command
     and excerpt with deliberate precedence — operational causes first
     (timeout/oom/infra ⇒ likely a re-run), then the command's tool, then
     excerpt keywords. Errs toward unclassified rather than guessing wrong.
   - Computed in `cli.buildFailedSteps` (the single step↔section chokepoint), so
     both the PR and run/job paths get it. JSON adds `failed_steps[].class`
     (omitted when unclassified); text appends a `[class]` tag to the step head.

### Pushed back / deferred

- **Unprompted triage hint** on every run — noise in pipes and `--json`;
  `--help` already serves that role.
