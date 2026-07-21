// GatewayClient holds the shim's single outbound WebSocket to the shuck
// gateway and speaks the wire protocol of internal/gateway/protocol.go:
// hello / subscribe / unsubscribe / ack / ping out, event frames in. It owns
// reconnection (full-jitter exponential backoff), the permanent stops —
// close codes 4401 unauthorized / 4409 replaced from the resident gateway,
// or the equivalent in-band "unauthorized" / "replaced" control frames from
// the serverless (API Gateway) one, which cannot send app close codes —
// event-id dedupe across replays, an application-level ping keepalive
// (refreshes the serverless gateway's presence rows and defeats API
// Gateway's 10-minute idle timeout; the resident gateway ignores it), and a
// client-side liveness watchdog (Bun's non-standard ws.ping()). One client
// speaks to both gateway flavors; expect the serverless one to force a
// routine reconnect at API Gateway's two-hour connection cap, which the
// backoff + replay path absorbs.
//
// stdout belongs to the MCP transport — nothing here may ever write to it.

export type GatewayEvent = {
  id: string
  seq: number
  repo: string
  pr: number
  kind: string
  summary: string
}

export type State = 'connecting' | 'open' | 'waiting' | 'stopped'

export type Options = {
  url: string
  token: string
  sessionID: string
  // onEvent delivers one deduplicated event; the frame is acked only after
  // it resolves, so a failed delivery is replayed on the next reconnect.
  onEvent: (ev: GatewayEvent) => void | Promise<void>
  // log receives one short line per lifecycle change (default: stderr).
  log?: (msg: string) => void
  // Tunables so tests run in milliseconds. Defaults in DEFAULTS.
  baseDelayMs?: number
  maxDelayMs?: number
  stableMs?: number
  pingIntervalMs?: number
  pongTimeoutMs?: number
  appPingIntervalMs?: number
  random?: () => number
}

export const DEFAULTS = {
  baseDelayMs: 1_000,
  maxDelayMs: 30_000,
  stableMs: 10_000,
  pingIntervalMs: 60_000,
  pongTimeoutMs: 10_000,
  appPingIntervalMs: 300_000,
}

// Close codes from internal/gateway/protocol.go.
export const CLOSE_UNAUTHORIZED = 4401
export const CLOSE_REPLACED = 4409

// The serverless gateway's in-band control frames — API Gateway cannot send
// application close codes, so these carry the same verdicts.
export const FRAME_UNAUTHORIZED = 'unauthorized'
export const FRAME_REPLACED = 'replaced'

const STOP_UNAUTHORIZED =
  'token rejected by the gateway (revoked or invalid); fix the token and restart the session'
const STOP_REPLACED =
  'replaced by a newer connection for this session (newest wins); this shim will stay disconnected'

// MaxFrameSize mirrors gateway.MaxFrameSize; the gateway drops connections
// that exceed it, so refuse to send oversized frames instead.
export const MAX_FRAME_SIZE = 32 * 1024

export const DEDUPE_CAP = 1024

type BunWebSocket = WebSocket & { ping?: () => void; terminate?: () => void }

export class GatewayClient {
  state: State = 'waiting'
  stopReason = ''

  private readonly opts: Required<Omit<Options, 'log' | 'random'>> & { log: (msg: string) => void; random: () => number }
  private ws: BunWebSocket | null = null
  private lastEventID = ''
  private readonly seen = new Set<string>()
  private attempt = 0
  private openedAt = 0
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null
  private pingTimer: ReturnType<typeof setInterval> | null = null
  private pongTimer: ReturnType<typeof setTimeout> | null = null
  private appPingTimer: ReturnType<typeof setInterval> | null = null
  private openWaiters: { resolve: () => void; reject: (err: Error) => void }[] = []

  constructor(opts: Options) {
    this.opts = {
      ...DEFAULTS,
      log: (msg: string) => console.error(`shuck-channel: ${msg}`),
      random: Math.random,
      ...opts,
    }
  }

  // start opens the first connection. Call at most once.
  start(): void {
    this.connect()
  }

  // stop closes the connection permanently (session shutdown).
  stop(): void {
    this.halt('stopped')
    this.ws?.close(1000)
    this.ws = null
  }

  async subscribe(repo: string, pr: number): Promise<void> {
    await this.ensureOpen()
    this.send({ type: 'subscribe', repo, pr })
  }

  async unsubscribe(repo: string, pr: number): Promise<void> {
    await this.ensureOpen()
    this.send({ type: 'unsubscribe', repo, pr })
  }

  private connect(): void {
    this.state = 'connecting'
    const ws = new WebSocket(this.opts.url) as BunWebSocket
    this.ws = ws
    ws.addEventListener('open', () => {
      if (this.ws !== ws) return
      const hello: Record<string, string> = {
        type: 'hello',
        token: this.opts.token,
        session_id: this.opts.sessionID,
      }
      if (this.lastEventID) hello.last_event_id = this.lastEventID
      this.send(hello)
      this.state = 'open'
      this.openedAt = Date.now()
      this.startWatchdog(ws)
      this.startAppPing(ws)
      for (const w of this.openWaiters.splice(0)) w.resolve()
    })
    ws.addEventListener('message', e => {
      if (this.ws === ws) void this.handleMessage(e.data)
    })
    ws.addEventListener('pong' as never, () => {
      if (this.ws === ws && this.pongTimer) {
        clearTimeout(this.pongTimer)
        this.pongTimer = null
      }
    })
    ws.addEventListener('close', e => {
      if (this.ws !== ws) return
      this.ws = null
      this.handleClose(e.code)
    })
    // A failed connection also fires close; the error listener just keeps
    // Bun from reporting an unhandled event.
    ws.addEventListener('error', () => {})
  }

  private handleClose(code: number): void {
    this.stopWatchdog()
    if (this.state === 'stopped') return
    if (code === CLOSE_UNAUTHORIZED) {
      this.halt(STOP_UNAUTHORIZED)
      return
    }
    if (code === CLOSE_REPLACED) {
      this.halt(STOP_REPLACED)
      return
    }
    const wasOpen = this.openedAt > 0
    if (wasOpen && Date.now() - this.openedAt >= this.opts.stableMs) this.attempt = 0
    this.openedAt = 0
    const cap = Math.min(this.opts.maxDelayMs, this.opts.baseDelayMs * 2 ** this.attempt)
    const delay = Math.floor(this.opts.random() * cap)
    this.attempt++
    this.state = 'waiting'
    this.failWaiters()
    this.opts.log(`disconnected (code ${code}); reconnecting in ${delay}ms`)
    this.reconnectTimer = setTimeout(() => this.connect(), delay)
  }

  private halt(reason: string): void {
    this.state = 'stopped'
    this.stopReason = reason
    this.stopWatchdog()
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer)
      this.reconnectTimer = null
    }
    this.failWaiters()
    if (reason !== 'stopped') this.opts.log(reason)
  }

  private async handleMessage(data: unknown): Promise<void> {
    let frame: unknown
    try {
      frame = JSON.parse(String(data))
    } catch {
      return // Malformed frames are ignored; the gateway never sends them.
    }
    const type = frame !== null && typeof frame === 'object' ? (frame as Record<string, unknown>).type : undefined
    if (type === FRAME_UNAUTHORIZED || type === FRAME_REPLACED) {
      // The serverless gateway's permanent-stop verdicts arrive in-band;
      // detach the socket first so its close event can't schedule a
      // reconnect.
      const ws = this.ws
      this.ws = null
      this.halt(type === FRAME_UNAUTHORIZED ? STOP_UNAUTHORIZED : STOP_REPLACED)
      ws?.close(1000)
      return
    }
    if (!isEvent(frame)) return
    const duplicate = this.seen.has(frame.id)
    if (!duplicate) {
      // Mark the id seen BEFORE awaiting onEvent: the serverless gateway's
      // hello replay and a concurrent deliver each drain the whole unacked
      // buffer, so the same id can arrive again while the notification is
      // still in flight — check-then-act across the await would notify twice.
      this.seen.add(frame.id)
      if (this.seen.size > DEDUPE_CAP) {
        for (const oldest of this.seen) {
          this.seen.delete(oldest)
          break
        }
      }
      try {
        await this.opts.onEvent(frame)
      } catch (err) {
        // Not acked, and un-mark the id so the gateway's replay on the next
        // reconnect can retry the notification.
        this.seen.delete(frame.id)
        this.opts.log(`event ${frame.id} delivery failed: ${err instanceof Error ? err.message : err}`)
        return
      }
    }
    // Duplicates (replays past our cursor) are acked too — the ack is what
    // drains the gateway's buffer row — but they never move lastEventID:
    // a late replay of an old event must not rewind the resume cursor.
    try {
      this.send({ type: 'ack', id: frame.id })
      if (!duplicate) this.lastEventID = frame.id
    } catch {
      // Connection died mid-ack; the event replays and dedupes next time.
    }
  }

  private send(frame: Record<string, unknown>): void {
    const data = JSON.stringify(frame)
    // The gateway's cap is bytes on the wire; String.length counts UTF-16
    // code units, which undercounts multibyte content.
    if (Buffer.byteLength(data, 'utf8') > MAX_FRAME_SIZE) throw new Error(`frame exceeds ${MAX_FRAME_SIZE} bytes`)
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) throw new Error('not connected')
    this.ws.send(data)
  }

  // ensureOpen resolves when the socket is open. A connection in progress is
  // awaited briefly (the first tool call races the initial connect); a
  // backoff wait or permanent stop fails fast so the caller can retry or
  // report.
  private ensureOpen(timeoutMs = 2_000): Promise<void> {
    switch (this.state) {
      case 'open':
        return Promise.resolve()
      case 'stopped':
        return Promise.reject(new Error(this.stopReason || 'connection stopped'))
      case 'waiting':
        return Promise.reject(new Error('not connected to the shuck gateway (reconnecting); retry shortly'))
      case 'connecting':
        return new Promise((resolve, reject) => {
          const waiter = {
            resolve: () => {
              clearTimeout(timer)
              resolve()
            },
            reject: (err: Error) => {
              clearTimeout(timer)
              reject(err)
            },
          }
          const timer = setTimeout(() => {
            this.openWaiters = this.openWaiters.filter(w => w !== waiter)
            reject(new Error('timed out connecting to the shuck gateway; retry shortly'))
          }, timeoutMs)
          this.openWaiters.push(waiter)
        })
    }
  }

  private failWaiters(): void {
    const err = new Error(
      this.state === 'stopped'
        ? this.stopReason || 'connection stopped'
        : 'not connected to the shuck gateway (reconnecting); retry shortly',
    )
    for (const w of this.openWaiters.splice(0)) w.reject(err)
  }

  // The gateway pings every ~30s but the standard WS API auto-pongs
  // invisibly, so a half-open link is undetectable from reads alone. Bun
  // extends the client with ping()/terminate(): probe on an interval and
  // treat a missing pong as a dead connection. No-op when unsupported.
  private startWatchdog(ws: BunWebSocket): void {
    if (typeof ws.ping !== 'function') return
    this.pingTimer = setInterval(() => {
      if (this.ws !== ws || ws.readyState !== WebSocket.OPEN) return
      ws.ping?.()
      this.pongTimer ??= setTimeout(() => {
        this.pongTimer = null
        this.opts.log('liveness ping timed out; dropping connection')
        ws.terminate?.()
      }, this.opts.pongTimeoutMs)
    }, this.opts.pingIntervalMs)
  }

  // The application-level ping is protocol traffic, not a liveness probe:
  // it resets API Gateway's idle timer and refreshes the serverless
  // gateway's durable presence row. The resident gateway ignores it.
  private startAppPing(ws: BunWebSocket): void {
    this.appPingTimer = setInterval(() => {
      if (this.ws !== ws || ws.readyState !== WebSocket.OPEN) return
      try {
        this.send({ type: 'ping' })
      } catch {
        // Connection died mid-ping; the close handler reconnects.
      }
    }, this.opts.appPingIntervalMs)
  }

  private stopWatchdog(): void {
    if (this.pingTimer) {
      clearInterval(this.pingTimer)
      this.pingTimer = null
    }
    if (this.pongTimer) {
      clearTimeout(this.pongTimer)
      this.pongTimer = null
    }
    if (this.appPingTimer) {
      clearInterval(this.appPingTimer)
      this.appPingTimer = null
    }
  }
}

function isEvent(frame: unknown): frame is GatewayEvent & { type: 'event' } {
  if (frame === null || typeof frame !== 'object') return false
  const f = frame as Record<string, unknown>
  return (
    f.type === 'event' &&
    typeof f.id === 'string' &&
    f.id !== '' &&
    typeof f.seq === 'number' &&
    typeof f.repo === 'string' &&
    typeof f.pr === 'number' &&
    typeof f.kind === 'string' &&
    typeof f.summary === 'string'
  )
}
