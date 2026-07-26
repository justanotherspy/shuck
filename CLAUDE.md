# CLAUDE.md

Guidance for agents working in this repository.

## What this is

`shuck` is one portable Go binary — a CLI and a Claude Code plugin — that
prints the exact failing CI step logs for a GitHub PR. Its centrepiece is
`shuck monitor`: a local background daemon that follows the working trees you
are in, tracks whichever PR the current branch belongs to, and turns new CI
failures, review comments, and stale action pins into events a session is
handed as they happen. On demand it also summarizes PR reviews, lists a repo's
security alerts, audits a checkout's workflow action pins, and SHA-pins a
single GitHub Action. State lives under `~/.cache/shuck`.

The command surface is deliberately narrow, and the monitor is the point:
everything shuck does answers "what is wrong with the branch I am on right
now?". Repo-hygiene audits that answer a quarterly question instead — settings
against a policy file, Dependabot config coverage, GHCR image digests — were
removed rather than carried; if a proposed feature is not something you would
want pushed into a session mid-task, it probably does not belong here.

There is nothing to deploy: no server, no webhook, no credential beyond the
GitHub token. `ci.yml` holds a standing portability budget — a step that fails
the build if the binary's import graph ever picks up a cloud SDK, a serverless
runtime, or a server framework. If a feature seems to need one, it belongs
outside shuck.

## Dogfood shuck

This repo runs its own tool on itself. It does that **through the plugin**, not
around it: `.claude/settings.json` enables `shuck@justanotherspy`, whose
marketplace entry sources `plugins/shuck` straight out of this repository, so
the skill, the monitor and the hooks an agent gets here are the ones being
edited. There is deliberately no second copy of the skill under `.claude/` —
one file, `plugins/shuck/skills/shuck/SKILL.md`, is the skill, the plugin's
skill, and the file `go:embed`-ed into the binary for `shuck setup`. In dev
environments the `shuck` binary is on PATH too.

**The loop here is that the monitor watches and you get told.** The plugin's own
monitor (`shuck monitor stream`) registers this working tree for the lifetime of
the session, so after you push you do not poll: the next CI failure arrives as a
notification, with the failing step's logs already in it. It is monitor output,
not a message from the user. **Never wait for CI in a shell** — no background
`shuck --watch`, no `--wait`, no sleep-and-recheck; the stream is already going
to tell you. Nothing gates a finish any more, though, so **closing the loop is
yours**: a `ci.failed` notification is the only warning you get, and a task that
ends in a pull request is done when CI is green or you have said why it is not.
A PR this tree cannot imply goes on the list with `shuck monitor watch <target>`
and comes off it with `unwatch`. `shuck monitor` answers "is my PR green?"
without spending a fetch.

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
  govulncheck, actionlint + shellcheck, and **Plugin validate** — which runs
  `claude plugin validate --strict` over `plugins/shuck` and the marketplace
  manifest, so a malformed manifest, hook or monitor entry fails the build.
- Coverage must stay ≥ `COVER_THRESHOLD` (80%); CI posts a sticky coverage
  comment on PRs.
- Issue and PR templates live in `.github/`. Security vulnerabilities go
  through `SECURITY.md`, not public issues.

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

Two ways in, one engine. **On demand** (the report commands): resolve target →
load/validate cache → fetch checks (cheap metadata) → drill only new
failed/cancelled jobs for logs → parse → extract errors → render → update
cache. **By subscription** (`shuck monitor`): a local daemon runs the same
fetch-and-distil steps on a timer for the working trees it follows, and emits
what changed as events.

| Package | Responsibility |
| --- | --- |
| `main.go` | Thin entry: dispatches `setup`, else `cli.Run`. Holds the `go:embed` of the plugin's `SKILL.md`. |
| `internal/cli` | Flag parsing + orchestration. Subcommands: `logs`, `reviews`, `all` (the bare-`shuck` default), `monitor`, `pins`, `action`, `security`, `version`, `upgrade`; single-letter aliases via `subcommandAliases` (`m` = monitor, `p` = pins, `s` = security). The pipeline cores (`Security`, `Action`, `Pins`) are front-end-agnostic: flag parsing and rendering sit outside them. `monitor.go` is a thin client over the daemon; `pins.go` also builds the cache-backed `pins.Resolver` the daemon is handed. |
| `internal/monitor` | The background monitor (`shuck monitor`): a local daemon that follows working trees, resolves each to its open PR, polls GitHub on an adaptive cadence, and publishes events. `git.go` reads a tree's repo + branch (worktrees included, no git library); `watch.go` the watch set; `poll.go` one target's round; `event.go` the event model + agent-facing rendering; `journal.go` the durable JSONL log + per-consumer cursors; `protocol.go`/`server.go`/`client.go` the one-line-JSON local IPC; `hook.go` the Claude Code hook entry points; `stream.go` the liveness marker a running `shuck monitor stream` leaves behind (with `stream_unix.go`/`stream_other.go` for the pid check), which gives a second stream on one tree its own identity and keeps an older installed `hooks.json` from delivering what a notification already carried; `pins.go` the per-tree workflow pin audit. |
| `internal/pins` | `shuck pins`: find the `uses:` references in a checkout's workflow and action files (`WorkflowFiles` → `Scan`, a schema-free `yaml.Node` walk keyed on any mapping key spelled `uses`) and classify each against its action's latest release (`Audit`, via a caller-supplied `Resolver`) into pinned / stale / unpinned / skipped, each finding carrying the corrected line. `Render` + `Document` are the text and stable-JSON views. Pure and offline-testable. |
| `internal/jsonout` | The stable, versioned `--json` schema. Its view types are deliberately separate from `model` so internal refactors don't break consumers. |
| `internal/action` | `shuck action`: pick the latest semver tag matching an `owner/action[@version]` ref (stable preferred, prerelease fallback; `Select`) → SHA-pin line / JSON (`action.Document`). |
| `internal/semver` | Tiny dependency-free semver (`Parse` / `Compare` / `Constraint.Matches`) behind `action`'s tag selection. |
| `internal/security` | Sort + render a `model.SecurityReport` (code scanning, secret scanning, Dependabot) to text / JSON. Pure presentation. |
| `internal/release` | Self-update: resolve the latest release, download + checksum-verify, replace the binary. Backs `version --check` / `upgrade`. |
| `internal/setup` | `shuck setup`: install the embedded skill to `~/.claude/skills/shuck` and add a managed CLAUDE.md note. |
| `internal/target` | Resolve owner/repo/PR from args or the local repo (go-git). |
| `internal/gh` | go-github (v89) wrappers: PR head (`GetPR`), open-PR lookup by branch (`FindOpenPR`), Actions runs/jobs/logs, checks, the free `RateRemaining` quota probe, security alerts, and `content.go`'s `FileContent`/`FileNotFound` (the file at a PR head, for review-comment context). Plus one hand-rolled client: GraphQL for reviews (`reviews.go`). `reviewcomments.go` is the REST review feed the monitor polls (`PRReviewsSince`, `PRReviewCommentsSince`, `PRCommentThread`). |
| `internal/cache` | On-disk cache under `~/.cache/shuck/…`: per-PR reports + whole raw job logs, action tag lists, security reports. `Purge(ttl, keep)` sweeps stale entries. An action/security entry ages by its one record file; a PR entry ages by the *newest* mtime across `cache.json` **and** its cached job logs, because the monitor writes logs there and never writes a report. Sweeping is a side effect of the paths that own a cache — `prReport` before it fetches a PR, `resolveAction` and `Security` before their TTL'd entries — plus the daemon (`monitor.Daemon.sweepCache`, hourly, 24h TTL), without which a machine whose only use of shuck is `shuck monitor` would never reclaim a byte. |
| `internal/logs` | Parse a job log into `##[group]`-delimited sections; extract the high-signal error excerpt. |
| `internal/distil` | The shared distillation core (`CIFailure`): raw job log + Actions-API step metadata → per-step failure detail (`FailedSteps`) + an agent-ready `Summary`. `CapSummary` byte-budgets a summary for delivery (UTF-8-safe line-prefix cut + caller's truncation note) — used for event bodies and for the text a hook injects. `ReviewComment` / `Review` format a review event for the monitor (goldens under `testdata/review/`); the CLI's reviews view is a separate GraphQL path. Pure — layers on `logs` / `classify` / `model`; backs `cli` and `monitor`. |
| `internal/render` | Format a `model.Report` to text. |
| `internal/model` | Shared domain types (imports nothing internal). |

## Key design notes

- **An excerpt earns its lines**: `logs.Extract` collapses runs of
  success/progress output — go's per-package `ok`, jest's ticks, pytest's
  `PASSED`, cargo's `Compiling` (`logs.DefaultNoisePattern`) — into one
  `… (N passing/progress lines omitted) …` marker. It applies *before* the
  `ShortThreshold` check, because a 25-line `go test ./...` failure is under any
  sane threshold and still nineteen-twentieths noise; "short" is a budget for
  what an agent has to read, not a licence to spend it on packages that passed.
  Two rules keep it safe: nothing is collapsed unless the log has failure signal
  to show instead (an unrecognized log is never thinned on a guess), and a line
  matching the failure pattern is kept however much it looks like noise — so
  extending the pattern for a new runner cannot cost a finding. `--full` clears
  it, because full has to mean untouched.
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
  See `cli.exitFor`.
- **Caching is advisory**: cheap metadata is always re-validated. On the same
  head commit, whole raw job logs are cached and re-parsed locally under the
  current `--full` / `--context` / `--pattern` flags, so re-runs cost no
  network. `logs` / `reviews` each persist their own dimension and copy the
  other from the existing cache, so neither clobbers the other. Action /
  Action / security caches are TTL'd (1h) and keyed on a cheap
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
- **A run is reported when it is finished, not as it fails**: failures are
  grouped by run *attempt* (`groupRuns`) and held until that run has no jobs
  left in flight, so an agent is handed everything one workflow got wrong at
  once instead of being pulled back three times for three jobs of the same run.
  Batching is scoped to the run and not to the commit deliberately — a
  six-minute CodeQL run is not a reason to sit on `ci.yml`'s failures for six
  minutes. Two things override the wait: a job that went red inside
  `fastFailWindow` (60s) reports immediately, because being early is the whole
  point of the monitor and a lint failure should not queue behind a build; and a
  run past `runStallAfter` (30m) reports what it has, because a wedged job that
  holds its run's failures forever reports them never. Unknown timings read as
  "not early" — that costs promptness, never accuracy. `ci.started` is retired:
  a notification an agent can do nothing with is one it learns to ignore.
- **Cancelled is neither failed nor passed**: a cancelled job never produces an
  event of its own (it used to produce a `ci.failed` each, which turned one
  fail-fast failure into four events saying nothing). It is carried as a note on
  its run's failure event, and on the terminal verdict — where the headline stops
  saying "all checks passed" and says what was cancelled instead. A cancelled
  job is a check that did not run, and a false all-clear before a merge is the
  worst thing the monitor could say. Cancellations on a superseded head need no
  rule: a new head SHA resets the CI half of `prState` and they are never seen.
- **The journal is the delivery contract**: events are appended to
  `~/.cache/shuck/monitor/events.jsonl` with per-consumer cursors in
  `cursors.json`, so a daemon restart neither replays history nor loses a
  failure. `Drain` advances the cursor as it hands events over, which makes
  delivery **at-most-once per consumer** on purpose: repeating a CI failure into
  a session that already acted on it is worse than missing the tail of a batch
  nobody read. `Peek` reads without advancing — nothing in the plugin uses it
  now that the Stop gate is gone, but it stays on the wire for a caller that
  wants to see what is waiting without spending it. A session and a stream over
  the same tree are still two consumers with two cursors. **Any consumer whose
  identity is derived rather than announced seeds its cursor with `SeekNew`
  (`OpSeek` + `IfNew`)**: a stream's `stream:<watch id>` on start-up, and a
  Claude Code session's id on the first `PostToolUse` of the session that
  carries one. An identity nobody has used starts at the present rather than at
  the head of the retained journal — otherwise a first read would be served
  another branch's finished business — while one that already exists is left
  where it is, so a restart resumes and a second seed is a no-op.
- **A session only hears about its own sources**: one journal, but a read
  carries a `Scope` (the reader's working tree) and `Daemon.scopeFilter` keeps
  only what that scope owns. A watch records the trees that registered it
  (`Watch.Scopes`, bounded, most-recent-kept), so `shuck monitor watch <pr>`
  subscribes the session that ran it rather than everyone on the machine. The
  filter runs **after** `Drain`, which is what keeps the cursor honest: it
  advances over everything scanned, so a filtered-out event is behind the cursor
  the instant it is skipped and can never loop. Matching is on the resolved
  absolute path, never containment — this repo keeps worktrees *inside* the main
  checkout (`.claude/worktrees/<name>`), so containment would call every worktree
  its parent. The rule is fail-toward-delivery: an event no watch claims reaches
  everyone, because an event reaching nobody is worse than one reaching too many.
  The single exception is an event stamped with another watch's id — that is
  somebody's news, and a note about a tree this session has never heard of is
  exactly the cross-talk scoping exists to end. A pin finding is the one event
  whose subject is a *repository* (that is what deduplicating it per repository
  made it), so it is targeted `owner/repo` and every tree watch on that repo
  claims that key; nothing else may, and `TestWatchNotesNameTheTreeTheyAreAbout`
  holds the producers to it.
- **The plugin monitor is the only channel and the only voice; one hook does the
  one thing it cannot**: `shuck monitor stream` is what
  `plugins/shuck/monitors/monitors.json` runs, and every line it writes to stdout
  becomes a notification, so it renders whole batches with `monitor.FormatFeed`
  in one write and never puts a diagnostic there. `PostToolUse` is the only hook
  still installed, because a separate process is spawned once in the directory
  the session opened in and can see neither a later `cd` nor a tool call: it
  re-registers the session's tree on every call (this is what follows an agent
  into a worktree — `.claude/worktrees/<name>` sessions in this repo depend on
  it entirely), pokes after a push, and seeds the session cursor on the first
  Bash call. **No hook reads the journal, and no hook gates a finish** — the
  binary enforces both rather than the manifest, so an older installed
  `hooks.json` cannot reintroduce either. `hookUserPrompt` only registers,
  `hookStop` is a bare `return nil`, and `hookSessionStart` / `hookSessionEnd`
  remain unwired-but-alive for the same reason; every path still exits 0.

  **The Stop gate is retired, and why it went is the useful part.** It blocked a
  turn while the monitor held a `SeverityAction` event, which is a sound idea
  implemented against the wrong state: it decided on the last *known* actionable
  event with no notion of whether a newer head had superseded it. Its everyday
  behavior was therefore to re-hand a session the failure it had already fixed
  and pushed, one poll interval before the pass arrived — a turn spent, every
  time — while the case it existed for cost nothing in any session that simply
  acted on the notification. **A gate whose false-positive rate is structural
  and whose true positives are unobservable is not paying for itself.** Delivery
  without enforcement is the accepted trade: the stream says what happened, and
  what the agent does about it is the agent's business.

  The stand-down this replaced is worth remembering, because it was correct when
  written and became wrong underneath itself: `hookUserPrompt` delivered unless
  `StreamServes` said a stream was following *this directory*, but once a stream
  followed the *session* the two predicates disagreed the moment an agent entered
  a worktree — the stream kept delivering under the session's scope while the
  hook, seeing a directory no marker named, decided nobody was serving it and
  delivered everything a second time. The lesson is not that the predicate needed
  fixing: **two channels arbitrating who speaks is the bug, and one channel is the
  fix.** `TestHookUserPromptStaysSilentWhateverTheStreamIsDoing` holds every
  stream state to the same answer so the arbitration cannot come back.

  The stream marker under `~/.cache/shuck/monitor/streams/`
  (`monitor.StreamServes`, one file per process as `<hash>-<pid>.json`, matched
  on the recorded path) and the per-process `monitor.StreamIdentity` still keep
  two sessions in one directory from revoking each other's claim or sharing one
  cursor. `StreamServes` no longer gates anything the plugin installs; it
  survives only for the unwired `SessionStart` entry point an older install may
  still call.
- **Hooks may never cost a session anything**: every path in `monitor.RunHook`
  exits 0 and writes nothing that changes what the session does — no daemon, no
  token, an unusable cache directory (`NewClient` → `monitor.Dir`), a malformed
  payload, an unknown event. Saying that a feed is never coming is the stream's
  job, not a hook's: `monitor stream` stands down in one plain sentence, which is
  the only voice the plugin has left now that nothing runs at `SessionStart` and
  nothing runs at `Stop`. `monitor stream` holds itself to the same bar: one
  prose line on stdout and exit 0 for every start-up failure — or, under
  `--json`, one more line of line-delimited JSON, since the mode a consumer
  opted into has to stay parseable through the line that says the feed ended.
  `SHUCK_MONITOR_DISABLE` opts out; the stream reads it once, at startup.
  `SHUCK_MONITOR_NO_STOP` is gone with the gate it disabled.
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
- **Soft degradation, never false results**: security sources degrade
  independently (404 ⇒ disabled, 403 ⇒ forbidden/skipped), so an unreadable
  source is *skipped*, never a false all-clear. A repo that 404s on every
  source would read as "everything disabled", so `cli.Security` confirms the
  repository exists (`gh.IsNotFound` on a cheap probe) before reporting that.
  The raw secret value is never read from the API, so it cannot leak.
- **Network clients are stubbable package vars** (`cli.NewTagLister`,
  `newSecurityLister`) so tests and embedders never hit the network.

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
  `distil`, `semver`, `action`, `target`, `pins`, `monitor`, `cache`, `gh`,
  `model`, `render`, and `release`. Targets assert semantic invariants
  (round-trips, selection contracts, fail-closed verification), not just
  panic-safety. Seed corpora run under `make test`;
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
- The Claude Code plugin source lives under `plugins/shuck/` (manifest,
  `monitors/monitors.json` — which execs `shuck monitor stream` directly, with no
  shim — the `PostToolUse` hook, the only one still installed, and its
  `scripts/monitor.sh` shim, skill); `.claude/settings.json` enables it
  from the
  `justanotherspy/claude-plugins` marketplace — *not* from this repo's own
  `.claude-plugin/marketplace.json`, so a plugin change here does not take
  effect in this repo's sessions until it lands there too. `monitors.json` must
  be a bare JSON array at the default path and must not also be declared in the
  manifest (a manifest declaration shadows the folder); nothing in CI reads that
  file, so `claude plugin validate` passing says nothing about whether the
  monitor will arm.
