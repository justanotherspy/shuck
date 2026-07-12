<!--
Source draft for the justanotherspy.com project page — reviewed here in the
shuck repo (version-controlled next to the code it describes) and published
from the separate site repo. Not a build artefact; no site tooling lives here.
-->

# shuck — close the CI loop without burning the agent's budget

## The premise

An agent working a pull request doesn't know when CI fails. Finding out means
polling — `gh run list`, `gh run view`, download the logs, scroll — and every
one of those calls spends tokens and wall-clock time inside the agent's context
window, to discover a fact (the run went red, here's the failing step) that a
webhook already knew seconds earlier.

shuck's core trick is to **shuck the husk and keep the kernel**: when CI goes
red, it drills a GitHub Actions run down to the failing *steps* and prints just
their error logs — tagged with a coarse class (`lint`/`test`/`build`/`timeout`/
`oom`/`infra`) and the check-run annotations that point at `file:line`. No
tab-hopping, no log scrolling. That's the portable CLI, and it's the default:
one Go binary, a GitHub token, `--json` for machines, and an MCP server so any
agent can call it as a typed tool.

The v2 story is what happens when you want that kernel **pushed to you** the
instant CI finishes, instead of paying an agent to poll for it.

## Two modes, and the second never breaks the first

shuck has two modes, and the compatibility contract between them is the whole
design discipline of the project:

1. **Portable mode (the default).** CLI + MCP + a GitHub token. Pull-based, zero
   infrastructure. Nothing below changes it.
2. **Self-hosted mode (strictly opt-in).** An event router you host: GitHub App
   → webhook ingest → queue → worker → gateway → a channel shim inside your
   Claude Code session. Push-based. Active only when an operator deploys the
   backend *and* a user installs the shim with a minted token.

CI enforces this literally: the portable binary's import graph is checked so it
never links the AWS SDK, the WebSocket stack, or any backend package. The
backend is separate binaries. You can adopt the router and still use every
pull-based tool; you can ignore the router entirely and lose nothing.

## The constraint that shaped the design: a stdio channel

Claude Code channels are stdio MCP servers. That single fact drove the delivery
design. The shim is a tiny MCP server that runs **once per session**, opens an
**outbound** WebSocket to your gateway, and turns pushed events into
`notifications/claude/channel` content the session sees. Outbound-only means no
inbound port on the developer's machine. One shim per session means the session
identity has to be part of every subscription key — which is why the whole
system is keyed on `user_id#session_id`. And an unconfigured shim is **inert**:
no token, no sockets, no timers, no stderr. Opt-in all the way down.

## The identity model

- **You are your token.** A `shk_` token, minted by a self-service portal only
  after GitHub identity verification and an org-membership check. The server
  stores only its SHA-256; you see the plaintext once.
- **A session is `CLAUDE_CODE_SESSION_ID`** — present in the stdio MCP
  environment (verified empirically), and stable across `--resume`.
- **Every key is `user_id#session_id`**, with the `user_id` coming from the
  token, not the client. Session IDs are untrusted; presenting someone else's
  gets you into their namespace only if you also hold their token, so it gets
  you nothing. Offboarding is GitHub org membership: a daily sweep revokes
  departed members — but a validation *error* is always "unknown", never a
  revoke, so an API blip can't nuke everyone's access.

## The self-hosted trust story

shuck is **single-operator, multi-user** — one org, one App installation, one
backend, all data in your infrastructure. Not multi-tenant SaaS. The public
surface is exactly one stateless endpoint that HMAC-verifies GitHub's signature
before it parses a byte; everything else is authenticated or private. Secrets
are env-injected everywhere — no Secrets Manager reads. And the project is
honest about the one boundary it can only narrow, not close: a distilled CI or
review summary is still attacker-influenceable text entering an agent's context,
so it's treated as untrusted external data, exactly like the PR it came from.
(The full analysis is the repo's threat model.)

Every parser of untrusted input is fuzzed, with minimized crashers committed as
regression seeds before the bug is fixed — the log parser, the distiller, the
webhook filter, the portal's cookie codec, and more.

## The engineering narrative: a dead end worth recording

The serverless gateway was supposed to run on AWS App Runner. It can't: App
Runner has no WebSocket support ([apprunner-roadmap#13](https://github.com/aws/apprunner-roadmap/issues/13),
closed not-planned), so an App Runner "gateway" could never accept a shim
connection. Fargate + ALB works but carries a fixed idle cost — against a design
goal of *idle cost near zero*.

The resolution was to re-architect the serverless gateway onto an **API Gateway
WebSocket API** with the gateway logic running as per-invocation Lambda
handlers. The interesting part is that this became the *second* shape of one
gateway rather than a fork: the resident WebSocket server (for Kubernetes) and
the serverless Lambda handlers (for the cheap solo path) share **one set of
DynamoDB stores, one wire protocol, and one deliver contract**. A single shim
serves both through a small **additive protocol superset** — because API Gateway
can't send application WebSocket close codes, the `unauthorized`/`replaced`
verdicts also exist as in-band control frames, and a 5-minute app-level ping
defeats API Gateway's idle timeout. The resident server simply ignores the extra
frames. Workers and shims never know which gateway they're talking to.

The payoff: the serverless target has **no fixed-cost component** — idle rounds
to $0, and the first real charges are API Gateway WS messages and CloudWatch
Logs. The routine reconnects forced by API Gateway's 2-hour connection cap are
invisible, because delivery is write-then-push with buffered replay: the buffer
is the source of truth, the socket is a latency optimisation, and a reconnect
just replays whatever wasn't acked.

## Try it

- **Portable, right now:** `brew install` / the install script, then
  `shuck <pr>` — or wire the MCP server into any agent.
- **The router:** one `terraform apply` to your own AWS account (serverless,
  idle ≈ $0), or the Helm chart for Kubernetes. Both walkthroughs, the
  architecture, and the threat model are in the repo.

Repo: [github.com/justanotherspy/shuck](https://github.com/justanotherspy/shuck)
· Architecture: `docs/ARCHITECTURE.md` · Threat model: `docs/THREAT-MODEL.md` ·
Deploy: `deploy/terraform` and `deploy/helm/shuck`.
