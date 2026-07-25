# Contributing

Thanks for taking the time to contribute! This document covers the basics for
working in this repository.

## Getting started

```sh
make tools          # install the pinned dev tools (lint, releaser, gopls…)
make build          # build the ./bin/shuck binary
make test           # tests with the race detector + coverage
make ci             # run the full pipeline CI runs
```

Run `make help` to see every target. `make hooks` installs the pre-commit /
pre-push hooks (mirroring the CI guardrails) if you use [pre-commit][pc].

[pc]: https://pre-commit.com

## Development workflow

1. Create a topic branch off `main`.
2. Make your change. Add or update tests where it makes sense.
3. Format, vet, lint, and test locally before pushing:

   ```sh
   make fmt    # gofmt + goimports via golangci-lint
   make vet    # go vet ./...
   make lint   # golangci-lint run
   make test   # tests with the race detector
   make vuln   # govulncheck vulnerability scan
   ```

4. Run `make tidy` if you touched dependencies, and keep `go.mod`/`go.sum`
   tidy.
5. Open a pull request, fill out the template, and make sure CI is green.

## Commit messages

Use clear, present-tense messages. Conventional-commit prefixes (`feat:`,
`fix:`, `docs:`, `chore:`, …) are encouraged.

The changelog and the next version number come from a pull request's **labels**,
not its commit prefixes — see [Releasing](#releasing). Label your PR, or the
release draft files it under the default patch bump.

## Code style & conventions

- Standard-library `flag` for CLI parsing (no cobra). Add new subcommands under
  `internal/cli/`.
- Keep `internal/model` dependency-free to avoid import cycles.
- Keep `make lint` clean; format with `make fmt` before committing.
- See [CLAUDE.md](CLAUDE.md) for the full architecture and conventions.

## Releasing

There is no `VERSION` file and no release commit. A release is a draft that
fills itself in as you merge, and publishing that draft is the whole trigger.

**1. The draft accumulates.** `release-drafter.yml` runs on every push to `main`
and rewrites a single draft release. Its notes and its version both come from
the merged PRs' **labels**, not from commit messages:

| Label | Next version |
| --- | --- |
| `major` | `X+1.0.0` |
| `minor`, `feature`, `enhancement` | `X.Y+1.0` |
| `fix`, `bug`, `chore`, `refactor`, `dependencies`, `docs`, `documentation` | `X.Y.Z+1` |
| *(none of the above)* | patch, by default |

Because the draft is regenerated on each push, hand-editing its body only
survives if nothing else merges before you publish. Edit it when you are ready
to ship, not before.

**2. Publishing the draft is the release.** Publish it **as a pre-release**.
GitHub creates the tag at that moment, and the tag push — not the publish — is
what starts `release.yml`.

**3. `release.yml` does the rest**, gated behind the `release` environment:

- GoReleaser builds every platform, signs `checksums.txt` with keyless cosign,
  attaches SPDX SBOMs, and pushes the Homebrew cask to
  `justanotherspy/homebrew-tap`. It runs with `mode: keep-existing`, so the
  notes you curated on the draft are kept and only the artifacts are added.
- A SLSA build-provenance attestation is generated over `checksums.txt`.
- **Then, and only then, the release is promoted**: `gh release edit` clears the
  pre-release flag and marks it latest.
- Finally the GHCR image is built and pushed with the semver tags and `:latest`.

The pre-release window in step 2 is the point of the whole shape. `shuck upgrade`
and `install.sh` both resolve through GitHub's `/releases/latest`, which skips
pre-releases — so a release is invisible to them until its binaries, cask and
signatures are actually attached. If any step fails, the release simply stays a
pre-release and nobody is handed a half-published version. Do not publish
straight to "latest", and do not push a tag by hand: a hand-pushed tag makes
GoReleaser create the release itself, which loses the curated notes.

**4. Keep the plugin's version in step.** A release that changes what the plugin
needs means updating, in the same PR:

- `plugins/shuck/.claude-plugin/plugin.json` — the plugin's own `version`.
- `plugins/shuck/scripts/check-prereqs.sh` — `MIN_VERSION`, the oldest `shuck`
  binary whose features the plugin's hooks rely on. The `SessionStart` hook
  compares the installed binary against it and says so when it falls short.

The marketplace serves plugin content from the default branch while users
install the binary from a release, so those two can drift apart in a way nothing
else catches.

## Reporting bugs & requesting features

Open an issue using one of the [issue templates][issues]. For security
vulnerabilities, follow the [security policy](SECURITY.md) instead of opening a
public issue.

[issues]: https://github.com/justanotherspy/shuck/issues/new/choose
