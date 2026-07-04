// GatewayClient holds the shim's single outbound WebSocket to the shuck
// gateway and speaks the wire protocol frozen in internal/gateway/protocol.go:
// hello / subscribe / unsubscribe / ack out, event frames in. It owns
// reconnection (full-jitter exponential backoff), the permanent-stop close
// codes (4401 unauthorized, 4409 replaced), event-id dedupe across replays,
// and a client-side liveness watchdog (Bun's non-standard ws.ping()).
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
  random?: () => number
}

export const DEFAULTS = {
  baseDelayMs: 1_000,
  maxDelayMs: 30_000,
  stableMs: 10_000,
  pingIntervalMs: 60_000,
  pongTimeoutMs: 10_000,
}

// Close codes from internal/gateway/protocol.go.
export const CLOSE_UNAUTHORIZED = 4401
export const CLOSE_REPLACED = 4409

// MaxFrameSize mirrors gateway.MaxFrameSize; the gateway drops connections
// that exceed it, so refuse to send oversized frames instead.
export const MAX_FRAME_SIZE = 32 * 1024

const DEDUPE_CAP = 1024

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
      this.halt('token rejected by the gateway (revoked or invalid); fix the token and restart the session')
      return
    }
    if (code === CLOSE_REPLACED) {
      this.halt('replaced by a newer connection for this session (newest wins); this shim will stay disconnected')
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
    if (!isEvent(frame)) return
    if (!this.seen.has(frame.id)) {
      try {
        await this.opts.onEvent(frame)
      } catch (err) {
        // Not acked: the gateway replays it on the next reconnect.
        this.opts.log(`event ${frame.id} delivery failed: ${err instanceof Error ? err.message : err}`)
        return
      }
      this.seen.add(frame.id)
      if (this.seen.size > DEDUPE_CAP) {
        for (const oldest of this.seen) {
          this.seen.delete(oldest)
          break
        }
      }
    }
    // Duplicates (replays past our cursor) are acked too — the ack is what
    // drains the gateway's buffer row.
    try {
      this.send({ type: 'ack', id: frame.id })
      this.lastEventID = frame.id
    } catch {
      // Connection died mid-ack; the event replays and dedupes next time.
    }
  }

  private send(frame: Record<string, unknown>): void {
    const data = JSON.stringify(frame)
    if (data.length > MAX_FRAME_SIZE) throw new Error(`frame exceeds ${MAX_FRAME_SIZE} bytes`)
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

  private stopWatchdog(): void {
    if (this.pingTimer) {
      clearInterval(this.pingTimer)
      this.pingTimer = null
    }
    if (this.pongTimer) {
      clearTimeout(this.pongTimer)
      this.pongTimer = null
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
