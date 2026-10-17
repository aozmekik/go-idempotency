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
- Request fingerprint over method, path, query, selected headers and the SHA-256 of the body.
- In-flight locks with a renewed lease and a fencing token; configurable TTL for stored
  responses.
- Storage policy (`ShouldStore`, default: everything below 500; panics count as 500),
  response header allowlist (`Set-Cookie` excluded unless listed, hop-by-hop headers
  rejected), request and response size limits.
- Per-caller key scopes (`Config.Scope`) and `KeyFromContext`.
- RFC 9457 problem details for every rejection, with a pluggable `ProblemWriter`.
- `memstore`: sharded in-memory store with TTL cleanup and an injectable clock.
- `storetest`: exported conformance suite for `Store` implementations.
- `redisstore` module: Redis store using go-redis v9 and Lua scripts.
- `pgstore` module: PostgreSQL store using pgx v5, with `schema.sql`, `Migrate` and
  `DeleteExpired`.
- `examples/payments`: demo API with a curl walkthrough.
