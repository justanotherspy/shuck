#!/usr/bin/env bash
# SessionStart hook for the shuck plugin.
#
# The plugin does not install shuck; the user installs it themselves (see the
# README) and keeps it current with `shuck upgrade`. This hook only verifies the
# prerequisites the plugin needs and reports any that are missing, without
# blocking the session or touching the network:
#
#   1. the `shuck` binary is on PATH (the MCP server and /shuck skill run it);
#   2. it is new enough to expose the MCP server (the `mcp` subcommand);
#   3. a GitHub token is available (GITHUB_TOKEN or GH_TOKEN).
#
# Stays silent when everything is in order.
set -euo pipefail

# First release whose binary speaks the MCP protocol (`shuck mcp`). Bump this
# when a future release changes the minimum the plugin relies on.
MIN_VERSION="0.3.0"

problems=""

if ! command -v shuck >/dev/null 2>&1; then
  problems+="\\n  - the 'shuck' binary is not on your PATH. Install it, then reload:"
  problems+="\\n      curl -fsSL https://raw.githubusercontent.com/justanotherspy/shuck/main/install.sh | bash"
  problems+="\\n      (or: go install github.com/justanotherspy/shuck@latest)"
else
  # `shuck version` prints 'shuck vX.Y.Z' on its first line and never hits the
  # network, so this check stays offline. Compare only the MAJOR.MINOR.PATCH
  # core (dropping any -prerelease/+build suffix, e.g. a -dirty dev build), and
  # trust anything that is not a plain semver. NOTE: the sort uses a real
  # newline ('\n'), unlike the '\\n' used to build the JSON message below.
  cur="$(shuck version 2>/dev/null | awk 'NR==1{print $2}' | sed 's/^v//')"
  core="${cur%%-*}"
  core="${core%%+*}"
  if printf '%s' "$core" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
    lowest="$(printf '%s\n%s\n' "$MIN_VERSION" "$core" | sort -V | head -1)"
    if [ "$lowest" != "$MIN_VERSION" ]; then
      problems+="\\n  - your shuck (v${cur}) predates the MCP server (needs >= v${MIN_VERSION}); update it with: shuck upgrade"
    fi
  fi
fi

if [ -z "${GITHUB_TOKEN:-}" ] && [ -z "${GH_TOKEN:-}" ]; then
  problems+="\\n  - no GitHub token found (set GITHUB_TOKEN or GH_TOKEN, or pass --token)"
fi

# Nothing to report: stay quiet.
if [ -z "$problems" ]; then
  exit 0
fi

msg="shuck plugin — prerequisites still missing:${problems}"
cat <<JSON
{
  "systemMessage": "${msg}",
  "hookSpecificOutput": {
    "hookEventName": "SessionStart",
    "additionalContext": "${msg}"
  }
}
JSON
