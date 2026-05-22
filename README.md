# 🌽 shuck

**shuck the husk, keep the kernel.**

`shuck` is a Go CLI that returns the *exact* failing CI step logs for a pull
request. Instead of clicking through GitHub, the `gh` CLI, or MCP calls to reach
the one error that matters, `shuck` drills GitHub Actions failures down to the
failing **steps** and prints just their error logs. It's built for devs and
agents who want the signal without the fluff.

## What it does

Given a PR, `shuck`:

1. Resolves the target PR and its head commit.
2. Reads the PR's checks via the GitHub API using your `GITHUB_TOKEN`.
3. Finds the **failed** GitHub Actions jobs and, within each, the failed **steps**.
4. Downloads only those jobs' logs and extracts the relevant error lines.
5. Lists non-Actions failures (external checks / commit statuses) by name — no
   logs are available for those.
6. Reports any checks still running.

A local cache under `~/.shuck` makes repeat runs cheap: it avoids re-downloading
logs for job attempts it has already inspected on the same commit.

## Install

Download a prebuilt binary (no Go toolchain needed). The script picks the
archive for your OS/arch, verifies its checksum, and installs `shuck` into an
on-PATH directory:

```sh
curl -fsSL https://raw.githubusercontent.com/justanotherspy/shuck/main/install.sh | bash
```

Pin a version or target directory with environment variables:

```sh
curl -fsSL https://raw.githubusercontent.com/justanotherspy/shuck/main/install.sh \
  | SHUCK_VERSION=v0.2.0 SHUCK_INSTALL_DIR=/usr/local/bin bash
```

Or build from source:

```sh
go install github.com/justanotherspy/shuck@latest
```

Binaries are also available on the
[releases](https://github.com/justanotherspy/shuck/releases) page (built with
GoReleaser).

## Usage

```sh
shuck <owner>/<repo> <pr>   # inspect an explicit PR
shuck <pr>                  # owner/repo inferred from the local repo's origin
shuck                       # inspect the open PR for the current branch
```

Authentication uses `GITHUB_TOKEN` (or `GH_TOKEN`), or pass `--token`.

```sh
export GITHUB_TOKEN=ghp_...
shuck justanotherspy/shuck 42
```

### Flags

| Flag | Default | Description |
| --- | --- | --- |
| `--context N` | 10 | Lines of context kept around each error match. |
| `--short-threshold N` | 100 | Logs with at most this many lines are shown whole. |
| `--tail N` | 100 | Lines tailed when a long log has no error match. |
| `--pattern RE` | — | Override the error-matching regexp. |
| `--full` | false | Show full, untrimmed logs for failed steps. |
| `--token T` | — | GitHub token (overrides `GITHUB_TOKEN`/`GH_TOKEN`). |
| `--refresh` | false | Ignore and rebuild the cache. |
| `--no-cache` | false | Do not read or write the cache. |
| `--offline` | false | Render only from cache, without network access. |

Exit codes: `0` no failing checks · `1` failing checks reported · `2` error.

### How log extraction works

For each failed step:

- **Short logs** (≤ `--short-threshold` lines) are shown whole.
- **Long logs** are grepped for error/failure tokens; `±--context` lines around
  each match are kept, with omitted spans marked.
- **Long logs with no match** are tailed to the last `--tail` lines (the
  "error only at the very end" case).

### Example output

```
justanotherspy/shuck PR #42 — fix flaky parser   (commit a1b2c3d)

Workflow: CI (.github/workflows/ci.yml)
Job: build  [failure]
Steps:
  1. Set up job (success)
  2. Checkout (success)
  3. Run tests (failure)
  4. Upload coverage (skipped)

  ▸ Step 3 — Run tests (failed)
    Step command:
      * bash run:
        ```
        go test ./...
        ```
    error logs:
      ```
      --- FAIL: TestParse (0.00s)
          parse_test.go:42: expected 1, got 2
      FAIL
      ##[error]Process completed with exit code 1.
      ```
```

## Development

```sh
make build   # build ./shuck
make test    # go test -race ./...
make lint    # golangci-lint (run `make lint-install` first)
make cover   # coverage report
```

## License

MIT — see [LICENSE](LICENSE).

