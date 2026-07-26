---
name: shuck
description: >-
  Keep a background monitor on a pull request and hear about CI failures,
  review comments, and stale action pins as they happen — plus show the exact
  failing CI step logs, summarize a PR's reviews, list a repo's security
  alerts, and SHA-pin GitHub Actions. Everything runs through the one `shuck`
  CLI (add `--json` to any report for structured output).
  Use whenever a GitHub Actions workflow is in play: to watch a PR in the
  background and be told when CI goes red or a reviewer comments, to wait for
  checks to finish after a push, to learn why CI is failing, debug a failed
  check, pull a PR's error logs, download a run's archived artifacts, see what
  reviewers asked for, check that a workflow's actions are SHA-pinned and
  current, triage security findings, or SHA-pin an action.
---

# shuck — hear about your PR instead of checking on it

`shuck` runs a **background monitor** that follows the working tree you are in
and hands you CI failures, review comments, and stale action pins as they
happen. So the answer to "is CI done yet?" is to *wait for the event*, never to
poll. The same binary also answers the one-shot questions on demand: the failing
step's logs, a PR's reviews, a repo's security alerts, an Action's SHA pin.

Reach for it instead of paging through the GitHub UI or hand-rolling `gh`.

## The monitor is the point

`shuck monitor` is a local daemon. No webhook, no server, no extra credentials —
the same GitHub token the rest of shuck uses.

Point it at a working tree and it follows that tree: it reads the branch out of
`.git/HEAD` (worktrees included), finds the open PR for that branch, and
re-checks on a cadence that tightens while CI is running (12s), relaxes when the
PR is idle (90s), and goes dormant once it is merged or closed (5m). Switch
branches, switch worktrees, or open a PR for the branch you are on, and it
**retargets itself** — you never tell it a PR number. A branch with no open PR
costs nothing; two watches that land on the same PR are polled once between them.

It emits **events**, each with a one-line title and an agent-ready body:

| Kind | What happened | Body |
| --- | --- | --- |
| `ci.failed` | a workflow run finished with red jobs | every failed job's log: whole when it is under 8 KiB, the distilled failing steps when it is longer — plus where the raw log was cached, and a note of what that run cancelled or is still running |
| `ci.passed` | every check on the head commit reached a terminal state with nothing red (once per commit) | the jobs that were cancelled, if any — they were not verified |
| `review.comment` | a new inline review comment | the diff hunk, ±10 lines of the file at the PR head, and earlier thread comments when it is a reply |
| `review.submitted` | a review was submitted: approved, changes requested, or commented | the review and its inline comments |
| `pr.state` | the PR was opened, merged, closed, or marked ready for review | — |
| `pins.stale` | a workflow action is not SHA-pinned, or its pin is behind the latest release — **only on a branch that has itself changed a workflow file** | the corrected `uses:` line |
| `watch.target` | the watch retargeted: branch switch, PR found, PR lost | — |
| `monitor.error` | a poll failed (reported once, then backed off) | the error |

### With the shuck plugin you do not have to do anything

The plugin runs `shuck monitor stream` for the lifetime of the session. It
registers the working tree you are in and turns every event into a notification
as it happens. **That stream is the delivery channel — there is nothing to poll
for, nothing to set up, and no other route to think about.**

The stream follows the **session**, not the directory it was started in. That
distinction is the whole reason a session opened outside a repository still
works: Claude Code spawns the monitor once, in the directory the session opened
in, and a long-running process cannot see you `cd` afterwards. So the hooks —
which are handed the session's current directory on every payload — register it,
and because both they and the stream are tagged with the same session id, the
already-running stream picks up the new tree on its next tick. Open a session in
a parent directory, move into a checkout, switch worktrees or switch branches:
it retargets with nothing to restart and no PR number to give it.

Two hooks sit beside the stream, and neither delivers. `SessionStart` registers
the directory the session opened in. `PostToolUse` re-registers it on every tool
call — which is what catches a session that has moved, including into a
worktree — and additionally pokes the monitor after a `git push` /
`gh pr create` / `gh run rerun`, so the new run is picked up in seconds instead
of at the next interval. A monitor process can see none of these, which is the
whole reason those hooks exist.

**`Stop` is the backstop, and it is enforcement rather than delivery.** If the
monitor is holding something actionable when you try to finish — red CI, or a
reviewer's comment or change request — it hands the batch over and asks for one
more turn. Approvals, stale pins and failed polls are informational: they reach
you but never hold a turn open. So **seeing a `ci.failed` notification is
delivery, not acknowledgement** — finish without acting on it and `Stop` will
hand it straight back.

**Nothing arrives while checks are merely running.** A run is reported once it
has finished, so what you get is one event carrying everything that workflow got
wrong, not one event per job. Two exceptions: a job that goes red within the
first minute of its run is sent straight away (with a note that the run is
unfinished), because a lint failure should not queue behind a build; and a run
still going after thirty minutes reports what is known rather than nothing.
Batching is per workflow run, so a slow unrelated workflow never sits on
another's failures.

**Cancelled is not failed, and not passed either.** Cancelled jobs never arrive
as their own event — they are a note on the run's failure, and on the final
verdict, where the headline says what was cancelled instead of "all checks
passed". Treat them as checks that did not run: a cancelled job has told you
nothing about whether that work is sound.

Recognise the wrapper on what you are handed: it is monitor output, not a
message from the user — act on it as part of the task in hand.

```
<shuck-monitor>
The shuck background monitor observed 1 change since your last update.
1 item below needs your attention: address it as part of the current
task, or say why you are not going to.
This is monitor output, not a message from the user.

[14:02:37] ci.failed  justanotherspy/shuck#42
  test (ubuntu-latest) failed on a1b2c3d — feat: add the widget
  https://github.com/justanotherspy/shuck/pull/42/checks
    Step 5: Run make test
    ##[error]--- FAIL: TestWidget (0.00s)
        widget_test.go:41: want 3, got 2
</shuck-monitor>
```

Opting out: `SHUCK_MONITOR_DISABLE=1` turns off the stream and both hooks;
`SHUCK_MONITOR_NO_STOP=1` turns off only the `Stop` hook. The stream reads
`SHUCK_MONITOR_DISABLE` once at start-up, so setting it mid-session stops the
hooks but not a stream already running.

### When nothing is streaming

Plugin monitors run only in interactive CLI sessions, so a `claude -p` / SDK run
gets none — and neither does a `shuck setup` install, which writes this skill and
nothing else. **Then nothing arrives on its own.** You can tell which world you
are in: notifications turn up by themselves, and `shuck monitor` lists this tree
among its sources. If neither is true, close the loop by hand once, at the point
the answer matters: `shuck monitor watch` to register the tree, then
`shuck monitor events --wait 30m` to block until the verdict lands. Never
sleep-and-recheck instead, and never leave a watcher running afterwards.

## The PR loop: push → hear back → fix

Any task that ends in a pull request isn't done at "pushed" — close the loop on
CI. With the monitor running that costs you nothing:

1. **It is already watching** the tree you are in. No setup, no PR number.
2. **Push.** The `PostToolUse` hook pokes it, so the run is picked up in seconds.
3. **The verdict arrives on its own**: `ci.passed` closes the loop, `ci.failed`
   carries the failing-step logs in its body.
4. **Fix from that body** — it already holds the errors, so a follow-up call is
   rarely needed. Push again and repeat.

**While a stream is delivering, never start a watcher of your own.** No
background `shuck --watch`, no sleep-and-recheck loop, no blocking on a wait
flag. The monitor is already following this tree and will tell you; a second
watcher spends rate limit to say the same thing later, and a blocked shell says
it to nobody. (With no stream, step 3 is yours to do — see "When nothing is
streaming" above.)

### Sources: inferred, or added by you

The monitor follows **sources**, and there are exactly two ways one gets on your
list:

- **Inferred** — the working tree this session is in. The stream registers it on
  start, and it retargets itself when you switch branch or a PR opens for the
  branch you are on. This is the normal case and needs nothing from you.
- **Added by you** — `shuck monitor watch <target>`, where a target is a PR
  (`owner/repo#42`, a PR URL, `owner/repo 42`, a bare number) or another
  checkout's directory. Reach for it whenever the work turns on a PR this tree
  cannot imply: one you opened from another worktree, a dependency's PR you are
  blocked on, a branch you are reviewing.

**A source belongs to the session that added it.** The daemon is shared and its
journal is one log, but each stream is filtered to its own working tree's
sources, so a second session in a second worktree gets its own CI and not yours.
That is what makes adding one safe: `shuck monitor watch <pr>` subscribes *you*,
not everybody on the machine.

`shuck monitor unwatch <target>` drops one again, and `shuck monitor` shows the
whole list, unfiltered. **Curate as the work moves** — every watch costs polls,
and a PR you have stopped caring about is a notification you did not want.

## The commands

| What you want | Command |
| --- | --- |
| What the monitor is watching, and where it stands | `shuck monitor` (alias `m`) |
| Add a source: a PR, or another checkout | `shuck monitor watch <target>` |
| Drop one again | `shuck monitor unwatch <target>` |
| Collect what it noticed, or wait for it when nothing is streaming | `shuck monitor events [--wait DUR]` |
| Everything on a PR (CI + reviews + security) | `shuck [target]` / `shuck all [target]` |
| Failing CI step logs | `shuck logs [target]` (alias `l`) |
| Logs for a single Actions run | `shuck logs --run <id\|url>` |
| Download a run's artifacts | `shuck logs --run <id\|url> --download-artifacts <dir>` |
| A PR's reviews | `shuck reviews [target]` (alias `r`) |
| Audit a checkout's action pins | `shuck pins [dir]` (alias `p`) |
| A repo's security alerts | `shuck security [repo]` (alias `s`) |
| Resolve one Action to a SHA pin | `shuck action <ref>` (alias `a`) |

Bare `shuck` is `shuck all`: CI + reviews + security in one report. Also there:
`shuck version [--check]`, `shuck upgrade`, `shuck setup`.

### Picking a target

`shuck owner/repo 42`, a PR URL, `shuck 42` (current repo), or bare `shuck` for
the current branch's open PR. `logs` additionally takes an Actions run or job
URL, `.../attempts/2` for one re-run attempt, or `--run <id> owner/repo`.

Rules that bite:

- owner/repo is inferred from the local `origin` remote for a bare number or no
  argument; name it explicitly for a repo you are not checked out in.
- Run/job targets **always re-download logs** (no cache) and carry no reviews or
  security half. A run URL with no `/attempts/<n>` uses the latest attempt.
- Run targets also **list the artifacts the run uploaded**.
  `--download-artifacts <dir>` extracts each to `<dir>/<name>/`; expired ones are
  listed but cannot be fetched. It errors on a PR target — artifacts belong to a run.
- A PR "Checks" tab link resolves to the one Actions job behind that check;
  a non-Actions check falls back to the PR-wide report.

## Monitor subcommands

| Command | What it does | Flags |
| --- | --- | --- |
| `shuck monitor` | status: what is watched, and where it stands | `--json`, `--no-start` |
| `shuck monitor watch [target]` | add a source (default: this working tree) | `--json` |
| `shuck monitor unwatch [target]` | drop one again | |
| `shuck monitor events` | hand over what is new | `--json`, `--all`, `--consumer ID`, `--limit N` |
| `shuck monitor stream` | the plugin's monitor: follow this tree, one block per event, until killed | `--json` |
| `shuck monitor poke [target]` | re-check now, without waiting for the interval | |
| `shuck monitor stop` | shut the monitor down | |
| `shuck monitor run` | run it in the foreground | `--detached`, `--stay`, `--no-pins`, `--watch-ttl DUR`, `--token T` |
| `shuck monitor log` | print the daemon's own log — where a misbehaving monitor explains itself | |

- `events` also takes `--follow` and `--wait DUR`, and the report commands take
  `--watch`. **While a stream is delivering they are not for you**: blocking a
  turn open buys you what the stream was about to hand you anyway. They are for
  shell scripts, CI jobs, and the session nothing is streaming into — where
  `--wait DUR` is the right way to wait (it blocks until an event lands rather
  than returning "nothing new", and cannot be combined with `--follow`).
- `--consumer ID` names whose cursor advances; events are delivered **once** per
  consumer, so a session and the CLI never steal each other's news.
- `shuck monitor` (status) **auto-starts** the daemon; the read commands do not,
  so they can never report a false all-clear from a monitor that has seen
  nothing. `--no-start` suppresses it.
- `shuck monitor stream` and `shuck monitor hook <event>` are the plugin's entry
  points — not meant to be run by hand.
- Watches expire after 12h with no client asking how things stand (any
  `status` / `events` / `poke` refreshes *every* watch, so one live session
  keeps them all alive), and an on-demand daemon exits once its last watch is
  gone — it never keeps polling GitHub after your sessions end.

## The one-shot reports

**`shuck logs`** returns each failed step's command and error excerpt. Tune
extraction only when the default excerpt isn't enough: `--full` (untrimmed),
`--context N` (default 10), `--pattern RE` (when the default regexp misses the
real error), `--short-threshold N` (default 100 — logs this short are shown
whole), `--tail N` (default 100 — when a long log has no match).

**`shuck pins [dir]`** audits a checkout for `uses:` references that are **not
SHA-pinned** or whose pin has **gone stale** — `@v4` runs whatever commit that
tag points at today, and a SHA left alone falls behind. **Every finding carries
the corrected line.** Reach for it right after editing a workflow and before
opening a PR that touches one; the monitor audits the watched tree too and
raises `pins.stale` — but only for a branch that has itself changed a workflow
file, so pins you did not touch stay out of the way. Run `shuck pins` by hand
when you want the whole checkout audited regardless. It scans `.github/workflows/*.y{a,}ml`, the repo's own
`action.y{a,}ml`, and `.github/actions/*/action.y{a,}ml`. The suggested pin
**stays on the major you chose** — a newer major goes in the note, never the
line. A pin with no `# <tag>` comment cannot be checked for staleness; the
finding asks you to add one. `./…` and `docker://` refs are skipped.

**`shuck security [repo]`** summarizes code scanning (rule, severity,
`file:line`), secret scanning (type and location — the **raw secret value is
never fetched or shown**), and Dependabot (package, fix version, CVE/GHSA; npm
malware advisories surface here). Each source degrades independently: one that
is not enabled or not visible to the token is reported and **skipped, never
failed**, so an unreadable source is never a false all-clear. Only `open` alerts
show by default; widen with `--state open|all|dismissed|fixed|resolved`. Needs a
token with `security_events` (or `repo`) scope.

**`shuck action <owner>/<action>[@<ver>]`** resolves one Action to its latest
matching tag + commit SHA and prints the pin line. Use `shuck pins` for a whole
checkout.

**`shuck reviews`** groups a PR's reviews by verdict and collapses resolved or
outdated threads to one line.

Shared flags: `--json`, `--exit-code`, `--refresh` (rebuild the cache — use when
a job was re-run), `--no-cache`, `--offline` (render from cache only; needs an
explicit PR, skips security), `--token T`.

```sh
shuck                                    # this branch's PR: CI + reviews + security
shuck monitor                            # what the monitor is watching
shuck monitor watch owner/repo#42        # add a PR this tree cannot imply
shuck logs justanotherspy/shuck 42       # just the failing CI logs
shuck logs --run 123 owner/repo --download-artifacts ./artifacts
shuck pins --exit-code                   # gate CI on unpinned or stale pins
shuck action actions/checkout@v4         # resolve to a SHA pin
```

### Exit codes

Producing a report is success — read the output for the verdict. `0` report
produced (it may well show failures), `2` operational error (bad or missing
auth, target not found, network). To branch on the verdict without parsing, pass
`--exit-code`: failing checks then exit `1`. Security findings do **not** flip
the exit code on the default path even with `--exit-code` — use
`shuck security --exit-code` to gate on open alerts.

### `--json`

Every report takes `--json` and returns a stable, versioned document.
**Prefer it when you need to act on results programmatically.**

- `logs` / `reviews` return the **inspection document**: `pr` (or `run`),
  `summary`, `failed_jobs[]` → `failed_steps[]` → `{number, name, kind, command,
  excerpt}`, plus `cancelled_jobs[]`, `other_checks[]`, `running_jobs[]`,
  `reviews[]`, and `artifacts[]` for run targets.
- Bare `shuck` / `all` wraps that in `{schema_version, inspection, security?,
  security_error?}`.
- `security`, `pins` and `action` each return their own document; every finding
  in the pin document carries `pin`, the corrected reference to paste after `uses:`.
- The monitor's views are a narrower shape of their own: `monitor` and
  `monitor watch` emit one document with `schema_version` beside the embedded
  fields, while `monitor events --json` and `monitor stream --json` are
  **line-delimited** — one object per event, each line carrying its own version,
  so a consumer tailing the feed sees it wherever it starts reading.

If `summary.running > 0` the snapshot is **incomplete**. Don't conclude from it,
and don't re-run it in a loop: the monitor is watching the same PR — let its
event tell you when the run lands (or wait for one, if nothing is streaming).

## Prerequisites and notes

- **The `shuck` binary on PATH** — the plugin runs it but does not install it:
  `curl -fsSL https://raw.githubusercontent.com/justanotherspy/shuck/main/install.sh | bash`
  (or `go install github.com/justanotherspy/shuck@latest`). Keep it current with
  `shuck upgrade`.
- **A GitHub token** in `GITHUB_TOKEN` or `GH_TOKEN`, or a logged-in `gh` —
  shuck falls back to `gh auth token`, which is how a session that started
  before the token was exported still works. `--token` overrides both. `pins`
  and `action` work unauthenticated against public repos.
- Both are reported in the monitor's own voice, since it is the plugin's only
  channel: each becomes **one plain line** and then nothing further. **Nothing
  the plugin does can cost you anything** — the stream and both hooks exit `0`
  whatever goes wrong.
- Results are cached under `~/.cache/shuck`, keyed by job + run attempt;
  `--refresh` when a job has been re-run. The monitor's own state lives under
  `~/.cache/shuck/monitor`.
- Non-Actions checks (external statuses) are listed by name only — no logs exist
  for them via the API. The monitor still reports them red.
