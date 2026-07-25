# shuck improvement plan

Where feedback from the agents that use `shuck` lands, and what is known to be
missing. Keep it in sync as items ship: an entry here is either something to do
or something to stop pretending is done.

## How to file something

When shuck's output falls short of what you needed to debug something — a
report that made you go back to the Actions UI, an event that arrived without
the detail to act on it, a flag you reached for that does not exist — write it
down here or open an issue. That feedback loop is what the file is for; the
first two rounds of it produced `--json`, the run/job targets, `--watch`,
check-run annotations, and failure classification, all since shipped and no
longer tracked here.

## Known gaps — the background monitor

The monitor (`internal/monitor`) is new. These are its honest edges, not
theoretical ones:

- **A green verdict is inferred, so it needs a witness.** Nothing in the API
  says a commit passed — the jobs listing returns only what failed, was
  cancelled, or is still running. `ci.passed` is deduced from having watched
  checks run and then stop, so a watch that starts *after* a run has finished
  reports nothing about it. That is the right default (a finished run is not
  news), but it means "is it green?" for a commit you did not watch is a
  question to ask (`shuck monitor status`), not something you will be told.
- **No progress while something is in flight.** `shuck --watch` prints progress
  to stderr as it polls; the monitor has no equivalent. Between "checks
  running" and the next terminal event there is nothing to see, and
  `shuck monitor events --follow` blocks silently. An agent that wants a
  heartbeat has to poll `shuck monitor status`.
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
  build must never delay a finish. Only `ci.failed`, `review.comment` and a
  `review.submitted` that is *not* an approval can hold a turn open;
  `watch.target`, `ci.started`, `pr.state`, `pins.stale` and `monitor.error`
  never do. Every one of them still reaches the session — as a notification
  where the plugin monitor runs, on the next prompt where it does not — but an
  agent that finishes right before a failure lands hears about it only then.
- **A stand-down lets the session's backlog grow.** While a stream serves the
  tree, `UserPromptSubmit` delivers nothing and consumes nothing, so the
  session's own cursor is not advanced between `SessionStart` and the first
  `Stop`. Nothing is lost, but the `Stop` hook's peek can then hold a long
  batch, and `capFeed` trims from the *end* at 3.5 KB — so a very long session
  on a busy PR can have the newest event, the one that justified the block,
  truncated out of the block reason. Giving that peek a `Limit` would fix it
  (the journal keeps the newest on overflow and the seek target is unchanged);
  it is left alone until someone actually hits it.
- **A killed stream is silent for up to its staleness window.** The liveness
  marker is removed on exit and on the signal path, but a SIGKILL leaves it
  behind and it reads as live until the heartbeat ages out. During that window
  the prompt hook stands down and the stream is not reading either. Again
  nothing is lost — the stand-down consumes nothing — but delivery waits for the
  first prompt after the marker expires.
- **Event delivery is at-most-once per consumer.** The cursor advances as
  events are handed over, so a caller that takes a batch and then dies has lost
  it. That is the deliberate trade (re-delivering a fixed CI
  failure is worse than dropping the tail of a batch nobody read); callers that
  need to look before committing pass `peek`.

## Deferred

- **Unprompted triage hint** on every run — noise in pipes and `--json`;
  `--help` already serves that role.
