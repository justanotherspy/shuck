#!/usr/bin/env bun
/**
 * shuck-channel: the channel shim between a Claude Code session and a
 * self-hosted shuck gateway (docs/V2.md). A stdio MCP server that Claude
 * Code spawns per session; it holds one outbound WebSocket to the gateway
 * and surfaces gateway events as notifications/claude/channel.
 *
 * Strictly opt-in: with no token/gateway configured this process opens no
 * sockets, starts no timers, and writes nothing to stderr. stdout is the
 * MCP transport — never write to it directly.
 */

import { Server } from '@modelcontextprotocol/sdk/server/index.js'
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js'
import { CallToolRequestSchema, ListToolsRequestSchema } from '@modelcontextprotocol/sdk/types.js'
import { GatewayClient } from './gateway-client.ts'
import { resolveConfig } from './config.ts'
import { parsePRURL } from './pr-url.ts'

const { config, warnings } = resolveConfig()
for (const w of warnings) console.error(w)

const INSTRUCTIONS = `This session has a channel to a self-hosted shuck gateway that pushes GitHub events for subscribed pull requests — no polling needed. After shuck_subscribe(pr_url), events for that PR arrive as <channel> notifications with meta {repo, pr, event, event_id}.

Event kinds and how to act:
- ci_failure: a workflow run for the PR failed or was cancelled. The content is the distilled failing-step summary (the same format as shuck's inspect_logs). Read the errors and fix them.
- pr_closed: the PR was merged or closed; the gateway removes its subscriptions automatically.
- review_comment / review: reviewer feedback on the PR.

A long summary may end with "[summary truncated — full logs: s3://…/raw/<repo>/<run_id>/]". That S3 prefix in the operator's bucket holds the whole raw job logs; fetch them with AWS credentials if the excerpt is not enough, or use the pull-based shuck tools (inspect_logs) on the same PR.

Subscriptions are per-session and survive shim reconnects on the gateway side; use shuck_unsubscribe(pr_url) when a PR no longer matters. This channel is independent of the shuck plugin's pull-based MCP tools — both work, together or alone.`

const mcp = new Server(
  { name: 'shuck-channel', version: '0.1.0' },
  {
    capabilities: { tools: {}, experimental: { 'claude/channel': {} } },
    instructions: INSTRUCTIONS,
  },
)

const PR_URL_SCHEMA = {
  type: 'object',
  properties: {
    pr_url: {
      type: 'string',
      description: 'GitHub pull request URL (https://github.com/owner/repo/pull/123) or owner/repo#123',
    },
  },
  required: ['pr_url'],
} as const

mcp.setRequestHandler(ListToolsRequestSchema, async () => ({
  tools: [
    {
      name: 'shuck_subscribe',
      description:
        'Subscribe this session to a pull request on the shuck gateway: CI failures (distilled logs), PR close, and review events arrive as channel notifications.',
      inputSchema: PR_URL_SCHEMA,
    },
    {
      name: 'shuck_unsubscribe',
      description: "Remove this session's shuck gateway subscription for a pull request.",
      inputSchema: PR_URL_SCHEMA,
    },
  ],
}))

let client: GatewayClient | null = null

mcp.setRequestHandler(CallToolRequestSchema, async req => {
  const fail = (text: string) => ({ content: [{ type: 'text' as const, text }], isError: true })
  const name = req.params.name
  if (name !== 'shuck_subscribe' && name !== 'shuck_unsubscribe') return fail(`unknown tool: ${name}`)
  if (!client) {
    return fail(
      'shuck channel not configured: set SHUCK_CHANNEL_GATEWAY_URL and SHUCK_CHANNEL_TOKEN (or ~/.config/shuck/channel.json with {"gateway_url", "token"}). Tokens are minted by your shuck gateway operator.',
    )
  }
  let ref
  try {
    ref = parsePRURL(String((req.params.arguments ?? {}).pr_url ?? ''))
  } catch (err) {
    return fail(err instanceof Error ? err.message : String(err))
  }
  try {
    if (name === 'shuck_subscribe') {
      await client.subscribe(ref.repo, ref.pr)
      return {
        content: [
          {
            type: 'text' as const,
            text: `subscribed to ${ref.repo}#${ref.pr} — CI failures, reviews, and close events for this PR will arrive as channel notifications`,
          },
        ],
      }
    }
    await client.unsubscribe(ref.repo, ref.pr)
    return { content: [{ type: 'text' as const, text: `unsubscribed from ${ref.repo}#${ref.pr}` }] }
  } catch (err) {
    return fail(err instanceof Error ? err.message : String(err))
  }
})

await mcp.connect(new StdioServerTransport())

if (config) {
  let sessionID = process.env.CLAUDE_CODE_SESSION_ID
  if (!sessionID) {
    sessionID = crypto.randomUUID()
    console.error(
      'shuck-channel: CLAUDE_CODE_SESSION_ID is not set; using an ephemeral session id (subscriptions will not survive --resume)',
    )
  }
  client = new GatewayClient({
    url: config.gatewayURL,
    token: config.token,
    sessionID,
    onEvent: async ev => {
      await mcp.notification({
        method: 'notifications/claude/channel',
        params: {
          content: ev.summary,
          meta: { repo: ev.repo, pr: ev.pr, event: ev.kind, event_id: ev.id },
        },
      })
    },
  })
  client.start()
}

// When Claude Code closes the session, stop the client so its reconnect
// timers don't keep the process alive.
mcp.onclose = () => {
  client?.stop()
  process.exit(0)
}
