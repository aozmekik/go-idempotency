// Package memstore provides an in-process [idempotency.Store] backed by a
// sharded map.
//
// It suits single-instance services, tests and development. Records are lost
// when the process exits and are not shared between instances; use the
// redisstore or pgstore module when more than one process serves the same
// keys.
package memstore

import (
	"context"
	"hash/maphash"
	"slices"
	"sync"
	"time"

	idempotency "github.com/aozmekik/go-idempotency"
)

// Defaults for [New].
const (
	DefaultShards          = 32
	DefaultCleanupInterval = time.Minute
)

// Option configures a [Store].
type Option func(*options)

type options struct {
	now             func() time.Time
	shards          int
	cleanupInterval time.Duration
}

// WithClock replaces time.Now as the source of the current time, which
// decides when leases and TTLs expire. Intended for tests.
func WithClock(now func() time.Time) Option {
	return func(o *options) { o.now = now }
}

// WithShards sets the number of independently locked shards, rounded up to a
// power of two (at most 65536). More shards reduce lock contention between unrelated keys.
// Default: [DefaultShards].
func WithShards(n int) Option {
	return func(o *options) { o.shards = n }
}

// WithCleanupInterval sets how often a background goroutine deletes expired
// records. Expired records are never returned regardless; cleanup only bounds
// memory. Zero or a negative value disables the goroutine.
// Default: [DefaultCleanupInterval].
func WithCleanupInterval(d time.Duration) Option {
	return func(o *options) { o.cleanupInterval = d }
}

// Store is an in-memory [idempotency.Store]. It is safe for concurrent use.
// Call [Store.Close] to stop the cleanup goroutine.
type Store struct {
	now    func() time.Time
	seed   maphash.Seed
	shards []shard
	mask   uint64

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

type shard struct {
	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	state     idempotency.State
	fp        idempotency.Fingerprint
	token     string // owner of an in-flight entry; empty once completed
	resp      *idempotency.Response
	expiresAt time.Time
}

var _ idempotency.Store = (*Store)(nil)

// New returns an empty Store and starts its cleanup goroutine.
func New(opts ...Option) *Store {
	o := options{now: time.Now, shards: DefaultShards, cleanupInterval: DefaultCleanupInterval}
	for _, opt := range opts {
		opt(&o)
	}
	n := shardCount(o.shards)
	s := &Store{
		now:    o.now,
		seed:   maphash.MakeSeed(),
		shards: make([]shard, n),
		mask:   n - 1,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	for i := range s.shards {
		s.shards[i].entries = make(map[string]*entry)
	}
	if o.cleanupInterval > 0 {
		go s.janitor(o.cleanupInterval)
	} else {
		close(s.done)
	}
	return s
}

// maxShards bounds WithShards; more shards than this only waste memory.
const maxShards = 1 << 16

// shardCount rounds n up to a power of two in [1, maxShards].
func shardCount(n int) uint64 {
	count := uint64(1)
	for count < maxShards && int(count) < n {
		count <<= 1
	}
	return count
}

// Close stops the cleanup goroutine and waits for it to exit. The store
// remains usable; expired records are then only removed when their key is
// claimed again. Close is idempotent and always returns nil.
func (s *Store) Close() error {
	s.closeOnce.Do(func() { close(s.stop) })
	<-s.done
	return nil
}

func (s *Store) shardFor(key string) *shard {
	return &s.shards[maphash.String(s.seed, key)&s.mask]
}

// live returns the unexpired entry for key; the shard lock must be held.
func (sh *shard) live(key string, now time.Time) *entry {
	e, ok := sh.entries[key]
	if !ok {
		return nil
	}
	if !now.Before(e.expiresAt) {
		delete(sh.entries, key)
		return nil
	}
	return e
}

// owned returns the live in-flight entry owned by token, or nil.
func (sh *shard) owned(key, token string, now time.Time) *entry {
	e := sh.live(key, now)
	if e == nil || e.state != idempotency.StateInFlight || e.token != token {
		return nil
	}
	return e
}

// Acquire implements [idempotency.Store].
func (s *Store) Acquire(ctx context.Context, key, token string, fp idempotency.Fingerprint, lease time.Duration) (idempotency.Acquisition, error) {
	if err := ctx.Err(); err != nil {
		return idempotency.Acquisition{}, err
	}
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	now := s.now()
	if e := sh.live(key, now); e != nil {
		return idempotency.Acquisition{Existing: idempotency.Record{
			State:       e.state,
			Fingerprint: e.fp,
			Response:    cloneResponse(e.resp),
		}}, nil
	}
	sh.entries[key] = &entry{
		state:     idempotency.StateInFlight,
		fp:        fp,
		token:     token,
		expiresAt: now.Add(lease),
	}
	return idempotency.Acquisition{Acquired: true}, nil
}

// Extend implements [idempotency.Store].
func (s *Store) Extend(ctx context.Context, key, token string, lease time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	now := s.now()
	e := sh.owned(key, token, now)
	if e == nil {
		return idempotency.ErrLockLost
	}
	e.expiresAt = now.Add(lease)
	return nil
}

// Complete implements [idempotency.Store].
func (s *Store) Complete(ctx context.Context, key, token string, resp *idempotency.Response, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stored := cloneResponse(resp)
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	now := s.now()
	e := sh.owned(key, token, now)
	if e == nil {
		return idempotency.ErrLockLost
	}
	e.state = idempotency.StateCompleted
	e.token = ""
	e.resp = stored
	e.expiresAt = now.Add(ttl)
	return nil
}

// Release implements [idempotency.Store].
func (s *Store) Release(ctx context.Context, key, token string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.owned(key, token, s.now()) == nil {
		return idempotency.ErrLockLost
	}
	delete(sh.entries, key)
	return nil
}

func (s *Store) janitor(interval time.Duration) {
	defer close(s.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.deleteExpired()
		}
	}
}

// deleteExpired removes expired entries one shard at a time and returns how
// many it removed.
func (s *Store) deleteExpired() int {
	removed := 0
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		now := s.now()
		for key, e := range sh.entries {
			if !now.Before(e.expiresAt) {
				delete(sh.entries, key)
				removed++
			}
		}
		sh.mu.Unlock()
	}
	return removed
}

// len returns the number of entries held, expired or not.
func (s *Store) len() int {
	n := 0
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		n += len(sh.entries)
		sh.mu.Unlock()
	}
	return n
}

func cloneResponse(r *idempotency.Response) *idempotency.Response {
	if r == nil {
		return nil
	}
	return &idempotency.Response{StatusCode: r.StatusCode, Header: r.Header.Clone(), Body: slices.Clone(r.Body)}
}
