# ADR 0003: What is stored, when, and what happens on failure

- Status: accepted
- Date: 2026-10-18

## Context

Once a request holds a key, the middleware runs the handler and must decide
what to keep for replays. The draft says the resource SHOULD respond to a
duplicate of a completed request "with the result of the previously
completed operation, success or an error" (draft-07, Section 2.6), and
leaves other errors, "such as 401, 403, 500, 502, 503, 504, 429", to the
resource's own documentation (Section 2.7). Real services also have to deal
with transient server errors, handlers that panic or stream, large
responses, headers that must not be repeated, and a store that can fail.

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

### Status policy: store answers, not passing conditions

`DefaultShouldStore` stores every final response except those that describe
a condition the same request can outlive:

- **5xx.** A 502, 503 or 504 often means the request did not take effect,
  and replaying it would turn a transient fault into a permanent one for that
  key.
- **408, 409, 423, 425 and 429.** Request Timeout, Conflict, Locked, Too
  Early and Too Many Requests say "not now". HTTP tells the client to wait
  and repeat the request, and a client following the draft repeats it with
  the same key. Storing a 429 from a rate limiter inside the wrapped chain
  would answer that retry, and every later one, with the stored 429 for the
  whole TTL.
- **401 and 403.** They depend on credentials and permissions, which are not
  part of the fingerprint: a client that refreshes an expired token and
  retries with the same key must not get the stored 401.

This **deviates from the draft's "success or an error"** on purpose, and
these are exactly the kinds of status the draft leaves to the resource's
documentation. Other 4xx responses, including 400, 404 and 422, are stored:
they are the handler's answer to that request, and a client that fixes the
request sends a different fingerprint or a new key. An API whose 404s are
expected to turn into success later (say, a resource created
asynchronously) can exclude them with its own `ShouldStore`. APIs that
prefer strict at-most-once execution can store everything with
`ShouldStore: func(int) bool { return true }`.

Authentication and rate limiting belong in front of the middleware anyway:
`Config.Scope` needs the caller's identity, and their rejections should not
consume a key. The default policy only makes the other order less harmful.

A handler that panics produces no response, and its outcome is unknown. It is
treated as a 500: with the default policy the key is released so a retry can
run; if `ShouldStore(500)` is true, a `request-failed` problem is stored so the
handler never runs twice for that key.

### Headers: allowlist only

Only headers in `Config.ReplayHeaders` are stored. The default list holds
the headers that describe the content or the outcome: `Allow`,
`Cache-Control`, `Content-Disposition`, `Content-Encoding`,
`Content-Language`, `Content-Location`, `Content-Type`, `ETag`,
`Last-Modified`, `Link`, `Location`, `Retry-After`, `Vary`,
`WWW-Authenticate` and `X-Content-Type-Options`. It includes the headers
RFC 9110 requires for statuses the policy may store (`Allow` on 405,
`WWW-Authenticate` on 401) and the `nosniff` that `http.Error` sets, since a
replay without them would be a different response. `Set-Cookie` is not
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
client, but is not stored. For a success (status below 400), releasing the
key would let a retry run the handler again, so the middleware stores a
`response-not-replayable` problem that names the original status instead.
At-most-once execution is preserved; the replay is less useful, and clients
are told why. A larger error response is released instead: it says the
request was not carried out, so running the handler again is harmless, and
a stored 500 claiming the request was processed would be false. Body bytes
written after a 204 or 304 are not counted: `net/http` discards them
(`ErrBodyNotAllowed`), so they are not part of the response.

### Responses that cannot be captured

A hijacked connection or a 101 Switching Protocols response cannot be
replayed, so the key is released.

### Store failures

- **Acquire fails: fail closed.** The middleware answers 503
  (`store-unavailable`) and does not run the handler. Running it without a
  claim would silently drop the guarantee the API advertises. A fail-open
  option is on the roadmap for APIs that prefer availability.
- **Complete or Release fails after the handler ran:** the error is logged
  and the response is still delivered. It has been written, but it usually
  sits in `net/http`'s buffer until the handler returns, so the store call
  delays it: by one round trip normally, and by at most the remaining lease,
  which bounds the call, if the store stalls. Flushing first would avoid the
  wait but would switch small responses to chunked encoding. The in-flight
  record stays until its lease expires (a duplicate meanwhile gets 409);
  after that a retry runs the handler again. This is the window ADR 0001
  describes.
- **Client disconnects:** store calls after the handler starts use
  `context.WithoutCancel`, so a disconnect does not stop the result from being
  recorded.

## Consequences

- A retry after a 5xx, 401, 403, 408, 409, 423, 425 or 429 re-executes the
  handler by default; handlers that can fail with one of these after a side
  effect should either return another status for that case or switch to a
  store-everything policy.
- Large-response endpoints need a larger `MaxResponseBytes`, or they lose
  useful replays (never correctness).
- Responses carrying a replayed `Content-Encoding` depend on the original
  request's `Accept-Encoding`; fingerprint that header if compression is
  negotiated per request.
