# RFC: a shuck backend + GitHub App that closes the loop (terminal-first)

Research and ideation for turning shuck from a *pull* tool (an agent asks "why
did CI fail?") into a *push* system: a GitHub App + backend that watches a PR's
"closing-the-loop" events — a CI failure, a new review, a new security alert —
does the GitHub-API heavy lifting server-side, **digests** the event with
shuck's own cores, and delivers a ready-to-act payload into a **terminal**
Claude Code session that has *subscribed* to that PR.

Status: exploratory. Nothing here is built yet. This revision folds in a round
of external research — what other tools in this space actually do, the verified
first-party mechanism for pushing events into a terminal session, and a ranked
menu for getting webhooks to a laptop. Sources are linked inline and collected
at the end.

---

## 0. Scope: terminal Claude Code, not the web system

The target is **Claude Code running in a host terminal** — the CLI on a
developer's own machine — **not** Claude Code on the web. That constraint is
load-bearing:

- The web platform's `subscribe_pr_activity` / `<github-webhook-activity>` /
  Auto-fix machinery (§1) is **web-only**. Useful as a *reference design*; a
  terminal user cannot ride it.
- A terminal host is **long-lived and local**, which *simplifies* the
  ephemeral-session problem but *introduces* the central one: a laptop behind
  NAT **cannot receive inbound webhooks**, so ingress needs a tunnel or an
  outbound-connected relay (§8).
- The good news, confirmed by research: there are **first-party terminal
  mechanisms built for exactly this** — **Channels** (`--channels`) and
  **Remote Control** — and Anthropic's own docs name "**reacting to external
  events like CI failures**" as the channels use case. So the question "can
  channels be the receiving mechanism?" is answered **yes, by design** (§3.3a),
  with one honest caveat about idle-session wake reliability (§7).

---

## 1. The shape already exists (as a reference)

The exact shape — a GitHub App receiving PR webhooks, a session that
**subscribes** to a PR, events that **wake** the session — ships in production
as Claude Code on the web's *Auto-fix PRs*. We can't use it directly (web-only),
but it proves the shape and maps the parts:

- The **Claude GitHub App** receives PR webhooks; installation is what enables
  Auto-fix.
- A session **subscribes** to a PR (`subscribe_pr_activity` /
  `unsubscribe_pr_activity`); events arrive in `<github-webhook-activity>` tags
  and wake the session.
- The generic, non-GitHub primitive underneath is **channels**.

**Where shuck's distinct value sits:** the platform delivers a *raw* event
("check_run failed"). It does *not* tell the agent the failing step's command,
the error excerpt, the failure class, the annotations, the collapsed review
threads, or the triaged alert. **That digestion is exactly what shuck already
does.** A webhook says *something happened*; shuck says *what it was and what to
do*. The crucial research finding (§6) is that **almost no existing tool does
this digest-and-push** — most either run a stateless agent in CI or feed a
vendor's own cloud agent.

---

## 2. "Closing-the-loop" events, and how shuck digests each

| GitHub webhook event | Trigger condition | App permission (read) | shuck core that digests it |
| --- | --- | --- | --- |
| `check_run` | `completed`, `conclusion ∈ {failure, cancelled, timed_out, action_required}` | Checks | `cli.Inspect` → failing step + excerpt + `classify.FailureClass` + annotations |
| `check_suite` | `completed` with a failed conclusion | Checks | same; debounce the per-`check_run` matrix storm here |
| `workflow_run` | `completed` | Actions | same; the natural "watch a whole run to done" signal |
| `status` | legacy commit status `failure/error` | Commit statuses | non-Actions CI; listed by name (no logs) |
| `pull_request_review` | `submitted` | Pull requests | `cli.Inspect` (reviews) → grouped verdict + collapsed threads |
| `pull_request_review_comment` | `created` | Pull requests | reviews digest (or the one new thread) |
| `issue_comment` | `created` on a PR | Issues | top-level PR comment (gate to non-bot) |
| `code_scanning_alert` | `created`/`reopened`/`appeared_in_branch` | Code scanning alerts | `cli.Security` → severity, file:line, CVE/GHSA |
| `secret_scanning_alert` | `created`/`reopened`/`validated` | Secret scanning alerts | `cli.Security` (raw secret never read) |
| `dependabot_alert` | `created`/`reopened` | Dependabot alerts | `cli.Security` → package, fix version, advisory |

Each maps onto a core shuck already exports (`cli.Inspect`, `cli.Security`). The
backend routes the event to the right core and ships the resulting
`jsonout.Document` / `security.Document`. Security alerts are the clearest case
for *push over poll*: there's no "poll-until-done" shape for "a new CVE
appeared." [GitHub: security alert webhooks](https://docs.github.com/en/webhooks/webhook-events-and-payloads)

---

## 3. Architecture

Three layers. Credentials and rate-limit cost live in the middle, never in the
session — mirroring how Claude Code on the web keeps the token out of the
container.

```
┌──────────────┐   webhooks    ┌────────────────────────────┐   digested      ┌───────────────────┐
│  GitHub      │ ────────────▶ │  shuck-relay (broker)      │ ── event ─────▶ │  shuck channel    │
│  (the App)   │  check_run,   │                            │   over an       │  (local MCP srv)  │
│              │  review,      │  • verify HMAC, dedupe     │   OUTBOUND      │   in the terminal │
│              │  *_alert …    │  • installation-token auth │   SSE/WS the    │   session         │
│              │               │  • run shuck cores         │   laptop dials  │  emits <channel>  │
│              │               │  • subscription registry   │ ◀── subscribe ──│  → wakes Claude   │
└──────────────┘               └────────────────────────────┘                 └───────────────────┘
```

### 3.1 The GitHub App
Read-only permissions for exactly the events in §2; webhook subscriptions gated
by those permissions. Holds the webhook HMAC secret and mints **installation
access tokens** (App JWT → `POST /app/installations/{id}/access_tokens`, ~1h
TTL, cached). One installation token absorbs the API cost instead of every
session burning a user token. The webhook payload carries the `installation` id,
so the handler knows which token to mint.
[App auth](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation)

### 3.2 The relay/broker
The one new piece of infra. Ingest (verify `X-Hub-Signature-256`, dedupe on
`X-GitHub-Delivery`, **ACK within ~10s** then process async); digest (call the
shuck core as a Go library — no subprocess); route to subscribers; hold
subscription state durably (sessions are ephemeral, §5). Debounce matrix
`check_run` storms on `check_suite`/`workflow_run` completion so the session
gets one "3 jobs failed" payload, not a storm.
[Webhook best practices](https://docs.github.com/en/webhooks/using-webhooks/best-practices-for-using-webhooks)

### 3.3 Delivery into the terminal session — the transports

This is the crux. Research narrowed the field; details and a reliability ranking
are in §7. The candidates:

#### (a) Channels (`--channels`) — the purpose-built, first-party primitive ✅ *recommended*
**Verified mechanism** ([channels reference](https://code.claude.com/docs/en/channels-reference)):
a channel is an MCP server (stdio subprocess of Claude Code) that declares the
`claude/channel` capability and emits `notifications/claude/channel` with
`content` + `meta`. The event lands in Claude's context as a
`<channel source="shuck" …>` tag and the session reacts. **Anthropic's own
reference example is a CI-failure webhook receiver** — literally this use case.
One-way is enough for alerts; a `reply` tool would make it two-way; permission
relay is available.

For shuck the clean shape is to **mirror the Telegram/Discord channel plugins**:
those don't expose an inbound port — they make an **outbound** connection to
their platform and poll/stream. The **shuck channel** likewise dials **outbound**
to the shuck-relay (SSE/WS), receives digested events, and re-emits them as
`notifications/claude/channel`. That sidesteps NAT entirely — the laptop never
needs an inbound port.

Requirements/caveats: research preview, Claude Code **v2.1.80+**; custom
channels aren't on the curated allowlist yet, so they need
`--dangerously-load-development-channels server:shuck` (or an
org `allowedChannelPlugins` entry) until listed. Notifications are **fire-and-
forget** — "if the session hasn't loaded your server as a channel … events are
dropped silently." And **must gate the sender** (HMAC/allowlist) — an ungated
channel is a prompt-injection vector, per the docs. The idle-wake reliability
gap is the real caveat (§7).

#### (b) Remote Control — first-party, but human-driven 🔸 *adjacent, not a push API*
[Remote Control](https://code.claude.com/docs/en/remote-control) (v2.1.51+)
drives a *running local* session from claude.ai/mobile over **outbound HTTPS
only, no inbound ports** — exactly the NAT-friendly model shuck wants. But it's
for a *human* steering from another device, not a programmatic event push; and
it can push **mobile notifications** when a task finishes. Useful as the "tell
me on my phone" leg, not as the event-injection transport.

#### (c) tmux `send-keys` / pty injection — the reliable idle-waker 🔧 *fallback*
The one mechanism that **reliably wakes a truly idle terminal REPL**, because it
writes to the pty/stdin the REPL is blocked on. Real and widely used:
[`obra/claude-session-driver`](https://github.com/obra/claude-session-driver) is
a versioned tool built entirely on it, and **Claude Code's own Agent Teams
feature spawns teammates via `tmux send-keys`**
([#23513](https://github.com/anthropics/claude-code/issues/23513)). Gotchas are
documented: the bracketed-paste→Enter race
([csd #20](https://github.com/obra/claude-session-driver/issues/20)) and CSI-u
newline loss ([#43169](https://github.com/anthropics/claude-code/issues/43169)).
Requires launching `claude` inside tmux up front, and is a prompt-injection
surface. A solid escape hatch when channel idle-wake fails.

#### (d) Agent SDK streaming daemon (`send_message`) — reliable, but you own the process 🔧 *unattended*
A long-lived process using the Agent SDK in streaming-input mode holds a session
open and injects each webhook as a new user turn. **Reliable** (a real turn, not
a droppable notification) — but only because *you own the loop*; it can't reach
a foreign `claude` someone already started in a terminal. The headless variant
is `claude -p "<digest>" --resume <id>` per event. Best for unattended,
server-side "watch this org" deployments.

#### (e) Polling `--watch` + a background task — what we ship today 🟢 *zero-infra*
`shuck --watch` polls a PR's checks until terminal, then reports. As a
background task it closes the loop for a **single PR you just pushed** with
**zero new infra and no webhook** — at the cost of polling. Bounded, cheap,
already shipped.

#### (x) MCP server→client primitives alone — ❌ *can't wake idle*
For completeness: plain MCP notifications, `resources/updated`, `elicitation`,
and `sampling` **cannot originate a turn in an idle session** — they're passive
or parasitic on an in-flight tool call. The `claude/channel` notification is the
*only* MCP-layer push wired to inject a turn, and even it is subject to the
idle-wake gap. MCP is great for responding *within* a turn, wrong for
*originating* one. ([MCP spec](https://modelcontextprotocol.io/specification))

### 3.4 Subscribe / unsubscribe
Two tools on the shuck channel (or shuck MCP) server, mirroring
`subscribe_pr_activity`: `shuck_subscribe(repo, pr, events?[])` registers
`(pr, session_id, filter)` in the relay and starts streaming that PR's digests;
`shuck_unsubscribe(repo, pr)` drops it. Subscription state is the relay's, so it
survives a session restart (a `shuck resubscribe` reattaches).

---

## 4. Worked example: a CI failure, end to end

1. Push to a PR branch; a job fails.
2. GitHub sends `check_run{completed, failure}` (and `check_suite{completed}`) to
   the App's webhook URL.
3. Relay verifies HMAC, ACKs `200`, enqueues; debounces the matrix on suite
   completion and digests **once**.
4. Relay mints an installation token, builds the PR `target.Target`, calls
   `cli.Inspect` → `jsonout.NewDocument`. It now holds the failing job/step,
   command, error excerpt, `FailureClass`, and annotations.
5. Relay finds the PR's subscribers and streams the digest down the laptop's
   outbound channel connection. The session sees:

   ```
   <channel source="shuck" kind="ci-failure" repo="o/r" pr="42" severity="high">
   1 job failed: "test (ubuntu, go 1.25)"   class: test
     step: go test ./...
     excerpt:
       --- FAIL: TestRoute (0.00s)
           route_test.go:88: got 500, want 200
     annotation: internal/route/route_test.go:88  test failure
   </channel>
   ```
6. The agent acts immediately — **zero GitHub API calls from the session**; the
   relay did the work once, and would for ten subscribers.

---

## 5. The genuinely hard parts

1. **Session lifetime vs. durable subscriptions.** A terminal session ends when
   closed; the relay outlives it. Milder than on the web (host is long-lived,
   user usually present, a closed session is a deliberate "stop watching"), so
   **drop-on-disconnect** is a defensible default, with the relay holding
   subscriptions durably for `resubscribe`.
2. **Idle-wake (§7).** The channel notification is fire-and-forget and the REPL
   prioritizes stdin when idle, so a channel event may not wake an idle session
   until the user interacts — the single biggest risk. Mitigations: tmux
   `send-keys` (c), an SDK/headless daemon (d), or accept "wakes on next
   interaction" for at-keyboard use.
3. **The 10-second webhook SLA.** Digestion (downloading + parsing logs) far
   exceeds 10s. Ingest must ACK-then-process.
   [Handling deliveries](https://docs.github.com/en/webhooks/using-webhooks/handling-webhook-deliveries)
4. **At-least-once, unordered delivery.** Dedupe on `X-GitHub-Delivery`; key
   digests on `(pr, head_sha, event)`; debounce matrices; don't trust arrival
   order.
5. **Security & blast radius.** Least-privilege read-only App perms; HMAC-verify
   every delivery; **gate the channel sender** (the docs call an ungated channel
   a prompt-injection vector); installation token stays in the relay. Treat
   webhook bodies and digested review/comment text as **untrusted input** framed
   as data, not instructions.
6. **Standing infra.** A stateful always-on relay with secrets is a real step up
   from "a CLI + a stdio MCP server." Justified only for push / multi-PR /
   security / away-from-keyboard.

---

## 6. What others are doing (market survey)

The space splits into two camps, plus a hybrid — and **the gap shuck fills is
real**: almost nobody builds a service that *digests CI/review/security events
into a prompt for an agent you control*.

**Review bots → persistent GitHub App + webhooks, on vendor infra.**
[CodeRabbit](https://docs.coderabbit.ai/platforms/github-com),
[Greptile](https://www.greptile.com/docs/integrations/github-gitlab-integration)
(persistent because it indexes the whole repo into a graph; v3 rebuilt on the
Claude Agent SDK), [Graphite Diamond](https://graphite.com/features/ai-reviews),
and [Sweep](https://github.com/sweepai/sweep) are all persistent Apps reacting
to PR-open/update webhooks with **zero customer-side Actions** and **installation-
token** auth. Reason: reviews must fire instantly on every PR without each team
writing workflow YAML, and the smart ones need persistent state.

**Code-writing agents → bifurcated.**
- *Ephemeral GitHub Action, Actions-token auth, stateless per run:* the
  [Claude Code GitHub Action](https://github.com/anthropics/claude-code-action)
  (fires on `issue_comment`/`@claude`; **has no native `workflow_run`→agent
  wiring — reacting to a CI failure is exactly the documented gap**),
  [Qodo PR-Agent](https://github.com/qodo-ai/pr-agent) (ships Action *and*
  self-hosted-App *and* SaaS off one codebase — a great reference), and the
  [OpenHands resolver](https://docs.openhands.dev/openhands/usage/run-openhands/github-action).
- *Persistent vendor backend using ephemeral sandboxes as compute:*
  [GitHub Copilot coding agent](https://docs.github.com/copilot/concepts/agents/coding-agent/about-coding-agent)
  (platform-managed loop that **diagnoses its own CI/test failures** and spawns
  a fresh session per review comment), [Devin](https://docs.devin.ai/work-with-devin/devin-review)
  ("by default Devin automatically responds to PR comments and CI failures"), and
  [Cursor background agents](https://cursor.com/docs/cloud-agent/automations)
  (triggered by GitHub webhooks, Slack, schedules, **or a custom webhook
  endpoint you POST to** — the closest analog to shuck's "push a digested event
  in").

**Implications for shuck.** (1) The ephemeral-Action camp — where shuck's
skill/MCP/CLI shape lives — is **weakest exactly at CI-failure reaction and
cross-event state**, the two things a persistent webhook App does naturally; the
shuck-relay supplies precisely that. (2) The architectural references worth
studying are **Cursor's custom-webhook endpoint** and **OpenHands' `GithubManager`**
(webhook → "is a job requested?" → conversation) — both formalize the
"event → digested job" boundary shuck sits on. (3) Qodo and OpenHands prove one
codebase can ship Action + self-hosted-App + SaaS; shuck could likewise offer
"poll mode (today)" and "relay mode (opt-in)" from one binary.

---

## 7. Terminal push mechanisms, ranked by idle-wake reliability

The decisive question is **state of the target session**: *mid-task* (agent
running a turn) vs *idle at the prompt* (REPL blocked on stdin). Mechanisms that
target the **pty/stdin** can wake an idle REPL; mechanisms at the **model/MCP
layer** only act when a turn is already running.

| Mechanism | Wakes an **idle** terminal REPL? | Works **mid-task**? | Notes |
| --- | --- | --- | --- |
| **tmux `send-keys` / pty injection** (c) | ✅ reliably | ✅ (queued to next prompt) | Writes to stdin; used by `claude-session-driver` and Claude's own Agent Teams. Must launch in tmux; injection surface. |
| **IPC (FIFO/socket/queue/file-watch) → `send-keys`** | ✅ (inherits pty write) | ✅ | IPC is the trigger; the pty write is what wakes it. |
| **Headless spawn per event** (`claude -p --resume`) (d) | ✅ N/A — creates a session | n/a | No idle session to wake; most robust for unattended webhook→agent. |
| **Agent SDK streaming `send_message`** (d) | ✅ *only in an SDK host you wrote* | ✅ | Reliable inside your own loop; can't reach a foreign terminal. |
| **Channels `notifications/claude/channel`** (a) | ⚠️ **unreliable when idle** | ✅ on next turn | First-party, built for CI alerts; fire-and-forget. Idle REPL prioritizes stdin; events may wait for interaction. Open issues: [#44380](https://github.com/anthropics/claude-code/issues/44380), [#61797](https://github.com/anthropics/claude-code/issues/61797), [#36411](https://github.com/anthropics/claude-code/issues/36411). |
| **Remote Control** (b) | 🔸 human-driven, not a push API | n/a | Outbound-HTTPS, NAT-friendly; for steering + phone notifications, not event injection. |
| **MCP notifications / `resources/updated` / elicitation / sampling** (x) | ❌ | ⚠️ passive / parasitic | Can't originate a turn. |

**Takeaway:** the *only* thing that reliably wakes an **idle** interactive
session is a **pty write** (tmux `send-keys`), optionally fronted by IPC. The
first-party **channel** is the right, supported path and works while the agent
is active or at-keyboard, but its idle-wake is unreliable in the current
research preview — so a robust shuck pairs the channel with **either** a tmux
fallback (interactive use) **or** a headless/SDK daemon (unattended use). For
fully unattended "external event → agent," the durable answer is the
**headless/daemon pattern**, not injecting into a human's open REPL.

---

## 8. Getting webhooks to the laptop — ranked menu (NAT-friendly first)

A laptop has no public IP and usually can't accept inbound connections, so every
viable option is **outbound**: the box dials out to a relay that receives
GitHub's POST and pushes it down the open connection. Ranked by least-infra:

| Rank | Option | Local box does… | Dev/Prod | Infra |
| --- | --- | --- | --- | --- |
| 1 | **`gh webhook forward`** ([cli/gh-webhook](https://github.com/cli/gh-webhook)) | outbound **WebSocket** to a GitHub-hosted relay | Dev only (official); **one subscriber per repo/org** | zero |
| 2 | **smee.io + smee-client** | outbound **SSE** to public relay | Dev only; channels unauthenticated | zero |
| 3 | **Cloudflare Tunnel** (`cloudflared`) | outbound QUIC to CF edge → public URL GitHub posts to | **prod-grade**, free | free tier |
| 4 | **ngrok** | outbound tunnel; local request inspector/replay | dev→prod (paid) | free→paid |
| 5 | **Tailscale Funnel** | outbound WireGuard | dev/light prod | free tier |
| 6 | **Hookdeck (managed)** / **Outpost**, **Convoy** (MIT), **Svix** (self-host) | gateway ingests + **retries/dedup/replay/queue**; local agent connects outbound | **prod-grade** | free→paid / self-host |

For shuck's recommended design the relay *is* option 6's role (self-hosted
gateway) and the **shuck channel connects outbound to it** — so the developer
never runs a tunnel. The dev-mode shortcut is `gh webhook forward` or smee into
a local `shuck broker`. Whatever the transport, the receiver still owns:
HMAC-verify `X-Hub-Signature-256` (constant-time), ACK-within-10s-then-queue,
and dedupe on `X-GitHub-Delivery` (at-least-once, unordered).
[Validating deliveries](https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries) ·
[gh CLI receiving webhooks](https://docs.github.com/en/webhooks/using-webhooks/receiving-webhooks-with-the-github-cli)

---

## 9. Recommendation (terminal-first)

**Channels are the receiving mechanism** — first-party, terminal-native, and
documented by Anthropic for "reacting to CI failures." Build in layers:

- **Tier 0 — ship nothing new (works today).** For the single-PR "tell me when
  CI is done or what broke" loop, use `shuck --watch` as a bounded background
  poll. Outbound-only, no App, no relay. Covers a large share of the need.

- **Tier 1 — the shuck channel over a self-hosted relay.** GitHub App + a small
  public **shuck-relay** receives webhooks and runs shuck's cores to **digest**;
  the local **shuck channel** (an MCP server mirroring the Telegram plugin)
  connects **outbound** to the relay (NAT solved, §8) and emits each digest as a
  `<channel>` event (§3.3a). Opt in with `--channels` + `shuck_subscribe`. This
  is the only path serving push, multi-PR, and **security-alert** events.
  - *Engineer around the idle-wake gap (§7):* for unattended use, run the
    **headless/SDK daemon** (`claude -p --resume` per digest) — the most robust
    "event→agent"; for an at-keyboard interactive session, offer a **tmux
    `send-keys` fallback** so an idle session still wakes; and use **Remote
    Control's mobile push** to ping the human's phone.
  - *Single-developer shortcut without a hosted relay:* `gh webhook forward` or
    smee into a local `shuck broker` that emits the channel events — everything
    on the laptop, zero hosted infra.

**Direct answers to the questions posed:**

- *Can channels be the mechanism for receiving events?* **Yes — that's the
  recommendation,** and it's now verified against the first-party channels
  reference (whose own example is a CI-failure receiver). A shuck channel is an
  MCP server emitting `notifications/claude/channel`; shuck already ships the MCP
  server to host it. Caveat: research-preview idle-wake reliability (§7), covered
  by the tmux/headless fallbacks.
- *Is a background process / monitor still best?* For the single-PR just-pushed
  loop, yes — `shuck --watch` is simplest and already shipped. The App + relay +
  channel is the *upgrade* for push, multi-PR, security, and away-from-keyboard.
- *How do we push "this PR had this error in the logs" back to CC?* The relay
  runs `cli.Inspect` the instant the `check_run` webhook lands and streams the
  `jsonout.Document` (failing step + excerpt + class + annotations) down the
  laptop's outbound channel connection as a `<channel>` event — the session gets
  the *answer*, having made zero GitHub API calls.

---

## 10. If we build Tier 1 — rough package shape

- `cmd/shuck-relay` (or `shuck broker`) — the public service: webhook receiver +
  queue + digest workers + subscription registry + an **outbound-friendly**
  fan-out endpoint (SSE/WS) the laptop dials up to. The same binary runs locally
  behind `gh webhook forward`/smee for the single-dev variant.
- `internal/app` — GitHub App auth: JWT signing, installation-token minting +
  caching, HMAC verification. (Distinct from `internal/gh`'s user-token wrappers,
  which the relay calls with an installation token.)
- `internal/broker` — durable subscription registry, dedupe/debounce, the
  event→core router, fan-out.
- The **shuck channel** — a small MCP server (TS or Go) declaring
  `claude/channel`, dialing the relay outbound, re-emitting digests as
  `notifications/claude/channel`, with `shuck_subscribe` / `shuck_unsubscribe`
  tools and **sender gating**. Package as a channel plugin for `--channels`.
- Reuse unchanged: `cli.Inspect` / `cli.Security` / `internal/jsonout` /
  `internal/security` / `internal/classify` / `internal/target`.

Build behind a flag; ship **one** event (`check_run` failure → channel)
end-to-end before fanning out to reviews and alerts.

---

## 11. Sources

**Claude Code mechanisms:**
[Channels](https://code.claude.com/docs/en/channels) ·
[Channels reference (build your own)](https://code.claude.com/docs/en/channels-reference) ·
[Remote Control](https://code.claude.com/docs/en/remote-control) ·
[Claude Code on the web / Auto-fix](https://code.claude.com/docs/en/claude-code-on-the-web) ·
[Agent SDK streaming input](https://code.claude.com/docs/en/agent-sdk/streaming-vs-single-mode) ·
[obra/claude-session-driver](https://github.com/obra/claude-session-driver) ·
idle-wake issues [#44380](https://github.com/anthropics/claude-code/issues/44380),
[#61797](https://github.com/anthropics/claude-code/issues/61797),
[#36411](https://github.com/anthropics/claude-code/issues/36411),
tmux-spawn [#23513](https://github.com/anthropics/claude-code/issues/23513),
paste [#43169](https://github.com/anthropics/claude-code/issues/43169).

**Market survey:**
[Claude Code Action](https://github.com/anthropics/claude-code-action) ·
[Qodo PR-Agent](https://github.com/qodo-ai/pr-agent) ·
[CodeRabbit](https://docs.coderabbit.ai/platforms/github-com) ·
[Greptile](https://www.greptile.com/docs/integrations/github-gitlab-integration) ·
[Graphite AI reviews](https://graphite.com/features/ai-reviews) ·
[Copilot coding agent](https://docs.github.com/copilot/concepts/agents/coding-agent/about-coding-agent) ·
[Devin Review](https://docs.devin.ai/work-with-devin/devin-review) ·
[Cursor automations](https://cursor.com/docs/cloud-agent/automations) ·
[OpenHands resolver](https://docs.openhands.dev/openhands/usage/run-openhands/github-action) ·
[Sweep](https://github.com/sweepai/sweep).

**GitHub App / webhooks / ingress:**
[Webhook events](https://docs.github.com/en/webhooks/webhook-events-and-payloads) ·
[Best practices](https://docs.github.com/en/webhooks/using-webhooks/best-practices-for-using-webhooks) ·
[Validating deliveries](https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries) ·
[App installation auth](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation) ·
[gh webhook forward](https://github.com/cli/gh-webhook) ·
[smee.io](https://smee.io/) ·
[Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/) ·
[Hookdeck Outpost](https://github.com/hookdeck/outpost) ·
[Convoy](https://github.com/frain-dev/convoy).

**Patterns:**
[LangGraph human-in-the-loop / agent inbox](https://docs.langchain.com/oss/python/langchain/human-in-the-loop) ·
[HumanLayer](https://www.humanlayer.dev/) ·
[Temporal for AI agents](https://temporal.io/blog/of-course-you-can-build-dynamic-ai-agents-with-temporal) ·
[Inngest durable execution](https://www.inngest.com/blog/durable-execution-key-to-harnessing-ai-agents).
