# CLAUDE.md

Guidance for agents working in this repository.

## What this is

`shuck` is one portable Go binary — a CLI, an MCP server, and a Claude Code
plugin — that prints the exact failing CI step logs for a GitHub PR. Its
centrepiece is `shuck monitor`: a local background daemon that follows the
working trees you are in, tracks whichever PR the current branch belongs to,
and turns new CI failures, review comments, and stale action pins into events a
session is handed as they happen. On demand it also summarizes PR reviews,
lists a repo's security alerts, checks live repo settings against a committed
compliance policy, audits a repo's Dependabot config against the ecosystems it
uses, audits workflow action pins, and SHA-pins GitHub Actions / GHCR images.
State lives under `~/.cache/shuck`.

There is nothing to deploy: no server, no webhook, no credential beyond the
GitHub token. `ci.yml` holds a standing portability budget — a step that fails
the build if the binary's import graph ever picks up a cloud SDK, a serverless
runtime, or a server framework. If a feature seems to need one, it belongs
outside shuck.

## Dogfood shuck

This repo bakes its own tool in for agents: the `shuck` skill
(`.claude/skills/shuck/`), the plugin's monitor hooks, the `shuck` MCP server
(`.mcp.json` → `shuck mcp`, tools `inspect_logs` / `inspect_reviews` /
`inspect_security` / `check_compliance` / `audit_dependabot` / `check_pins` /
`inspect_action` / `inspect_images` / `monitor_status` / `monitor_events` /
`monitor_watch` / `monitor_unwatch`), and — in dev environments — the `shuck`
binary on PATH.

**The loop here is that the monitor watches and you get told.** The plugin
registers this working tree at `SessionStart`, so after you push you do not
poll: the next CI failure arrives in the conversation as a `<shuck-monitor>`
block, with the failing step's logs already in it. To wait for a verdict
deliberately, call `monitor_events` with `wait_seconds` rather than sleeping and
re-checking. `monitor_status` answers "is my PR green?" without spending a
fetch.

**When you do want to pull something yourself — here or in any repo — reach for
shuck before raw GitHub API calls or the Actions UI:**

```sh
shuck logs <owner>/<repo> <pr>   # the exact failing step logs
shuck <pr>                       # CI + reviews + security for a PR
shuck pins                       # unpinned / stale workflow actions
shuck monitor                    # what the monitor is watching, and where it stands
```

If the binary is not on PATH, build it (`make build`, then `./bin/shuck`) or
install a release (`curl -fsSL https://raw.githubusercontent.com/justanotherspy/shuck/main/install.sh | bash`).

When shuck's output falls short of what you needed to debug something, that is
a finding: record it in `docs/PLAN.md` (the agent-feedback roadmap) or open an
issue.

## Contributing

`CONTRIBUTING.md` is the contributor entrypoint. The short version:

- Branch off `main`; use conventional-commit messages (`feat:` / `fix:` /
  `docs:` / `chore:` …) — release-drafter groups the changelog by them.
- Before pushing, run what CI runs: `make ci`
  (deps + lint + modernize-check + test + cover-check + build).
- CI (`ci.yml`) additionally gates on: `go.mod`/`go.sum` tidiness, `go vet`,
  govulncheck, actionlint + shellcheck, and **Plugin validate** — which fails
  if `.claude/skills/shuck/SKILL.md` or `.mcp.json` drift from their sources
  of truth under `plugins/shuck/` (the plugin's `SKILL.md` is also
  `go:embed`-ed into the binary for `shuck setup`). Update them together.
- Coverage must stay ≥ `COVER_THRESHOLD` (80%); CI posts a sticky coverage
  comment on PRs.
- Issue and PR templates live in `.github/`. Security vulnerabilities go
  through `SECURITY.md`, not public issues.
- The repo's own `.github/compliance.yml` is a full snapshot of its settings —
  if repo settings change, re-sync it with `shuck compliance discover`.

## Commands

Run `make help` for the full list. The essentials:

```sh
make tools           # install the pinned dev tools (lint, releaser, gopls…)
make build           # build ./bin/shuck
make test            # go test -race + coverage (coverage.out; main.go excluded)
make lint            # golangci-lint run
make fmt             # gofmt + goimports via golangci-lint
make modernize-check # fail if `go fix` modernizations are pending (CI gate)
make cover-check     # fail if coverage < COVER_THRESHOLD (80%; CI gate)
make vuln            # govulncheck vulnerability scan
make fuzz FUZZ=Fuzz… # actively fuzz one target (make fuzz-all: every target)
make ci              # exactly what CI runs
```

`make fix` / `fix-check` alias `modernize` / `modernize-check`. Also there:
`vet`, `tidy`, `bench`, `profile` / `pprof-cpu` / `pprof-mem`, `docker-build`,
`snapshot` (local goreleaser), and `hooks` (pre-commit).

## Architecture

Two ways in, one engine. **On demand** (the CLI / MCP tools): resolve target →
load/validate cache → fetch checks (cheap metadata) → drill only new
failed/cancelled jobs for logs → parse → extract errors → render → update
cache. **By subscription** (`shuck monitor`): a local daemon runs the same
fetch-and-distil steps on a timer for the working trees it follows, and emits
what changed as events.

| Package | Responsibility |
| --- | --- |
| `main.go` | Thin entry: dispatches `mcp` and `setup`, else `cli.Run`. Holds the `go:embed` of the plugin's `SKILL.md`. |
| `internal/cli` | Flag parsing + orchestration. Subcommands: `logs`, `reviews`, `all` (the bare-`shuck` default), `monitor`, `pins`, `action`, `image`, `security`, `compliance` (+ `discover`), `dependabot` (+ `discover`, `fix`), `version`, `upgrade`; single-letter aliases via `subcommandAliases` (`m` = monitor, `p` = pins). The exported cores (`Inspect`, `Security`, `Compliance`, `ComplianceDiscover`, `Dependabot`, `DependabotDiscover`, `DependabotFix`, `Action`, `Image`, `Images`, `Pins`) back both the CLI and the MCP server. `monitor.go` is a thin client over the daemon; `pins.go` also builds the cache-backed `pins.Resolver` the daemon is handed. |
| `internal/monitor` | The background monitor (`shuck monitor`): a local daemon that follows working trees, resolves each to its open PR, polls GitHub on an adaptive cadence, and publishes events. `git.go` reads a tree's repo + branch (worktrees included, no git library); `watch.go` the watch set; `poll.go` one target's round; `event.go` the event model + agent-facing rendering; `journal.go` the durable JSONL log + per-consumer cursors; `protocol.go`/`server.go`/`client.go` the one-line-JSON local IPC; `hook.go` the Claude Code hook entry points; `pins.go` the per-tree workflow pin audit. |
| `internal/pins` | `shuck pins` / `check_pins`: find the `uses:` references in a checkout's workflow and action files (`WorkflowFiles` → `Scan`, a schema-free `yaml.Node` walk keyed on any mapping key spelled `uses`) and classify each against its action's latest release (`Audit`, via a caller-supplied `Resolver`) into pinned / stale / unpinned / skipped, each finding carrying the corrected line. `Render` + `Document` are the text and stable-JSON views. Pure and offline-testable. |
| `internal/mcp` | Stdio MCP server (`shuck mcp`): a thin typed front-end over the `cli` cores (`mcp.go`) plus the monitor and pin tools (`monitor.go`: `monitor_status` / `monitor_events` / `monitor_watch` / `monitor_unwatch` / `check_pins`). |
| `internal/jsonout` | The stable, versioned `--json` schema. Its view types are deliberately separate from `model` so internal refactors don't break consumers. |
| `internal/action` | `shuck action`: pick the latest semver tag matching an `owner/action[@version]` ref (stable preferred, prerelease fallback; `Select`) → SHA-pin line / JSON (`action.Document`). |
| `internal/image` | `shuck image`: resolve a GHCR image ref's latest matching tag + manifest digest (`Select`) → digest-pin line / JSON (`image.Document` / `ListDocument`). |
| `internal/semver` | Tiny dependency-free semver (`Parse` / `Compare` / `Constraint.Matches`) shared by `action` / `image`. |
| `internal/security` | Sort + render a `model.SecurityReport` (code scanning, secret scanning, Dependabot) to text / JSON. Pure presentation. |
| `internal/compliance` | Strict-parse `.github/compliance.yml` (`Parse`) and `Evaluate` it against live settings into a `model.ComplianceReport`; the inverse snapshot (`Discover` / `FromActual`, comment-preserving yaml.Node patching) lives in `discover.go`. Pure logic. |
| `internal/dependabot` | Strict-parse `.github/dependabot.yml` (`Parse`), detect the repo's ecosystems from its file paths (`Detect`, `ecosystem.go`), and `Audit` the two into a `model.DependabotReport` (coverage + best-practice findings). `Discover` scaffolds/extends a best-practice config (comment-preserving yaml.Node append); `Fix` fills best-practice fields onto existing entries in place (comment-preserving yaml.Node patch). Pure logic. |
| `internal/release` | Self-update: resolve the latest release, download + checksum-verify, replace the binary. Backs `version --check` / `upgrade`. |
| `internal/setup` | `shuck setup`: install the embedded skill to `~/.claude/skills/shuck`, add a managed CLAUDE.md note, optionally register the MCP at user scope. |
| `internal/target` | Resolve owner/repo/PR from args or the local repo (go-git). |
| `internal/gh` | go-github (v89) wrappers: PR head (`GetPR`), open-PR lookup by branch (`FindOpenPR`), Actions runs/jobs/logs, checks, the free `RateRemaining` quota probe, security alerts, compliance reads (repo settings, branch protection incl. rulesets, Actions policy), the recursive Git Trees file listing (`RepoTree`, for dependabot ecosystem detection), GHCR Packages API. Plus two hand-rolled clients: GraphQL for reviews (`reviews.go`) and anonymous OCI registry-v2 (`registry.go`). `reviewcomments.go` is the REST review feed the monitor polls (`PRReviewsSince`, `PRReviewCommentsSince`, `PRCommentThread`). |
| `internal/cache` | On-disk cache under `~/.cache/shuck/…`: per-PR reports + whole raw job logs, action tag lists, security reports, image listings. `Purge(ttl, keep)` sweeps stale entries on every run. |
| `internal/logs` | Parse a job log into `##[group]`-delimited sections; extract the high-signal error excerpt. |
| `internal/distil` | The shared distillation core (`CIFailure`): raw job log + Actions-API step metadata → per-step failure detail (`FailedSteps`) + an agent-ready `Summary`. `CapSummary` byte-budgets a summary for delivery (UTF-8-safe line-prefix cut + caller's truncation note) — used for event bodies and for the text a hook injects. `ReviewComment` / `Review` format a review event for the monitor (goldens under `testdata/review/`); the CLI's reviews view is a separate GraphQL path. Pure — layers on `logs` / `classify` / `model`; backs `cli`, `mcp`, and `monitor`. |
| `internal/render` | Format a `model.Report` to text. |
| `internal/model` | Shared domain types (imports nothing internal). |

## Key design notes

- **Step commands come from the logs**, not workflow YAML
  (`logs.Section.Command` / `Kind`).
- **Step↔section matching** (`distil.CIFailure`; `cli.buildFailedSteps` is a
  thin wrapper) is the trickiest part: failed API steps are paired with
  `##[error]`-bearing log sections by order, with a whole-log fallback.
  Cancelled jobs are drilled the same way, best-effort, and never flip the
  exit code. Cover changes here with corpus cases in
  `internal/distil/testdata` (each case: `log.txt` + `job.json` +
  `result.golden.json`, also rendered to CLI goldens in
  `internal/cli/testdata/golden`); regenerate goldens with
  `go test ./internal/distil ./internal/cli -run Golden -update` — only when
  the output is *meant* to change.
- **Exit codes are operational, gating is opt-in**: report commands exit `0`
  whenever a report is produced (even one showing failures) and `2` on an
  operational error; `--exit-code` makes failures exit `1` for CI gating.
  Exception: `compliance` gates by default (`1` on drift; `--exit-zero`
  suppresses). See `cli.exitFor`.
- **Caching is advisory**: cheap metadata is always re-validated. On the same
  head commit, whole raw job logs are cached and re-parsed locally under the
  current `--full` / `--context` / `--pattern` flags, so re-runs cost no
  network. `logs` / `reviews` each persist their own dimension and copy the
  other from the existing cache, so neither clobbers the other. Action /
  security / image caches are TTL'd (1h) and keyed on a cheap
  `gh.DefaultBranchSHA` probe; `--refresh` forces a re-fetch.
- **Reviews**: `gh.PRReviews` (GraphQL — thread resolution is GraphQL-only)
  groups by verdict, collapses resolved/outdated threads to one line, and caps
  comments at `--review-comment-limit`. A cheap `gh.ReviewsFingerprint`
  short-circuits the full pull when nothing changed.
- **The monitor follows trees, not PR numbers**: a tree watch re-reads
  `.git/HEAD` every tick (`monitor.ReadCheckout`, worktree `gitdir:` pointer and
  shared `commondir` included) and re-resolves through `gh.FindOpenPR` when the
  checkout moves — or once a `ResolveInterval` while a branch still has no PR.
  Watches are what you register; *targets* (`owner/repo#42`) are what gets
  polled, so two watches on one PR cost one poll (a watch with no PR number is
  skipped by `due` entirely). Cadence per target: `ActiveInterval` 12s while a
  run is in flight, `IdleInterval` 90s when terminal, `DormantInterval` 5m once
  merged/closed, exponential `backoff` to `MaxBackoff` on errors, everything
  doubled below `LowRateThreshold` remaining REST quota.
- **A poll round is cheap first, expensive last**: `GetPR` → `ListJobs` +
  `OtherChecks` → `JobLog` **only** for jobs whose `<id>/<attempt>` is not
  already in `ReportedJobs`, since downloading a log is the one genuinely
  expensive call. The review half leads with the one-query
  `gh.ReviewsFingerprint`; while it is unchanged the REST listings are never
  issued. A first sighting records high-water marks and reports nothing — a PR's
  existing history is not news.
- **The journal is the delivery contract**: events are appended to
  `~/.cache/shuck/monitor/events.jsonl` with per-consumer cursors in
  `cursors.json`, so a daemon restart neither replays history nor loses a
  failure. `Drain` advances the cursor as it hands events over, which makes
  delivery **at-most-once per consumer** on purpose: repeating a CI failure into
  a session that already acted on it is worse than missing the tail of a batch
  nobody read. `Peek` (the Stop hook) reads without advancing.
- **Hooks may never cost a session anything**: every path in `monitor.RunHook`
  writes nothing and exits 0 — no daemon, no token, a malformed payload, an
  unknown event. The Stop hook stands down the instant `stop_hook_active` is
  set (that is what keeps it from looping), blocks only on `SeverityAction`
  events, and seeks past what it hands over. `SHUCK_MONITOR_DISABLE` /
  `SHUCK_MONITOR_NO_STOP` opt out.
- **Pin audit is repo-driven and ref-driven**: the checkout's own files are the
  source of truth (`.github/workflows/*.y{a,}ml`, the root `action.y{a,}ml`,
  `.github/actions/*/action.y{a,}ml`), walked as `yaml.Node` so line numbers and
  trailing comments are exact and any mapping key spelled `uses` matches — job
  steps, composite actions, and reusable-workflow `jobs.<id>.uses` alike.
  Whether a reference is unpinned is a property of the ref, **not** of whether
  resolution succeeded: a resolver failure costs the finding its suggested fix,
  never the finding. A suggested pin stays on the major the author chose (`@v4`
  → newest 4.x.x); a newer major goes in the note. A file that will not parse is
  one skipped finding, not an aborted audit.
- **Soft degradation, never false results**: security sources and compliance
  reads degrade independently (404 ⇒ disabled, 403 ⇒ forbidden/skipped); an
  unreadable setting is a *skipped* check, never a false pass or fail. Merge
  settings are only visible to classic tokens; branch protection unions
  classic rules with repository rulesets (stricter wins). The raw secret value
  is never read from the API, so it cannot leak.
- **Compliance config is the source of truth and partial**: only declared keys
  are checked; `compliance.Parse` rejects unknown keys and invalid enum values
  so a typo can't silently skip a check.
- **Dependabot audit is repo-driven**: the repo's *files* are the source of
  truth for which ecosystems exist (`dependabot.Detect` maps manifest paths to
  ecosystems), and the config is checked against them. Detection is conservative
  — an ecosystem the config declares but no manifest matched is an *info* note,
  never a false failure; coverage gaps are warnings (errors with
  `--error-on-missing-ecosystem`). `dependabot.Parse` is strict (unknown keys,
  bad ecosystems/intervals rejected). A missing config is a finding, not a fatal
  error; report exit codes follow the same opt-in `--exit-code` gating as the
  other report commands. Detection reads the local working tree for the local
  repo and `gh.RepoTree` (recursive Git Trees) for an explicit one.
- **Image pinning**: listing an owner's packages requires a classic token with
  `read:packages`; resolving one image falls back to the anonymous OCI
  registry-v2 API when tokenless or rejected. Cosign/referrer tags
  (`image.IsReferrerTag`) never win tag selection.
- **Network clients are stubbable package vars** (`cli.NewTagLister`,
  `cli.NewImageLister`, `newSecurityLister`, `newComplianceLister`) so tests
  and embedders never hit the network.

## Conventions

- Standard library `flag` for CLI parsing; no cobra. New subcommands go in
  `internal/cli`.
- `internal/model` stays dependency-free to avoid import cycles; domain types
  pass by value on purpose (gocritic's `hugeParam` / `rangeValCopy` are
  disabled in `.golangci.yml`).
- Errors from `fmt.Fprint*` to stdout/stderr are intentionally ignored (see
  the errcheck exclusion in `.golangci.yml`).
- `GOTOOLCHAIN=auto` (set by the Makefile): bump go.mod's `toolchain`
  directive when a newer Go patch fixes a govulncheck finding.
- Tests are table-driven where practical; pure logic is unit-tested without
  network.

## Testing, fuzzing & profiling

- `make test` writes `coverage.out` with `main.go` filtered out
  (`COVER_EXCLUDE`) — the numbers reflect `internal/` only. CI renders the
  report on PRs and gates at 80% (`make cover-check`).
- Every parser of untrusted input is fuzzed: `fuzz_test.go` in `logs`,
  `distil`, `semver`, `action`, `image`, `target`, `compliance`, `dependabot`,
  `pins`, and `release`. Targets
  assert semantic invariants (round-trips, selection contracts, fail-closed
  verification), not just panic-safety. Seed corpora run under `make test`;
  the nightly `fuzz.yml` runs `make fuzz-all`. Keep fuzz-target names unique
  module-wide; commit minimized crashers under `testdata/fuzz/<FuzzXxx>/` as
  regression seeds, then fix the bug.
- Benchmarks use `for b.Loop()` + `b.ReportAllocs()` (see
  `internal/logs/bench_test.go`); `make bench` runs them.

## Distribution & repo automation

- Tag-triggered `release.yml` runs GoReleaser: multi-platform builds, a cosign
  keyless signature over `checksums.txt`, SPDX SBOMs, SLSA provenance, and the
  Homebrew cask push. Versioning is `git describe`-derived (injected into
  `internal/cli.version`); there is no `VERSION` file.
- `docker.yml` builds/pushes the single multi-arch GHCR image
  (`ghcr.io/justanotherspy/shuck`, cosign-signed): `:edge` + `:sha-*` on pushes
  to main, semver tags + `:latest` via a `workflow_call` from `release.yml` (a
  `release:` trigger would never fire — token-created events don't trigger
  workflows).
- `ghcr-cleanup.yml` prunes GHCR weekly: only `sha-*` tags (keeping the 2
  newest) and untagged orphans are candidates; `edge` / `latest` / semver tags
  are never touched.
- Other automation: `scorecard.yml`, `semgrep.yml`, `secret-scan.yml`,
  `zizmor.yml` (workflow security), `labeler.yml`, `release-drafter.yml`, and
  Dependabot.
- The Claude Code plugin source lives under `plugins/shuck/` (manifest, hooks,
  prereq script, skill); `.claude/settings.json` enables it from the
  `justanotherspy/claude-plugins` marketplace.
