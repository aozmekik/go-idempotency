// Package pgstore provides an [idempotency.Store] backed by PostgreSQL,
// using github.com/jackc/pgx/v5.
//
// Records live in the idempotency_keys table defined in schema.sql (also
// available as [Schema], or applied with [Store.Migrate]). Acquire is a
// single INSERT ... ON CONFLICT DO UPDATE ... WHERE statement that either
// inserts an in-flight row or takes over an expired one; the primary key
// makes it atomic across any number of application instances. Extend,
// Complete and Release are conditional UPDATE/DELETE statements that match
// the caller's lock token.
//
// By default every expiry decision uses the database clock (now()), so
// application servers with skewed clocks still agree. [WithClock] replaces
// it with an application clock, which tests use to move time forward.
package pgstore

import (
	"context"
	_ "embed" // for Schema
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	idempotency "github.com/aozmekik/go-idempotency"
)

// Schema is the DDL that creates the idempotency_keys table and its index.
// It is idempotent (CREATE ... IF NOT EXISTS). Projects that manage
// migrations with their own tool should copy it into a migration.
//
//go:embed schema.sql
var Schema string

// DB is the subset of pgx used by the store. *pgxpool.Pool and *pgx.Conn
// implement it; use a pool, because the middleware calls the store
// concurrently.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Option configures a [Store].
type Option func(*Store)

// WithClock makes the store compute expiry from now instead of the
// database's now(). Intended for tests; in production the database clock is
// the safer single source of time.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// Store is a PostgreSQL-backed [idempotency.Store]. It is safe for
// concurrent use if its DB is.
type Store struct {
	db  DB
	now func() time.Time // nil: use the database clock
}

var _ idempotency.Store = (*Store)(nil)

// New returns a Store that uses db. The table must exist; see [Schema] and
// [Store.Migrate].
func New(db DB, opts ...Option) *Store {
	s := &Store{db: db}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Migrate creates the table and index if they do not exist.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.Exec(ctx, Schema); err != nil {
		return fmt.Errorf("pgstore: migrate: %w", err)
	}
	return nil
}

// clock returns the timestamp parameter for a statement: nil makes the SQL
// fall back to now() through COALESCE.
func (s *Store) clock() *time.Time {
	if s.now == nil {
		return nil
	}
	t := s.now()
	return &t
}

// SQL statements. $now is always the last parameter and may be NULL, in
// which case the database clock is used. Durations are passed in
// microseconds, PostgreSQL's timestamp resolution.
const (
	acquireSQL = `
INSERT INTO idempotency_keys AS k (key, fingerprint, lock_token, expires_at)
VALUES ($1, $2, $3, COALESCE($5::timestamptz, now()) + $4::bigint * interval '1 microsecond')
ON CONFLICT (key) DO UPDATE
SET fingerprint = EXCLUDED.fingerprint,
    lock_token  = EXCLUDED.lock_token,
    status_code = NULL,
    header      = NULL,
    body        = NULL,
    expires_at  = EXCLUDED.expires_at
WHERE k.expires_at <= COALESCE($5::timestamptz, now())
RETURNING true`

	selectSQL = `
SELECT fingerprint, lock_token IS NOT NULL, status_code, header, body
FROM idempotency_keys
WHERE key = $1 AND expires_at > COALESCE($2::timestamptz, now())`

	extendSQL = `
UPDATE idempotency_keys
SET expires_at = COALESCE($4::timestamptz, now()) + $3::bigint * interval '1 microsecond'
WHERE key = $1 AND lock_token = $2 AND expires_at > COALESCE($4::timestamptz, now())`

	completeSQL = `
UPDATE idempotency_keys
SET lock_token  = NULL,
    status_code = $3,
    header      = $4,
    body        = $5,
    expires_at  = COALESCE($7::timestamptz, now()) + $6::bigint * interval '1 microsecond'
WHERE key = $1 AND lock_token = $2 AND expires_at > COALESCE($7::timestamptz, now())`

	releaseSQL = `
DELETE FROM idempotency_keys
WHERE key = $1 AND lock_token = $2 AND expires_at > COALESCE($3::timestamptz, now())`

	deleteExpiredSQL = `
DELETE FROM idempotency_keys
WHERE expires_at <= COALESCE($1::timestamptz, now())`
)

// maxAcquireAttempts bounds the insert-then-select loop in Acquire. A retry
// is only needed when the conflicting row disappears (released or expired)
// between the two statements.
const maxAcquireAttempts = 3

// Acquire implements [idempotency.Store].
func (s *Store) Acquire(ctx context.Context, key, token string, fp idempotency.Fingerprint, lease time.Duration) (idempotency.Acquisition, error) {
	for range maxAcquireAttempts {
		var inserted bool
		err := s.db.QueryRow(ctx, acquireSQL, key, fp[:], token, lease.Microseconds(), s.clock()).Scan(&inserted)
		if err == nil {
			return idempotency.Acquisition{Acquired: true}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return idempotency.Acquisition{}, fmt.Errorf("pgstore: acquire: %w", err)
		}
		// A live row holds the key; read it.
		rec, found, err := s.load(ctx, key)
		if err != nil {
			return idempotency.Acquisition{}, err
		}
		if found {
			return idempotency.Acquisition{Existing: rec}, nil
		}
	}
	return idempotency.Acquisition{}, fmt.Errorf("pgstore: acquire: key %q changed state %d times in a row", key, maxAcquireAttempts)
}

func (s *Store) load(ctx context.Context, key string) (rec idempotency.Record, found bool, err error) {
	var (
		fp       []byte
		inFlight bool
		status   *int32
		header   []byte
		body     []byte
	)
	err = s.db.QueryRow(ctx, selectSQL, key, s.clock()).Scan(&fp, &inFlight, &status, &header, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return idempotency.Record{}, false, nil
	}
	if err != nil {
		return idempotency.Record{}, false, fmt.Errorf("pgstore: acquire: reading existing record: %w", err)
	}
	if len(fp) != len(rec.Fingerprint) {
		return idempotency.Record{}, false, fmt.Errorf("pgstore: acquire: key %q: fingerprint has %d bytes", key, len(fp))
	}
	copy(rec.Fingerprint[:], fp)
	if inFlight {
		rec.State = idempotency.StateInFlight
		return rec, true, nil
	}
	if status == nil { // excluded by the table's CHECK constraint
		return idempotency.Record{}, false, fmt.Errorf("pgstore: acquire: key %q: completed record without status", key)
	}
	var h http.Header
	if len(header) > 0 {
		if err := json.Unmarshal(header, &h); err != nil {
			return idempotency.Record{}, false, fmt.Errorf("pgstore: acquire: key %q: decoding headers: %w", key, err)
		}
	}
	rec.State = idempotency.StateCompleted
	rec.Response = &idempotency.Response{StatusCode: int(*status), Header: h, Body: body}
	return rec, true, nil
}

// Extend implements [idempotency.Store].
func (s *Store) Extend(ctx context.Context, key, token string, lease time.Duration) error {
	return s.fenced(ctx, "extend", extendSQL, key, token, lease.Microseconds(), s.clock())
}

// Complete implements [idempotency.Store].
func (s *Store) Complete(ctx context.Context, key, token string, resp *idempotency.Response, ttl time.Duration) error {
	header, err := json.Marshal(resp.Header)
	if err != nil {
		return fmt.Errorf("pgstore: complete: encoding headers: %w", err)
	}
	body := resp.Body
	if body == nil {
		body = []byte{} // keep "empty body" distinct from SQL NULL
	}
	return s.fenced(ctx, "complete", completeSQL, key, token, resp.StatusCode, header, body, ttl.Microseconds(), s.clock())
}

// Release implements [idempotency.Store].
func (s *Store) Release(ctx context.Context, key, token string) error {
	return s.fenced(ctx, "release", releaseSQL, key, token, s.clock())
}

func (s *Store) fenced(ctx context.Context, op, sql string, args ...any) error {
	tag, err := s.db.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("pgstore: %s: %w", op, err)
	}
	if tag.RowsAffected() != 1 {
		return idempotency.ErrLockLost
	}
	return nil
}

// DeleteExpired deletes expired records and returns how many it removed.
// Expired records are already invisible to the middleware; deleting them
// only reclaims space. Run it periodically, for example from a ticker or a
// cron job. It relies on the index on expires_at.
func (s *Store) DeleteExpired(ctx context.Context) (int64, error) {
	tag, err := s.db.Exec(ctx, deleteExpiredSQL, s.clock())
	if err != nil {
		return 0, fmt.Errorf("pgstore: delete expired: %w", err)
	}
	return tag.RowsAffected(), nil
}
