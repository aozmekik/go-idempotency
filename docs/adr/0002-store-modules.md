# ADR 0002: Redis and PostgreSQL stores as nested modules

- Status: accepted
- Date: 2026-10-18

## Context

The middleware needs pluggable storage: an in-memory store for single
instances and tests, and shared stores (Redis, PostgreSQL) for services that
run more than one instance. The Redis store depends on
`github.com/redis/go-redis/v9` and, for tests, `github.com/alicebob/miniredis/v2`;
the PostgreSQL store depends on `github.com/jackc/pgx/v5`. A user of the
PostgreSQL store should not have Redis client libraries in their module graph,
and a user of the memory store should need neither.

## Options considered

1. **One module, stores as packages.** Simplest to version. Go only compiles
   the packages a consumer imports, but go-redis, pgx and their dependencies
   would still be part of every consumer's module graph (`go list -m all`)
   and `go.sum`, and tools that scan the module graph would report them.
2. **One module with build tags.** Hides code behind tags that users must
   remember, and does not change the dependency graph.
3. **Nested modules in this repository** (`redisstore/go.mod`,
   `pgstore/go.mod`). Each store is its own module with its own dependencies
   and its own version tags (`redisstore/v0.1.0`).
4. **Separate repositories.** The cleanest isolation, but changes to the
   `Store` contract would need coordinated releases across repositories.

## Decision

Option 3. The core module (`github.com/aozmekik/go-idempotency`) has no
dependencies outside the standard library and contains the middleware, the
`Store` interface, the memory store (`memstore`) and the conformance suite
(`storetest`). `redisstore` and `pgstore` are nested modules that require the
core module.

During development each nested module points at the core module in the same
checkout with `replace github.com/aozmekik/go-idempotency => ../`. Consumers
ignore replace directives, so a released store module resolves the core
version in its `require` line. Every store, including third-party ones, runs
the same `storetest.Run` suite, which is what keeps the implementations
consistent.

## Consequences

- `go get github.com/aozmekik/go-idempotency` brings in nothing but the
  standard library.
- Releases need one tag per module: `v0.1.0`, `redisstore/v0.1.0` and
  `pgstore/v0.1.0`. Until those tags exist, the store modules cannot be
  installed with `go get` (their `require` line names a core version that
  is not yet published); CONTRIBUTING.md lists the release steps.
- A store module's `require` line must be raised whenever it starts using a
  new core API, otherwise consumers would build it against an older core.
- CI lints and tests each module separately, and Dependabot is configured
  per module directory and told to leave the core requirement alone.
- go-redis v9.23.0 declares `go 1.26.0`, so `redisstore/go.mod` says
  `go 1.26.0`; the core module and `pgstore` say `go 1.26`.
