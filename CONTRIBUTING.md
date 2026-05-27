# Contributing

Thanks for taking the time to contribute! This document covers the basics for
working in this repository.

## Getting started

```sh
make lint-install   # install the pinned golangci-lint (one time)
make build          # build the ./shuck binary
make test           # go test -race ./...
```

## Development workflow

1. Create a topic branch off `main`.
2. Make your change. Add or update tests where it makes sense.
3. Format, vet, lint, and test locally before pushing:

   ```sh
   make fmt    # gofmt -w .
   make vet    # go vet ./...
   make lint   # golangci-lint run ./...
   make test   # tests with the race detector
   ```

4. Run `make tidy` if you touched dependencies, and keep `go.mod`/`go.sum`
   tidy.
5. Open a pull request, fill out the template, and make sure CI is green.

## Commit messages

Use clear, present-tense messages. Conventional-commit prefixes (`feat:`,
`fix:`, `docs:`, `chore:`, …) are encouraged — release-drafter uses them to
group changelog entries and pick the next version.

## Code style & conventions

- Standard-library `flag` for CLI parsing (no cobra). Add new subcommands under
  `internal/cli/`.
- Keep `internal/model` dependency-free to avoid import cycles.
- Keep `make lint` clean; format with `make fmt` before committing.
- See [CLAUDE.md](CLAUDE.md) for the full architecture and conventions.

## Reporting bugs & requesting features

Open an issue using one of the [issue templates][issues]. For security
vulnerabilities, follow the [security policy](SECURITY.md) instead of opening a
public issue.

[issues]: https://github.com/justanotherspy/shuck/issues/new/choose
