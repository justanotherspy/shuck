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
| `internal/cli` | Flag parsing + pipeline orchestration; the `app.drill` / `app.buildFailedSteps` logic that pairs failed API steps with error log sections. Also the `version` / `upgrade` subcommands. |
| `internal/release` | Self-update: resolve the latest GitHub release, download + checksum-verify the matching archive, and replace the running binary in place. Backs `shuck version --check` / `shuck upgrade`. |
| `internal/setup` | `shuck setup`: install the embedded skill into `~/.claude/skills/shuck`, add a managed note to the user's `CLAUDE.md`, and optionally register the MCP at user scope (`claude mcp add`). The skill is `go:embed`-ed from the plugin in `main.go`, so the standalone install and the marketplace stay in sync. |
| `internal/target` | Resolve owner/repo/PR from args or the local repo (via go-git). |
| `internal/gh` | go-github wrappers: PR head, Actions runs/jobs, job-log download, non-Actions checks. Also a small hand-rolled GraphQL client (`reviews.go`) for PR reviews + comment threads, since `isResolved`/`resolvedBy` are GraphQL-only. |
| `internal/cache` | `~/.shuck/cache/<owner>/<repo>/<pr>/cache.json` load/save + inspected-job indexing. |
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
- **Caching is advisory**: cheap metadata (head SHA, run/job listing) is always
  re-validated; only log downloads are skipped, keyed by `(job id, run attempt)`
  so replays and newly-finished jobs are re-inspected.
- **Non-Actions checks** are listed only (no logs exist for them via the API).
- **Reviews** (`gh.PRReviews`, rendered grouped by verdict) collapse resolved/
  outdated threads to a one-line reason and cap active-thread comments at
  `--review-comment-limit`. A cheap `gh.ReviewsFingerprint` short-circuits the
  full review pull when nothing changed; `--ci-only`/`--reviews-only` focus the
  output (and skip the cache write to avoid clobbering the other dimension).

## Releasing & distribution

- **Release flow**: `release-drafter` drafts a release (notes + next `vX.Y.Z`
  tag) on every push to `main`. A human reviews the draft and publishes it as a
  **pre-release** — which creates the tag and triggers `.github/workflows/
  release.yml`. GoReleaser then builds the binaries, attaches them to that
  existing release (`release.mode: keep-existing` preserves the drafted notes),
  and a final `gh release edit … --prerelease=false --latest=true` step promotes
  it to the full "Latest" release.
- **Homebrew tap lives in this repo**: GoReleaser's `homebrew_casks` block
  regenerates [`Casks/shuck.rb`](Casks/shuck.rb) on `main` each release, so the
  repo doubles as a tap (`brew tap justanotherspy/shuck <url>` →
  `brew install --cask shuck`). The cask is macOS-only and strips the
  quarantine xattr in a postflight hook (binaries aren't notarized). Don't
  hand-edit the cask except to seed a version before the next release —
  GoReleaser overwrites it.

## Conventions

- Standard library `flag` for CLI parsing; no cobra.
- Keep `internal/model` dependency-free to avoid import cycles.
- Errors from `fmt.Fprint*` to stdout/stderr are intentionally ignored (see the
  errcheck exclusion in `.golangci.yml`).
- Tests are table-driven where practical; pure logic in `logs`/`target`/`render`
  is unit-tested without network.
