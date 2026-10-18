// Package idempotency is net/http middleware that implements the
// Idempotency-Key HTTP header field, as specified by the IETF HTTPAPI working
// group draft "The Idempotency-Key HTTP Header Field"
// (draft-ietf-httpapi-idempotency-key-header-07).
//
// A client sends a unique key with a non-idempotent request (POST, PATCH):
//
//	POST /payments HTTP/1.1
//	Idempotency-Key: "8e03978e-40d5-43e8-bc93-6894a57f9324"
//
// The first request with a key runs the handler, and its response is stored.
// A retry with the same key and the same request content gets the stored
// response back, marked with "Idempotent-Replayed: true", without running the
// handler again. The middleware answers with RFC 9457 problem details when:
//
//   - the key is missing on a request that requires one (400),
//   - the key is not a valid Structured Field String (400),
//   - the original request is still in flight (409),
//   - the key was used before for a request with a different fingerprint (422).
//
// Requests are compared by fingerprint: a SHA-256 over the method, host,
// path, query, selected headers and the SHA-256 of the body. In-flight
// requests hold a lock with a lease that is renewed while the handler runs,
// so a crashed process cannot block a key for longer than one lease.
//
// Records live in a [Store]. The memstore package provides an in-process
// store; Redis and PostgreSQL stores are separate modules
// (github.com/aozmekik/go-idempotency/redisstore and .../pgstore) so that
// this module has no third-party dependencies.
package idempotency
