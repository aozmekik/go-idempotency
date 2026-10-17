package memstore

import (
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	idempotency "github.com/aozmekik/go-idempotency"
	"github.com/aozmekik/go-idempotency/storetest"
)

// fakeClock is a manually advanced clock, safe for concurrent use.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestConformance(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T) (idempotency.Store, func(time.Duration)) {
		t.Helper()
		clock := newFakeClock()
		s := New(WithClock(clock.Now), WithCleanupInterval(0))
		t.Cleanup(func() { _ = s.Close() })
		return s, clock.Advance
	})
}

func TestConformanceSingleShard(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T) (idempotency.Store, func(time.Duration)) {
		t.Helper()
		clock := newFakeClock()
		s := New(WithClock(clock.Now), WithShards(1), WithCleanupInterval(0))
		t.Cleanup(func() { _ = s.Close() })
		return s, clock.Advance
	})
}

func TestShardCountRoundsUpToPowerOfTwo(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ in, want int }{{-3, 1}, {0, 1}, {1, 1}, {2, 2}, {3, 4}, {17, 32}, {64, 64}} {
		s := New(WithShards(tt.in), WithCleanupInterval(0))
		if got := len(s.shards); got != tt.want {
			t.Errorf("WithShards(%d): %d shards, want %d", tt.in, got, tt.want)
		}
		if s.mask != uint64(tt.want-1) {
			t.Errorf("WithShards(%d): mask %d, want %d", tt.in, s.mask, tt.want-1)
		}
	}
}

func TestDeleteExpired(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	s := New(WithClock(clock.Now), WithCleanupInterval(0))
	ctx := t.Context()
	var fp idempotency.Fingerprint
	for _, key := range []string{"a", "b", "c"} {
		if _, err := s.Acquire(ctx, key, "t", fp, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Complete(ctx, "c", "t", &idempotency.Response{StatusCode: 200}, time.Hour); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Second)
	if n := s.deleteExpired(); n != 2 {
		t.Errorf("deleteExpired removed %d entries, want 2 (the expired locks)", n)
	}
	if n := s.len(); n != 1 {
		t.Errorf("%d entries left, want 1 (the completed record)", n)
	}
}

func TestJanitorRemovesExpiredEntries(t *testing.T) {
	t.Parallel()
	// synctest runs the janitor's ticker on a virtual clock, so the test
	// observes several cleanup cycles without sleeping.
	synctest.Test(t, func(t *testing.T) {
		s := New(WithCleanupInterval(time.Minute))
		defer func() { _ = s.Close() }()
		var fp idempotency.Fingerprint
		if _, err := s.Acquire(t.Context(), "k", "t", fp, 30*time.Second); err != nil {
			t.Fatal(err)
		}
		time.Sleep(59 * time.Second)
		synctest.Wait()
		if n := s.len(); n != 1 {
			t.Fatalf("before the first cleanup: %d entries, want 1", n)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if n := s.len(); n != 0 {
			t.Fatalf("after cleanup: %d entries, want 0", n)
		}
	})
}

func TestCloseIsIdempotentAndStopsJanitor(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := New(WithCleanupInterval(time.Second))
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		// synctest.Test fails if the janitor goroutine were still running.
	})
}

func TestStoredResponseIsIsolatedFromCallers(t *testing.T) {
	t.Parallel()
	s := New(WithCleanupInterval(0))
	ctx := t.Context()
	var fp idempotency.Fingerprint
	if _, err := s.Acquire(ctx, "k", "t", fp, time.Minute); err != nil {
		t.Fatal(err)
	}
	resp := &idempotency.Response{StatusCode: 201, Header: http.Header{"A": {"1"}}, Body: []byte("body")}
	if err := s.Complete(ctx, "k", "t", resp, time.Hour); err != nil {
		t.Fatal(err)
	}
	resp.Header["A"][0] = "mutated"
	resp.Body[0] = 'X'

	acq, err := s.Acquire(ctx, "k", "t2", fp, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	got := acq.Existing.Response
	if got.Header.Get("A") != "1" || string(got.Body) != "body" {
		t.Fatalf("stored response changed through the caller's copy: %+v", got)
	}
	got.Body[0] = 'Y'
	acq, err = s.Acquire(ctx, "k", "t3", fp, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if string(acq.Existing.Response.Body) != "body" {
		t.Fatalf("stored response changed through a returned copy: %q", acq.Existing.Response.Body)
	}
}
