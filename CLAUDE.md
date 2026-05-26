# CLAUDE.md

Guidance for agents working in this repository.

## What this is

`shuck` is a Go CLI that prints the exact failing CI step logs for a GitHub PR.
It resolves a PR, reads its checks via the GitHub API, drills GitHub Actions
failures down to the failed steps + their error logs, and caches results under
`~/.shuck` to avoid redundant log downloads.

## Commands

```sh
make build   # build the ./shuck binary
make test    # go test -race ./...
make vet     # go vet ./...
make lint    # golangci-lint run ./...  (run `make lint-install` once first)
make cover   # coverage summary
make fmt     # gofmt -w .
make tidy    # go mod tidy
```

Always run `make test` and `make lint` before pushing; CI runs both.

## Architecture

The pipeline is: resolve target → load/validate cache → fetch checks (cheap
metadata) → drill only new failed jobs for logs → parse → extract errors →
render → update cache.

| Package | Responsibility |
| --- | --- |
| `main.go` | Thin entry; dispatches the `mcp` and `setup` subcommands, else calls `cli.Run`. Holds the `go:embed` of the plugin's `SKILL.md` that `setup` installs. |
| `internal/cli` | Flag parsing + pipeline orchestration; the `app.drill` / `app.buildFailedSteps` logic that pairs failed API steps with error log sections. Subcommands `logs` (CI only, `--run` for a single run), `reviews` (reviews only), `all` (CI + reviews + security — also the bare-`shuck` default, see `inspectAll`/`emitAll` in `all.go`), plus `action` / `security` / `version` / `upgrade`. Single-letter aliases (`l`/`r`/`a`/`s`) resolve via `subcommandAliases`. The exported `cli.Inspect` / `cli.Security` / `cli.Action` cores back both the CLI and the MCP server. |
| `internal/action` | `shuck action`: parse an `owner/action[@version]` ref and pick the latest matching semver tag from a repo's tag list (pure selection in `Select`; stable preferred, prerelease only as a fallback), then render the SHA-pin line / JSON. The stable JSON shape is exported as `action.Document` (`NewDocument` projects it; `EncodeJSON` reuses it) so the MCP `inspect_action` tool returns typed output. |
| `internal/security` | `shuck security`: sort + render a `model.SecurityReport` (code scanning, secret scanning, Dependabot alerts) to text / versioned JSON. Pure presentation; the gh layer fetches, the `cli.Security` core assembles. |
| `internal/release` | Self-update: resolve the latest GitHub release, download + checksum-verify the matching archive, and replace the running binary in place. Backs `shuck version --check` / `shuck upgrade`. |
| `internal/setup` | `shuck setup`: install the embedded skill into `~/.claude/skills/shuck`, add a managed note to the user's `CLAUDE.md`, and optionally register the MCP at user scope (`claude mcp add`). The skill is `go:embed`-ed from the plugin in `main.go`, so the standalone install and the marketplace stay in sync. |
| `internal/target` | Resolve owner/repo/PR from args or the local repo (via go-git). |
| `internal/gh` | go-github wrappers: PR head, Actions runs/jobs, job-log download, non-Actions checks, and the security-alert lists (`security.go`: code scanning / secret scanning / Dependabot). Also a small hand-rolled GraphQL client (`reviews.go`) for PR reviews + comment threads, since `isResolved`/`resolvedBy` are GraphQL-only. |
| `internal/cache` | `~/.shuck/cache/<owner>/<repo>/<pr>/cache.json` load/save plus whole raw job logs under that PR's `logs/<jobID>-<attempt>.log` (re-parsed locally on re-run). Also `~/.shuck/actions/<owner>/<repo>/tags.json` for `shuck action`'s tag list and `~/.shuck/security/<owner>/<repo>/alerts.json` for `shuck security` (both keyed on the default-branch SHA + TTL by the CLI). `Purge(ttl, keep)` sweeps stale entries (by record mtime) off disk; every command calls it, exempting the active target. |
| `internal/logs` | Parse a job log into `##[group]`-delimited sections; extract the high-signal error excerpt. |
| `internal/render` | Format a `model.Report` to text. |
| `internal/model` | Shared domain types (imports nothing internal). |

## Key design notes

- **Step commands come from the logs**, not workflow YAML: the `##[group]Run …`
  header gives the action ref or shell command. See `logs.Section.Command/Kind`.
- **Step↔section matching** (`cli.buildFailedSteps`) is the trickiest part: failed
  API steps are paired with `##[error]`-bearing log sections by order, with a
  whole-log fallback when no error marker is present. Cover changes here with
  fixtures in `internal/logs/testdata`.
- **Caching is advisory**: cheap metadata (head SHA, run/job listing, reviews
  fingerprint) is always re-validated. On the same head commit a job's **whole raw
  log is cached** (`cache.SaveJobLog`/`LoadJobLog`, keyed by `(job id, run
  attempt)`) and **re-parsed locally** under the current `--full`/`--context`/
  `--pattern` flags via `buildFailedSteps` — so re-runs with extra context cost no
  network. Only newly-finished attempts are downloaded. The focused `logs` /
  `reviews` subcommands now cache too: each persists its own dimension and copies
  the other from the existing cache (a merged `toSave` copy) so neither clobbers
  the other. A 1h `Purge` sweeps stale entries off disk on every run.
- **Non-Actions checks** are listed only (no logs exist for them via the API).
- **Action pinning** (`shuck action`): `gh.ListActionTags` pages the repo's tags
  (each carries the peeled commit SHA), `action.Select` filters by the requested
  major / major.minor and picks the highest semver — preferring a stable tag over
  a prerelease of the same version, falling back to a prerelease only when nothing
  stable matches. Tag lists are cached for `actionCacheTTL` (1h) and keyed on the
  repo's default-branch SHA: within the TTL a cheap `gh.DefaultBranchSHA` (one
  `GetCommitSHA1(…, "HEAD")` call) decides reuse vs. re-fetch — unchanged ⇒ reuse,
  moved ⇒ re-page tags, and a failed SHA check leaves the fresh cache standing.
  `--refresh` forces a re-fetch. The fetch client is the exported `NewTagLister`
  package var (interface `TagLister`) so embedders and tests stub the network.
  Auth is optional here, so `gh.New("")` is unauthenticated.
- **Reviews** (`gh.PRReviews`, rendered grouped by verdict) collapse resolved/
  outdated threads to a one-line reason and cap active-thread comments at
  `--review-comment-limit`. A cheap `gh.ReviewsFingerprint` short-circuits the
  full review pull when nothing changed. The `logs` / `reviews` subcommands focus
  on one dimension (they set the internal `ciOnly` / `reviewsOnly` gates) but now
  still write the cache, persisting the un-fetched dimension from the existing
  cache so neither subcommand clobbers the other; the bare `shuck` / `shuck all`
  path runs both plus security. The old `--ci-only` / `--reviews-only` flags were
  removed in favor of the subcommands.
- **Security** (`shuck security`): `cli.Security` resolves a repo (no PR — see
  `target.ResolveRepo`) and fetches three sources sequentially via the
  `newSecurityLister` package var (stubbed in tests). Each source **degrades
  independently** — a 404 ⇒ `disabled`, 403 ⇒ `forbidden` (see
  `classifySecurityErr`) — so a missing feature never fails the command; only an
  all-sources error is fatal. The `--state` value maps per source (vocabularies
  differ; a source without an equivalent is reported `disabled`). **The raw
  secret value is never read** from the API, so it cannot leak — `model` has no
  field for it. Reports cache for `securityCacheTTL` (1h), keyed by state and the
  repo's default-branch SHA (same cheap `gh.DefaultBranchSHA` reuse logic as
  `shuck action`); a result with any errored source is not cached. Exit is `0` on
  success, `2` on an operational error; `--exit-code` makes open findings exit `1`.

## Conventions

- Standard library `flag` for CLI parsing; no cobra.
- Keep `internal/model` dependency-free to avoid import cycles.
- Errors from `fmt.Fprint*` to stdout/stderr are intentionally ignored (see the
  errcheck exclusion in `.golangci.yml`).
- Tests are table-driven where practical; pure logic in `logs`/`target`/`render`
  is unit-tested without network.
