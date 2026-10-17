# Contributing

Thanks for taking a look. Issues and pull requests are welcome; for anything larger than a
bug fix, please open an issue first so we can agree on the approach.

## Layout

The repository holds three Go modules:

| Directory | Module | Contents |
| --- | --- | --- |
| `.` | `github.com/aozmekik/go-idempotency` | middleware, `memstore`, `storetest`, `internal/sfv`, `examples/payments` |
| `redisstore/` | `github.com/aozmekik/go-idempotency/redisstore` | Redis store |
| `pgstore/` | `github.com/aozmekik/go-idempotency/pgstore` | PostgreSQL store |

The store modules use a `replace` directive to build against the core module in the same
checkout (see [ADR 0002](docs/adr/0002-store-modules.md)). `./...` does not cross module
boundaries, so run commands in each module directory.

## Requirements

- Go 1.26 or later (CI tests the two latest Go releases).
- [golangci-lint](https://golangci-lint.run/) v2.15 for linting.
- Optional: Redis and PostgreSQL servers for the store integration tests.

## Build and test

```sh
go build ./... && go test -race ./...                       # core module
(cd redisstore && go build ./... && go test -race ./...)    # uses miniredis
(cd pgstore && go build ./... && go test -race ./...)       # skips without a database
```

Integration tests against real servers run when these variables are set:

```sh
export IDEMPOTENCY_TEST_REDIS_ADDR=localhost:6379
export IDEMPOTENCY_TEST_PG_DSN='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable'
(cd redisstore && go test -race ./...)
(cd pgstore && go test -race ./...)
```

The PostgreSQL tests create the `idempotency_keys` table if it does not exist and write rows
with random key prefixes; use a throwaway database.

## Lint and format

```sh
golangci-lint run ./...                  # in each module directory; the root .golangci.yml applies
golangci-lint fmt ./...                  # gofumpt + goimports
go mod tidy -diff                        # in each module directory
actionlint                               # if you change .github/workflows
```

The lint configuration is strict on purpose; please fix findings rather than adding
`//nolint` directives. When a directive is unavoidable, it must name the linter and give a
reason.

## Fuzzing and conformance

```sh
go test -run '^$' -fuzz '^FuzzParseItem$' -fuzztime 60s ./internal/sfv
go test -run '^$' -fuzz '^FuzzParseKey$' -fuzztime 60s .
go test -run '^$' -fuzz '^FuzzFormatKey$' -fuzztime 60s .

git clone https://github.com/httpwg/structured-field-tests /tmp/sf-tests
SFV_TEST_VECTORS=/tmp/sf-tests go test -run TestVectors -v ./internal/sfv
```

## Benchmarks

```sh
go test -run '^$' -bench . -benchmem -count=10 .
(cd redisstore && IDEMPOTENCY_TEST_REDIS_ADDR=localhost:6379 go test -run '^$' -bench . -benchmem -count=10 .)
(cd pgstore && IDEMPOTENCY_TEST_PG_DSN=... go test -run '^$' -bench . -benchmem -count=10 .)
```

If you report numbers, include the command, the hardware, the Go version and how loaded the
machine was. Do not report miniredis numbers as Redis performance.

## Commits

Use [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`,
`test:`, `ci:`, `chore:`, `perf:`, `refactor:`), with the module as the scope where it helps
(`feat(pgstore): ...`). Keep commits focused, and add a CHANGELOG entry under
`[Unreleased]` for user-visible changes.

## Releasing

Each module is tagged separately. For version `vX.Y.Z`:

1. Move the `[Unreleased]` entries in CHANGELOG.md under a new `[X.Y.Z] - YYYY-MM-DD` heading.
2. In `redisstore/go.mod` and `pgstore/go.mod`, set `require github.com/aozmekik/go-idempotency vX.Y.Z`.
3. Tag the core module first, then the stores, all on the same commit:
   `git tag vX.Y.Z`, `git tag redisstore/vX.Y.Z`, `git tag pgstore/vX.Y.Z`.
4. Push the tags and check that `go get github.com/aozmekik/go-idempotency/pgstore@vX.Y.Z`
   works from an empty module.
