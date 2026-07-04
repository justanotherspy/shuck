import { describe, expect, test } from 'bun:test'
import { mkdtempSync, writeFileSync } from 'fs'
import { tmpdir } from 'os'
import { join } from 'path'
import { normalizeGatewayURL, resolveConfig } from './config.ts'

// Obviously-fake credentials only: this file is scanned by secret scanners.
const URL_ENV = { SHUCK_CHANNEL_GATEWAY_URL: 'wss://gw.example.test/ws' }
const TOKEN_ENV = { SHUCK_CHANNEL_TOKEN: 'test-token-not-real' }

function tempConfig(content?: string): string {
  const dir = mkdtempSync(join(tmpdir(), 'shuck-channel-test-'))
  const path = join(dir, 'channel.json')
  if (content !== undefined) writeFileSync(path, content)
  return path
}

describe('resolveConfig', () => {
  test('unconfigured: no env, no file → null, no warnings', () => {
    const { config, warnings } = resolveConfig({}, tempConfig())
    expect(config).toBeNull()
    expect(warnings).toEqual([])
  })

  test('env only', () => {
    const { config, warnings } = resolveConfig({ ...URL_ENV, ...TOKEN_ENV }, tempConfig())
    expect(config).toEqual({ gatewayURL: 'wss://gw.example.test/ws', token: 'test-token-not-real' })
    expect(warnings).toEqual([])
  })

  test('file only', () => {
    const path = tempConfig(JSON.stringify({ gateway_url: 'https://gw.example.test', token: 'file-token-not-real' }))
    const { config } = resolveConfig({}, path)
    expect(config).toEqual({ gatewayURL: 'wss://gw.example.test/ws', token: 'file-token-not-real' })
  })

  test('per-field precedence: env wins, file fills gaps', () => {
    const path = tempConfig(JSON.stringify({ gateway_url: 'wss://file.example.test/ws', token: 'file-token-not-real' }))
    const { config } = resolveConfig(URL_ENV, path)
    expect(config).toEqual({ gatewayURL: 'wss://gw.example.test/ws', token: 'file-token-not-real' })
  })

  test('partial config → null (token without URL)', () => {
    expect(resolveConfig(TOKEN_ENV, tempConfig()).config).toBeNull()
    expect(resolveConfig(URL_ENV, tempConfig()).config).toBeNull()
  })

  test('empty/whitespace env values count as unset', () => {
    const { config } = resolveConfig({ SHUCK_CHANNEL_GATEWAY_URL: '  ', SHUCK_CHANNEL_TOKEN: '' }, tempConfig())
    expect(config).toBeNull()
  })

  test('malformed file → warning, treated absent', () => {
    const { config, warnings } = resolveConfig({}, tempConfig('{nope'))
    expect(config).toBeNull()
    expect(warnings).toHaveLength(1)
  })

  test('non-object file → warning', () => {
    const { warnings } = resolveConfig({}, tempConfig('[1,2]'))
    expect(warnings).toHaveLength(1)
  })

  test('invalid gateway URL → warning + null', () => {
    const { config, warnings } = resolveConfig({ SHUCK_CHANNEL_GATEWAY_URL: '::nope::', ...TOKEN_ENV }, tempConfig())
    expect(config).toBeNull()
    expect(warnings).toHaveLength(1)
  })

  test('non-string file fields are ignored', () => {
    const path = tempConfig(JSON.stringify({ gateway_url: 42, token: true }))
    expect(resolveConfig({}, path).config).toBeNull()
  })
})

describe('normalizeGatewayURL', () => {
  test.each([
    ['https://gw.example.test', 'wss://gw.example.test/ws'],
    ['http://localhost:8080', 'ws://localhost:8080/ws'],
    ['wss://gw.example.test/', 'wss://gw.example.test/ws'],
    ['wss://gw.example.test/custom/ws', 'wss://gw.example.test/custom/ws'],
  ])('%s → %s', (input, want) => {
    expect(normalizeGatewayURL(input)).toBe(want)
  })

  test.each(['ftp://x.example.test', 'not a url'])('rejects %s', input => {
    expect(normalizeGatewayURL(input)).toBeNull()
  })
})
