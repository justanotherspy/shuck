#!/usr/bin/env bash
# SessionStart hook for the shuck plugin.
#
# 1. Best-effort: make sure the `shuck` binary is installed in the plugin's
#    bin/ (install-shuck.sh downloads the latest signed release on first use).
# 2. Verify a GitHub token is available (GITHUB_TOKEN or GH_TOKEN).
#
# Stays silent when everything is in order. Otherwise it surfaces a note to the
# user and adds the same note to Claude's context, without blocking the session.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"

# install-shuck.sh prints a one-line note to stdout only when it freshly
# downloads the binary; it stays silent if shuck is already available.
install_note=""
if note="$(bash "$here/install-shuck.sh" 2>/dev/null)"; then
  install_note="$note"
fi

infos=""
problems=""

if [ -n "$install_note" ]; then
  infos+="\\n  - ${install_note}"
fi

if ! command -v shuck >/dev/null 2>&1 \
  && [ ! -x "$here/../bin/shuck" ] \
  && [ ! -x "$here/../bin/shuck.exe" ]; then
  problems+="\\n  - the 'shuck' binary could not be installed automatically; install it manually with: go install github.com/justanotherspy/shuck@latest"
fi

if [ -z "${GITHUB_TOKEN:-}" ] && [ -z "${GH_TOKEN:-}" ]; then
  problems+="\\n  - no GitHub token found (set GITHUB_TOKEN or GH_TOKEN, or pass --token to shuck)"
fi

# Nothing to report: stay quiet.
if [ -z "$infos" ] && [ -z "$problems" ]; then
  exit 0
fi

msg="shuck plugin:"
[ -n "$infos" ] && msg+="${infos}"
[ -n "$problems" ] && msg+="\\n  prerequisites still missing before /shuck can run:${problems}"

cat <<JSON
{
  "systemMessage": "${msg}",
  "hookSpecificOutput": {
    "hookEventName": "SessionStart",
    "additionalContext": "${msg}"
  }
}
JSON
