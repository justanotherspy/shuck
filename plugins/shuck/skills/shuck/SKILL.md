---
name: shuck
description: >-
  Keep a background monitor on a pull request and hear about CI failures,
  review comments, and stale action pins as they happen — plus show the exact
  failing CI step logs, summarize a PR's reviews, list a repo's security
  alerts, check settings against a committed compliance policy, audit the
  Dependabot config, and pin GitHub Actions to SHAs. Works two ways: the
  `shuck` CLI (`--json` for structured output) or the shuck MCP tools
  (`monitor_status`, `monitor_events`, `monitor_watch`, `monitor_unwatch`,
  `check_pins`, `inspect_logs`, `inspect_reviews`, `inspect_security`,
  `check_compliance`, `audit_dependabot`, `inspect_action`, `inspect_images`).
  Use whenever a GitHub Actions workflow is in play: to watch a PR in the
  background and be told when CI goes red or a reviewer comments, to wait for
  checks to finish after a push, to learn why CI is failing, debug a failed
  check, pull a PR's error logs, download a run's archived artifacts, see what
  reviewers asked for, check that a workflow's actions are SHA-pinned and
  current, triage security findings, verify settings match policy, check
  Dependabot coverage, SHA-pin an action, or digest-pin a GHCR image.
---

# shuck — failing CI logs, reviews, and security for a PR

`shuck` drills GitHub Actions failures down to the failing steps and returns only
their error logs, summarizes a PR's reviews, lists a repo's security alerts, and
resolves an Action to a SHA pin. Reach for it instead of paging through the
GitHub UI or `gh`.

It also runs a **background monitor** that follows the working tree you are in
and hands you CI failures, review comments, and stale action pins as they
happen — so the usual answer to "is CI done yet?" is to wait for the event, not
to poll.

## The background monitor

`shuck monitor` is a local daemon. No webhook, no server, no extra credentials —
the same GitHub token the rest of shuck uses.

Point it at a working tree and it follows that tree: it reads the branch out of
`.git/HEAD` (worktrees included), finds the open PR for that branch, and
re-checks on a cadence that tightens while CI is running (12s) and relaxes when
the PR is idle (90s) or dormant (5m — a merged or closed PR, or a branch with no
PR). Switch branches, switch worktrees, or open a PR for the branch you are on,
and it **retargets itself** — you never tell it a PR number. Two watches that
land on the same PR are polled once between them.

It emits **events**, each with a one-line title and an agent-ready body:

| Kind | What happened | Body |
| --- | --- | --- |
| `ci.failed` | a job went red | the distilled failing-step logs — the same excerpt `shuck logs` prints |
| `ci.passed` | every check on the head commit finished green (once per commit) | — |
| `ci.started` | checks registered for a new head commit | — |
| `review.comment` | a new inline review comment | the diff hunk, ±10 lines of the file at the PR head, and the earlier thread comments when it is a reply |
| `review.submitted` | a submitted review: approved, changes requested, or commented | the review and its inline comments |
| `pr.state` | the PR was opened, merged, closed, or marked ready for review | — |
| `pins.stale` | a workflow references an action that is not SHA-pinned, or whose pin is behind the latest release | the corrected `uses:` line |
| `watch.target` | the watch retargeted: branch switch, PR found, PR lost | — |
| `monitor.error` | a poll failed (reported once, then backed off) | the error |

### In Claude Code you do not have to do anything

The shuck plugin wires the monitor into the session with hooks, so events arrive
on their own. **There is nothing to poll for.**

| Hook | What it does |
| --- | --- |
| `SessionStart` | registers this session's working tree (starting the daemon if none is running) and fast-forwards the session's cursor, so you hear about what happens *next*, not the last hour of history |
| `UserPromptSubmit` | delivers new events into the conversation as a `<shuck-monitor>` block |
| `PostToolUse` (Bash) | after a `git push` / `gh pr create` / `gh pr ready` / `gh workflow run` / `gh run rerun`, pokes the monitor to re-check immediately instead of waiting out the interval |
| `Stop` | if the monitor is holding something actionable (red CI, a review comment) when you try to finish, it hands it over and asks for one more turn. It stands down the moment `stop_hook_active` is set, so it can never loop |
| `SessionEnd` | retires the session's cursor |

A delivered batch looks like this. Recognise the wrapper: it is monitor output,
not a message from the user — act on it as part of the task in hand.

```
<shuck-monitor>
The shuck background monitor observed 1 change since your last update.
1 item below need your attention: address them as part of the current
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

Opting out: `SHUCK_MONITOR_DISABLE=1` turns off every hook;
`SHUCK_MONITOR_NO_STOP=1` turns off only the `Stop` hook.

## The PR loop: push → hear back → fix

Any task that ends in a pull request isn't done at "pushed" — close the loop on
CI. With the monitor running (in Claude Code the plugin already started it),
that costs you nothing:

1. **The monitor is already watching** the working tree you are in. There is
   nothing to set up and no PR number to supply.
2. **Push.** The `PostToolUse` hook pokes the monitor, so the new run is picked
   up in seconds instead of at the next interval.
3. **The verdict arrives on its own** as a `<shuck-monitor>` block: `ci.passed`
   closes the loop, `ci.failed` carries the failing-step logs in its body.
4. **Fix from that body** — it already holds the errors, so a follow-up call is
   rarely needed. Push again and repeat until `ci.passed` arrives.

**Do not start a `shuck --watch` in the background while the monitor is
running.** It is already watching; a second watcher only spends rate limit to
tell you the same thing later.

When there is no monitor — no plugin, `SHUCK_MONITOR_DISABLE=1`, or a PR you are
not checked out on — close the loop yourself, either way:

- **MCP**: `monitor_watch` the PR (`repo` + `pr`, or `url`), then call
  `monitor_events` with `wait_seconds` — it blocks until something happens and
  hands back the `ci.passed` / `ci.failed`. That is the no-polling way to wait.
- **CLI**: `shuck --watch --exit-code --watch-timeout 30m <pr>`, run in the
  background (Bash `run_in_background`). Exit `0` clean, `1` failing checks with
  the logs already in the report, `2` operational error. Confirm checks have
  registered for the new head commit first — a watch started before any run
  exists reports all-clear immediately. Full flags in "Watching CI to completion
  (CLI)" below.

## Two ways in — use either or both

shuck exposes the same capabilities through two front-ends that share one engine,
so they return the same data; pick whichever is wired up.

| Front-end | How you call it | Best when |
| --- | --- | --- |
| **CLI** (`shuck …`, Bash) | run the binary; add `--json` for structured data | the binary is on PATH; you want to **watch** CI to completion, script exit codes, or pipe `--json` |
| **MCP tools** | call `monitor_status` / `monitor_events` / `monitor_watch` / `monitor_unwatch` / `check_pins` / `inspect_logs` / `inspect_reviews` / `inspect_security` / `check_compliance` / `audit_dependabot` / `inspect_action` / `inspect_images` | the shuck MCP server is registered; you want typed structured output with no parsing |

Both front-ends talk to the same monitor daemon, so it does not matter which one
started it. For one-shot inspection the two are interchangeable; only the CLI
does `--watch`.

## The commands at a glance

| What you want | CLI | MCP tool |
| --- | --- | --- |
| What the monitor is watching, and where it stands | `shuck monitor` (alias `m`) | `monitor_status` |
| Follow a working tree or PR in the background | `shuck monitor watch [target]` | `monitor_watch` |
| Collect what the monitor noticed (or wait for it) | `shuck monitor events` | `monitor_events` |
| Stop following something | `shuck monitor unwatch [target]` | `monitor_unwatch` |
| Audit a checkout's action pins | `shuck pins [dir]` (alias `p`) | `check_pins` |
| Everything on a PR (CI + reviews + security) | `shuck [target]` / `shuck all [target]` | (call the three below) |
| Failing CI step logs | `shuck logs [target]` (alias `l`) | `inspect_logs` |
| Logs for a single Actions run | `shuck logs --run <id\|url>` | `inspect_logs` with `run` |
| Download a run's artifacts | `shuck logs --run <id\|url> --download-artifacts <dir>` | `inspect_logs` with `run` + `download_artifacts` |
| A PR's reviews | `shuck reviews [target]` (alias `r`) | `inspect_reviews` |
| A repo's security alerts | `shuck security [repo]` (alias `s`) | `inspect_security` |
| Check settings against policy | `shuck compliance [repo]` (alias `c`) | `check_compliance` |
| Bootstrap/sync the policy file | `shuck compliance discover [repo]` | (CLI only) |
| Audit the Dependabot config | `shuck dependabot [repo]` (alias `d`) | `audit_dependabot` |
| Scaffold/extend the Dependabot config | `shuck dependabot discover [repo]` | (CLI only) |
| Fix best-practice gaps in existing entries | `shuck dependabot fix [repo]` | (CLI only) |
| Resolve an Action to a SHA pin | `shuck action <ref>` (alias `a`) | `inspect_action` |
| List GHCR images / pin one to a digest | `shuck image [ref]` (alias `i`) | `inspect_images` |

Running `shuck` with **no subcommand** is the same as `shuck all`: CI + reviews +
security in one report. Use `logs` / `reviews` to focus on one dimension.

## Picking a target

The PR-oriented entry points accept the same target forms:

| You have | CLI | MCP (`inspect_logs` / `inspect_reviews`) |
| --- | --- | --- |
| owner/repo + PR number | `shuck owner/repo 42` | `repo` + `pr` |
| a PR URL | `shuck <pr-url>` | `url` |
| a PR number, current repo | `shuck 42` | `pr` alone |
| the current branch's open PR | `shuck` | (no fields) |
| an Actions run/job URL (logs only) | `shuck logs <run-url>` | `inspect_logs` `run` = the URL |
| a specific re-run attempt (logs only) | `shuck logs <run-url>/attempts/2` | `inspect_logs` `run` = that URL |
| a PR "Checks" tab link | `shuck <checks-url>` | `inspect_logs` `url` = the link |
| a run ID + repo (logs only) | `shuck logs --run 123 owner/repo` | `inspect_logs` `run` = `"123"`, `repo` |

Rules that bite:

- For the MCP PR tools, setting `repo` **without** `pr` is an error; owner/repo is
  inferred from the local origin remote only when you pass `pr` alone or nothing.
- Run/job targets (URLs ending `/actions/runs/123`, `.../job/456`, or
  `.../attempts/2`, or `logs --run`) skip the PR-wide scan and **always
  re-download logs** (no cache); they carry no reviews or security half. A run
  URL with no `/attempts/<n>` uses the latest attempt.
- Run targets also **list the artifacts the run uploaded** (name, size,
  expiry). Add `--download-artifacts <dir>` (MCP: `download_artifacts`) to
  download them: each artifact's zip archive is extracted to `<dir>/<name>/`
  and the report shows the path per artifact. Expired artifacts are listed but
  cannot be downloaded. The flag requires a run target — artifacts belong to
  one workflow run, so it errors on a PR target.
- A PR "Checks" tab link (`.../pull/<n>/checks?check_run_id=<id>`) is resolved to
  just the Actions job behind that check — so it behaves like a job target. If
  the check isn't a GitHub Actions check, it falls back to the PR-wide report.

The monitor takes a slightly different **watch target**: a directory to follow,
or a pull request — `owner/repo#42`, a PR URL, `owner/repo 42`, or a bare number
for the local repository. With no argument it follows the current working tree,
which is the normal case.

## Using the CLI

```sh
shuck [flags] [target]          # CI + reviews + security, once
shuck --json [flags] [target]   # same, but emit the stable JSON document
shuck --watch [flags] [target]  # poll until every check finishes, then report
```

### Subcommands

| Command (alias) | What it does |
| --- | --- |
| `shuck [target]` / `shuck all [target]` | CI + reviews + repo security in one report (the default) |
| `shuck monitor [sub]` (`m`) | the background monitor: status, watch, unwatch, events, poke, stop, run, log |
| `shuck pins [dir]` (`p`) | audit a checkout's workflows for unpinned or stale `uses:` references |
| `shuck logs [target] [--run <id\|url>]` (`l`) | failing CI step logs for a PR or a single run |
| `shuck reviews [target]` (`r`) | a PR's reviews and review-comment threads |
| `shuck security [owner/repo \| url]` (`s`) | a repo's security alerts (code scanning, secrets, Dependabot) |
| `shuck compliance [owner/repo \| url]` (`c`) | check a repo's settings against its `.github/compliance.yml` |
| `shuck compliance discover [owner/repo \| url]` | snapshot the live settings into the local `.github/compliance.yml` (create it, or sync drifted declared keys) |
| `shuck dependabot [owner/repo \| url]` (`d`) | audit `.github/dependabot.yml` against the ecosystems the repo uses |
| `shuck dependabot discover [owner/repo \| url]` | scaffold or extend `.github/dependabot.yml` from the detected ecosystems |
| `shuck dependabot fix [owner/repo \| url]` | add missing best-practice fields (groups, labels, cooldown, PR limit, commit-message prefix) to existing entries |
| `shuck action <owner>/<action>[@<ver>]` (`a`) | resolve an Action to its latest tag + commit SHA for pinning |
| `shuck image [owner \| ghcr.io/owner/name[:tag]]` (`i`) | list an owner's GHCR images, or resolve one to its latest digest for pinning |
| `shuck version [--check]` | print the installed version; `--check` looks for a newer release |
| `shuck upgrade` | download + install the latest release in place (and refresh the installed skill) |
| `shuck setup` | install this skill + a CLAUDE.md note (and, optionally, the MCP) |
| `shuck mcp` | run as a local MCP (stdio) server — used by the MCP front-end |

### Monitor subcommands

| Command | What it does | Flags |
| --- | --- | --- |
| `shuck monitor` | status: what is watched, and where it stands | `--json`, `--no-start` |
| `shuck monitor watch [target]` | follow something (default: this working tree) | `--json` |
| `shuck monitor unwatch [target]` | stop following it | |
| `shuck monitor events` | hand over what is new | `--json`, `--all`, `--follow`, `--consumer ID`, `--limit N`, `--wait DUR` |
| `shuck monitor poke [target]` | re-check now, without waiting for the interval | |
| `shuck monitor stop` | shut the monitor down | |
| `shuck monitor run` | run it in the foreground | `--detached`, `--stay`, `--no-pins`, `--watch-ttl DUR`, `--token T` |
| `shuck monitor log` | print the monitor's own log | |

- `--consumer ID` names whose cursor advances: events are delivered **once** per
  consumer, so a session and the CLI do not steal each other's news. The CLI
  defaults to `cli`; each Claude Code session uses its own session ID.
- `--wait DUR` blocks until an event lands (or the wait elapses) instead of
  returning "nothing new" — the CLI equivalent of `wait_seconds`.
- `shuck monitor` (status) **auto-starts** the daemon; the other read commands do
  not, so they never report a false all-clear from a monitor that has seen
  nothing. `--no-start` suppresses the auto-start.
- Watches expire after 12h with nobody asking about them, and a daemon that was
  started on demand exits once its last watch is gone — so it never keeps
  polling GitHub after your sessions end. `shuck monitor run --stay` keeps a
  hand-started one alive with nothing to watch.

### Flags

Extraction (tune only when the default excerpt isn't enough), on the default
path and on `logs`:

- `--full` — print full, untrimmed logs for failed steps.
- `--context N` (default 10) — lines of context kept around each error match.
- `--pattern RE` — override the error-matching regexp when the default misses
  the real error.
- `--short-threshold N` (default 100) — logs at most this many lines are shown whole.
- `--tail N` (default 100) — lines tailed when a long log has no error match.

Focusing and selection:

- `--run <id|url>` (on `logs`) — inspect one workflow run instead of a PR.
- `--download-artifacts DIR` (run targets only; default path and `logs`) —
  download the run's uploaded artifacts, each extracted to `DIR/<name>/`.
- `--state open|all|dismissed|fixed|resolved` (default `open`) — which security
  alerts to include in the default/`all` view (and on `shuck security`).

Output, cache, auth (default path and the focus subcommands):

- `--json` — emit the stable JSON document instead of text.
- `--exit-code` — exit `1` when failing checks are found (for CI gating; the
  default is exit `0` whenever the report is produced).
- `--refresh` — ignore and rebuild the cache (use when a job was re-run).
- `--no-cache` — do not read or write the cache.
- `--offline` — render only from cache, no network (requires an explicit PR;
  not valid with `--watch`; skips the security half).
- `--token T` — GitHub token, overriding `GITHUB_TOKEN` / `GH_TOKEN`.

Watch-only (default/`all` path):

- `--watch` — poll until every check reaches a terminal state, then print.
- `--interval D` (default 15s) — poll interval.
- `--watch-timeout D` (default 0 = no limit) — give up after this long and print
  the latest snapshot instead of waiting forever.

### Exit codes

Producing a report is success — read the output (or `--json`) for the verdict:

- `0` — the report was produced (it may well show failing checks).
- `2` — operational error (bad/missing auth, target not found, network).

To branch on the verdict without parsing output, pass `--exit-code`: failing
checks then exit `1`. Security findings do **not** flip the exit code on the
default/`all` path even with `--exit-code`; use `shuck security --exit-code`
to gate on open alerts.

### Examples

```sh
shuck                                             # current branch's open PR: CI + reviews + security
shuck monitor                                     # what the background monitor is watching
shuck monitor watch                               # follow this working tree
shuck monitor events --wait 15m                   # block until the monitor has something
shuck pins --exit-code                            # gate CI on unpinned or stale action pins
shuck logs justanotherspy/shuck 42                # just the failing CI logs
shuck reviews 42                                  # just the reviews
shuck logs --run https://github.com/owner/repo/actions/runs/123  # one run
shuck logs --run 123 owner/repo                   # one run, by ID
shuck logs --run 123 owner/repo --download-artifacts ./artifacts  # …and pull its artifacts
shuck --json https://github.com/owner/repo/pull/42  # combined structured output
shuck security owner/repo                         # a repo's open security alerts
shuck action actions/checkout@v4                  # resolve to a SHA pin
```

## The JSON document

`--json` returns a stable, versioned shape. **Prefer it when you need to act on
results programmatically.**

- `shuck logs --json` and `shuck reviews --json` (and the `inspect_logs` /
  `inspect_reviews` MCP tools' structured output) return the **inspection
  document**: `schema_version`, `pr` `{owner, repo, number, title, head_sha,
  head_branch}` (or `run` instead, for run/job targets), `summary`
  `{failed, cancelled, running, other_failed}`, `failed_jobs[]`
  `{id, run_id, name, conclusion, workflow_name, workflow_path, failed_steps[]}`
  where each step is `{number, name, kind, command, excerpt}`, plus
  `cancelled_jobs[]` (same shape as `failed_jobs[]`; its `failed_steps[]` hold
  the step that was interrupted by the cancellation and its last output),
  `other_checks[]`, `running_jobs[]`, and `reviews[]`. Run targets with
  uploaded artifacts also carry `artifacts[]` `{id, run_id, name, size_bytes,
  expired, created_at, expires_at, path?}` — `path` is the local directory an
  artifact was extracted to, present only when a download was requested.
- `shuck` / `shuck all --json` (a PR target) wrap that in a **combined envelope**:
  `{schema_version, inspection: <the document above>, security?: <the security
  document>, security_error?}`. The `security` half is omitted (and
  `security_error` set) if the security fetch failed; it is absent entirely for
  run/offline targets, which emit the plain inspection document instead.
- `shuck security --json` returns the **security document** (see below);
  `shuck action --json` returns the resolved-pin document
  (`{schema_version, action, owner, repo, tag, sha, ref, pin}`);
  `shuck pins --json` returns the **pin document** (see "Keeping workflow
  actions pinned").
- `shuck monitor --json` returns the monitor's status, and
  `shuck monitor events --json` emits one JSON event per line.

If `summary.running > 0` the snapshot is **incomplete** — some checks are still
running. To wait for the final verdict, let the monitor tell you
(`monitor_events` with `wait_seconds`) or watch the PR (below).

## Using the MCP tools

The MCP server (`shuck mcp`) exposes twelve tools. Each returns the rendered
report as text **and** the matching JSON document as structured output.

| Tool | Use it for | Inputs |
| --- | --- | --- |
| `monitor_status` | what the monitor is watching and where those PRs stand: per-PR verdict, head commit, lifecycle, next check, plus GitHub quota headroom and how many events wait for you | optional `consumer` (your session identifier) |
| `monitor_events` | collect what the monitor noticed since you last looked | optional `consumer`, `limit`, `wait_seconds`, `peek`, `all` |
| `monitor_watch` | follow a working tree or a specific PR | `path` (default: the server's working directory), **or** `repo` + `pr`, **or** `url` |
| `monitor_unwatch` | stop following something | `id` (from `monitor_status`), **or** `path`, **or** `repo` + `pr` / `url` |
| `check_pins` | audit a checkout's workflow action pins | optional `path` (default: the working directory), `refresh`, `offline`, `token` |
| `inspect_logs` | a PR's failing CI, or one run | PR target fields per the table above; **or** `run` (a run/job URL, or a bare run ID with `repo`) |
| `inspect_reviews` | a PR's reviews and comment threads | PR target fields; optional `review_comment_limit` |
| `inspect_security` | a repo's security alerts | `repo` (`owner/repo`) **or** `url`, or none → the local repo; optional `state`, `refresh` |
| `check_compliance` | a repo's settings vs its compliance config | `repo` (`owner/repo`) **or** `url`, or none → the local repo; optional `config`, `ref` |
| `audit_dependabot` | a repo's Dependabot config vs the ecosystems it uses | `repo` (`owner/repo`) **or** `url`, or none → the local repo; optional `config`, `ref`, `error_on_missing_ecosystem` |
| `inspect_action` | resolve an Action to a SHA pin | `action` (`owner/action[/subpath][@version]`); optional `refresh` |
| `inspect_images` | list GHCR images, or resolve one to a digest | `image` (an owner, `owner/repo`, a URL, or `ghcr.io/owner/name[:tag]`), or none → the local repo; optional `refresh` |

- **`monitor_events`'s `wait_seconds` is how you wait for CI without polling**:
  watch the PR, push, then call it with `wait_seconds` and act on the
  `ci.passed` / `ci.failed` that comes back. `peek` returns the pending events
  without consuming them; `all` re-reads the whole retained journal. Pass
  `consumer` so events reach you exactly once.
- `monitor_status` starts a daemon if none is running; `monitor_events` never
  does, because a freshly started monitor has seen nothing and its silence would
  read as all-clear.
- `inspect_logs` also accepts the extraction knobs (`full`, `context`, `pattern`,
  `short_threshold`, `tail`), the cache knobs (`refresh`, `no_cache`, `offline`),
  and `download_artifacts` (a directory; run targets only) to download the run's
  uploaded artifacts.
- The inspection tools are one-shot snapshots. There is no combined `all` MCP
  tool: call `inspect_logs` + `inspect_reviews` + `inspect_security` for the
  full picture. `audit_dependabot` and the compliance discover step are
  read-only one-shots like the rest; only the CLI writes files
  (`shuck dependabot discover`, `shuck compliance discover`).

## Keeping workflow actions pinned

`shuck pins [dir]` (alias `p`) and the `check_pins` tool audit a checkout for
`uses:` references that are **not SHA-pinned**, or whose pin has **gone stale**.
`uses: actions/checkout@v4` runs whatever commit that tag points at today — the
tag can be moved — and a SHA pin left alone falls behind the releases it was
taken from, so both halves are reported. **Every finding comes with the exact
corrected line.**

Reach for it **right after writing or editing a workflow file, and before
opening a PR that touches one.** (The monitor audits the watched tree too, and
raises a `pins.stale` event when it finds something.)

```sh
shuck pins                       # audit the current checkout
shuck pins ../other-repo         # audit another checkout
shuck pins --exit-code           # exit 1 when something needs attention (CI gating)
shuck pins --offline             # list the references without resolving; no network
shuck pins --json                # the stable JSON document
```

- It scans `.github/workflows/*.yml` and `*.yaml`, the repository's own
  `action.yml` / `action.yaml`, and the action manifests one level deep under
  `.github/actions/` — the only places GitHub itself reads action definitions
  from.
- The suggested pin **stays on the major you chose**: `@v4` resolves within
  `4.x.x`, because silently proposing a major bump would change behaviour. A
  newer major is mentioned in the finding's note, never in the suggested line.
- A pin with no `# <tag>` comment cannot be checked for staleness — the finding
  asks you to add one. Local (`./…`) and `docker://` references are skipped with
  a note; docker images pin with `shuck image` instead.
- Flags: `--json`, `--exit-code`, `--refresh`, `--offline`, `--token`. Auth is
  optional for public actions; a token lifts the unauthenticated rate limit. Tag
  lists are cached for an hour.

The pin JSON document (also `check_pins`'s structured output):
`schema_version` (int), `root`, `checked_at`, `summary`
`{total, pinned, stale, unpinned, skipped}`, and `findings[]` — each
`{file, line, ref, slug?, version?, comment?, kind, status, latest?, sha?,
pin?, note?}`, where `pin` is the corrected reference to paste after `uses:`.

## Security alerts

`shuck security` (CLI) and `inspect_security` (MCP) summarize a repository's
GitHub security alerts in one shot, so you can triage what to fix without paging
through the Security tab. Three sources:

- **Code scanning** (e.g. CodeQL) — rule, severity, `file:line`.
- **Secret scanning** — secret type and file locations. The **raw secret value
  is never fetched or shown** — only its type, location, and state.
- **Dependabot** — vulnerable package, ecosystem, fix version, CVE/GHSA IDs. npm
  **malware** advisories surface here (no separate malware endpoint).

```sh
shuck security                       # the local working directory's repo
shuck security owner/repo            # an explicit repo (or a github.com URL)
shuck security --state all owner/repo  # include dismissed/fixed/resolved
shuck security --json owner/repo     # the stable JSON document
shuck security --exit-code owner/repo  # exit 1 when open alerts are found
```

Each source degrades independently: one that is **not enabled** or **not visible
to the token** is reported and skipped, not failed — so a repo with only some
features on still produces output. By default only **open** alerts show; widen
with `--state open|all|dismissed|fixed|resolved`.

The security JSON document (also `inspect_security`'s structured output):

- `schema_version` (int), `repo` `{owner, repo}`, `state`.
- `summary` `{total, by_severity{critical…unknown}, by_source{code_scanning, secret_scanning, dependabot}}`.
- `sources` — each of `code_scanning` / `secret_scanning` / `dependabot` with a
  `{status, message?}` where status is `ok` | `disabled` | `forbidden` | `error`.
- `code_scanning_alerts[]`, `secret_scanning_alerts[]`, `dependabot_alerts[]` —
  per-alert detail (severity, location, package → `first_patched_version`, IDs,
  `html_url`). No raw secret value is ever present.

Exit code (CLI): `0` on any successful run, `2` only on an operational error;
`--exit-code` makes open findings exit `1` for CI gating. Results are cached
under `~/.cache/shuck/security/<owner>/<repo>` for an hour; `--refresh` re-fetches.
Security data — especially private repos — needs a token with the
`security_events` (or `repo`) scope.

## Settings compliance

`shuck compliance [owner/repo | url]` (alias `c`) and the `check_compliance` tool
check a repository's **live GitHub settings** against a `.github/compliance.yml`
committed in the repo. That file is the **definitive statement of intended
settings** — merge options, features, security, branch protection — so a CI job
can gate on drift.

```sh
shuck compliance                       # the local checkout's .github/compliance.yml
shuck compliance owner/repo            # fetch the config from the repo and check it
shuck compliance --config policy.yaml owner/repo   # an explicit local config file
shuck compliance --json owner/repo     # the stable JSON document
shuck compliance --exit-zero owner/repo  # report-only (never fail the build)
```

How it behaves, and the rules that bite:

- The config is **partial**: only the keys it declares are checked. A repo can
  assert just what it cares about.
- A **typo'd key is rejected** (the parse fails) rather than silently skipping a
  check — so a misspelled setting can't pass by accident.
- A setting the token **cannot read** (branch protection and `security_and_analysis`
  need admin / `repo` access) is reported as **skipped**, never a false pass. An
  unprotected branch that the config says should be protected **fails**.
- Config discovery: a bare `shuck compliance` reads the **checked-out** file (the
  CI case); an explicit `owner/repo` **fetches** `.github/compliance.yml` from the
  repo (use `--ref` for a branch/tag/SHA); `--config` overrides both with a path.

### Bootstrapping the config: `shuck compliance discover`

`shuck compliance discover [owner/repo | url]` writes the config for you from the
repository's **live settings** (general, security, and the default branch's
protection):

```sh
shuck compliance discover              # snapshot the local repo into .github/compliance.yml
shuck compliance discover owner/repo   # snapshot an explicit repo's settings
shuck compliance discover --dry-run    # preview without writing
shuck compliance discover --json       # machine-readable result
```

- **No config yet** → a complete snapshot of every readable setting is created.
- **Config exists** → its declared keys are kept (partial stays partial); each
  declared value that drifted from the live settings is updated **in place**,
  preserving comments and key order.
- **Up to date** → nothing is written.
- Unreadable settings (need admin) are omitted / left untouched, with a note.
- Exit `0` on success (created, updated, or up to date), `2` on operational error.

Config shape (all sections and keys optional):

```yaml
repository:        # general settings
  visibility: public            # public | private | internal
  allow_merge_commit: false
  allow_squash_merge: true
  delete_branch_on_merge: true
  has_wiki: false
  web_commit_signoff_required: true
security:          # security_and_analysis (needs admin to read)
  secret_scanning: true
  secret_scanning_push_protection: true
  dependabot_security_updates: true
  vulnerability_alerts: true
branch_protection: # keyed by branch name
  main:
    required_approving_review_count: 1
    dismiss_stale_reviews: true
    require_code_owner_reviews: true
    enforce_admins: true
    required_linear_history: true
    allow_force_pushes: false
    allow_deletions: false
    required_conversation_resolution: true
    required_signatures: true
    required_status_checks: [test, lint]   # order-insensitive set
```

The compliance JSON document (also `check_compliance`'s structured output):
`schema_version` (int), `repo` `{owner, repo}`, `config_source`, `compliant`
(bool), `summary` `{total, pass, fail, skipped}`, and `checks[]` — each
`{category, setting, expected, actual?, status, message?}` where status is
`pass` | `fail` | `skipped` | `error`.

Exit code (CLI): `0` when compliant, `1` when a setting drifted (CI gating —
suppress with `--exit-zero`), `2` on an operational error. Reading branch
protection and security settings needs a token with the `repo` scope and admin
access.

## Dependabot config audit

`shuck dependabot [owner/repo | url]` (alias `d`) and the `audit_dependabot` tool
check a repo's `.github/dependabot.yml` against the package ecosystems the repo
**actually uses**. shuck detects ecosystems from manifest files (`go.mod`,
`package.json`, `Dockerfile`, `*.tf`, `*.csproj`, GitHub Actions workflows, …)
and reports:

- **Coverage** — ecosystems used but with **no update entry** (and directories an
  otherwise-covered ecosystem omits). The headline check: "are we even updating
  this?"
- **Best practice** — per update entry, whether it sets `groups` (batch PRs),
  `assignees`/`reviewers`, `labels`, a `cooldown` (minimum release age),
  `open-pull-requests-limit`, and a `commit-message` prefix.
- **Config** — a missing config, or one at `.github/dependabot.yaml` (the spelling
  GitHub **ignores**).

```sh
shuck dependabot                         # the local checkout
shuck dependabot owner/repo              # detect ecosystems from the repo's file tree
shuck dependabot --json owner/repo       # the stable JSON document
shuck dependabot --exit-code --error-on-missing-ecosystem owner/repo  # gate CI on coverage
```

Findings have three levels: **error**, **warning**, **info**. Exit is `0`
whenever a report is produced (even with findings) and `2` on an operational
error; `--exit-code` makes **error**-level findings exit `1`,
`--error-on-missing-ecosystem` promotes an uncovered ecosystem to an error, and
`--strict` makes warnings gate too. Ecosystem detection scans the working
directory for the local repo and the GitHub file tree (`--ref` to pick a branch)
for an explicit one.

### Scaffolding the config: `shuck dependabot discover`

`shuck dependabot discover [owner/repo | url]` writes a best-practice config from
the detected ecosystems:

```sh
shuck dependabot discover               # scaffold .github/dependabot.yml for the local repo
shuck dependabot discover --dry-run     # preview without writing
shuck dependabot discover owner/repo    # detect from an explicit repo's file tree
```

- **No config yet** → a full config is scaffolded (weekly schedule, a
  minor/patch group, a label, a cooldown, an open-PR limit, a commit-message
  prefix) for each ecosystem.
- **Config exists** → an entry is appended for each detected ecosystem it does
  not cover, preserving the existing comments and order.
- Assignees are left out — shuck can't know who should own the PRs — so add them.

### Fixing existing entries: `shuck dependabot fix`

`discover` only closes **coverage** gaps (it adds whole entries); it never edits
the entries already in the config. To clear the **best-practice** findings the
audit reports on existing entries, use `shuck dependabot fix`:

```sh
shuck dependabot fix                    # patch the local .github/dependabot.yml
shuck dependabot fix --dry-run          # preview the patched config without writing
```

For every existing update entry, `fix` fills in the best-practice fields it is
missing — `groups`, `labels`, `cooldown`, `open-pull-requests-limit`, and a
`commit-message` prefix — preserving the file's comments and key order and never
touching fields that are already set. It adds and removes no entries (that is
`discover`'s job) and makes no network calls. Assignees are never added — shuck
can't know who should own the PRs — so entries missing them are noted for you to
fill in.

The dependabot JSON document (also `audit_dependabot`'s structured output):
`schema_version` (int), `repo` `{owner, repo}`, `config_source`, `has_config`
(bool), `ok` (bool), `summary` `{total, error, warning, info}`, `ecosystems[]`
`{ecosystem, directories[], covered}`, and `findings[]` — each
`{level, category, ecosystem?, directory?, message, suggestion?}` where level is
`error` | `warning` | `info` and category is `config` | `coverage` |
`best-practice`.

## Pinning actions to SHAs

`shuck action <owner>/<action>[@<version>]` (alias `a`) and the `inspect_action`
tool resolve an Action to its latest matching tag and commit SHA, and print a pin
line ready to drop after `uses:`:

```sh
shuck action actions/checkout          # latest stable
shuck action actions/checkout@v4       # latest matching v4
shuck action --json github/codeql-action/init@v3
```

Auth is optional for public repos; a token lifts the unauthenticated rate limit.
Tags are cached for a day; `--refresh` re-fetches. To check a whole checkout at
once rather than one reference, use `shuck pins` above.

## Pinning GHCR images to digests

`shuck image [owner | ghcr.io/owner/name[:tag]]` (alias `i`) and the
`inspect_images` tool do the same job for GitHub Container Registry images,
resolving to an immutable digest instead of a SHA:

```sh
shuck image chainguard                       # list every image under an owner
shuck image ghcr.io/justanotherspy/shuck     # resolve the latest stable tag
shuck image ghcr.io/justanotherspy/shuck:v1  # resolve the latest matching v1.x
shuck image --json ghcr.io/owner/name        # the stable JSON document
```

- A bare **owner** (or `owner/repo`, a github.com URL, or nothing → the local
  repo's owner) **lists** every image published under that owner, each with its
  latest tag and digest.
- A full **`ghcr.io/owner/name[:tag]`** reference **resolves** that one image to
  its newest matching tag and manifest digest, and prints a digest-pinned
  reference ready to use (`ghcr.io/owner/name@sha256:… # tag`). For a multi-arch
  image the digest is the image-index digest — the correct value to pin.

Listing an owner's images uses the GitHub Packages API and needs a classic token
with `read:packages`; resolving a single public image works without a token via
the anonymous registry API (private images need a token). Stable tags win over
prereleases, and cosign/referrer tags are never selected. Results are cached for
an hour; `--refresh` re-fetches.

## Watching CI to completion (CLI)

`shuck --watch` is the one-shot way to block on CI: it still exists, it is still
right for a CI job or a shell script, and it is the fallback when no monitor is
running. **When the monitor is running, use it instead** — it is already
watching, and a second watcher only doubles the polling.

```sh
shuck --watch <target>
```

It re-checks every `--interval` and returns **only when no jobs are still
running** — every check has reached a terminal state (success, failure,
cancelled, timed out, …). Then it prints the final report (CI + reviews +
security); add `--exit-code` to exit with the verdict (`0` clean, `1` failures,
`2` error).

How to run it well:

- **CI can take many minutes.** Run the watch command in the background (Bash
  `run_in_background`) or with a generous timeout — don't block the foreground on
  it. You'll be notified when it returns.
- **Bound the wait** with `--watch-timeout <dur>` (e.g. `--watch-timeout 30m`);
  on timeout shuck prints the latest snapshot instead of waiting forever.
- **Want structured final output?** Add `--json`, or once watch reports failures
  (exit `1` with `--exit-code`) call `inspect_logs` for the typed failing-step
  detail.
- Progress lines ("N running, M failed so far …") go to **stderr**; the final
  report (text or `--json`) is the only thing on **stdout**.

```sh
shuck --watch --watch-timeout 30m justanotherspy/shuck 42
shuck --watch --json https://github.com/justanotherspy/shuck/pull/42
```

Caveat: watch keys off "no jobs still running", so if you start it before CI has
registered any runs for the head commit it reports all-clear immediately. Start
watching once at least one check exists (e.g. right after a CI event), or after
an initial inspection shows running jobs. The monitor has no such caveat — it
reports `ci.started` when a new head commit's checks register, and `ci.passed`
only once they all finish green.

## Prerequisites

- The `shuck` binary on your PATH (the MCP server and the monitor hooks also run
  it). Install it once:

  ```sh
  curl -fsSL https://raw.githubusercontent.com/justanotherspy/shuck/main/install.sh | bash
  # or: go install github.com/justanotherspy/shuck@latest
  ```

  Keep it current with `shuck upgrade` (and check with `shuck version --check`).
  `shuck upgrade` also refreshes this skill and the managed CLAUDE.md note in
  your Claude config in place if you installed them with `shuck setup`.
- A GitHub token in `GITHUB_TOKEN` or `GH_TOKEN` (the MCP server and the monitor
  daemon read it from their environment; the CLI also accepts `--token`).
  `shuck action` and `shuck pins` work unauthenticated against public repos, but
  a token lifts the rate limit.

The plugin's prereq hook stays quiet when both are satisfied. It warns (without
blocking) if `shuck` is not on PATH, is too old to run the MCP server
(`shuck upgrade` fixes that), or a token is missing. Every monitor hook exits
quietly when anything is wrong — a background convenience is never the reason a
session stalls.

## Notes

- Results are cached under `~/.cache/shuck`, keyed by job + run attempt, so repeat runs
  are cheap; pass `--refresh` / `refresh` when a job has been re-run. The
  monitor keeps its own state (journal, cursors, watches, log) under
  `~/.cache/shuck/monitor`; `shuck monitor log` prints the daemon's log, which is
  where a misbehaving monitor explains itself.
- Non-Actions checks (external statuses) are listed by name only — no logs exist
  for them via the API. The monitor still reports them red.
- If shuck reports no token, ask the user to set `GITHUB_TOKEN` / `GH_TOKEN` or
  pass `--token`.
