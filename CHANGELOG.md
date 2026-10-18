# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Before 1.0.0, minor versions may contain breaking changes.

## [Unreleased]

## [0.1.0] - 2026-10-18

First release. Not yet tagged or published.

### Added

- `idempotency` middleware implementing draft-ietf-httpapi-idempotency-key-header-07:
  400 for missing or malformed keys, 409 while the original request is in flight, 422 when a
  key is reused for a different request, and replay of stored responses with
  `Idempotent-Replayed: true`.
- RFC 9651 Structured Field Item parser for the key, checked against the HTTP WG
  structured-field-tests vectors; `ParseKey` and `FormatKey`.
- Request fingerprint over method, host, path, query, selected headers and the SHA-256 of the
  body, with an independent Python implementation in `testdata/fingerprint.py`.
- In-flight locks with a fencing token and a lease that is renewed while the handler runs;
  failed renewals are retried against the lease deadline. Every store call is bounded by the
  lease. Configurable TTL for stored responses.
- Storage policy (`ShouldStore`; by default everything except 5xx, 401, 403, 408, 409, 423,
  425 and 429; panics count as 500), response header allowlist (`Set-Cookie` excluded unless
  listed, hop-by-hop headers rejected), request and response size limits.
- Per-caller key scopes (`Config.Scope`) and `KeyFromContext`.
- RFC 9457 problem details for every rejection, with a pluggable `ProblemWriter`;
  `Retry-After` on 409 and 503.
- `memstore`: sharded in-memory store with TTL cleanup and an injectable clock.
- `storetest`: exported conformance suite for `Store` implementations.
- `redisstore` module: Redis store using go-redis v9 and Lua scripts.
- `pgstore` module: PostgreSQL store using pgx v5, with `schema.sql`, `Migrate` (safe to run
  from several instances at once) and `DeleteExpired`.
- `examples/payments`: demo API with a curl walkthrough.
