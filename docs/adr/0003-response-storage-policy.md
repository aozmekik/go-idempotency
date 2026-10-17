# ADR 0003: What is stored, when, and what happens on failure

- Status: accepted
- Date: 2026-10-18

## Context

Once a request holds a key, the middleware runs the handler and must decide
what to keep for replays. The draft says a duplicate of a completed request
should get the original result, "whether success or error" (draft-07,
Section 2.6). Real services also have to deal with transient server errors,
handlers that panic or stream, large responses, headers that must not be
repeated, and a store that can fail.

## Decisions

### Capture: write-through, snapshot at `WriteHeader`

The response is passed to the client as the handler writes it, and captured on
the side. The alternative, buffering the whole response until it is stored,
would break streaming and flushing handlers and add latency for no gain: the
side effects have already happened either way. The headers are snapshotted at
the moment the status is written, because that is what the client received;
later changes to the header map are not sent by `net/http` and are not stored.
Informational responses (1xx, for example 103 Early Hints) pass through and
are not treated as the result.

### Status policy: store everything except 5xx by default

`DefaultShouldStore` stores every final status below 500. This **deviates from
the draft's "whether success or error"** on purpose: a 502, 503 or 504 often
means the request did not take effect, and replaying it would turn a transient
fault into a permanent one for that key. 4xx responses are stored, because
they are a deterministic answer to that request. APIs that prefer strict
at-most-once execution can store everything with
`ShouldStore: func(int) bool { return true }`.

A handler that panics produces no response, and its outcome is unknown. It is
treated as a 500: with the default policy the key is released so a retry can
run; if `ShouldStore(500)` is true, a `request-failed` problem is stored so the
handler never runs twice for that key.

### Headers: allowlist only

Only headers in `Config.ReplayHeaders` are stored. The default list is
`Content-Type`, `Content-Encoding`, `Content-Language`, `Content-Location`,
`Location`, `ETag`, `Last-Modified`, `Link` and `Vary`. `Set-Cookie` is not
in it: replaying a session cookie to whoever presents the same key is rarely
intended, and the original client already received it. Adding it to the list
is the explicit opt-in. Hop-by-hop headers (RFC 9110 Section 7.6.1, plus the
legacy `Keep-Alive` and `Proxy-Connection`) describe one connection, so `New`
rejects them, and headers that a response nominates as hop-by-hop through its
`Connection` field are dropped from the snapshot. `Content-Length` is
recomputed on replay. Replays add `Idempotent-Replayed: true`, a convention
from existing payment APIs; it is not part of the draft.

### Size limits

The request body is read into memory to be hashed, so it is bounded by
`MaxBodyBytes` (1 MiB, 413 beyond). The captured response is bounded by
`MaxResponseBytes` (1 MiB). A larger response still reaches the original
client, but is not stored. Releasing the key in that case would let a retry
run the handler again, so the middleware stores a `response-not-replayable`
problem instead. At-most-once execution is preserved; the replay is less
useful, and clients are told why.

### Responses that cannot be captured

A hijacked connection or a 101 Switching Protocols response cannot be
replayed, so the key is released.

### Store failures

- **Acquire fails: fail closed.** The middleware answers 503
  (`store-unavailable`) and does not run the handler. Running it without a
  claim would silently drop the guarantee the API advertises. A fail-open
  option is on the roadmap for APIs that prefer availability.
- **Complete or Release fails after the handler ran:** the response has
  already gone to the client, so the error is logged. The in-flight record
  stays until its lease expires (a duplicate meanwhile gets 409); after that
  a retry runs the handler again. This is the window ADR 0001 describes.
- **Client disconnects:** store calls after the handler starts use
  `context.WithoutCancel`, so a disconnect does not stop the result from being
  recorded.

## Consequences

- A retry after a 5xx re-executes the handler by default; handlers that can
  fail after a side effect should either return a non-5xx status for that case
  or switch to a store-everything policy.
- Large-response endpoints need a larger `MaxResponseBytes`, or they lose
  useful replays (never correctness).
- Responses carrying a replayed `Content-Encoding` depend on the original
  request's `Accept-Encoding`; fingerprint that header if compression is
  negotiated per request.
