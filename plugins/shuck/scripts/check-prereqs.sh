#!/usr/bin/env bash
# SessionStart hook for the shuck plugin.
#
# The plugin does not install shuck; the user installs it themselves (see the
# README) and keeps it current with `shuck upgrade`. This hook only verifies the
# prerequisites the plugin needs and reports any that are missing, without
# blocking the session or touching the network:
#
#   1. the `shuck` binary is on PATH (the plugin's monitor, its hooks and the
#      /shuck skill all run it);
#   2. it is new enough to have the event stream the plugin's monitor runs
#      (`shuck monitor stream`);
#   3. a GitHub token is available (GITHUB_TOKEN or GH_TOKEN).
#
# Stays silent when everything is in order.
set -euo pipefail

# First release with `shuck monitor stream`, the command the plugin's monitor
# runs — the monitor arrived in 1.0.0, the stream that feeds it notifications in
# 1.1.0. This floor matters most for the stream: a monitor speaks only through
# stdout, so an older binary rejects the subcommand on stderr and the session
# simply never hears anything. Bump it when a future release changes the
# minimum the plugin relies on.
MIN_VERSION="1.1.0"

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
  #
  # The `|| true` is what keeps this hook from ever blocking a session: under
  # `set -euo pipefail` a `shuck version` that exits non-zero (a broken
  # install, an unreadable binary) fails the whole assignment and kills the
  # script mid-run — non-zero exit, no JSON, no explanation, from a check whose
  # entire job is to be advisory. Swallowing it leaves `cur` empty, which falls
  # through the semver test into the intended "trust it" branch.
  cur="$(shuck version 2>/dev/null | awk 'NR==1{print $2}' | sed 's/^v//')" || true
  core="${cur%%-*}"
  core="${core%%+*}"
  # A `git describe` build carries a '-<commits>-g<sha>' suffix, and its core is
  # the tag it is built *after* — 1.0.0-2-g34cc959 is two commits newer than
  # v1.0.0, not older. Comparing on the core alone would tell every contributor
  # working between releases that their binary is too old, which is both wrong
  # and the fastest way to teach people to ignore this warning. Trust a dev
  # build the way the branch below trusts any version it cannot parse: whether
  # it has the feature is the business of whoever built it.
  if printf '%s' "$cur" | grep -Eq -- '-[0-9]+-g[0-9a-f]+'; then
    core=""
  fi
  if printf '%s' "$core" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
    lowest="$(printf '%s\n%s\n' "$MIN_VERSION" "$core" | sort -V | head -1)"
    if [ "$lowest" != "$MIN_VERSION" ]; then
      problems+="\\n  - your shuck (v${cur}) predates the monitor's event stream, so this session will get no notifications (needs >= v${MIN_VERSION}); update it with: shuck upgrade"
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
