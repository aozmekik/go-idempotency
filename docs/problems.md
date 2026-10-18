# Problem types

The middleware answers rejected requests with [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457)
problem details (`Content-Type: application/problem+json`). The `type` member
is `Config.ProblemTypeBase` followed by one of the slugs below; by default it
points at the matching section of this page, as the draft
([Section 2.7](https://datatracker.ietf.org/doc/html/draft-ietf-httpapi-idempotency-key-header-07#section-2.7))
suggests linking error responses to documentation.

```json
{
  "type": "https://github.com/aozmekik/go-idempotency/blob/main/docs/problems.md#key-reused",
  "title": "Idempotency key reused with a different request",
  "status": 422,
  "detail": "This key was already used for a request with a different method, host, path, query, headers or body. Use a new key for a new request."
}
```

Every problem response also carries `Cache-Control: no-store`, and the two
that ask the client to retry with the same key (`request-in-flight` and
`store-unavailable`) carry `Retry-After: 1` as a hint. Titles name no header,
so they stay correct when `Config.HeaderName` is customised; the `detail`
names the configured header. Replace the whole format with
`Config.ProblemWriter` if your API has its own error envelope; `Problem.Slug`
tells the writer which case it is handling, and the `Retry-After` header is
already set when it is called.

| Slug | Status | Draft | Client should |
| --- | --- | --- | --- |
| [`key-missing`](#key-missing) | 400 | Section 2.7 | add a key |
| [`key-invalid`](#key-invalid) | 400 | Section 2.1 | fix the header syntax |
| [`key-reused`](#key-reused) | 422 | Section 2.7 | use a new key for a new request |
| [`request-in-flight`](#request-in-flight) | 409 | Sections 2.6, 2.7 | retry later with the same key |
| [`body-too-large`](#body-too-large) | 413 | not covered | send a smaller body |
| [`body-unreadable`](#body-unreadable) | 400 | not covered | retry |
| [`store-unavailable`](#store-unavailable) | 503 | not covered | retry with the same key |
| [`response-not-replayable`](#response-not-replayable) | 500 | not covered | not retry blindly |
| [`request-failed`](#request-failed) | 500 | not covered | not retry blindly |

## key-missing

**400 Bad Request.** The route requires an `Idempotency-Key` header and the
request has none. `Config.KeyRequired` decides which requests require one.

## key-invalid

**400 Bad Request.** The header is present but is not a single
[RFC 9651](https://www.rfc-editor.org/rfc/rfc9651) Structured Field String,
the key is empty, or it is longer than `Config.MaxKeyLength`
(255 bytes by default). The key must be quoted:

```http
Idempotency-Key: "8e03978e-40d5-43e8-bc93-6894a57f9324"
```

An unquoted UUID is not a String and is rejected. Two header lines are
combined into a list, which is not a single Item, so they are rejected too.
Go clients can use `idempotency.FormatKey` to build the value.

## key-reused

**422 Unprocessable Content.** The key was already used, within its retention
period, for a request with a different fingerprint: a different method, path,
host, query, selected header values (`Config.FingerprintHeaders`) or body. The
handler did not run. This happens whether the earlier request is still in
flight or has completed.

## request-in-flight

**409 Conflict.** A request with this key and the same fingerprint is still
being processed. The handler did not run. Retry later with the same key
(`Retry-After` suggests one second): once the original completes, the retry
receives its stored response.

## body-too-large

**413 Content Too Large.** The request body exceeds `Config.MaxBodyBytes`
(1 MiB by default). The body has to be read into memory to be fingerprinted,
so the limit protects the server.

## body-unreadable

**400 Bad Request.** Reading the request body failed, usually because the
client disconnected mid-upload.

## store-unavailable

**503 Service Unavailable.** The middleware could not reach its store to claim
the key, so it did not run the handler: running it without the claim could
execute the request twice. Retrying with the same key is safe; `Retry-After`
suggests one second. The cause is logged through `Config.Logger`.

## response-not-replayable

**500 Internal Server Error** (stored and replayed with
`Idempotent-Replayed: true`). The original request completed with a status
below 400, which the detail names, but its response body exceeded
`Config.MaxResponseBytes` (1 MiB by default), so it was not stored. The
handler is not run again for this key; the request's effect already
happened. An oversized error response (400 or above) is not stored at all,
so a retry runs the handler again instead.

## request-failed

**500 Internal Server Error** (stored and replayed with
`Idempotent-Replayed: true`). The original handler panicked, so its outcome is
unknown. This is only stored when `Config.ShouldStore` returns true for 500,
that is, when the API has opted into at-most-once execution for server errors.
With the default policy a panic releases the key instead, and a retry runs the
handler again.
