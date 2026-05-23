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
| `main.go` | Thin entry; calls `cli.Run` and sets the exit code. |
| `internal/cli` | Flag parsing + pipeline orchestration; the `app.drill` / `app.buildFailedSteps` logic that pairs failed API steps with error log sections. Also the `version` / `upgrade` subcommands. |
| `internal/release` | Self-update: resolve the latest GitHub release, download + checksum-verify the matching archive, and replace the running binary in place. Backs `shuck version --check` / `shuck upgrade`. |
| `internal/target` | Resolve owner/repo/PR from args or the local repo (via go-git). |
| `internal/gh` | go-github wrappers: PR head, Actions runs/jobs, job-log download, non-Actions checks. |
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

## Conventions

- Standard library `flag` for CLI parsing; no cobra.
- Keep `internal/model` dependency-free to avoid import cycles.
- Errors from `fmt.Fprint*` to stdout/stderr are intentionally ignored (see the
  errcheck exclusion in `.golangci.yml`).
- Tests are table-driven where practical; pure logic in `logs`/`target`/`render`
  is unit-tested without network.
