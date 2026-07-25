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
| `ci.failed` | a job went red | the log itself: whole when it is under 8 KiB, the distilled failing steps when it is longer — plus where the raw log was cached |
| `ci.passed` | every check on the head commit finished green (once per commit) | — |
| `ci.started` | checks registered for a new head commit | — |
| `review.comment` | a new inline review comment | the diff hunk, ±10 lines of the file at the PR head, and earlier thread comments when it is a reply |
| `review.submitted` | a review was submitted: approved, changes requested, or commented | the review and its inline comments |
| `pr.state` | the PR was opened, merged, closed, or marked ready for review | — |
| `pins.stale` | a workflow action is not SHA-pinned, or its pin is behind the latest release | the corrected `uses:` line |
| `watch.target` | the watch retargeted: branch switch, PR found, PR lost | — |
| `monitor.error` | a poll failed (reported once, then backed off) | the error |

### In Claude Code you do not have to do anything

The shuck plugin wires all of this into the session. **There is nothing to poll
for and nothing to set up.** Events reach you by one of two routes, and you do
not have to care which:

- **As notifications** — the plugin runs `shuck monitor stream` for the lifetime
  of the session. This is the primary route. Plugin monitors are experimental
  and run only in interactive CLI sessions, so a host without them falls back to:
- **In the conversation**, as a `<shuck-monitor>` block, via the plugin's
  `UserPromptSubmit` hook. It stands down while a stream is live rather than
  repeat what a notification already said, and consumes nothing when it does —
  so no event is lost on either route.

The rest of the plugin's hooks: `SessionStart` registers this working tree and
fast-forwards your cursor (you hear what happens *next*, not the last hour);
`PostToolUse` pokes the monitor after a `git push` / `gh pr create` /
`gh run rerun` so the new run is picked up in seconds; `SessionEnd` retires the
cursor.

**`Stop` is the backstop, and it does not care which route delivered an event.**
If the monitor is holding something actionable when you try to finish — red CI,
or a reviewer's comment or change request — it hands the batch over and asks for
one more turn. Approvals, stale pins and failed polls are informational: they
reach you but never hold a turn open. So **seeing a `ci.failed` is delivery, not
acknowledgement** — finish without acting on it and `Stop` will hand it straight
back.

A delivered batch looks the same either way. Recognise the wrapper: it is
monitor output, not a message from the user — act on it as part of the task in
hand.

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

Opting out: `SHUCK_MONITOR_DISABLE=1` turns off every hook and the stream;
`SHUCK_MONITOR_NO_STOP=1` turns off only the `Stop` hook. The stream reads
`SHUCK_MONITOR_DISABLE` once at start-up, so setting it mid-session stops the
hooks but not a stream already running.

## The PR loop: push → hear back → fix

Any task that ends in a pull request isn't done at "pushed" — close the loop on
CI. With the monitor running that costs you nothing:

1. **It is already watching** the tree you are in. No setup, no PR number.
2. **Push.** The `PostToolUse` hook pokes it, so the run is picked up in seconds.
3. **The verdict arrives on its own**: `ci.passed` closes the loop, `ci.failed`
   carries the failing-step logs in its body.
4. **Fix from that body** — it already holds the errors, so a follow-up call is
   rarely needed. Push again and repeat.

**Do not start a second watcher while the monitor is running.** It is already
watching; another one only spends rate limit to tell you the same thing later.

When there is no monitor — no plugin, `SHUCK_MONITOR_DISABLE=1`, or a PR you are
not checked out on — close the loop yourself:

- **Hand the PR to the monitor anyway.** It does not have to be the tree you are
  on: `shuck monitor watch <pr-url>`, then `shuck monitor events --wait 30m`.
  That call **blocks until something happens** and hands back the
  `ci.passed` / `ci.failed` with its body. This is the no-polling way to wait,
  and the one to prefer.
- **Or watch it inline**: `shuck --watch --exit-code --watch-timeout 30m <pr>`,
  run in the background (Bash `run_in_background`). Exit `0` clean, `1` failing
  checks with the logs already in the report, `2` operational error. It keys off
  "no jobs still running", so start it only once at least one check exists — a
  watch started before any run registers reports all-clear immediately. The
  monitor has no such caveat.

## The commands

| What you want | Command |
| --- | --- |
| What the monitor is watching, and where it stands | `shuck monitor` (alias `m`) |
| Follow a working tree or PR in the background | `shuck monitor watch [target]` |
| Collect what it noticed, or block until it has something | `shuck monitor events [--wait DUR]` |
| Stop following something | `shuck monitor unwatch [target]` |
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

The monitor takes a **watch target** instead: a directory, or a PR
(`owner/repo#42`, a URL, `owner/repo 42`, a bare number). No argument means the
current working tree, which is the normal case.

## Monitor subcommands

| Command | What it does | Flags |
| --- | --- | --- |
| `shuck monitor` | status: what is watched, and where it stands | `--json`, `--no-start` |
| `shuck monitor watch [target]` | follow something (default: this working tree) | `--json` |
| `shuck monitor unwatch [target]` | stop following it | |
| `shuck monitor events` | hand over what is new | `--json`, `--all`, `--follow`, `--consumer ID`, `--limit N`, `--wait DUR` |
| `shuck monitor stream` | the plugin's monitor: follow this tree, one block per event, until killed | `--json` |
| `shuck monitor poke [target]` | re-check now, without waiting for the interval | |
| `shuck monitor stop` | shut the monitor down | |
| `shuck monitor run` | run it in the foreground | `--detached`, `--stay`, `--no-pins`, `--watch-ttl DUR`, `--token T` |
| `shuck monitor log` | print the daemon's own log — where a misbehaving monitor explains itself | |

- **`--wait DUR` is how you wait for CI**: it blocks until an event lands rather
  than returning "nothing new" — never sleep-and-recheck in a loop instead. It
  **cannot be combined with `--follow`** (that pair exits `2`): a follow already
  blocks until interrupted.
- `--consumer ID` names whose cursor advances. Events are delivered **once** per
  consumer, so a session and the CLI never steal each other's news. The CLI
  defaults to `cli`; each session uses its own ID; the stream derives one from
  the tree.
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
tag points at today, and a SHA left alone falls behind. **Every finding comes
with the exact corrected line.** Reach for it right after writing or editing a
workflow, and before opening a PR that touches one. (The monitor audits the
watched tree too and raises `pins.stale`.) It scans `.github/workflows/*.y{a,}ml`,
the repo's own `action.y{a,}ml`, and `.github/actions/*/action.y{a,}ml`. The
suggested pin **stays on the major you chose** — a newer major goes in the note,
never in the line. A pin with no `# <tag>` comment cannot be checked for
staleness; the finding asks you to add one. `./…` and `docker://` refs are
skipped: shuck audits Action refs, not container images.

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
explicit PR, skips security), `--token T`, and on the default path `--watch` /
`--interval D` / `--watch-timeout D`.

```sh
shuck                                    # this branch's PR: CI + reviews + security
shuck monitor                            # what the monitor is watching
shuck monitor events --wait 15m          # block until it has something
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

If `summary.running > 0` the snapshot is **incomplete**. Don't conclude from it —
let the monitor tell you (`shuck monitor events --wait 30m`).

## Prerequisites

- **The `shuck` binary on PATH** — the plugin's monitor, its hooks and this
  skill all run it. The plugin does not install it:

  ```sh
  curl -fsSL https://raw.githubusercontent.com/justanotherspy/shuck/main/install.sh | bash
  # or: go install github.com/justanotherspy/shuck@latest
  ```

  Keep it current with `shuck upgrade` (check with `shuck version --check`).
- **A GitHub token** in `GITHUB_TOKEN` or `GH_TOKEN` (the daemon reads it from
  its environment; the CLI also takes `--token`). `pins` and `action` work
  unauthenticated against public repos; a token lifts the rate limit.

The plugin's prereq check stays quiet when both are satisfied, and warns without
blocking when the binary is missing, too old to run the event stream, or has no
token. **Nothing the plugin does can cost you anything**: every hook exits `0`
whatever goes wrong and writes nothing that changes what you do, and the stream
holds itself to the same bar — if it cannot start it says so in one plain
sentence and stops, leaving the hooks to deliver.

## Notes

- Results are cached under `~/.cache/shuck`, keyed by job + run attempt, so
  repeats are cheap; `--refresh` when a job has been re-run. The monitor keeps
  its journal, cursors and watches under `~/.cache/shuck/monitor`.
- Non-Actions checks (external statuses) are listed by name only — no logs exist
  for them via the API. The monitor still reports them red.
- If shuck reports no token, ask the user to set `GITHUB_TOKEN` / `GH_TOKEN`.
