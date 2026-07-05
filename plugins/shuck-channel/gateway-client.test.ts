import { afterEach, describe, expect, test } from 'bun:test'
import type { ServerWebSocket } from 'bun'
import { GatewayClient, type GatewayEvent } from './gateway-client.ts'

// FakeGateway mirrors the wire protocol in internal/gateway/protocol.go just
// far enough to exercise the client: it records inbound frames per
// connection and lets tests push event frames and close with app codes.
class FakeConn {
  frames: Record<string, unknown>[] = []
  constructor(readonly ws: ServerWebSocket<unknown>) {}

  event(ev: Partial<GatewayEvent> & { id: string }): void {
    this.ws.send(
      JSON.stringify({
        type: 'event',
        seq: 1,
        repo: 'o/r',
        pr: 1,
        kind: 'ci_failure',
        summary: 'job failed',
        ...ev,
      }),
    )
  }

  sendRaw(data: string): void {
    this.ws.send(data)
  }

  close(code: number, reason = ''): void {
    this.ws.close(code, reason)
  }

  async frame(i: number): Promise<Record<string, unknown>> {
    await until(() => this.frames.length > i)
    return this.frames[i]!
  }
}

class FakeGateway {
  conns: FakeConn[] = []
  private readonly server: ReturnType<typeof Bun.serve>

  constructor() {
    const gateway = this
    this.server = Bun.serve({
      port: 0,
      hostname: '127.0.0.1',
      fetch(req, server) {
        if (server.upgrade(req, { data: undefined })) return
        return new Response('not a websocket', { status: 400 })
      },
      websocket: {
        open(ws) {
          gateway.conns.push(new FakeConn(ws))
        },
        message(ws, raw) {
          const conn = gateway.conns.find(c => c.ws === ws)
          conn?.frames.push(JSON.parse(String(raw)) as Record<string, unknown>)
        },
      },
    })
  }

  get url(): string {
    return `ws://127.0.0.1:${this.server.port}/ws`
  }

  async conn(i: number): Promise<FakeConn> {
    await until(() => this.conns.length > i)
    return this.conns[i]!
  }

  stop(): void {
    this.server.stop(true)
  }
}

async function until(cond: () => boolean, timeoutMs = 2_000): Promise<void> {
  const deadline = Date.now() + timeoutMs
  while (!cond()) {
    if (Date.now() > deadline) throw new Error('condition not met in time')
    await Bun.sleep(2)
  }
}

const noop = () => {}

type Harness = { gw: FakeGateway; client: GatewayClient; events: GatewayEvent[]; logs: string[] }

let cleanup: (() => void)[] = []
afterEach(() => {
  for (const f of cleanup.splice(0)) f()
})

function harness(opts: Partial<ConstructorParameters<typeof GatewayClient>[0]> = {}): Harness {
  const gw = new FakeGateway()
  const events: GatewayEvent[] = []
  const logs: string[] = []
  const client = new GatewayClient({
    url: gw.url,
    token: 'test-token-not-real',
    sessionID: 'session-1',
    onEvent: ev => {
      events.push(ev)
    },
    log: msg => {
      logs.push(msg)
    },
    baseDelayMs: 5,
    maxDelayMs: 40,
    stableMs: 10_000,
    random: () => 0.99,
    ...opts,
  })
  cleanup.push(() => {
    client.stop()
    gw.stop()
  })
  return { gw, client, events, logs }
}

describe('GatewayClient', () => {
  test('hello is the first frame and carries no last_event_id initially', async () => {
    const { gw, client } = harness()
    client.start()
    const conn = await gw.conn(0)
    const hello = await conn.frame(0)
    expect(hello).toEqual({ type: 'hello', token: 'test-token-not-real', session_id: 'session-1' })
  })

  test('event → onEvent then ack', async () => {
    const { gw, client, events } = harness()
    client.start()
    const conn = await gw.conn(0)
    await conn.frame(0)
    conn.event({ id: 'ev-1', summary: 'make test failed' })
    const ack = await conn.frame(1)
    expect(ack).toEqual({ type: 'ack', id: 'ev-1' })
    expect(events).toHaveLength(1)
    expect(events[0]!.summary).toBe('make test failed')
  })

  test('duplicate event id notifies once but acks every time', async () => {
    const { gw, client, events } = harness()
    client.start()
    const conn = await gw.conn(0)
    await conn.frame(0)
    conn.event({ id: 'ev-1' })
    conn.event({ id: 'ev-1' })
    await conn.frame(2)
    expect(conn.frames.slice(1)).toEqual([
      { type: 'ack', id: 'ev-1' },
      { type: 'ack', id: 'ev-1' },
    ])
    expect(events).toHaveLength(1)
  })

  test('1001 drain → reconnects and resumes with last_event_id', async () => {
    const { gw, client } = harness()
    client.start()
    const first = await gw.conn(0)
    await first.frame(0)
    first.event({ id: 'ev-7' })
    await first.frame(1)
    first.close(1001, 'draining')
    const second = await gw.conn(1)
    const hello = await second.frame(0)
    expect(hello.last_event_id).toBe('ev-7')
  })

  test('failed onEvent is not acked and replays on reconnect', async () => {
    let fail = true
    const events: GatewayEvent[] = []
    const { gw, client } = harness({
      onEvent: ev => {
        if (fail) throw new Error('notification failed')
        events.push(ev)
      },
    })
    client.start()
    const first = await gw.conn(0)
    await first.frame(0)
    first.event({ id: 'ev-1' })
    await Bun.sleep(20)
    expect(first.frames).toHaveLength(1) // hello only, no ack
    fail = false
    first.close(1001)
    const second = await gw.conn(1)
    const hello = await second.frame(0)
    expect(hello.last_event_id).toBeUndefined()
    second.event({ id: 'ev-1' })
    const ack = await second.frame(1)
    expect(ack).toEqual({ type: 'ack', id: 'ev-1' })
    expect(events).toHaveLength(1)
  })

  test('4401 unauthorized → stops permanently', async () => {
    const { gw, client } = harness()
    client.start()
    const conn = await gw.conn(0)
    await conn.frame(0)
    conn.close(4401, 'unauthorized')
    await until(() => client.state === 'stopped')
    await Bun.sleep(30)
    expect(gw.conns).toHaveLength(1)
    expect(client.subscribe('o/r', 1)).rejects.toThrow(/token rejected/)
  })

  test('4409 replaced → stops permanently', async () => {
    const { gw, client } = harness()
    client.start()
    const conn = await gw.conn(0)
    await conn.frame(0)
    conn.close(4409, 'replaced')
    await until(() => client.state === 'stopped')
    await Bun.sleep(30)
    expect(gw.conns).toHaveLength(1)
    expect(client.subscribe('o/r', 1)).rejects.toThrow(/replaced/)
  })

  test('in-band unauthorized frame → stops permanently (serverless gateway)', async () => {
    const { gw, client } = harness()
    client.start()
    const conn = await gw.conn(0)
    await conn.frame(0)
    conn.sendRaw(JSON.stringify({ type: 'unauthorized' }))
    await until(() => client.state === 'stopped')
    await Bun.sleep(30)
    expect(gw.conns).toHaveLength(1)
    expect(client.subscribe('o/r', 1)).rejects.toThrow(/token rejected/)
  })

  test('in-band replaced frame → stops permanently (serverless gateway)', async () => {
    const { gw, client } = harness()
    client.start()
    const conn = await gw.conn(0)
    await conn.frame(0)
    conn.sendRaw(JSON.stringify({ type: 'replaced' }))
    await until(() => client.state === 'stopped')
    await Bun.sleep(30)
    expect(gw.conns).toHaveLength(1)
    expect(client.subscribe('o/r', 1)).rejects.toThrow(/replaced/)
  })

  test('application-level ping keepalive is sent on an interval', async () => {
    const { gw, client } = harness({ appPingIntervalMs: 15 })
    client.start()
    const conn = await gw.conn(0)
    await conn.frame(0)
    const ping = await conn.frame(1)
    expect(ping).toEqual({ type: 'ping' })
    const again = await conn.frame(2)
    expect(again).toEqual({ type: 'ping' })
  })

  test('backoff delays grow and are capped', async () => {
    const { gw, client, logs } = harness()
    client.start()
    for (let i = 0; i < 5; i++) {
      const conn = await gw.conn(i)
      await conn.frame(0)
      conn.close(1006 as number, 'abnormal')
      await until(() => logs.length > i)
    }
    const delays = logs.map(l => Number(/in (\d+)ms/.exec(l)?.[1]))
    // random() = 0.99, base 5ms, cap 40ms: ~5, ~10, ~20, ~40, ~40.
    for (let i = 1; i < delays.length; i++) expect(delays[i]!).toBeGreaterThanOrEqual(delays[i - 1]!)
    expect(delays[3]).toBe(delays[4])
    expect(delays.at(-1)!).toBeLessThan(40)
  })

  test('attempt counter resets after a stable connection', async () => {
    const { gw, client, logs } = harness({ stableMs: 10 })
    client.start()
    for (let i = 0; i < 3; i++) {
      const conn = await gw.conn(i)
      await conn.frame(0)
      if (i < 2) {
        conn.close(1006 as number)
        await until(() => logs.length > i)
      }
    }
    await Bun.sleep(15) // let the third connection become "stable"
    const third = await gw.conn(2)
    third.close(1006 as number)
    await until(() => logs.length > 2)
    const delays = logs.map(l => Number(/in (\d+)ms/.exec(l)?.[1]))
    expect(delays[2]!).toBeLessThanOrEqual(delays[0]!)
  })

  test('subscribe/unsubscribe frame shapes', async () => {
    const { gw, client } = harness()
    client.start()
    const conn = await gw.conn(0)
    await conn.frame(0)
    await client.subscribe('justanotherspy/shuck', 163)
    await client.unsubscribe('justanotherspy/shuck', 163)
    expect(await conn.frame(1)).toEqual({ type: 'subscribe', repo: 'justanotherspy/shuck', pr: 163 })
    expect(await conn.frame(2)).toEqual({ type: 'unsubscribe', repo: 'justanotherspy/shuck', pr: 163 })
  })

  test('subscribe while connecting waits for the socket to open', async () => {
    const { gw, client } = harness()
    client.start()
    const done = client.subscribe('o/r', 5)
    const conn = await gw.conn(0)
    await done
    expect(await conn.frame(1)).toEqual({ type: 'subscribe', repo: 'o/r', pr: 5 })
  })

  test('subscribe while reconnecting fails fast', async () => {
    const { gw, client } = harness({ baseDelayMs: 500, maxDelayMs: 500 })
    client.start()
    const conn = await gw.conn(0)
    await conn.frame(0)
    conn.close(1006 as number)
    await until(() => client.state === 'waiting')
    expect(client.subscribe('o/r', 1)).rejects.toThrow(/reconnecting/)
  })

  test('oversized frames are refused client-side', async () => {
    const { gw, client } = harness()
    client.start()
    const conn = await gw.conn(0)
    await conn.frame(0)
    expect(client.subscribe('o/'.padEnd(40 * 1024, 'x'), 1)).rejects.toThrow(/exceeds/)
  })

  test('malformed and unknown server frames are ignored', async () => {
    const { gw, client, events } = harness()
    client.start()
    const conn = await gw.conn(0)
    await conn.frame(0)
    conn.sendRaw('{nope')
    conn.sendRaw(JSON.stringify({ type: 'mystery' }))
    conn.sendRaw(JSON.stringify({ type: 'event', id: '' })) // invalid event
    conn.event({ id: 'ev-ok' })
    const ack = await conn.frame(1)
    expect(ack).toEqual({ type: 'ack', id: 'ev-ok' })
    expect(events).toHaveLength(1)
  })

  test('liveness watchdog does not kill a healthy connection', async () => {
    const { gw, client } = harness({ pingIntervalMs: 5, pongTimeoutMs: 50 })
    client.start()
    const conn = await gw.conn(0)
    await conn.frame(0)
    await Bun.sleep(40) // several ping cycles; the server auto-pongs
    expect(gw.conns).toHaveLength(1)
    expect(client.state).toBe('open')
  })
})
