# ADR 0001: Fingerprint and lock semantics

- Status: accepted
- Date: 2026-10-18

## Context

The middleware has to answer three questions for every request that carries
an `Idempotency-Key`:

1. **Is this the same request as the one that first used the key?** The
   draft says that an attempt to reuse a key "with a different request
   payload" SHOULD get a 422 (draft-07, Section 2.7), and allows, but does
   not require, an idempotency fingerprint for detecting it (Section 2.4).
2. **Is the first request still running?** A duplicate that arrives while the
   original is in progress SHOULD get a 409 (Sections 2.6 and 2.7), and the
   handler must not run twice.
3. **What if the first request never finishes?** The process handling it can
   crash or be killed. Its claim on the key must not block the key forever,
   but a slow request that is still running must not be mistaken for a dead
   one.

The store may be shared by many application instances (Redis, PostgreSQL),
so whatever we decide has to be atomic across processes, not just goroutines.

## Options considered

### Fingerprint

| Option | Detects | Problem |
| --- | --- | --- |
| A. No fingerprint, key only | nothing | a reused key silently replays the wrong response |
| B. Hash of the body only | changed payloads | a key reused on another endpoint (`POST /refunds` vs `POST /payments`, or the same path on another host) with the same body replays the wrong resource |
| C. Method, host, path, query, selected headers and body hash | changed payloads and changed targets | header selection needs care (see below) |
| D. Application-supplied canonical form (for example parsed JSON) | semantic changes only | needs per-endpoint code; a library cannot guess it |

### Lock

| Option | Problem |
| --- | --- |
| A. No lock; check, run, then store | two concurrent duplicates both see "no record" and both run the handler |
| B. Lock with no expiry | a crashed holder blocks the key until its TTL, often a day |
| C. Lock with a fixed lease | a handler that runs longer than the lease looks dead, and a duplicate runs it again |
| D. Lease renewed while the handler runs, plus a per-claim fencing token | more moving parts |

Duplicates of an in-flight request could also wait for the original to
finish (long polling) instead of getting 409. That ties up a connection and a
goroutine per duplicate, and the draft specifies 409.

## Decision

**Fingerprint: option C.** The fingerprint is SHA-256 over a versioned,
length-prefixed encoding of:

- the method,
- the host (`Request.Host`, lowercased),
- the escaped path and the raw query,
- the values of the headers listed in `Config.FingerprintHeaders`
  (canonical names, sorted, each with its value count), and
- the SHA-256 of the body.

Every field is prefixed with its length (uvarint), so bytes cannot move
between adjacent fields without changing the hash (`/ab?c` and `/a?bc`
differ; so do one header value `"1,2"` and two values `"1"`, `"2"`). The
encoding starts with a version string, so a future change cannot collide with
stored records; `TestFingerprintIsStable` pins the output to the digest that
[`testdata/fingerprint.py`](../../testdata/fingerprint.py), an independent
Python implementation, prints.

The host is part of the target, for the same reason as the path: one
middleware and store may serve several virtual hosts (for example Go 1.22
patterns such as `POST api.example.com/charges` and
`POST sandbox.api.example.com/charges`), and a key reused across them must
not replay the other host's response. `net/http` moves the `Host` header into
`Request.Host`, so it cannot be selected through `FingerprintHeaders`, and
`New` rejects it there. The host is compared case-insensitively but
otherwise as sent: `example.com` and `example.com:443` differ. The scheme is
not included: behind a TLS-terminating proxy, which is the common
deployment, the middleware cannot see it.

No headers are fingerprinted by default. Headers such as `User-Agent`,
`Date` or tracing headers legitimately change between retries and would turn
honest retries into 422s. APIs whose semantics depend on a header (for
example a tenant or API-version header) opt in.

Authentication is **not** part of the fingerprint. Folding the caller's
identity into the fingerprint would make a key collision between two callers
a 422, which leaks that the key exists. Instead `Config.Scope` puts each
caller in its own key space, as the draft's security considerations suggest
(Section 5: combine the client's key with attributes known only to the
resource). The storage key is `strconv.QuoteToASCII(scope) + ":" + key`;
quoting is injective, so scopes cannot collide through separators, and the
result is printable ASCII for every store.

**Lock: option D.** `Store.Acquire` atomically either creates an in-flight
record or returns the existing one ("lock or get"). The record carries:

- a **lease**: it expires `Config.Lease` (30 s) after the last renewal;
- a **token**: a 128-bit random string generated by the middleware for this
  claim.

While the handler runs, the middleware renews the lease every `Lease/3`. It
tracks the deadline by which the lease expires at the latest: the start of
the last confirmed `Acquire` or `Extend` plus `Lease` (the store set the
expiry after that moment). A renewal that fails or times out is retried
against that deadline:

- each attempt is bounded by half of the remaining lease, and by at most
  `Lease/6`, so that another attempt fits before the deadline even when this
  one hangs until its timeout;
- the next attempt starts after a quarter of the remaining lease, so
  attempts become more frequent as the deadline approaches;
- neither the timeout nor the pause drops below `Lease/100`, which keeps
  renewal from spinning; past the deadline, attempts return to the normal
  interval, since they can only confirm the loss.

The lease therefore lapses only if the holder stops renewing (the process
died or stalled) or the store stays unreachable for almost a whole lease.
`TestRenewalSurvivesStoreOutages` checks, on a virtual clock with a 30 s
lease, that renewal survives outages that end 1 s (immediate failures) or
2 s (attempts that hang until their timeout) before the deadline, and
`TestRenewalOutageLongerThanLeaseLosesLease` shows what happens beyond it.
An earlier version gave each attempt a full `Lease/3` timeout and retried
only on the normal schedule, so one hung renewal pushed the next attempt to
the moment the lease expired.

`Extend`, `Complete` and `Release` succeed only if the presented token still
owns the record; anything else returns `ErrLockLost` and changes nothing.
This is a fencing token: a holder that was presumed dead cannot overwrite the
record of the request that took over the key. Fencing is also why renewal
never delays the response: when the handler returns, a renewal in progress
is canceled rather than waited for, and the store sees the fenced `Extend`
and `Complete` safely in either order.

Every store call is bounded by the lease it serves. `Acquire` gets at most
`Lease/6`, so a stalled store produces a 503 rather than a hung request.
If `Acquire` fails, the store may still have applied the claim (the reply
was lost, or the client disconnected mid-call), so the middleware releases
its own token on a best-effort basis after sending the 503; otherwise a retry
would get 409 until the lease ran out. `Complete` and `Release` get what
remains of the lease: a later write would be rejected by the fence anyway,
and the response usually waits in `net/http`'s buffer until they return.

Expiry decisions are made by one clock where possible: Redis key TTLs (the
Redis server's clock) and PostgreSQL `now()` (the database's clock). Only the
memory store, which lives in one process, uses the process clock.

## Consequences

- Concurrent duplicates are safe: exactly one claim succeeds and the others
  get 409 (`TestConcurrentDuplicatesRunHandlerOnce` sends 64 concurrent
  requests over real HTTP; `storetest.ConcurrentAcquire` checks the same
  property for each store).
- A crashed holder blocks its key for at most one lease (30 s by default),
  after which a retry runs the handler (`TestCrashedHolderLeaseExpires`).
- A slow handler keeps its claim for as long as it runs
  (`TestLeaseIsRenewedWhileHandlerRuns`, which runs a handler for ten leases
  on a virtual clock).
- **This is not exactly-once.** If the store is unreachable for almost a
  whole lease while a handler runs, or the process stalls that long (for
  example a stopped VM), another request can take over the key and run the
  handler a second time. The fencing token stops the stale request from
  writing to the idempotency store, but it cannot undo side effects in other
  systems. When that matters, pass the key downstream (`KeyFromContext`, for
  example to a payment provider that is itself idempotent) or record the side
  effect and the idempotency record in one database transaction. The second
  option is on the roadmap for the PostgreSQL store.
- The store itself can lose records, with the same result. Redis can evict
  keys that have a TTL, as every record here does, under most
  `maxmemory-policy` settings, and an asynchronous-replication failover can
  drop recent writes. The Redis store therefore needs
  `maxmemory-policy noeviction` and makes no promise across a failover (see
  the `redisstore` package documentation).
- Each new key costs two store round trips (`Acquire`, `Complete`), plus one
  `Extend` per `Lease/3` for long handlers. A replay costs one round trip
  with Redis (the `Acquire` script returns the record) and two with
  PostgreSQL (the `INSERT ... ON CONFLICT` that finds a live row returns
  nothing, so a `SELECT` follows); the memory store makes no network calls.
- The original client receives its response only after `Complete` returns:
  normally one round trip, at most the remaining lease if the store stalls.
- Retries must be byte-identical in method, host (apart from case), path,
  query, selected headers and body. A client that re-serializes JSON with a
  different key order on retry gets 422. This is deliberate: the middleware
  cannot know which differences are semantic.
