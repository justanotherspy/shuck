# CLAUDE.md

Guidance for agents working in this repository.

## What this is

`shuck` is a Go CLI that prints the exact failing CI step logs for a GitHub PR.
It resolves a PR, reads its checks via the GitHub API, drills GitHub Actions
failures down to the failed steps + their error logs, and caches results under
`~/.cache/shuck` to avoid redundant log downloads.

## Commands

Run `make help` for the full list. The essentials:

```sh
make tools           # install the pinned dev tools (lint, releaser, gopls…)
make build           # build ./bin/shuck
make test            # go test -race -covermode=atomic … (coverage in coverage.out)
make vet             # go vet ./...
make lint            # golangci-lint run
make fmt             # gofmt + goimports via golangci-lint
make modernize       # go fix ./...  (apply Go 1.26 modernizers)
make modernize-check # fail if any modernization is pending (CI gate; alias: fix-check)
make cover-report    # Markdown coverage report (CI posts it on PRs)
make cover-check     # fail if coverage is below COVER_THRESHOLD (80%; CI gate)
make vuln            # govulncheck vulnerability scan
make fuzz FUZZ=Fuzz… # actively fuzz one target (FUZZTIME, FUZZPKG)
make fuzz-all        # briefly fuzz every target (nightly workflow)
make bench           # run benchmarks (BENCH, BENCHPKG, BENCHTIME)
make docker-build    # build the container image locally
make snapshot        # local goreleaser snapshot (no publish)
make tidy            # go mod tidy
make ci              # what CI runs: deps + lint + modernize-check + test + cover-check + build
```

`make fix` / `make fix-check` remain as aliases of `modernize` / `modernize-check`.
Always run `make test` and `make lint` before pushing; CI runs both.

## Architecture

The pipeline is: resolve target → load/validate cache → fetch checks (cheap
metadata) → drill only new failed/cancelled jobs for logs → parse → extract
errors → render → update cache.

| Package | Responsibility |
| --- | --- |
| `main.go` | Thin entry; dispatches the `mcp` and `setup` subcommands, else calls `cli.Run`. Holds the `go:embed` of the plugin's `SKILL.md` that `setup` installs. |
| `internal/cli` | Flag parsing + pipeline orchestration; the `app.drill` / `app.buildFailedSteps` logic that pairs failed API steps with error log sections. Subcommands `logs` (CI only, `--run` for a single run), `reviews` (reviews only), `all` (CI + reviews + security — also the bare-`shuck` default, see `inspectAll`/`emitAll` in `all.go`), plus `action` / `image` / `security` / `compliance` (with its `discover` sub-subcommand) / `version` / `upgrade`. Single-letter aliases (`l`/`r`/`a`/`s`/`c`/`i`) resolve via `subcommandAliases`. The exported `cli.Inspect` / `cli.Security` / `cli.Compliance` / `cli.ComplianceDiscover` / `cli.Action` / `cli.Image` / `cli.Images` cores back both the CLI and the MCP server. |
| `internal/action` | `shuck action`: parse an `owner/action[@version]` ref and pick the latest matching semver tag from a repo's tag list (pure selection in `Select`; stable preferred, prerelease only as a fallback), then render the SHA-pin line / JSON. The stable JSON shape is exported as `action.Document` (`NewDocument` projects it; `EncodeJSON` reuses it) so the MCP `inspect_action` tool returns typed output. The semver parsing/ordering is shared with `internal/semver`. |
| `internal/image` | `shuck image`: parse an image ref (`[ghcr.io/]owner/name[:tag]`, or a bare `owner` to list all) and pick the latest matching version + manifest digest (pure selection in `Select`, sharing `internal/semver`; non-semver tags fall back to most-recent). Renders the digest-pin line / JSON; the stable shapes `image.Document` (single resolve) and `image.ListDocument` (list-all) back the MCP `inspect_images` tool. |
| `internal/semver` | Tiny dependency-free semver slice (`Parse` / `Compare` / `Constraint.Matches`) shared by `action` and `image` for "pick the latest matching tag". |
| `internal/security` | `shuck security`: sort + render a `model.SecurityReport` (code scanning, secret scanning, Dependabot alerts) to text / versioned JSON. Pure presentation; the gh layer fetches, the `cli.Security` core assembles. |
| `internal/compliance` | `shuck compliance`: parse a `.github/compliance.yml` (`Parse`, strict / unknown-key-rejecting via yaml.v3) into a `Config`, then `Evaluate` it against the live settings the gh layer fetched (`Actual`) into a `model.ComplianceReport` — one pass/fail/skipped check per declared key. Renders text / versioned JSON (`Document`). Also the inverse, `shuck compliance discover` (`discover.go`): `Discover` snapshots the live settings into config bytes — a full `FromActual` snapshot when no config exists, or an in-place yaml.Node patch of an existing config's drifted declared keys (comments preserved). Pure logic; the `cli.Compliance` / `cli.ComplianceDiscover` cores do the I/O. |
| `internal/release` | Self-update: resolve the latest GitHub release, download + checksum-verify the matching archive, and replace the running binary in place. Backs `shuck version --check` / `shuck upgrade`. |
| `internal/setup` | `shuck setup`: install the embedded skill into `~/.claude/skills/shuck`, add a managed note to the user's `CLAUDE.md`, and optionally register the MCP at user scope (`claude mcp add`). The skill is `go:embed`-ed from the plugin in `main.go`, so the standalone install and the marketplace stay in sync. |
| `internal/target` | Resolve owner/repo/PR from args or the local repo (via go-git). |
| `internal/gh` | go-github (v88) wrappers: PR head, Actions runs/jobs, job-log download, non-Actions checks, the security-alert lists (`security.go`), the repo-settings / branch-protection / vulnerability-alerts / file-content reads for compliance (`compliance.go`, same soft 403/404 degradation as security), and the GHCR Packages API (`packages.go`: `ListContainerPackages` / `ListImageVersions`, org-then-user 404 fallback). Also two small hand-rolled HTTP clients over `c.http`/`c.token`: a GraphQL client (`reviews.go`) for PR reviews + comment threads (`isResolved`/`resolvedBy` are GraphQL-only), and an OCI registry-v2 client (`registry.go`: `RegistryTags` / `RegistryDigest`) for resolving a public image's digest anonymously. |
| `internal/cache` | `~/.cache/shuck/cache/<owner>/<repo>/<pr>/cache.json` load/save plus whole raw job logs under that PR's `logs/<jobID>-<attempt>.log` (re-parsed locally on re-run). Also `~/.cache/shuck/actions/<owner>/<repo>/tags.json` for `shuck action`, `~/.cache/shuck/security/<owner>/<repo>/alerts.json` for `shuck security`, and `~/.cache/shuck/images/<owner>/images.json` for `shuck image` (all keyed on the default-branch SHA + TTL by the CLI). `Purge(ttl, keep)` sweeps stale entries (by record mtime) off disk; every command calls it, exempting the active target. |
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
- **Cancelled jobs are drilled too** (best-effort): GitHub writes
  `##[error]The operation was canceled.` into the interrupted step's section, so
  the same pairing recovers what was running when the job was cancelled. The
  step-count is capped at the error sections found (queued steps are also marked
  "cancelled" by the API but have no section), a missing log degrades to a bare
  listing instead of an error step, and cancellation never flips the exit code.
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
- **Image pinning** (`shuck image`): two modes, split on the ref. A bare `owner`
  (or `owner/repo` / URL) **lists** every container package via the GitHub
  Packages API (`gh.ListContainerPackages` + `gh.ListImageVersions`, where the
  version `Name` is the `sha256:` digest and `metadata.container.tags` the tags) —
  this **requires a token** with `read:packages` (no anonymous enumeration), so a
  tokenless list errors with guidance. A full `ghcr.io/owner/name[:tag]` **resolves**
  one image: authed it reuses the Packages data; tokenless it falls back to the
  anonymous OCI **registry v2** API (`gh.RegistryTags` + `gh.RegistryDigest`, the
  `Docker-Content-Digest` header — for multi-arch images that is the image-index
  digest, the correct pin target). Selection (`image.Select`) shares
  `internal/semver`; non-semver tags fall back to the most recently updated
  version. Listings cache for `imageCacheTTL` (1h) under `~/.cache/shuck/images/<owner>`,
  keyed on the owner's default-branch SHA with the same cheap reuse logic as
  `shuck action`. The fetch client is the exported `NewImageLister` package var
  (interface `ImageLister`) so embedders and tests stub the network.
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
- **Compliance** (`shuck compliance`): `cli.Compliance` loads the intended
  settings from a `.github/compliance.yml` and compares them against the repo's
  live settings. **The config is the source of truth** and is **partial** — only
  declared keys are checked, and `compliance.Parse` rejects unknown keys (a typo
  must not silently skip a check). Config discovery: `--config <path>` wins; else a
  bare `shuck compliance` reads the **local checkout** (`PreferLocal`), falling
  back to fetching it from the repo, while an explicit `owner/repo` fetches it from
  the repo (`gh.FileContent`, `--ref` for a branch/tag/SHA). The gh reads
  **degrade like security**: branch protection / `security_and_analysis` need
  admin, so an unreadable setting is a **skipped** check (via `model.SettingsSource`),
  never a false pass; a 404 on branch protection means "not protected" ⇒ each
  declared protection **fails**. The fetch client is the `newComplianceLister`
  package var (interface `complianceLister`), stubbed in tests. Compliance is
  **uncached** (a few cheap reads, and the config is usually local). Exit is `0`
  when compliant, `1` on drift (CI gating, suppress with `--exit-zero`), `2` on an
  operational error.
- **Compliance discover** (`shuck compliance discover`): the inverse direction —
  `cli.ComplianceDiscover` snapshots the **live settings into the local config**.
  No config ⇒ `compliance.FromActual` generates a complete one (every readable
  setting: repository, security, the default branch's protection); existing
  config ⇒ `compliance.Discover` keeps **only its declared keys** (partial stays
  partial) and patches drifted values **in place via the yaml.Node tree**
  (`patchYAML`), preserving comments and key order. Unreadable settings are
  omitted (new) / left untouched (existing), reported as `Notes`. An up-to-date
  config is not rewritten. The pure logic (`Discover`/`FromActual`/`diffConfig`)
  lives in `internal/compliance/discover.go`; `cli.ComplianceDiscover` does the
  I/O (read/write the file, fetch live settings via the same `complianceLister`).
  `--dry-run` previews, `--json` emits `compliance.DiscoveryDocument`. Exit is `0`
  on success (created / updated / already up to date), `2` on an operational error.

## Conventions

- Standard library `flag` for CLI parsing; no cobra.
- Keep `internal/model` dependency-free to avoid import cycles. The domain types
  are passed by value on purpose; `gocritic`'s `hugeParam`/`rangeValCopy` checks
  are disabled in `.golangci.yml` so this stays idiomatic.
- Errors from `fmt.Fprint*` to stdout/stderr are intentionally ignored (see the
  errcheck exclusion in `.golangci.yml`).
- `GOTOOLCHAIN=auto` (set by the Makefile) lets the `toolchain` directive in
  `go.mod` fetch a patched Go on demand; bump that directive when a newer patch
  fixes a govulncheck finding.
- Tests are table-driven where practical; pure logic in `logs`/`target`/`render`
  is unit-tested without network.

## Testing, fuzzing & profiling

- **Coverage on PRs.** `make test` writes `coverage.out` with `main.go` filtered
  out (`COVER_EXCLUDE`) — the numbers reflect the `internal/` packages only, since
  `main.go` is a thin untested entrypoint. CI renders it with `make cover-report`
  into the job summary and one sticky PR comment, then `make cover-check` gates
  the build: it fails when total coverage is below `COVER_THRESHOLD` (80%).
- **Fuzzing.** `FuzzXxx` targets live next to the code (see
  `internal/logs/fuzz_test.go`, which fuzzes the log parsers). Seed corpora run
  as ordinary unit tests under `make test`; `make fuzz FUZZ=FuzzParse` does active
  mutation locally and the nightly `fuzz.yml` workflow runs `make fuzz-all`
  (auto-discovers every target). Commit any minimized crasher under
  `testdata/fuzz/<FuzzXxx>/` as a regression seed, then fix the bug.
- **Benchmarks & profiling.** Use the modern `for b.Loop() { … }` form with
  `b.ReportAllocs()` (see `internal/logs/bench_test.go`). `make bench` runs them;
  `make profile BENCH=…` captures CPU+mem profiles and `make pprof-cpu`/`pprof-mem`
  open them.

## Distribution

- Tag-triggered `release.yml` runs GoReleaser: multi-platform builds, a cosign
  keyless signature over `checksums.txt`, an SPDX SBOM per archive (syft), an
  SLSA build-provenance attestation, and the Homebrew cask push to
  `justanotherspy/homebrew-tap`. `docker.yml` builds/pushes a multi-arch image to
  GHCR (cosign-signed + provenance). Versioning stays `git describe`-derived
  (injected into `internal/cli.version`); there is no `VERSION` file.
