package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"time"
)

// ErrLockLost is returned by [Store.Extend], [Store.Complete] and
// [Store.Release] when the caller's token no longer owns the key: its lease
// expired (and another request may have claimed the key since), or the record
// was already completed or released.
var ErrLockLost = errors.New("idempotency: lock not held")

// Fingerprint is a SHA-256 digest that identifies the content of a request.
// Two requests that reuse a key must have equal fingerprints. See
// docs/adr/0001-fingerprint-and-lock-semantics.md for what goes into it.
type Fingerprint [sha256.Size]byte

// String returns the fingerprint in lowercase hexadecimal.
func (f Fingerprint) String() string { return hex.EncodeToString(f[:]) }

// State is the lifecycle state of a stored [Record].
type State uint8

const (
	// StateInFlight means a request holds the key and its handler is running.
	StateInFlight State = iota + 1
	// StateCompleted means the handler finished and its response is stored.
	StateCompleted
)

// String returns a human-readable name for the state.
func (s State) String() string {
	switch s {
	case StateInFlight:
		return "in-flight"
	case StateCompleted:
		return "completed"
	default:
		return "unknown"
	}
}

// Response is a stored HTTP response that can be replayed.
type Response struct {
	// StatusCode is the final HTTP status code the handler wrote.
	StatusCode int
	// Header holds only the replayable headers (see Config.ReplayHeaders).
	Header http.Header
	// Body is the complete response body.
	Body []byte
}

// Record is the state stored under an idempotency key.
type Record struct {
	// State tells whether the original request is still running.
	State State
	// Fingerprint is the fingerprint of the request that claimed the key.
	Fingerprint Fingerprint
	// Response is the stored response; it is non-nil only when State is
	// StateCompleted.
	Response *Response
}

// Acquisition is the result of [Store.Acquire].
type Acquisition struct {
	// Acquired reports whether the caller now owns the key's in-flight lock.
	// The owner must eventually call Complete or Release with its token, or
	// let the lease expire.
	Acquired bool
	// Existing is the live record already stored under the key. It is only
	// meaningful when Acquired is false.
	Existing Record
}

// Store persists idempotency records.
//
// Implementations must be safe for concurrent use, and Acquire must be atomic
// with respect to every other caller sharing the store, including other
// processes. Records whose lease (in flight) or TTL (completed) has passed
// must behave exactly as if they did not exist. Tokens act as fencing tokens:
// an operation presenting a token that does not own the key's current
// in-flight record must fail with [ErrLockLost] and change nothing.
//
// Keys passed by [Middleware] consist of printable ASCII (0x20-0x7E): the
// client's key prefixed with the quoted scope. Tokens are random base32
// strings.
//
// The storetest package provides a conformance suite for implementations.
type Store interface {
	// Acquire claims key for a new request. If no live record exists, it
	// stores an in-flight record with fingerprint fp, owned by token and
	// expiring after lease, and returns Acquired true. Otherwise it returns
	// Acquired false and a copy of the existing record.
	Acquire(ctx context.Context, key, token string, fp Fingerprint, lease time.Duration) (Acquisition, error)

	// Extend resets the lease of the in-flight record owned by token so that
	// it expires lease from now.
	Extend(ctx context.Context, key, token string, lease time.Duration) error

	// Complete replaces the in-flight record owned by token with a completed
	// record holding resp, which expires ttl from now.
	Complete(ctx context.Context, key, token string, resp *Response, ttl time.Duration) error

	// Release deletes the in-flight record owned by token, so that the next
	// request with the same key runs the handler again.
	Release(ctx context.Context, key, token string) error
}
