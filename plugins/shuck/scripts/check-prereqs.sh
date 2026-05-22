#!/usr/bin/env bash
# SessionStart hook for the shuck plugin.
#
# Verifies the two things /shuck needs before it can run:
#   1. the `shuck` binary is on PATH
#   2. a GitHub token is available (GITHUB_TOKEN or GH_TOKEN)
#
# When both are present it exits silently. When something is missing it emits a
# warning to the user and adds the same note to Claude's context, without
# blocking the session.
set -euo pipefail

problems=""

if ! command -v shuck >/dev/null 2>&1; then
  problems+="\\n  - the 'shuck' binary is not on PATH (install: go install github.com/justanotherspy/shuck@latest)"
fi

if [ -z "${GITHUB_TOKEN:-}" ] && [ -z "${GH_TOKEN:-}" ]; then
  problems+="\\n  - no GitHub token found (set GITHUB_TOKEN or GH_TOKEN, or pass --token to shuck)"
fi

# All good: stay quiet.
if [ -z "$problems" ]; then
  exit 0
fi

msg="shuck plugin: prerequisites missing before /shuck can run:${problems}"

cat <<JSON
{
  "systemMessage": "${msg}",
  "hookSpecificOutput": {
    "hookEventName": "SessionStart",
    "additionalContext": "${msg}"
  }
}
JSON
