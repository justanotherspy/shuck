# 🌽 shuck

**shuck the husk, keep the kernel.**

`shuck` is a Go CLI that returns the *exact* failing CI step logs for a pull
request. Instead of clicking through GitHub, the `gh` CLI, or MCP calls to reach
the one error that matters, `shuck` drills GitHub Actions failures down to the
failing **steps** and prints just their error logs. It's built for devs and
agents who want the signal without the fluff.

**When CI goes red on a PR, `shuck <pr>` is the first move.** One command takes
you from "a check failed" to the precise error lines — no tab-hopping, no log
scrolling. If you use the [Claude Code plugin](#claude-code-plugin), the binary
may already be on your `PATH`.

## What it does

Given a PR, `shuck`:

1. Resolves the target PR and its head commit.
2. Reads the PR's checks via the GitHub API using your `GITHUB_TOKEN`.
3. Finds the **failed** GitHub Actions jobs and, within each, the failed **steps**.
4. Downloads only those jobs' logs and extracts the relevant error lines.
5. Lists non-Actions failures (external checks / commit statuses) by name — no
   logs are available for those.
6. Surfaces cancelled jobs and any checks still running, with an upfront
   `N failed, M cancelled, …` summary so nothing is silently dropped.

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
shuck <pr-url>              # inspect a PR from its GitHub URL
shuck <run-url>             # inspect a single GitHub Actions run
shuck <job-url>             # inspect a single GitHub Actions job
shuck <pr>                  # owner/repo inferred from the local repo's origin
shuck                       # inspect the open PR for the current branch
```

Pass a GitHub Actions URL to skip the PR-wide scan and look at just one run or
job — handy when a CI-failure notification already points at the failing job:

```sh
shuck https://github.com/justanotherspy/shuck/actions/runs/123          # whole run
shuck https://github.com/justanotherspy/shuck/actions/runs/123/job/456  # one job
```

A run/job target reports only that run's Actions jobs (no PR-wide non-Actions
checks) and bypasses the cache, so its logs are always freshly downloaded.

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
| `--json` | false | Emit machine-readable JSON (stable schema) instead of text. |
| `--version` | false | Print the shuck version and exit. |

Run `shuck --help` to print this usage and the full flag list. Flags may appear
before or after the target (`shuck owner/repo 42 --json` works), and accept one
or two dashes (`-json` and `--json` are equivalent). A leading Unicode dash is
tolerated too, so a flag mangled by macOS "smart dashes" or a rich-text
copy-paste (`shuck 42 —full`) still works.

Exit codes: `0` no failing checks · `1` failing checks reported · `2` error.
Cancelled jobs are reported in the summary but do **not** by themselves set a
non-zero exit code — cancellation is often deliberate (a superseded run, a
manual stop), so it stays `0` unless a real failure is also present.

### JSON output

`--json` emits a stable, versioned document instead of the pretty text, so an
agent or script can consume results deterministically. The exit code is
unchanged, so `--json` still composes in pipelines.

```jsonc
{
  "schema_version": 1,
  "pr": { "owner": "…", "repo": "…", "number": 42, "title": "…",
          "head_sha": "…", "head_branch": "…" },
  "summary": { "failed": 1, "cancelled": 0, "running": 0, "other_failed": 0 },
  "failed_jobs": [
    {
      "id": 7, "run_id": 9, "name": "build", "conclusion": "failure",
      "workflow_name": "CI", "workflow_path": ".github/workflows/ci.yml",
      "failed_steps": [
        { "number": 3, "name": "Run tests", "kind": "bash",
          "command": "go test ./...", "excerpt": "--- FAIL: TestParse …" }
      ]
    }
  ],
  "cancelled_jobs": [],
  "other_checks": [],
  "running_jobs": []
}
```

For a run/job URL target the `pr` object is left zero-valued and a `run` object
carries the head context instead:

```jsonc
{
  "schema_version": 1,
  "pr": { "owner": "", "repo": "", "number": 0, "title": "", "head_sha": "", "head_branch": "" },
  "run": { "owner": "…", "repo": "…", "run_id": 123, "job_id": 456,
           "title": "…", "head_sha": "…", "head_branch": "…", "workflow_name": "CI" },
  "summary": { "failed": 1, "cancelled": 0, "running": 0, "other_failed": 0 },
  "failed_jobs": [ /* … */ ]
}
```

`schema_version` is bumped only on a breaking change; new fields (like `run`)
are added without a bump. Lists are always present (`[]`, never `null`).

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

Summary: 1 failed

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

## Claude Code plugin

`shuck` also ships as a [Claude Code](https://claude.com/claude-code) plugin so
agents can pull failing CI logs for you. It adds a `/shuck` skill and a
`SessionStart` hook that auto-installs the matching signed `shuck` release binary
(verified against `checksums.txt`) and checks that a GitHub token is present.

Add the marketplace and install the plugin from within Claude Code:

```
/plugin marketplace add justanotherspy/shuck
/plugin install shuck@shuck-marketplace
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

