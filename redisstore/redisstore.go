// Package redisstore provides an [idempotency.Store] backed by Redis, using
// github.com/redis/go-redis/v9.
//
// Each record is a Redis hash under one key, so the store works with Redis
// Cluster. Every operation is a single Lua script, which Redis runs
// atomically: Acquire is "lock or get" in one round trip, and Extend,
// Complete and Release check the caller's token before changing anything.
// Expiry uses Redis key TTLs, so every application instance agrees on when a
// lease or a stored response expires: there is one clock, the Redis server's.
//
// # Deployment requirements
//
// The store's guarantees are only as strong as the Redis deployment's.
//
// Eviction: every record has a TTL, so it is a candidate for eviction under
// the volatile-* policies (volatile-lru is the default of several managed
// Redis services) as well as the allkeys-* ones. An evicted in-flight lock
// lets a duplicate run the handler while the original is still running; an
// evicted response lets a retry run it again. Configure the instance with
// maxmemory-policy noeviction, preferably dedicated to this store. A full
// instance then rejects writes instead: Acquire fails and the middleware
// answers 503, and a Complete that fails is logged and leaves that key in
// flight until its lease expires.
//
// Replication: Redis replicates asynchronously, so a lock or response
// acknowledged by the primary can be missing on a replica promoted by a
// failover, and a retry then runs the handler again. Token fencing does not
// help here, and WAIT narrows the window without closing it. Where a second
// execution after a failover is unacceptable, make the downstream effect
// idempotent as well (pass the key on, see idempotency.KeyFromContext) or
// keep the records in the same database as the effect.
package redisstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	idempotency "github.com/aozmekik/go-idempotency"
)

// DefaultPrefix is prepended to every Redis key the store writes.
const DefaultPrefix = "idempotency:"

// Hash fields. Single letters keep large key spaces compact.
//
//	s  state: "i" (in flight) or "c" (completed)
//	f  request fingerprint, 32 raw bytes
//	t  lock token; present only while in flight
//	c  status code
//	h  replayable headers, JSON object of arrays of base64 values (encodeHeader)
//	b  body
var (
	// acquireScript returns the existing record as {s, f, c, h, b}, or nil
	// after creating an in-flight record.
	// KEYS[1] record key; ARGV[1] token, ARGV[2] fingerprint, ARGV[3] lease ms.
	acquireScript = redis.NewScript(`
local rec = redis.call('HMGET', KEYS[1], 's', 'f', 'c', 'h', 'b')
if rec[1] then
  return rec
end
redis.call('HSET', KEYS[1], 's', 'i', 'f', ARGV[2], 't', ARGV[1])
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return false
`)

	// extendScript resets the TTL if the token owns the record.
	// KEYS[1] record key; ARGV[1] token, ARGV[2] lease ms.
	extendScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 't') ~= ARGV[1] then
  return 0
end
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return 1
`)

	// completeScript stores the response if the token owns the record.
	// KEYS[1] record key; ARGV[1] token, ARGV[2] status, ARGV[3] headers,
	// ARGV[4] body, ARGV[5] ttl ms.
	completeScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 't') ~= ARGV[1] then
  return 0
end
redis.call('HDEL', KEYS[1], 't')
redis.call('HSET', KEYS[1], 's', 'c', 'c', ARGV[2], 'h', ARGV[3], 'b', ARGV[4])
redis.call('PEXPIRE', KEYS[1], ARGV[5])
return 1
`)

	// releaseScript deletes the record if the token owns it.
	// KEYS[1] record key; ARGV[1] token.
	releaseScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 't') ~= ARGV[1] then
  return 0
end
redis.call('DEL', KEYS[1])
return 1
`)
)

// Option configures a [Store].
type Option func(*Store)

// WithPrefix sets the prefix of every Redis key the store writes.
// Default: [DefaultPrefix].
func WithPrefix(prefix string) Option {
	return func(s *Store) { s.prefix = prefix }
}

// Store is a Redis-backed [idempotency.Store]. It is safe for concurrent use.
type Store struct {
	client redis.UniversalClient
	prefix string
}

var _ idempotency.Store = (*Store)(nil)

// New returns a Store that uses client. The caller owns the client and is
// responsible for closing it.
func New(client redis.UniversalClient, opts ...Option) *Store {
	s := &Store{client: client, prefix: DefaultPrefix}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Acquire implements [idempotency.Store].
func (s *Store) Acquire(ctx context.Context, key, token string, fp idempotency.Fingerprint, lease time.Duration) (idempotency.Acquisition, error) {
	reply, err := acquireScript.Run(ctx, s.client, []string{s.prefix + key}, token, fp[:], millis(lease)).Slice()
	if errors.Is(err, redis.Nil) {
		return idempotency.Acquisition{Acquired: true}, nil
	}
	if err != nil {
		return idempotency.Acquisition{}, fmt.Errorf("redisstore: acquire: %w", err)
	}
	rec, err := decodeRecord(reply)
	if err != nil {
		return idempotency.Acquisition{}, fmt.Errorf("redisstore: acquire: key %q: %w", key, err)
	}
	return idempotency.Acquisition{Existing: rec}, nil
}

// Extend implements [idempotency.Store].
func (s *Store) Extend(ctx context.Context, key, token string, lease time.Duration) error {
	return s.fenced(ctx, "extend", extendScript, key, token, millis(lease))
}

// Complete implements [idempotency.Store].
func (s *Store) Complete(ctx context.Context, key, token string, resp *idempotency.Response, ttl time.Duration) error {
	header, err := encodeHeader(resp.Header)
	if err != nil {
		return fmt.Errorf("redisstore: complete: encoding headers: %w", err)
	}
	return s.fenced(ctx, "complete", completeScript, key, token, resp.StatusCode, header, resp.Body, millis(ttl))
}

// Release implements [idempotency.Store].
func (s *Store) Release(ctx context.Context, key, token string) error {
	return s.fenced(ctx, "release", releaseScript, key, token)
}

// fenced runs a script that returns 1 when the token owned the record and 0
// otherwise.
func (s *Store) fenced(ctx context.Context, op string, script *redis.Script, key, token string, args ...any) error {
	n, err := script.Run(ctx, s.client, []string{s.prefix + key}, append([]any{token}, args...)...).Int()
	if err != nil {
		return fmt.Errorf("redisstore: %s: %w", op, err)
	}
	if n != 1 {
		return idempotency.ErrLockLost
	}
	return nil
}

// millis converts d to whole milliseconds for PEXPIRE, rounding up so that
// a positive duration never becomes 0 (which would delete the key).
func millis(d time.Duration) int64 {
	ms := (d + time.Millisecond - 1) / time.Millisecond
	return max(int64(ms), 1)
}

var errCorrupt = errors.New("corrupt record")

func decodeRecord(reply []any) (idempotency.Record, error) {
	if len(reply) != 5 {
		return idempotency.Record{}, fmt.Errorf("%w: %d fields", errCorrupt, len(reply))
	}
	field := func(i int) (string, bool) {
		v, ok := reply[i].(string)
		return v, ok
	}
	state, _ := field(0)
	fp, ok := field(1)
	if !ok || len(fp) != len(idempotency.Fingerprint{}) {
		return idempotency.Record{}, fmt.Errorf("%w: bad fingerprint", errCorrupt)
	}
	var rec idempotency.Record
	copy(rec.Fingerprint[:], fp)
	switch state {
	case "i":
		rec.State = idempotency.StateInFlight
		return rec, nil
	case "c":
		rec.State = idempotency.StateCompleted
	default:
		return idempotency.Record{}, fmt.Errorf("%w: unknown state %q", errCorrupt, state)
	}
	statusText, _ := field(2)
	status, err := strconv.Atoi(statusText)
	if err != nil {
		return idempotency.Record{}, fmt.Errorf("%w: bad status %q", errCorrupt, statusText)
	}
	headerJSON, _ := field(3)
	header, err := decodeHeader([]byte(headerJSON))
	if err != nil {
		return idempotency.Record{}, fmt.Errorf("%w: bad headers: %w", errCorrupt, err)
	}
	body, _ := field(4)
	rec.Response = &idempotency.Response{StatusCode: status, Header: header, Body: []byte(body)}
	return rec, nil
}

// encodeHeader serializes h as a JSON object that maps each field name to
// its values, base64-encoded (encoding/json's encoding of []byte). Values are
// bytes, not text: HTTP allows obs-text (0x80-0xFF) in field values and
// net/http sends control bytes through, but a JSON string would replace
// invalid UTF-8 with U+FFFD. Names are tokens, so they are plain ASCII.
func encodeHeader(h http.Header) ([]byte, error) {
	raw := make(map[string][][]byte, len(h))
	for name, values := range h {
		vs := make([][]byte, len(values))
		for i, v := range values {
			vs[i] = []byte(v)
		}
		raw[name] = vs
	}
	return json.Marshal(raw)
}

// decodeHeader reverses encodeHeader.
func decodeHeader(data []byte) (http.Header, error) {
	var raw map[string][][]byte
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	h := make(http.Header, len(raw))
	for name, values := range raw {
		vs := make([]string, len(values))
		for i, v := range values {
			vs[i] = string(v)
		}
		h[name] = vs
	}
	return h, nil
}
