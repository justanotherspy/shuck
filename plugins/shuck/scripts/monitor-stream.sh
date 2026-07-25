#!/usr/bin/env bash
# Plugin-monitor shim for shuck's background monitor.
#
# Claude Code starts this once per session, in the session's working directory,
# and turns every line it reads on stdout into a notification. `shuck monitor
# stream` registers that working tree with the local daemon and then writes one
# block per event, so the session hears about a red build without anyone asking.
# All the logic lives in the binary; this script exists only so a session
# without shuck installed degrades to silence instead of a monitor that dies
# with an error — the plugin does not install shuck, the user does (see the
# README).
#
# Nothing here writes to stdout that is not a monitor event. Stdout is the
# loudest channel the plugin has, and a diagnostic delivered as a notification
# costs the session more than saying nothing: an older shuck without the
# `stream` subcommand fails on stderr, which is invisible to the session, and
# the hooks keep delivering events in the conversation as they always did.
set -uo pipefail

command -v shuck >/dev/null 2>&1 || exit 0

# exec so the process Claude Code tracks — and signals when the session ends —
# is shuck itself, with no shell in between to swallow the signal or to hold a
# finished event in a pipe buffer. Bash would otherwise exit 127 straight out of
# a failed exec; execfail keeps the quiet exit below reachable for a shuck that
# vanished between the check above and here.
shopt -s execfail
exec shuck monitor stream
exit 0
