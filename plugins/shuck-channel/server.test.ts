import { afterEach, describe, expect, test } from 'bun:test'
import { Client } from '@modelcontextprotocol/sdk/client/index.js'
import { StdioClientTransport } from '@modelcontextprotocol/sdk/client/stdio.js'
import type { ServerWebSocket } from 'bun'
import { mkdtempSync } from 'fs'
import { tmpdir } from 'os'
import { join } from 'path'

// Spawns the real server over stdio, exactly as Claude Code would.
async function spawnServer(extraEnv: Record<string, string> = {}) {
  const transport = new StdioClientTransport({
    command: 'bun',
    args: ['server.ts'],
    cwd: import.meta.dir,
    // A bare env (fresh HOME, no SHUCK_* vars) is the unconfigured case.
    env: { PATH: process.env.PATH ?? '', HOME: mkdtempSync(join(tmpdir(), 'shuck-channel-home-')), ...extraEnv },
    stderr: 'pipe',
  })
  let stderr = ''
  transport.stderr?.on('data', chunk => {
    stderr += String(chunk)
  })
  const client = new Client({ name: 'smoke', version: '0.0.0' }, { capabilities: {} })
  await client.connect(transport)
  return { client, stderr: () => stderr }
}

let closers: (() => Promise<void> | void)[] = []
afterEach(async () => {
  for (const f of closers.splice(0)) await f()
})

describe('server (stdio smoke)', () => {
  test('unconfigured: capabilities + tools present, tool errors, zero stderr', async () => {
    const { client, stderr } = await spawnServer()
    closers.push(() => client.close())

    const caps = client.getServerCapabilities()
    expect(caps?.experimental?.['claude/channel']).toBeDefined()
    expect(client.getInstructions()).toContain('summary truncated')

    const tools = await client.listTools()
    expect(tools.tools.map(t => t.name).sort()).toEqual(['shuck_subscribe', 'shuck_unsubscribe'])

    const res = await client.callTool({
      name: 'shuck_subscribe',
      arguments: { pr_url: 'https://github.com/o/r/pull/1' },
    })
    expect(res.isError).toBe(true)
    expect(JSON.stringify(res.content)).toContain('not configured')

    const bad = await client.callTool({ name: 'shuck_subscribe', arguments: { pr_url: 'nope' } })
    expect(bad.isError).toBe(true)

    // The zero-noise rule, end to end: an unconfigured shim says nothing.
    await client.close()
    expect(stderr()).toBe('')
  })

  test('configured: subscribe reaches the gateway and events surface as channel notifications', async () => {
    // Minimal fake gateway (protocol.go wire shapes).
    const conns: { ws: ServerWebSocket<unknown>; frames: Record<string, unknown>[] }[] = []
    const gw = Bun.serve({
      port: 0,
      hostname: '127.0.0.1',
      fetch(req, server) {
        if (server.upgrade(req)) return
        return new Response('no', { status: 400 })
      },
      websocket: {
        open(ws) {
          conns.push({ ws, frames: [] })
        },
        message(ws, raw) {
          conns.find(c => c.ws === ws)?.frames.push(JSON.parse(String(raw)) as Record<string, unknown>)
        },
      },
    })
    closers.push(() => gw.stop(true))

    const { client, stderr } = await spawnServer({
      SHUCK_CHANNEL_GATEWAY_URL: `ws://127.0.0.1:${gw.port}/ws`,
      SHUCK_CHANNEL_TOKEN: 'test-token-not-real',
      CLAUDE_CODE_SESSION_ID: 'session-under-test',
    })
    closers.push(() => client.close())

    const notifications: Record<string, unknown>[] = []
    client.fallbackNotificationHandler = async n => {
      if (n.method === 'notifications/claude/channel') notifications.push(n.params as Record<string, unknown>)
    }

    const until = async (cond: () => boolean) => {
      const deadline = Date.now() + 3_000
      while (!cond()) {
        if (Date.now() > deadline) throw new Error('condition not met in time')
        await Bun.sleep(5)
      }
    }

    await until(() => conns.length === 1 && conns[0]!.frames.length >= 1)
    expect(conns[0]!.frames[0]).toEqual({
      type: 'hello',
      token: 'test-token-not-real',
      session_id: 'session-under-test',
    })

    const res = await client.callTool({
      name: 'shuck_subscribe',
      arguments: { pr_url: 'https://github.com/justanotherspy/shuck/pull/163' },
    })
    expect(res.isError).toBeUndefined()
    await until(() => conns[0]!.frames.length >= 2)
    expect(conns[0]!.frames[1]).toEqual({ type: 'subscribe', repo: 'justanotherspy/shuck', pr: 163 })

    conns[0]!.ws.send(
      JSON.stringify({
        type: 'event',
        id: 'ev-1',
        seq: 1,
        repo: 'justanotherspy/shuck',
        pr: 163,
        kind: 'ci_failure',
        summary: 'test job failed: TestX',
      }),
    )
    await until(() => notifications.length === 1 && conns[0]!.frames.length >= 3)
    expect(notifications[0]).toEqual({
      content: 'test job failed: TestX',
      meta: { repo: 'justanotherspy/shuck', pr: 163, event: 'ci_failure', event_id: 'ev-1' },
    })
    expect(conns[0]!.frames[2]).toEqual({ type: 'ack', id: 'ev-1' })

    // Configured + healthy: still nothing on stderr.
    expect(stderr()).toBe('')
  })

  test('configured without CLAUDE_CODE_SESSION_ID: one stderr warning, ephemeral UUID session id', async () => {
    const conns: { ws: ServerWebSocket<unknown>; frames: Record<string, unknown>[] }[] = []
    const gw = Bun.serve({
      port: 0,
      hostname: '127.0.0.1',
      fetch(req, server) {
        if (server.upgrade(req)) return
        return new Response('no', { status: 400 })
      },
      websocket: {
        open(ws) {
          conns.push({ ws, frames: [] })
        },
        message(ws, raw) {
          conns.find(c => c.ws === ws)?.frames.push(JSON.parse(String(raw)) as Record<string, unknown>)
        },
      },
    })
    closers.push(() => gw.stop(true))

    const { client, stderr } = await spawnServer({
      SHUCK_CHANNEL_GATEWAY_URL: `ws://127.0.0.1:${gw.port}/ws`,
      SHUCK_CHANNEL_TOKEN: 'test-token-not-real',
      // No CLAUDE_CODE_SESSION_ID: the shim must fall back to a UUID.
    })
    closers.push(() => client.close())

    const until = async (cond: () => boolean) => {
      const deadline = Date.now() + 3_000
      while (!cond()) {
        if (Date.now() > deadline) throw new Error('condition not met in time')
        await Bun.sleep(5)
      }
    }

    await until(() => conns.length === 1 && conns[0]!.frames.length >= 1)
    const hello = conns[0]!.frames[0]!
    expect(hello.type).toBe('hello')
    expect(String(hello.session_id)).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/)

    await until(() => stderr().includes('ephemeral session id'))
    const lines = stderr().split('\n').filter(l => l.trim() !== '')
    expect(lines).toHaveLength(1) // exactly one warning, nothing else
    expect(lines[0]).toContain('CLAUDE_CODE_SESSION_ID is not set')
  })
})
