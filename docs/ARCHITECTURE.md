# shuck architecture

How shuck is built, end to end — the constraint everything else follows from,
the two ways to use it, the on-demand pipeline, the background monitor, the
Claude Code integration, and what ends up on disk.

This is the **as-built** reference. Where it describes behaviour, the behaviour
is in `internal/`; where it describes a number, the number is a named constant.

## What shuck is, and the one hard constraint

shuck answers one question well — *why is this pull request red?* — and a
handful of adjacent ones: what did reviewers ask for, what security alerts are
open, are the workflow actions SHA-pinned.

The constraint that shapes everything: **shuck is one portable binary you drop
on a laptop.** A CLI and a local background monitor in the same executable,
driven by a GitHub token from the environment. No service to
deploy, no webhook to receive, no account, no state anyone else can see.

That is not a preference, it is a gate. `ci.yml` runs the binary's import graph
through a check on every build:

```sh
go list -deps . | grep -E 'aws-sdk-go|aws-lambda-go|cloud\.google\.com|…'
```

A match fails the build. If a feature seems to need a cloud SDK, a serverless
runtime, or a server framework, it belongs outside shuck. The dependency budget
that follows from it is small on purpose: three direct modules (go-git,
go-github, `yaml.v3`).

## Two ways to use it, one engine

```
                  ┌──────────────────────────────┐
  on demand       │                              │
  shuck <pr> ────▶│ target ▸ cache ▸ gh ▸ distil │───▶ text / JSON
  shuck logs      │         ▸ render             │
                  │                              │
                  │    the same fetch + distil   │
  subscription    │                              │      ┌─────────┐  · CLI
  shuck monitor ─▶│ watch ▸ poll ▸ diff ▸ event  │─────▶│ journal │─▸· hooks
                  └──────────────────────────────┘
                                 │
                                 ▼
                        GitHub REST + GraphQL
```

**On demand (pull).** You run a command, it fetches, it prints, it exits. This
is `shuck`, `shuck logs`, `shuck reviews`, `shuck pins`, and the rest of the
report commands. Nothing persists but the cache.

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
gating. Producing a report is the success condition; whether the report is
happy reading is the caller's business.

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
upgraded together, so there is no version negotiation to get wrong. That
assumption is what `shuck upgrade` protects: it stops the running daemon after
replacing the binary, so the next hook starts one from the version now on disk
rather than leaving yesterday's daemon answering today's clients.

Clients are short-lived by design: a CLI subcommand or a hook.
Nothing keeps a connection open, so nothing leaks when a hook is killed
mid-call. The one exception is `events` with `Wait`: the daemon parks the
request on a broadcast channel that is closed and replaced on every publish, so
a waiter that arrives between two events still sees the second one.

A single parked read is capped at `monitor.MaxWait` (10 minutes) so no client
can reserve a goroutine and a connection indefinitely. That cap is the daemon's
business, not the caller's: `shuck monitor events --wait 30m` re-asks until the
half hour it was given has actually elapsed, because an early "nothing new" is
indistinguishable — same text, same exit code — from a genuinely quiet wait.

A `status`, `events`, or `poke` call refreshes the last-seen time of *every*
watch (`registry.TouchAll`), not just the one it named; registering a watch
refreshes that one. Beyond registration there is no per-watch refresh path, and
that is the intent: a client asking the monitor anything at all is the evidence
that somebody is still there, and which watch the question happened to be about
says nothing about whether the others still matter.

### Watches and targets are different things

A **watch** is what you registered. A **target** is what it currently resolves
to.

| | Watch | Target |
| --- | --- | --- |
| Identity | `tree:/abs/path` or `pr:owner/repo#42` | `owner/repo#42` |
| Created by | a client (`shuck monitor watch`, or a hook) | resolution, on a tick |
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

Watches expire after `DefaultWatchTTL` (12 hours) with no client asking the
monitor anything — not 12 hours of that particular watch going unmentioned,
since any call keeps every watch alive. A laptop closed overnight should not
still be polling GitHub in the morning.

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
lifecycle is recorded without an event, and one listing of each review feed
seeds the high-water marks and reports nothing — arriving at a PR with forty
comments is the state of the world, not forty things that just happened.

Every mark is a timestamp GitHub itself stamped on that data, never the daemon's
clock. The two clocks can disagree and the filtering happens on GitHub's, so a
mark taken from the local one is silently wrong: a second fast and it lands in
GitHub's future, swallowing every review submitted inside the skew — a reviewer
asks for changes and nobody is ever told. The newest timestamp in the data
cannot fail that way, because it came from the clock the filter uses. The ids
seen are remembered alongside the marks, because the next query deliberately
reaches back a second (`watermarkSlack`, covering GitHub's whole-second stamps)
and the newest existing review would otherwise arrive as news.

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

A CI failure's body is sized to the log rather than to a fixed budget: under
`wholeLogLimit` (8 KiB) the raw log goes in verbatim with GitHub's per-line
timestamps stripped, and above it `render.Job` produces the same digest
`shuck logs` prints. Both are then capped at `summaryLimit` (12 KiB) for the
journal, and the body always ends with the path the raw log was cached at
(`cache.JobLogPath`) — so an agent handed a trimmed excerpt reads the rest off
disk instead of going back to GitHub. Caching it there also warms the cache
`shuck logs` reuses.

Every event carries an id, a time, the watch and target it came from, a
one-line `Title`, an optional `Body`, and a URL. `Title` is enough to decide
whether to care; `Body` is enough to act without a follow-up call. That split is
what lets a consumer show a digest and expand on demand.

Severity is not quite a function of the kind. `Kind.Severity` starts from it —
`ci.failed`, `review.comment` and `review.submitted` are **action**, everything
else is **info** — and `Event.Severity`, which is what a consumer should ask,
demotes one case on the event's own detail: a submitted review whose headline
reads as an approval. (The verdict is not a field on `Event`; the journal record
and IPC payload are not widened for one consumer, so `approvedHeadline` reads it
back out of the headline the poller wrote.) Only the Stop hook currently reads
any of this, and it is the difference between "your build is red" holding a turn
open and "your build is green" not.

Three things sit on the quiet side of that line deliberately. An **approval** is
news, not a request: blocking a finish on one has the Stop hook hand back
"address this as part of the current task" over a reviewer saying the work is
done, which is the one intervention guaranteed to be wrong. `monitor.error` is
the monitor's own problem, and a network blip must not hold a finished turn
open. `pins.stale` is repo hygiene in a checkout nobody mentioned — telling an
agent mid-task to go fix something it did not break is not what it was asked to
do. Nothing is lost by any of the three: the `UserPromptSubmit` hook delivers
every event whatever its severity, so all of them still reach the session. They
just cannot block a finish.

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

A consumer is any stable string: a Claude Code session id, `cli`, whatever
`--consumer` names. `Drain` returns everything after that consumer's cursor and
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
| `DefaultWatchTTL` | 12h | every watch, once no client has asked the monitor anything for that long |
| `pinScanInterval` | 10m | floor between two pin scans of one tree, the file walk included |

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
first, cheap work second — each tree watch is offered to the pin audit, which
reads the tree's workflow files (`.github/workflows/*.y{a,}ml`, the root
`action.y{a,}ml`, and `.github/actions/*/action.y{a,}ml`) and classifies every
`uses:` reference in them.

A deadline is all the pacing there is. `scanPins` reads a tree at most once
every `pinScanInterval` (10 minutes) and hands its state back untouched
otherwise, and that deadline gates the filesystem walk itself rather than only
the release lookups behind it — a content hash can tell two versions of a tree
apart only *after* reading every file, which is exactly the cost worth avoiding
when a round happens once a second and workflow files change at human speed. The
deadline also moves before the audit runs, so a tree with no workflows at all,
or one whose `.github` cannot be read, stops costing a walk a second just as an
audited one does.

Two consequences are worth stating plainly. An edit to a workflow file surfaces
on the next scan rather than on the keystroke — nothing here watches the
filesystem. And a tree nobody has touched is still re-audited every ten minutes,
because an action can cut a release without anyone editing this repo, and a pin
goes stale exactly then.

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
rather than every time you touch the file. That state is written back only when
it actually changed — the moved deadline counts as a change — because the
alternative is rewriting `pins.json` once a second per watched tree to record
what it recorded a second ago.

## The Claude Code integration

The integration is hooks, not polling. The plugin (`plugins/shuck/`) registers
five monitor hooks — plus its prereq check at `SessionStart` — each a one-line
shim that runs `shuck monitor hook <event>`. All the logic is in the binary,
which reads the hook payload on stdin and writes the hook response on stdout;
the shim exists only so a session without shuck installed degrades to silence
instead of a hook error on every prompt.

| Hook | Reads from the payload | What it does | Writes |
| --- | --- | --- | --- |
| `SessionStart` | `cwd` (or `CLAUDE_PROJECT_DIR`, or the process cwd), `session_id`, `source` | Registers the tree as a watch — starting the daemon if this is the first session — then seeks the session's cursor to the present, but only when `source` says the session is genuinely new: a resume or a compaction fires `SessionStart` too, and seeking there would discard events the conversation has not been shown. | `hookSpecificOutput.additionalContext`: what the monitor is watching and what will arrive unasked. |
| `UserPromptSubmit` | `session_id` | Drains that session's pending events. Never starts a daemon — a prompt is not the moment. | `additionalContext`: a `<shuck-monitor>` block, or nothing at all when the batch is empty. |
| `PostToolUse` | `tool_name`, `tool_input.command` | On a Bash call matching `git push`, `gh pr create`, `gh pr ready`, `gh workflow run`, or `gh run rerun`, pokes the monitor. | nothing |
| `Stop` | `session_id`, `stop_hook_active` | Peeks at pending events; if any are actionable, seeks past them and blocks. | `{"decision":"block","reason":…}`, top level only — never also in `hookSpecificOutput`, because a response carrying the reason in both shapes risks the same CI failure being injected into the session twice. |
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
├── security/<owner>/<repo>/       security reports (1h TTL)
└── monitor/
    ├── daemon.sock                the listener, and the single-instance lock
    ├── endpoint.json              how to dial: network, address, token, pid
    ├── watches.json               the registered watch set
    ├── targets.json               per-PR poll state (head SHA, verdict, marks)
    ├── pins.json                  per-tree next-scan deadline + reported findings
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

The inspection caches are TTL'd and swept from both ends (`cache.Purge`). A
report command purges on every run, at an hour, exempting the entry the run in
front of you is writing. The daemon sweeps at the end of a round, no more often
than `cachePurgeInterval` (1 hour — a purge walks the whole cache tree while the
daemon wakes every second) and at a `cachePurgeTTL` of 24 hours, with no
exemption: it polls many targets rather than writing one entry, and nothing it
wrote this round is within a day of being a candidate anyway. The daemon has to
be one of the sweepers, because it is the only part of shuck that writes to that
cache without ever running a report command — for someone whose whole use of
shuck is `shuck monitor`, no other sweep runs and the job logs accumulate for
the life of the machine. The longer TTL is what makes the event body's promise
good: a day outlives `DefaultWatchTTL`, so a cached log survives for as long as
the watch that produced it possibly can. Purging stays advisory, so a failure is
logged and the round carries on.

An action or security entry ages by its one record file; a PR entry ages by the
newest of its record *and* its cached job logs, because the monitor writes logs
there and never writes a report — aging by the record alone would leave a PR
only the monitor ever touched growing logs that nothing reclaims. The
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
