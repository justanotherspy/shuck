#!/usr/bin/env bash
# Plugin-monitor shim for shuck's background monitor.
#
# Claude Code starts this once per session, in the session's working directory,
# and turns every line it reads on stdout into a notification. `shuck monitor
# stream` registers that working tree with the local daemon and then writes one
# block per event, so the session hears about a red build without anyone asking.
# All the logic lives in the binary; this script exists only to check the two
# things the binary cannot tell you about itself — that it is installed, and
# that it is new enough to have the `stream` subcommand at all.
#
# This is now the plugin's only voice. The prerequisite warning used to be a
# SessionStart hook, and there is no SessionStart hook any more: delivery is the
# monitor's job, so a session that will get nothing has to be told so here or
# not at all. That makes stdout expensive — every line is a notification — so
# each failure below is one plain sentence and then exit 0, never a diagnostic
# per tick and never a stack trace. A monitor that exits is simply a monitor
# that is not running.
#
# The token is deliberately not checked here. The binary resolves one from
# --token, GITHUB_TOKEN, GH_TOKEN or `gh auth token`, and only the binary knows
# which of those answered; a shell check of the two environment variables would
# now warn on machines where shuck works perfectly well. `shuck monitor stream`
# stands down with its own single line when it genuinely has no token.
set -uo pipefail

# The oldest shuck whose features this plugin relies on: the monitor arrived in
# 1.0.0, and `shuck monitor stream` — the command this shim execs — in v1.0.1.
# The floor matters most for the stream, because an older binary fails the
# subcommand on stderr, which is invisible to a session that only ever reads
# stdout, and the failure looks exactly like a monitor with nothing to say.
#
# It must name a version somebody can actually install. This floor briefly read
# 1.1.0 — a version that was only ever an unpublished draft — which meant every
# user on the latest release was told their binary predated a feature it in fact
# had. A floor above the newest release warns everybody, forever, and teaches
# them to ignore the one warning that explains a silent monitor. Bump it only to
# a published release that genuinely carries what the plugin needs.
MIN_VERSION="1.0.1"

# One line, prefixed so a notification says who is talking.
say() {
  printf 'shuck plugin: %s\n' "$1"
}

if ! command -v shuck >/dev/null 2>&1; then
  say "the 'shuck' binary is not on your PATH, so nothing will tell you when this branch's CI goes red or a reviewer comments. Install it with: curl -fsSL https://raw.githubusercontent.com/justanotherspy/shuck/main/install.sh | bash (or: go install github.com/justanotherspy/shuck@latest), then reload."
  exit 0
fi

# `shuck version` prints 'shuck vX.Y.Z' on its first line and never hits the
# network, so this check stays offline and costs one process at session start.
# Compare only the MAJOR.MINOR.PATCH core, dropping any -prerelease/+build
# suffix, and trust anything that is not plain semver — whether an unusual build
# has the feature is the business of whoever built it.
#
# The `|| true` is what keeps this shim advisory. Under `pipefail` a `shuck
# version` that exits non-zero (a broken install, an unreadable binary) would
# leave a non-zero status behind; swallowing it leaves `cur` empty, which falls
# through the semver test into the intended "trust it" branch.
cur="$(shuck version 2>/dev/null | awk 'NR==1{print $2}' | sed 's/^v//')" || true
core="${cur%%-*}"
core="${core%%+*}"
# A `git describe` build carries a '-<commits>-g<sha>' suffix, and its core is
# the tag it is built *after* — 1.0.0-2-g34cc959 is two commits newer than
# v1.0.0, not older. Comparing on the core alone would tell every contributor
# working between releases that their binary is too old, which is both wrong and
# the fastest way to teach people to ignore this warning. '-dirty' says the same
# thing in the one case the commit-count suffix cannot: a build made from a
# modified tree sitting exactly on a tag, which is what `make build` produces in
# this repo between releases. Whether either has the feature is the business of
# whoever built it.
if printf '%s' "$cur" | grep -Eq -- '-[0-9]+-g[0-9a-f]+|-dirty'; then
  core=""
fi
if printf '%s' "$core" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  # NOTE: this sort uses a real newline, and the comparison is "is MIN_VERSION
  # still the lower of the two?" — anything else means the installed core sorts
  # below the floor.
  lowest="$(printf '%s\n%s\n' "$MIN_VERSION" "$core" | sort -V | head -1)"
  if [ "$lowest" != "$MIN_VERSION" ]; then
    say "your shuck (v${cur}) predates the monitor's event stream, so this session will get no notifications about CI or reviews (needs >= v${MIN_VERSION}); update it with: shuck upgrade"
    exit 0
  fi
fi

# exec so the process Claude Code tracks — and signals when the session ends —
# is shuck itself, with no shell in between to swallow the signal or to hold a
# finished event in a pipe buffer. Bash would otherwise exit 127 straight out of
# a failed exec; execfail keeps the quiet exit below reachable for a shuck that
# vanished between the checks above and here.
shopt -s execfail
exec shuck monitor stream
exit 0
