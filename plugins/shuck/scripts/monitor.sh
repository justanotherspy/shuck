#!/usr/bin/env bash
# Hook shim for shuck's background monitor.
#
# All the logic lives in the binary (`shuck monitor hook <event>`), which reads
# the hook payload on stdin and writes the hook response on stdout. This script
# exists only so a session without shuck installed degrades to silence instead
# of a hook error on every tool call — the plugin does not install shuck, the
# user does (see the README).
#
# Only two events are still wired (see hooks/hooks.json), and both are here
# because a monitor structurally cannot do their job: `post-tool-use` sees the
# agent's own `git push` and brings the next check forward, which no separate
# process can observe, and `stop` is enforcement rather than delivery — a
# notification tells the agent, only this refuses to let the turn finish on a
# red build. Everything else the monitor now delivers itself.
#
# Every path here exits 0. A background convenience must never be the reason a
# session stalls or a prompt is rejected.
set -uo pipefail

command -v shuck >/dev/null 2>&1 || exit 0

# `shuck monitor hook` is itself written to exit 0 whatever happens; the `|| true`
# covers the binary being killed or replaced mid-call.
shuck monitor hook "${1:-}" || true
exit 0
