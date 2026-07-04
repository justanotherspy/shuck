// Channel configuration. The shim is strictly opt-in: when this resolves to
// null the server must behave as if the channel feature does not exist — no
// sockets, no timers, no stderr.

import { readFileSync } from 'fs'
import { homedir } from 'os'
import { join } from 'path'

export type Config = { gatewayURL: string; token: string }

export type Resolved = { config: Config | null; warnings: string[] }

export function defaultConfigPath(): string {
  return join(homedir(), '.config', 'shuck', 'channel.json')
}

// resolveConfig reads SHUCK_CHANNEL_GATEWAY_URL / SHUCK_CHANNEL_TOKEN with a
// per-field fallback to the config file ({"gateway_url", "token"}). Both
// fields must resolve, else config is null. A missing file is silent; a
// present-but-malformed file or gateway URL is a configuration *attempt*, so
// it warns (the caller prints warnings to stderr only).
export function resolveConfig(
  env: Record<string, string | undefined> = process.env,
  path: string = defaultConfigPath(),
): Resolved {
  const warnings: string[] = []
  let file: Record<string, unknown> = {}
  let raw: string | undefined
  try {
    raw = readFileSync(path, 'utf8')
  } catch {
    // No config file: nothing to report.
  }
  if (raw !== undefined) {
    try {
      const parsed: unknown = JSON.parse(raw)
      if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('not an object')
      file = parsed as Record<string, unknown>
    } catch {
      warnings.push(`shuck-channel: ignoring malformed config file ${path}`)
    }
  }
  const rawURL = pick(env.SHUCK_CHANNEL_GATEWAY_URL, str(file.gateway_url))
  const token = pick(env.SHUCK_CHANNEL_TOKEN, str(file.token))
  if (!rawURL || !token) return { config: null, warnings }
  const gatewayURL = normalizeGatewayURL(rawURL)
  if (!gatewayURL) {
    warnings.push(`shuck-channel: invalid gateway URL ${JSON.stringify(rawURL)}; channel disabled`)
    return { config: null, warnings }
  }
  return { config: { gatewayURL, token }, warnings }
}

// normalizeGatewayURL maps an http(s) or ws(s) base URL to the gateway's WS
// endpoint, appending the /ws path when the base has none.
export function normalizeGatewayURL(raw: string): string | null {
  let url: URL
  try {
    url = new URL(raw)
  } catch {
    return null
  }
  const scheme: Record<string, string> = { 'http:': 'ws:', 'https:': 'wss:', 'ws:': 'ws:', 'wss:': 'wss:' }
  const proto = scheme[url.protocol]
  if (!proto) return null
  url.protocol = proto
  if (url.pathname === '' || url.pathname === '/') url.pathname = '/ws'
  return url.toString()
}

function pick(...vals: (string | undefined)[]): string | undefined {
  for (const v of vals) {
    const t = v?.trim()
    if (t) return t
  }
  return undefined
}

function str(v: unknown): string | undefined {
  return typeof v === 'string' ? v : undefined
}
