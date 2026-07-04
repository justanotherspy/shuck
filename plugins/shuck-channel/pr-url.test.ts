import { describe, expect, test } from 'bun:test'
import { parsePRURL } from './pr-url.ts'

describe('parsePRURL', () => {
  const valid: [string, string, number][] = [
    ['https://github.com/justanotherspy/shuck/pull/163', 'justanotherspy/shuck', 163],
    ['https://github.com/justanotherspy/shuck/pull/163/files', 'justanotherspy/shuck', 163],
    ['https://github.com/o/r/pull/7?diff=split#discussion_r1', 'o/r', 7],
    ['https://www.github.com/o/r/pull/7', 'o/r', 7],
    ['github.com/o/some.repo-name_x/pull/42', 'o/some.repo-name_x', 42],
    ['justanotherspy/shuck#163', 'justanotherspy/shuck', 163],
    ['  https://github.com/o/r/pull/1  ', 'o/r', 1],
  ]
  test.each(valid)('%s', (input, repo, pr) => {
    expect(parsePRURL(input)).toEqual({ repo, pr })
  })

  const invalid = [
    '',
    'not a url',
    'https://gitlab.com/o/r/pull/1',
    'https://github.com/o/r/issues/1',
    'https://github.com/o/r/pull/0',
    'https://github.com/o/r/pull/abc',
    'https://github.com/o/r',
    'o/r#0',
    'o#1',
    'https://github.com/-bad/r/pull/1',
  ]
  test.each(invalid)('rejects %s', input => {
    expect(() => parsePRURL(input)).toThrow()
  })
})
