# RFC: a shuck backend + GitHub App that closes the loop

Research and ideation for turning shuck from a *pull* tool (an agent asks "why
did CI fail?") into a *push* system: a GitHub App + backend that watches a PR's
"closing-the-loop" events — a CI failure, a new review, a new security alert —
does the GitHub-API heavy lifting server-side, **digests** the event with
shuck's own cores, and delivers a ready-to-act payload into a Claude Code
session that has *subscribed* to that PR.

Status: exploratory. Nothing here is built yet. The point is to pick an
architecture and name the hard parts before writing code.

---

## 1. The key realization: most of this already exists

Before designing anything, note that the exact shape the user describes — a
GitHub App receiving PR webhooks, a session that **subscribes** to a PR, events
that **wake** the session to react to CI failures and review comments — is a
**shipping production feature**: *Auto-fix pull requests* in Claude Code on the
web.

- The **Claude GitHub App** receives PR webhooks; installation is what enables
  Auto-fix (it "uses the App to receive PR webhooks").
- A session **subscribes** to a PR's GitHub activity. The agent-facing tools are
  `subscribe_pr_activity` / `unsubscribe_pr_activity`.
- Events arrive wrapped in `<github-webhook-activity>` tags and **wake the
  session**; the agent investigates each event and pushes a fix or asks for
  clarification.
- The generic, non-GitHub primitive underneath is **channels**: an MCP server
  that pushes `<channel>` events into a running session so Claude reacts to
  things that happen while you're away (Telegram, Discord, webhooks).

This matters two ways:

1. **We don't have to invent the session-push transport.** Two real ones exist
   (the platform's `subscribe_pr_activity`, and the generic `--channels` MCP
   mechanism). The design question is *which to ride*, not *whether one can
   exist*.
2. **It tells us where shuck's distinct value is.** The platform delivers a
   **raw** GitHub event ("check_run failed", "a review was submitted"). It does
   *not* tell the agent the failing step's command, the extracted error
   excerpt, the failure class, the check-run annotations, the collapsed review
   threads, or the triaged security finding. **That digestion is exactly what
   shuck already does.** So shuck's role is not "another webhook fan-out" — it's
   the layer that turns a one-line ping into an actionable report.

> The honest framing: a webhook only says *something happened*. shuck says
> *what it was and what to do about it*. The backend is the glue that runs
> shuck's cores the instant the webhook lands, so the session is handed the
> answer instead of a pointer.

---

## 2. What "closing the loop" events are, and how shuck digests each

| GitHub webhook event | Trigger condition | App permission (read) | shuck core that digests it |
| --- | --- | --- | --- |
| `check_run` | `action=completed`, `conclusion ∈ {failure, cancelled, timed_out, action_required}` | Checks | `cli.Inspect` (logs) → failing step + excerpt + `classify.FailureClass` + annotations |
| `check_suite` | `action=completed` with a failed conclusion | Checks | same; use to debounce the per-`check_run` storm in a matrix |
| `workflow_run` | `action=completed` | Actions | same; the natural "watch a whole run to done" signal |
| `status` | legacy commit status `state=failure/error` | Commit statuses | non-Actions CI; listed by name (no logs exist) |
| `pull_request_review` | `action=submitted` | Pull requests | `cli.Inspect` (reviews) → grouped verdict + collapsed threads |
| `pull_request_review_comment` | `action=created` | Pull requests | reviews digest (or just surface the one new thread) |
| `issue_comment` | `action=created` on a PR | Issues | top-level PR comment (filter to humans / non-bot) |
| `code_scanning_alert` | `action ∈ {created, reopened, appeared_in_branch}` | Code scanning alerts | `cli.Security` → severity, file:line, CVE/GHSA |
| `secret_scanning_alert` | `action ∈ {created, reopened, validated}` | Secret scanning alerts | `cli.Security` (raw secret never read) |
| `dependabot_alert` | `action ∈ {created, reopened}` | Dependabot alerts | `cli.Security` → package, fix version, advisory |

Every one of these maps onto a core shuck *already* exports for the CLI and the
MCP server (`Inspect`, `Security`, …). The backend doesn't need new GitHub
plumbing per event type — it needs to route each event to the right existing
core and ship the resulting `jsonout` / `security.Document` / `model.Report`.

Security alerts are the clearest case for *push over poll*: there is no
"poll-until-done" shape for "a new CVE appeared" — you either get the webhook or
you scan on a timer. `check_run`/`workflow_run` have a natural terminal state, so
they can be *either* pushed or polled (today shuck polls them with `--watch`).

---

## 3. Architecture

Three layers. Credentials and rate-limit cost live in the middle layer, never in
the session — mirroring how Claude Code on the web keeps the GitHub token out of
the container behind its GitHub proxy.

```
┌──────────────┐   webhooks    ┌────────────────────────────┐   digested      ┌───────────────────┐
│  GitHub      │ ────────────▶ │  shuck backend (broker)    │ ── event ─────▶ │  Claude Code      │
│  (the App)   │  check_run,   │                            │   over a        │  session(s)       │
│              │  review,      │  • verify HMAC, dedupe     │   transport     │                   │
│              │  *_alert …    │  • installation-token auth │                 │  subscribe(pr)    │
│              │               │  • run shuck cores         │ ◀── subscribe ──│  unsubscribe(pr)  │
└──────────────┘               │    (Inspect/Security/…)    │     / unsub     └───────────────────┘
                               │  • subscription registry   │
                               │  • fan-out to subscribers  │
                               └────────────────────────────┘
```

### 3.1 The GitHub App

A registered App (`shuck`) with **read-only** permissions for exactly the events
above (Checks, Actions, Pull requests, Issues, Code/Secret/Dependabot alerts)
and webhook subscriptions for those events. Installed per-repo or per-org. The
App identity:

- holds the webhook secret (HMAC signing key),
- mints **installation access tokens** (App JWT → `POST
  /app/installations/{id}/access_tokens`) so the backend calls the GitHub API
  *as the installation*, not as any user.

This is the "heavy lifting on the GitHub API side using an app" the user asked
for: one installation token absorbs the rate-limit cost of fetching logs /
reviews / alerts, instead of every session burning its own user token.

### 3.2 The backend broker

A small always-on service (the one new piece of infra). Responsibilities:

1. **Ingest.** Verify `X-Hub-Signature-256` (HMAC-SHA256), dedupe on
   `X-GitHub-Delivery`, and **ACK within GitHub's ~10s budget** by enqueuing the
   delivery and returning `200` immediately. Digestion happens off the request
   path.
2. **Digest.** Pull an installation token, resolve the event to a
   `target.Target`, and call the matching shuck core
   (`cli.Inspect` / `cli.Security`). Because shuck is a Go library, the broker
   can `import` the cores directly — no subprocess, no re-implementation. Output
   is the existing stable `jsonout.Document` (or `security.Document`).
3. **Route.** Look up which sessions subscribed to this PR (and which event
   kinds they want), then fan the digest out to each.
4. **Hold state.** The subscription registry and a short dedupe/debounce window
   live here. **This must be durable**, because sessions are ephemeral (see §5).

Debounce matters: a matrix build emits one `check_run` per leg. The broker
should coalesce on `check_suite` / `workflow_run` completion and digest **once**,
so the session gets a single "3 jobs failed, here are the steps" payload rather
than a storm.

### 3.3 Delivery into the session — the crux

This is the part the user is really asking about ("how would we push to CC? Are
channels an option? Is a background process still best? A monitor?"). There are
five candidate transports; they are not mutually exclusive.

#### (a) MCP channel — the purpose-built primitive ✅ *recommended primary*

shuck **already ships an MCP server** (`shuck mcp`, stdio, registered by the
plugin). A channel is just "an MCP server that pushes events into the session."
So we extend the existing server: alongside its request/response tools it opens a
long-lived connection to the broker (SSE or WebSocket) and emits
`notifications/...` that Claude Code surfaces as `<channel>` events, waking the
session. Reuses all of shuck's existing MCP plumbing; the session opts in with
`--channels shuck`.

> **Caveat — be honest about this.** There is a cluster of *currently-open*
> Claude Code bugs where channel / MCP server-initiated notifications are
> **silently dropped when the session is idle** (the REPL prioritizes stdin over
> MCP notifications) and **don't reliably wake an idle session**
> (anthropics/claude-code #44380, #61797, #36411, #41733, #58469, #36665). So
> today this path is reliable while the agent is *active* but not (yet) for the
> away-from-keyboard case. It is the *right* primitive and improving, but we
> can't ship on it alone for idle delivery.

#### (b) Ride the platform's `subscribe_pr_activity` (web/remote only) ✅ *recommended companion*

On Claude Code on the web, the platform *already* delivers
`<github-webhook-activity>` and exposes `subscribe_pr_activity` /
`unsubscribe_pr_activity`, and it *already* wakes idle sessions reliably (that's
the shipped Auto-fix feature). In this world shuck does **not** build transport
at all — it stays the **digestion layer the woken session calls**: the platform
wakes the session with the raw event, the session calls `inspect_logs` /
`inspect_security`. That is literally what this repo's `CLAUDE.md` already tells
agents to do. Zero new infra; works today.

The limitation: it's tied to the Anthropic-managed web runtime and delivers raw
(undigested) events, and we don't control its event set (e.g. it explicitly does
*not* deliver merge-conflict or CI-success transitions).

#### (c) Agent SDK streaming input (`send_message`) — *most reliable injection*

If shuck owns the agent lifecycle — a `shuck watch --agent` that spawns Claude
via the Claude Agent SDK in **streaming-input mode** — the broker injects a
fully-formed user turn into the running session with `send_message` over the
control protocol's `--input-format stream-json` stdin. Unlike a notification
that can be dropped, this is a real turn the agent *must* process. The cost: shuck
launches and owns the CC process (headless / CI / a daemon), rather than living
inside a session the user already started. Best for unattended, server-side
"watch this org's PRs" deployments.

#### (d) Background process + hook/file bridge — *local, no backend*

A `SessionStart` hook starts a background `shuck watch` that writes digested
events to a spool file/socket; the agent drains it. Crude, no infra, but doesn't
solve idle-wake any better than (a) and needs the agent to look.

#### (e) Polling with `--watch` + a Monitor/background task — *what we do today*

shuck already has `shuck --watch`: poll the checks every `--interval` until the
run is terminal, then report with a failure-aware exit code. Run as a background
task and surfaced by the harness's Monitor primitive, this closes the loop for a
**single PR you just pushed** with **zero new infra and no webhook** — at the cost
of polling. It has a terminal state, so it's bounded and cheap.

### 3.4 Subscribe / unsubscribe

Two new MCP tools on shuck's server, mirroring `subscribe_pr_activity`'s
ergonomics:

- `shuck_subscribe(repo, pr, events?[])` → registers `(pr, session_id, filter)`
  in the broker and starts the channel stream for it. `events` defaults to all
  closing-the-loop kinds; a session can ask for only `ci` or only `security`.
- `shuck_unsubscribe(repo, pr)` → drops the registration.

The **session id** is the routing key. In cloud sessions it's available as
`CLAUDE_CODE_REMOTE_SESSION_ID`; the channel connection carries it so the broker
knows which stream a digest belongs to. Subscription state is the broker's, not
the session's, so it survives a session restart.

---

## 4. Worked example: a CI failure, end to end

1. Developer pushes to a PR branch. GitHub Actions runs, a job fails.
2. GitHub sends `check_run{action:completed, conclusion:failure}` (and, at suite
   end, `check_suite{...}`) to the App's webhook URL.
3. Broker verifies HMAC, ACKs `200`, enqueues. On `check_suite.completed` it
   debounces the matrix and digests **once**.
4. Broker mints an installation token, builds the `target.Target` for the PR,
   and calls `cli.Inspect(ctx, tgt, opts)` → `*model.Report` →
   `jsonout.NewDocument(report)`. It now holds: failing job name, failing step
   command, the extracted error excerpt, `FailureClass` (`lint|test|build|…`),
   and check-run annotations (`path:line:message`).
5. Broker looks up subscribers for this PR, finds the session, and pushes the
   digest over the channel. The session sees something like:

   ```
   <shuck-event kind="ci-failure" repo="o/r" pr="42">
   1 job failed: "test (ubuntu, go 1.25)"
     step: go test ./...   class: test
     excerpt:
       --- FAIL: TestRoute (0.00s)
           route_test.go:88: got 500, want 200
     annotations:
       internal/route/route_test.go:88  test failure
   </shuck-event>
   ```
6. The agent acts immediately — it never made a single GitHub API call; the
   backend did all of it once, and would have done it once even for ten
   subscribed sessions.

Reviews and security alerts follow the same path with `cli.Inspect`
(reviews-only) and `cli.Security`.

---

## 5. The genuinely hard parts (don't hand-wave these)

1. **Ephemeral sessions vs. durable subscriptions.** Cloud containers are
   reclaimed on inactivity; a laptop session ends when closed. So "push to the
   session that subscribed" breaks the moment that session is gone. The broker
   must hold subscriptions durably, *and* decide what to do when the target
   session is dead: drop, queue-until-resume, or **start a new session**. The
   platform's answer is routines/`/schedule` (it can spin up a fresh web
   session); a shuck backend reusing the web API could do the same, or fall back
   to posting the digest as a PR comment that the platform's own subscription
   then surfaces.
2. **Idle-wake is unsolved on the generic channel path** (§3.3a). Until those
   Claude Code bugs close, the dependable wake paths are the platform's
   `subscribe_pr_activity` (b) and Agent-SDK injection (c). Plan for both, treat
   the raw MCP channel as best-effort-while-active.
3. **The 10-second webhook SLA.** Digestion (downloading + parsing job logs) is
   far slower than 10s. Ingest must ACK then digest async; never digest on the
   request thread.
4. **Delivery semantics.** Webhooks are at-least-once and can arrive out of
   order. Dedupe on delivery id; key digests on `(pr, head_sha, event)`;
   debounce matrices on suite/run completion.
5. **Security & blast radius.** Least-privilege App perms (read-only, only the
   events we use). HMAC-verify every delivery. The installation token stays in
   the backend (the session never sees it), matching the web proxy model.
   Treat webhook payload bodies and the digested review/comment text as
   **untrusted input** — a review comment is attacker-controlled and could try to
   redirect the agent; the digest should be framed as data, not instructions
   (the platform already wraps such content in `<untrusted_external_data>`).
6. **Cost of the new daemon.** It's a stateful always-on service with secrets —
   a real operational step up from "a CLI + a stdio MCP server." Worth it only
   for push/away-from-keyboard/security/multi-PR; not for the single-PR
   just-pushed loop.

---

## 6. Recommendation

A **layered** answer, not one transport:

- **Tier 0 — ship nothing new, use what exists.** For the single-PR "I just
  pushed, tell me when CI is done or what broke" loop, keep using
  `shuck --watch` as a bounded background poll (3.3e). On Claude Code on the web,
  lean on the platform's `subscribe_pr_activity` and make shuck the **digestion
  layer the woken session calls** (3.3b) — this is already how `CLAUDE.md`
  steers agents and needs no backend. *This covers most of the stated need
  today.*

- **Tier 1 — build the App + broker for the cases polling can't serve:** push
  (no poll cost), org-wide / multi-PR, **security-alert** events (no
  poll-until-done shape), and away-from-keyboard. Reuse shuck's cores server-side
  for digestion; expose `shuck_subscribe` / `shuck_unsubscribe` MCP tools;
  deliver primarily over the **MCP channel** (3.3a, the purpose-built primitive
  shuck is already 90% set up to host), with **Agent-SDK `send_message`** (3.3c)
  as the reliable-injection path for owned/headless sessions and a **PR-comment
  fallback** for dead sessions.

**Direct answers to the questions posed:**

- *Are channels an option?* Yes — they are *the* designed-for-this primitive, and
  shuck already runs the MCP server that would host one. Caveat: idle-delivery
  bugs make them best-effort-while-active for now.
- *Is a background process / monitor still best?* For the single-PR
  just-pushed loop, yes — `shuck --watch` as a background task is simplest,
  already shipped, and needs no infra. The App+backend is the *upgrade* for push,
  multi-PR, security, and unattended operation — not a replacement for the poll.
- *How do we push "this PR had this error in the logs" back to CC?* The broker
  runs `cli.Inspect` the instant the `check_run` webhook lands and ships the
  resulting `jsonout.Document` (failing step + excerpt + class + annotations) as
  a `<shuck-event>` over the channel to every subscribed session — so the session
  receives the *answer*, having made zero GitHub API calls itself.

---

## 7. If we build Tier 1 — rough package shape

- `cmd/shuck-broker` (or a `shuck broker` subcommand) — the always-on service:
  HTTP webhook receiver + queue + digest workers + subscription registry +
  channel fan-out endpoint (SSE/WS).
- `internal/app` — GitHub App auth: JWT signing, installation-token minting +
  caching, HMAC verification. (Distinct from `internal/gh`, which is user-token
  API wrappers; the broker would call those cores with an installation token.)
- `internal/broker` — subscription registry (durable), dedupe/debounce, the
  event→core router, fan-out.
- Extend `internal/mcp` — add a channel mode (long-lived broker connection +
  `notifications/...` emission) and the `shuck_subscribe` / `shuck_unsubscribe`
  tools.
- Reuse, unchanged: `cli.Inspect` / `cli.Security` / `internal/jsonout` /
  `internal/security` / `internal/classify` / `internal/target`.

Open the build behind a flag and start with **one** event (`check_run`
failure → channel) end-to-end before fanning out to reviews and alerts.
