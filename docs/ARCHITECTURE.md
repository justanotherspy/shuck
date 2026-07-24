# shuck architecture

How shuck is built, end to end — the constraint everything else follows from,
the two ways to use it, the on-demand pipeline, the background monitor, the
Claude Code integration, and what ends up on disk.

This is the **as-built** reference. Where it describes behaviour, the behaviour
is in `internal/`; where it describes a number, the number is a named constant.

## What shuck is, and the one hard constraint

shuck answers one question well — *why is this pull request red?* — and a
handful of adjacent ones: what did reviewers ask for, what security alerts are
open, do the repo's settings match its policy, is Dependabot covering the
ecosystems in the tree, are the workflow actions SHA-pinned.

The constraint that shapes everything: **shuck is one portable binary you drop
on a laptop.** A CLI, an MCP server, and a local background monitor in the same
executable, driven by a GitHub token from the environment. No service to
deploy, no webhook to receive, no account, no state anyone else can see.

That is not a preference, it is a gate. `ci.yml` runs the binary's import graph
through a check on every build:

```sh
go list -deps . | grep -E 'aws-sdk-go|aws-lambda-go|cloud\.google\.com|…'
```

A match fails the build. If a feature seems to need a cloud SDK, a serverless
runtime, or a server framework, it belongs outside shuck. The dependency budget
that follows from it is small on purpose: five direct modules (go-git,
go-github, the MCP SDK, `x/term`, `yaml.v3`).

## Two ways to use it, one engine

```
                  ┌──────────────────────────────┐
  on demand       │                              │
  shuck <pr> ────▶│ target ▸ cache ▸ gh ▸ distil │───▶ text / JSON
  MCP tools       │         ▸ render             │
                  │                              │
                  │    the same fetch + distil   │
  subscription    │                              │      ┌─────────┐  · CLI
  shuck monitor ─▶│ watch ▸ poll ▸ diff ▸ event  │─────▶│ journal │─▸· MCP
  (a local daemon)│                              │      └─────────┘  · hooks
                  └──────────────────────────────┘
                                 │
                                 ▼
                        GitHub REST + GraphQL
```

**On demand (pull).** You run a command, it fetches, it prints, it exits. This
is `shuck`, `shuck logs`, `shuck reviews`, `shuck pins`, and the MCP inspection
tools. Nothing persists but the cache.

**By subscription (the monitor).** You register a working tree once. A local
daemon follows it, notices what changed, and hands each change to whoever asks
next. This is `shuck monitor` and the Claude Code hooks.

They are not two implementations. The monitor calls the same `internal/gh`
client, the same `internal/distil` core, the same `internal/pins` audit, and the
same cache-backed tag resolver the CLI builds. What differs is only what starts
the work (a command vs. a timer) and where the result goes (stdout vs. a
journal).

## The on-demand pipeline

```
resolve target ─▸ load/validate cache ─▸ fetch check metadata ─▸ drill new
failures ─▸ parse log ─▸ extract errors ─▸ render ─▸ update cache
```

1. **Resolve the target** (`internal/target`). `owner/repo 42`, `owner/repo#42`,
   a PR URL, an Actions run/job URL, a PR "Checks" tab link, a bare number, or
   nothing at all — in which case the local checkout's remote and branch decide.
2. **Load and validate the cache** (`internal/cache`). Cheap metadata is always
   re-validated against GitHub; the cache is advisory, never authoritative. On
   the same head commit, whole raw job logs already downloaded are re-parsed
   locally under the *current* `--full` / `--context` / `--pattern` flags, so
   re-running with different extraction knobs costs no network.
3. **Fetch check metadata** (`internal/gh`). Runs for the head SHA, their jobs,
   and the non-Actions check runs. This is the cheap half.
4. **Drill only what is new.** A job log is the one expensive call in the whole
   pipeline, so only failed and cancelled jobs are drilled, and only those not
   already cached for this commit.
5. **Parse and extract** (`internal/logs`, `internal/distil`). The log is split
   into `##[group]`-delimited sections; failed API steps are paired with
   `##[error]`-bearing sections by order, with a whole-log fallback. Step
   commands come from the log, never from workflow YAML. Each failed step gets
   an error excerpt and a coarse failure class (`internal/classify`).
6. **Render** (`internal/render`) or emit the versioned document
   (`internal/jsonout`).

Exit codes are operational: `0` whenever a report was produced (even one full
of failures), `2` on an operational error, `1` only when `--exit-code` opts into
gating. `shuck compliance` inverts that default because drift is the whole
point of running it.

## The background monitor

`internal/monitor`. A long-lived local process that keeps track of the pull
requests you are actually working on and turns what changes on GitHub into a
stream of events.

### Daemon lifecycle, and one instance by construction

There is no lock file and no PID check. **The listener is the lock**: the daemon
binds `~/.cache/shuck/monitor/daemon.sock`, and a unix socket path can only be
bound once. A second daemon that finds the path taken *dials* it — an answer
means a live daemon (`ErrAlreadyRunning`, which the CLI reports as "already
running", not as a failure), and a refused connection means the socket outlived
a crash and may be removed and rebound.

Where a unix socket is not available, the daemon falls back to `127.0.0.1:0`
and mints a 32-byte random token. There the address grants no authority, so
every request must present the token, compared in constant time.

Once listening it writes `endpoint.json` (network, address, token, pid) and
removes it, with the socket, on the way out. Its presence is a hint that a
daemon exists, never a promise.

Nobody starts the daemon by hand. A client that cannot reach one re-execs the
same binary as `shuck monitor run --detached`, with the parent's environment
(so it polls with the same token), its standard streams detached, and waits up
to `startTimeout` (1.5s) for it to answer a ping. Two clients racing to do this
is normal: one wins the bind, the other gets `ErrAlreadyRunning`.

The daemon exits on: a client's `stop`, a signal, or — when it was started on
demand — running out of watches. That last rule is exactly
`ExitWhenIdle = detached && !stay`: a daemon a client spawned should not outlive
the sessions that wanted it, while one run in the foreground, or detached with
`--stay`, keeps waiting because somebody is about to give it something.

### The IPC protocol

One JSON request per line, one JSON response per line, connection closed.

```
client                                   daemon
  │  {"op":"events","consumer":"sess-1"}   │
  ├───────────────────────────────────────▶│  read line (30s deadline,
  │                                        │   extended by Wait)
  │  {"ok":true,"events":[…],"cursor":47}  │
  │◀───────────────────────────────────────┤
  │  close                                 │
```

Ops: `ping`, `status`, `watch`, `unwatch`, `events`, `seek`, `poke`, `stop`.
The protocol is deliberately dull — both ends ship in the same binary and are
upgraded together, so there is no version negotiation to get wrong. A client
that meets a daemon it cannot talk to restarts it.

Clients are short-lived by design: a CLI subcommand, an MCP tool call, a hook.
Nothing keeps a connection open, so nothing leaks when a hook is killed
mid-call. The one exception is `events` with `Wait`: the daemon parks the
request on a broadcast channel that is closed and replaced on every publish, so
a waiter that arrives between two events still sees the second one.

Every op that touches a watch refreshes its last-seen time. A client asking
about a watch is exactly the evidence that somebody still cares about it.

### Watches and targets are different things

A **watch** is what you registered. A **target** is what it currently resolves
to.

| | Watch | Target |
| --- | --- | --- |
| Identity | `tree:/abs/path` or `pr:owner/repo#42` | `owner/repo#42` |
| Created by | a client (`watch`, a hook, an MCP tool) | resolution, on a tick |
| Holds | path, branch, last resolution, TTL clock | head SHA, verdict, review high-water marks, next poll |
| Persisted in | `watches.json` | `targets.json` |

The split is what makes retargeting free and polling cheap. A tree watch is
re-read every tick (`ReadCheckout`: the enclosing `.git`, HEAD, and the origin
URL from the shared config — a linked worktree's `gitdir:` pointer and
`commondir` are followed, so worktrees behave like clones). If the repository or
branch moved, or the branch has no PR and `ResolveInterval` (1 minute) has
passed, `gh.FindOpenPR` runs and the watch retargets, emitting a
`watch.target` event. A settled watch that already has its PR costs a couple of
small file reads per tick and no network at all.

Because targets are keyed by `owner/repo#42`, two watches that land on the same
pull request — a tree and an explicitly pinned PR, say, or two worktrees — are
polled **once** between them and produce one event each time. Poll state for a
target no watch points at any more is pruned, so moving through ten branches
does not leave ten pollers behind.

Watches expire after `DefaultWatchTTL` (12 hours) with nobody asking about
them. A laptop closed overnight should not still be polling GitHub in the
morning.

### One poll round, in order, with what it costs

`poller.Poll` runs for one target and never returns an error — a failed round
is itself reportable, and a monitor that stopped because one call failed would
be worse than useless.

| Step | Calls | When |
| --- | --- | --- |
| `GetPR` | 1 REST | always — it is what the rest of the round is diffed against |
| lifecycle diff | 0 | in memory; emits `pr.state` on a change |
| `ListJobs` | 1 REST per run for the head SHA, +1 for the run list | always |
| `OtherChecks` | 1 REST (paginated) | always; a failure is logged, not fatal |
| `JobLog` + `distil.CIFailure` | 1 REST per **newly** failed job | only for a `<job id>/<attempt>` not already reported |
| `ReviewsFingerprint` | 1 small GraphQL query | always |
| `PRReviewsSince` / `PRReviewCommentsSince` | 2 REST | **only** when the fingerprint moved |
| `PRCommentThread` | 1 REST per reply | only for comments that are replies |
| `FileContent` | 1 REST per commented file | only for new comments, right side |
| `RateRemaining` | 1 REST, free (does not count against quota) | always, to pace the next interval |

Two things carry most of the cost saving. **A log is downloaded once**: a job
that failed three polls ago has not changed its mind, so `ReportedJobs`
suppresses the re-drill, and a re-run — a new attempt number — is a new key and
is drilled again. **The review fingerprint is a gate**: while that one query's
answer is unchanged, nothing about the PR's reviews has moved and the two REST
listings are never issued.

A push resets the CI half: a new head SHA clears the announced flag, the
verdict, and the reported-job set, because every conclusion held was about a
commit that is no longer current.

First sightings are deliberately silent. The first time a PR is seen its
lifecycle is recorded without an event, and the review high-water marks are set
to *now* — arriving at a PR with forty comments is the state of the world, not
forty things that just happened.

### The event model

An event exists because an agent would act differently knowing it, not because
GitHub changed a field.

| Kind | Fires when | Body |
| --- | --- | --- |
| `ci.failed` | a job newly failed or was cancelled | the distilled failing steps, capped at 12 KiB |
| `ci.passed` | every check on the head commit reached a green terminal state | — |
| `ci.started` | first sighting of checks for a head commit, with something still running | — |
| `review.comment` | a new inline comment | comment + diff hunk + ±10 lines of the file at the comment's commit + the thread it replies to |
| `review.submitted` | a review was submitted | verdict + body, inline comments folded in |
| `pr.state` | the PR changed lifecycle | — |
| `pins.stale` | a workflow reference is unpinned or behind its release | the corrected `uses:` line |
| `watch.target` | a watch retargeted, or explained why it cannot | — |
| `monitor.error` | a round failed | the error |

`ci.passed` deserves a note, because it is inferred rather than read. Nothing
in the API says a commit is green: `ListJobs` returns only failed, cancelled,
and running jobs, and `OtherChecks` returns only non-Actions checks that have
already gone red. So the pass is deduced from having watched checks run and
then stop — `prState.Announced` records that this commit had jobs in flight,
and the round that finds none left, none failed, and no red external check is
the round they all passed. A commit whose checks were already finished when the
watch began stays silent, which is correct: it is a fact, not news. The
consequence to know about is the converse — a watch started mid-run reports the
verdict, one started after the run does not.

Every event carries an id, a time, the watch and target it came from, a
one-line `Title`, an optional `Body`, and a URL. `Title` is enough to decide
whether to care; `Body` is enough to act without a follow-up call. That split is
what lets a consumer show a digest and expand on demand.

Each kind maps to a severity: `ci.failed`, `review.comment`, `review.submitted`
and `pins.stale` are **action**, everything else — including `monitor.error` —
is **info**. Only the Stop hook currently reads it, and it is the difference
between "your build is red" holding a turn open and "your build is green" not.
`monitor.error` is deliberately on the quiet side of that line: a failed poll is
the monitor's problem, and a network blip must not hold a finished turn open.

Repeat suppression lives in the per-target state: reported job keys, reported
review and comment ids (bounded at 200 each — they exist to suppress duplicates
across a couple of polls, not to be a permanent record), the last lifecycle, the
last error string. `monitor.error` reports the *first* failure of a run of
identical failures; the rest lengthen the backoff instead of filling the feed.

### The journal and cursors

```
events.jsonl   append-only, one Event per line, id-ordered
               trimmed to the newest 1500 past 2000, rewritten atomically
cursors.json   { "<consumer>": <last delivered id>, … }
```

A consumer is any stable string: a Claude Code session id, `cli`, whatever an
MCP caller passes. `Drain` returns everything after that consumer's cursor and
advances it; `Since` reads without one; `Seek` moves a cursor without
delivering; `Peek` reads pending events and leaves the cursor alone.

Two properties matter:

- **Durable.** The daemon outlives the sessions reading from it and can be
  restarted underneath them. A session that reconnects after a restart must not
  be told CI is fine because the failure died with the previous process — so
  both events and cursors are on disk, and a corrupt or truncated line is
  skipped rather than treated as fatal.
- **At-most-once per consumer.** The cursor advances as the events are handed
  over, before the caller has done anything with them. That is the deliberate
  trade: re-delivering a CI failure into a session that already fixed it is
  worse than losing the tail of a batch nobody read. Consumers that need to look
  before committing use `Peek`.

A consumer starting fresh should call `Seek` first — its cursor then sits at the
present, and it hears what happens next rather than the last hour of another
session's history. That is exactly what the `SessionStart` hook does.

Cursors that fall more than a journal-length behind the retained window are
dropped on the next save, so sessions coming and going do not grow the file
without bound.

### Cadence and rate discipline

The daemon wakes every second. That is not the poll interval — each target
carries its own deadline and the tick is only the resolution at which deadlines
are honoured. It is what makes a branch switch show up immediately instead of at
the next poll.

| Constant | Value | Applies to |
| --- | --- | --- |
| `ActiveInterval` | 12s | a run still in flight (or no verdict yet) |
| `IdleInterval` | 90s | an open PR whose checks are all terminal |
| `DormantInterval` | 5m | merged or closed |
| `ResolveInterval` | 1m | a tree watch that has not found a PR |
| `MaxBackoff` | 15m | ceiling on the ×3 error backoff |
| `LowRateThreshold` | 500 | remaining REST quota below which every interval doubles |
| `DefaultWatchTTL` | 12h | a watch nobody has asked about |
| `pinScanInterval` | 10m | floor between two pin audits of one tree |

A target claims its next slot *before* the poll runs, so a slow round does not
queue up behind itself on the next tick. `poke` (what the `PostToolUse` hook
calls after a push) sets the next poll to now and clears the failure count,
because right after a push the interesting answer is seconds away and waiting
out the interval is latency for nothing.

The quota check is honest self-defence rather than politeness: the monitor
shares one token with everything else the developer is doing, and a monitor left
running must never be the reason a `git push` cannot be checked.

### The pin audit

The monitor also watches the tree itself. After the PR polls — expensive work
first, cheap work second — each tree watch's workflow files are collected
(`.github/workflows/*.y{a,}ml`, the root `action.y{a,}ml`, and
`.github/actions/*/action.y{a,}ml`) and hashed, contents and names both.

The hash is what makes this affordable. Reading a handful of small YAML files
every second costs nothing; asking GitHub about every action they reference does
not. So the audit runs when the hash moves — which is to say, exactly when you
have just written or edited a workflow — or once every `pinScanInterval`
otherwise, because an action can cut a release without anyone touching this
repo, and a pin goes stale exactly then.

`internal/pins` splits in two so both halves stay testable offline. `Scan` is
pure text work: a schema-free `yaml.Node` walk that matches any mapping key
spelled `uses`, which covers job steps, composite actions, and reusable-workflow
`jobs.<id>.uses` without tracking GitHub's schema, and keeps exact line numbers
and trailing comments. `Audit` classifies each reference — pinned, stale,
unpinned, skipped — asking a caller-supplied `Resolver` for the network part.
The monitor is handed the same cache-backed resolver `shuck action` and
`shuck pins` use, so a suggested pin is identical wherever it comes from.

Findings already reported for a tree are remembered by file, line, and
reference, so an unpinned action you have decided not to fix is mentioned once
rather than every time you touch the file.

## The Claude Code integration

The integration is hooks, not polling. The plugin (`plugins/shuck/`) registers
five monitor hooks — plus its prereq check at `SessionStart` — each a one-line
shim that runs `shuck monitor hook <event>`. All the logic is in the binary,
which reads the hook payload on stdin and writes the hook response on stdout;
the shim exists only so a session without shuck installed degrades to silence
instead of a hook error on every prompt.

| Hook | Reads from the payload | What it does | Writes |
| --- | --- | --- | --- |
| `SessionStart` | `cwd` (or `CLAUDE_PROJECT_DIR`, or the process cwd), `session_id` | Registers the tree as a watch — starting the daemon if this is the first session — then seeks the session's cursor to the present. | `hookSpecificOutput.additionalContext`: what the monitor is watching and what will arrive unasked. |
| `UserPromptSubmit` | `session_id` | Drains that session's pending events. Never starts a daemon — a prompt is not the moment. | `additionalContext`: a `<shuck-monitor>` block, or nothing at all when the batch is empty. |
| `PostToolUse` | `tool_name`, `tool_input.command` | On a Bash call matching `git push`, `gh pr create`, `gh pr ready`, `gh workflow run`, or `gh run rerun`, pokes the monitor. | nothing |
| `Stop` | `session_id`, `stop_hook_active` | Peeks at pending events; if any are actionable, seeks past them and blocks. | `{"decision":"block","reason":…}` at the top level **and** inside `hookSpecificOutput` — both shapes have been current, and unknown fields are ignored by the reader. |
| `SessionEnd` | `session_id` | Retires the session's cursor. | nothing |

Three properties make the Stop hook safe rather than a trap. It stands down the
instant `stop_hook_active` is set, so it can never loop. It blocks only on
events that actually ask for something, so a passing build never delays a
finish. And it peeks rather than drains before deciding, so events it chooses
not to act on are still there for the next prompt.

The whole integration is written to be impossible to blame. `RunHook` returns 0
on every path — no daemon, no token, a malformed payload, an unknown event — and
writes nothing when it has nothing to say. A background convenience must never
be the reason a session stalls or a prompt is rejected. The only thing a broken
monitor should cost you is the monitoring. Injected context is capped at 3.5 KB
(Claude Code truncates a large `additionalContext` silently, so shuck does the
cut itself and points at `shuck monitor events --all` for the rest), and a whole
hook interaction is bounded at 3 seconds.

Two environment variables opt out: `SHUCK_MONITOR_DISABLE` (any value) turns
every hook into a no-op; `SHUCK_MONITOR_NO_STOP` disables only the Stop hook.

## State on disk

Everything lives under one root — `~/.cache/shuck` by default, or `$SHUCK_HOME`
when set, which lets a test redirect the whole of shuck's on-disk state with one
variable.

```
~/.cache/shuck/
├── cache/<owner>/<repo>/<pr>/     per-PR report + whole raw job logs
├── actions/<owner>/<repo>/        action tag lists (1h TTL)
├── images/<owner>/                GHCR image listings (1h TTL)
├── security/<owner>/<repo>/       security reports (1h TTL)
└── monitor/
    ├── daemon.sock                the listener, and the single-instance lock
    ├── endpoint.json              how to dial: network, address, token, pid
    ├── watches.json               the registered watch set
    ├── targets.json               per-PR poll state (head SHA, verdict, marks)
    ├── pins.json                  per-tree workflow digest + reported findings
    ├── events.jsonl               the event journal
    ├── cursors.json               per-consumer delivery cursors
    └── daemon.log                 the daemon's own diagnostics
```

Directories are `0700` and files `0600`, everywhere. That is not decoration:
these files hold CI logs and review comments from private repositories, and the
socket's containing directory *is* the access control on the IPC. Every state
file the daemon rewrites (endpoint, watches, targets, pins, cursors, and a
rotated journal) is written through a temp file and renamed, so a reader sees
either the previous contents or the new ones, never half of each — the daemon
rewrites on a tick while clients read at will.

The inspection caches are TTL'd and swept on every run (`cache.Purge`). The
journal is bounded at 2000 events (rotation trims back to 1500, so a
rewrite happens once every few hundred appends rather than on every one);
history older than that belongs in the pull
request, not here.

## Security and privacy

- **Nothing leaves the machine except calls to GitHub.** There is no telemetry,
  no phone-home, and no shuck-operated service to talk to. The portability gate
  in CI is also the enforcement mechanism: a binary with no cloud SDK in its
  import graph cannot quietly grow one.
- **The GitHub token comes from the environment** (`GITHUB_TOKEN` / `GH_TOKEN`,
  or `--token`) and is never written to disk. A daemon started by a client
  inherits the parent's environment, which is also why the *client* resolves the
  token: a missing one is reported by the command the person just ran, rather
  than failing silently inside a background process. `shuck monitor status`
  reports quota headroom, never the token.
- **The IPC is local and unauthenticated by filesystem permission.** A unix
  socket in a `0700` directory says who may connect; there is no credential to
  manage or leak. The loopback fallback, where any local process could reach the
  port, requires a 32-byte random bearer token compared in constant time.
- **The journal holds sensitive text.** Distilled CI logs and review comments
  from private repositories sit in `events.jsonl` in the clear, at `0600`. It is
  the same exposure as the existing report cache next to it, and the same
  mitigation. It is bounded and rotated; delete the directory to clear it.
- **Secrets are not fetched.** The security commands never read a secret
  scanning alert's raw value from the API, so it cannot be logged, cached, or
  rendered.
- **Reads degrade, they do not lie.** A source that is disabled (404) or
  invisible to the token (403) is reported as skipped — never a false pass and
  never a false failure. The same rule governs the pin audit: a reference that
  could not be resolved keeps its unpinned finding and loses only the suggested
  fix.
