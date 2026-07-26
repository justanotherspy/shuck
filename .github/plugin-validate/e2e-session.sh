#!/usr/bin/env bash
# Start a real interactive Claude Code session against THIS checkout's plugin and
# report whether the plugin monitor actually started.
#
# Why this exists: nothing else proves it. `claude plugin validate` reads the
# manifest, `plugin details` lists skills and hooks but does not enumerate
# monitors at all (they are an experimental component), and every unit test in
# this repo stubs the harness away. A monitor that never starts fails silently —
# it has no exit code anybody sees and no output but the notifications it does
# not send — so the only honest check is to run one and look for the process.
#
# This is deliberately NOT wired into CI: it needs an authenticated Claude Code,
# a terminal, and about two minutes. Run it by hand when you change anything
# under plugins/shuck/monitors, plugins/shuck/scripts, or the stream itself.
#
# Usage: .github/plugin-validate/e2e-session.sh [checkout-dir]
set -uo pipefail

ROOT="${1:-$(git rev-parse --show-toplevel)}"
ROOT="$(cd "$ROOT" && pwd)"
CLI="$ROOT/.github/plugin-validate/node_modules/.bin/claude"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

[ -x "$CLI" ] || {
  echo "error: $CLI is missing — run: (cd $ROOT/.github/plugin-validate && npm ci)" >&2
  exit 2
}
[ -x "$ROOT/bin/shuck" ] || {
  echo "error: $ROOT/bin/shuck is missing — run: make build" >&2
  exit 2
}

# The plugin has to come from this checkout, not from the published marketplace,
# or the run proves nothing about the code in front of you.
#
# NOTE the name: a directory marketplace is keyed by the "name" in its own
# .claude-plugin/marketplace.json — here "shuck" — NOT by the key used under
# extraKnownMarketplaces. Enabling "shuck@<settings-key>" silently matches
# nothing, the plugin never loads, and the session looks completely normal while
# testing none of your changes. That mistake costs a confusing hour.
cat >"$ROOT/.claude/settings.local.json" <<JSON
{
  "extraKnownMarketplaces": {
    "shuck-local": {
      "source": { "source": "directory", "path": "${ROOT}" }
    }
  },
  "enabledPlugins": {
    "shuck@shuck": true,
    "shuck@justanotherspy": false
  }
}
JSON

# Look for the monitor while the session is up. Its absence is the failure this
# script exists to catch, so the poll outlives the session's start-up.
(
  for _ in $(seq 1 75); do
    p="$(pgrep -af 'monitor stream' 2>/dev/null | grep -v pgrep | head -1)"
    [ -n "$p" ] && { echo "$p" >"$WORK/seen"; break; }
    sleep 1
  done
) &
poller=$!

# A pty is required: plugin monitors run only in interactive sessions, so a
# `claude -p` run would report a clean pass having started nothing. The
# CLAUDE_CODE_* markers are cleared so a session launched from inside another
# Claude Code session is not treated as a child of it. The keystrokes answer the
# first-run folder-trust prompt and then quit with two interrupts.
cd "$ROOT" || exit 2
{ sleep 2; printf '1\r'; sleep 45; printf '\003'; sleep 1; printf '\003'; sleep 2; } |
  PATH="$ROOT/bin:$PATH" timeout 90 \
    env -u CLAUDE_CODE_CHILD_SESSION -u CLAUDE_CODE_SESSION_ID -u CLAUDECODE \
        -u CLAUDE_CODE_ENTRYPOINT -u CLAUDE_CODE_AGENT -u CLAUDE_CODE_BRIDGE_SESSION_ID \
    script -qec "$CLI 'reply with the single word READY and nothing else'" /dev/null \
    >"$WORK/session.log" 2>&1
wait "$poller" 2>/dev/null

if [ -s "$WORK/seen" ]; then
  echo "✔ plugin monitor started: $(cat "$WORK/seen")"
  exit 0
fi

echo "✘ the plugin monitor never started." >&2
echo "  Check, in order: that .claude/settings.local.json enables shuck@shuck;" >&2
echo "  that plugins/shuck/monitors/monitors.json is valid" >&2
echo "  (.github/plugin-validate/validate-monitors.sh); and that shuck is on PATH," >&2
echo "  since monitors.json now execs the binary directly." >&2
echo "  Session transcript tail:" >&2
tail -20 "$WORK/session.log" >&2
exit 1
