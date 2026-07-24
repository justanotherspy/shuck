# shuck improvement plan

Where feedback from the agents that use `shuck` lands, and what is known to be
missing. Keep it in sync as items ship: an entry here is either something to do
or something to stop pretending is done.

## How to file something

When shuck's output falls short of what you needed to debug something — a
report that made you go back to the Actions UI, an event that arrived without
the detail to act on it, a flag you reached for that does not exist — write it
down here or open an issue. That feedback loop is what the file is for; the
first two rounds of it produced `--json`, the run/job targets, `--watch`, the
MCP server, check-run annotations, and failure classification, all since
shipped and no longer tracked here.

## Known gaps — the background monitor

The monitor (`internal/monitor`) is new. These are its honest edges, not
theoretical ones:

- **A green verdict is inferred, so it needs a witness.** Nothing in the API
  says a commit passed — the jobs listing returns only what failed, was
  cancelled, or is still running. `ci.passed` is deduced from having watched
  checks run and then stop, so a watch that starts *after* a run has finished
  reports nothing about it. That is the right default (a finished run is not
  news), but it means "is it green?" for a commit you did not watch is a
  question to ask (`shuck monitor status` / `monitor_status`), not something
  you will be told.
- **No progress while something is in flight.** `shuck --watch` prints progress
  to stderr as it polls; the monitor has no equivalent. Between "checks
  running" and the next terminal event there is nothing to see, and
  `monitor events --follow` blocks silently. An agent that wants a heartbeat has
  to poll `monitor_status`.
- **Pending non-Actions checks are invisible.** `gh.OtherChecks` returns only
  non-Actions check runs that have *completed* and failed, so a third-party
  check still in progress cannot hold a verdict back. The verdict is about the
  Actions jobs plus whatever non-Actions checks have already gone red.
- **One daemon per machine, one token.** The first client to start the daemon
  donates its environment, and every watch is polled with that token. A watch on
  a repository the token cannot see fails with `monitor.error` and backs off;
  there is no per-watch credential and no way to swap the token without
  `shuck monitor stop` and a restart.
- **The Stop hook only speaks up for actionable events.** By design — a passing
  build must never delay a finish — but it means `watch.target`, `ci.started`
  and `pr.state` will never hold a turn open, and an agent that finishes right
  before a failure lands hears about it only in the next session's first prompt.
- **`monitor_events` delivery is at-most-once per consumer.** The cursor
  advances as events are handed over, so a caller that takes a batch and then
  dies has lost it. That is the deliberate trade (re-delivering a fixed CI
  failure is worse than dropping the tail of a batch nobody read); callers that
  need to look before committing pass `peek`.

## Deferred

- **Unprompted triage hint** on every run — noise in pipes and `--json`;
  `--help` already serves that role.
