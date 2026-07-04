// PR reference parsing for the subscribe/unsubscribe tools.

export type PRRef = { repo: string; pr: number }

// owner: alphanumeric + inner hyphens; repo: alphanumeric . _ -
const OWNER_REPO = /^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?\/[A-Za-z0-9._-]+$/

// parsePRURL accepts a GitHub PR URL (https://github.com/owner/repo/pull/N,
// tolerating a trailing path, query, or fragment) or the owner/repo#N
// shorthand. Anything else throws.
export function parsePRURL(input: string): PRRef {
  const s = input.trim()
  const short = /^([^\s#]+\/[^\s#]+)#(\d+)$/.exec(s)
  if (short) return validated(short[1]!, short[2]!, input)
  let url: URL
  try {
    url = new URL(s.includes('://') ? s : `https://${s}`)
  } catch {
    throw new Error(`not a GitHub PR reference: ${input}`)
  }
  if (url.hostname !== 'github.com' && url.hostname !== 'www.github.com') {
    throw new Error(`not a github.com PR URL: ${input}`)
  }
  const m = /^\/([^/]+)\/([^/]+)\/pull\/(\d+)(?:\/|$)/.exec(url.pathname)
  if (!m) throw new Error(`not a GitHub PR reference: ${input}`)
  return validated(`${m[1]}/${m[2]}`, m[3]!, input)
}

function validated(repo: string, digits: string, input: string): PRRef {
  const pr = Number(digits)
  if (!OWNER_REPO.test(repo) || !Number.isSafeInteger(pr) || pr < 1) {
    throw new Error(`not a GitHub PR reference: ${input}`)
  }
  return { repo, pr }
}
