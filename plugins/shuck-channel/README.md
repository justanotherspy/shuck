# shuck-channel

The channel shim for [shuck's self-hosted event router](../../docs/V2.md)
(JUS-84/JUS-89): a stdio MCP server that Claude Code spawns per session. It
holds one outbound WebSocket to your shuck gateway and pushes distilled
CI-failure / PR events into the running session as
`notifications/claude/channel` notifications — no polling, no tokens burned
on `gh` calls.

This plugin is **separate from and independent of** the `shuck` plugin.
Installing one never installs, requires, or enables the other, and a session
can use both: the channel pushes events in; the pull-based `inspect_*` tools
keep working as the fallback.

## Requirements

- [Bun](https://bun.sh) (the server is a Bun script)
- Claude Code ≥ 2.1.80 with [channels](https://code.claude.com/docs/en/channels)
  available (research preview; Team/Enterprise orgs need `channelsEnabled`)
- An operator-deployed shuck gateway and a minted token (`docs/V2.md`)

## Configuration

Two values, from environment variables or a config file (env wins per field):

| Env | `~/.config/shuck/channel.json` | Meaning |
| --- | --- | --- |
| `SHUCK_CHANNEL_GATEWAY_URL` | `gateway_url` | Gateway base URL (`https://` or `wss://`; `/ws` is appended when the path is bare) |
| `SHUCK_CHANNEL_TOKEN` | `token` | Your per-user shuck token, minted by the gateway operator |

**Unconfigured means off.** With either value missing the shim opens no
sockets, starts no timers, and prints no warnings; the tools return an
explanatory error if called. There is no local state beyond this
configuration — subscriptions, buffering, and replay all live on the gateway.

## Install

```text
/plugin marketplace add justanotherspy/shuck
/plugin install shuck-channel@shuck
```

Then restart with the channel named for the session (channels are per-session
opt-in):

```bash
claude --channels plugin:shuck-channel@shuck
```

## Tools

| Tool | Effect |
| --- | --- |
| `shuck_subscribe(pr_url)` | Subscribe this session to a PR (`https://github.com/owner/repo/pull/123` or `owner/repo#123`) |
| `shuck_unsubscribe(pr_url)` | Remove the subscription |

Events arrive as `<channel>` notifications with
`meta {repo, pr, event, event_id}`; kinds are `ci_failure` (distilled failing
step logs), `pr_closed` (subscriptions auto-removed), and
`review_comment` / `review`. A long summary may end with
`[summary truncated — full logs: s3://…/raw/<repo>/<run_id>/]` — the full raw
job logs at that prefix in the operator's bucket.

## Protocol notes

The shim speaks the wire protocol of `internal/gateway/protocol.go`:
`hello {token, session_id, last_event_id?}` / `subscribe` / `unsubscribe` /
`ack {id}` / `ping` out, `event {id, seq, repo, pr, kind, summary}` in.
Session identity is `CLAUDE_CODE_SESSION_ID` (present in the stdio MCP
environment; `--resume` keeps it, so subscriptions and buffered events
reattach). The client reconnects with full-jitter exponential backoff,
resumes from the last acked event id, dedupes replayed event ids, and stops
permanently on close codes 4401 (token rejected) and 4409 (replaced by a
newer connection for the same session). Subscribe/unsubscribe frames get no
gateway reply by design; the tools report success once the frame is written
on an authenticated connection.

One shim speaks to both gateway deployments. Against the serverless gateway
(API Gateway WebSockets, the JUS-92 Terraform target), the permanent-stop
verdicts arrive as in-band `{"type":"unauthorized"}` / `{"type":"replaced"}`
control frames instead of close codes (API Gateway cannot send them), the
periodic `ping` frame keeps the connection inside API Gateway's 10-minute
idle timeout and refreshes the gateway's durable presence row, and API
Gateway's two-hour connection cap forces a routine reconnect that the
backoff + replay path absorbs. The resident gateway ignores `ping` and never
sends control frames, so no configuration is needed either way.

## Development

```bash
cd plugins/shuck-channel
bun install --frozen-lockfile
bun run typecheck
bun test
```

The tests run the client against an in-process fake gateway that mirrors
`protocol.go`, and the smoke test drives the real server over stdio exactly
as Claude Code does (including asserting that an unconfigured shim writes
nothing to stderr).

To run a development copy inside Claude Code (custom channels aren't on the
research-preview allowlist, hence the dev flag — see the
[channels reference](https://code.claude.com/docs/en/channels-reference)):

```bash
claude --plugin-dir ./plugins/shuck-channel \
  --channels plugin:shuck-channel \
  --dangerously-load-development-channels
```

### Acceptance loop against a local gateway

`cmd/shuck-gateway` needs DynamoDB (e.g. [DynamoDB Local](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/DynamoDBLocal.html))
with the three tables from `docs/V2.md` § JUS-88. Seed a token row keyed on
the hex SHA-256 of a raw token (`gateway.HashToken`), export
`SHUCK_CHANNEL_GATEWAY_URL=ws://localhost:8080/ws` and
`SHUCK_CHANNEL_TOKEN=<raw token>`, then:

1. **Isolation** — three sessions (two from the same parent directory),
   subscribe each to a distinct PR, deliver events with `curl`: each event
   reaches only its session.
2. **Resume** — kill a session, `claude --resume` it: the same
   `CLAUDE_CODE_SESSION_ID` reattaches and unacked buffered events replay.
3. **Deliver** — `curl -X POST localhost:8080/internal/deliver -H "X-Shuck-Deliver-Secret: $SHUCK_DELIVER_SECRET" -d '{"schema":1,"event_id":"e1","kind":"ci_failure","repo":"o/r","pr":1,"summary":"boom"}'`
   → the `<channel>` event appears in the subscribed session and Claude
   reacts to it.
4. **Gateway restart** — restart `shuck-gateway`: the shim reconnects unaided
   (1001 drain → backoff) and resumes from its last acked event.

## Supply chain

Dependencies are exact-pinned in `package.json` and locked in the committed
`bun.lock`; CI installs with `--frozen-lockfile` and Dependabot watches the
directory. The plugin ships as source from this repository's marketplace —
there are no separately published artefacts.
