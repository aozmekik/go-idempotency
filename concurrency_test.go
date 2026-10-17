package idempotency_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	idempotency "github.com/aozmekik/go-idempotency"
	"github.com/aozmekik/go-idempotency/memstore"
)

type result struct {
	status   int
	replayed bool
	body     string
}

func post(t *testing.T, client *http.Client, url, key string) result {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(`{"amount":100}`))
	if err != nil {
		t.Error(err)
		return result{}
	}
	req.Header.Set(idempotency.DefaultHeaderName, key)
	resp, err := client.Do(req)
	if err != nil {
		t.Error(err)
		return result{}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Error(err)
	}
	return result{resp.StatusCode, resp.Header.Get(idempotency.ReplayedHeader) == "true", string(body)}
}

// TestConcurrentDuplicatesRunHandlerOnce sends the same key from many
// goroutines over real HTTP connections while the first request is held
// inside the handler. Exactly one request may run the handler; every other
// one must get 409 Conflict. The handler is released only after all the
// conflicts have been observed, so the outcome does not depend on timing.
func TestConcurrentDuplicatesRunHandlerOnce(t *testing.T) {
	t.Parallel()
	const n = 64
	var calls, conflicts atomic.Int32
	release := make(chan struct{})
	var releaseOnce sync.Once
	mw := newMiddleware(t, idempotency.Config{})
	srv := httptest.NewServer(mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		created(w, r)
	})))
	t.Cleanup(srv.Close)
	client := srv.Client()
	client.Transport.(*http.Transport).MaxIdleConnsPerHost = n

	results := make([]result, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			results[i] = post(t, client, srv.URL, testKey)
			if results[i].status == http.StatusConflict && conflicts.Add(1) == n-1 {
				releaseOnce.Do(func() { close(release) })
			}
		})
	}
	close(start)
	wg.Wait()

	var okCount int
	for _, r := range results {
		switch r.status {
		case http.StatusCreated:
			okCount++
		case http.StatusConflict:
		default:
			t.Errorf("unexpected status %d: %s", r.status, r.body)
		}
	}
	if calls.Load() != 1 || okCount != 1 || conflicts.Load() != n-1 {
		t.Fatalf("handler calls = %d, 201s = %d, 409s = %d; want 1, 1, %d", calls.Load(), okCount, conflicts.Load(), n-1)
	}
	// Now that the first request has completed, every duplicate is a replay.
	for range 8 {
		if r := post(t, client, srv.URL, testKey); r.status != http.StatusCreated || !r.replayed {
			t.Fatalf("after completion: status %d replayed %v, want a 201 replay", r.status, r.replayed)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("handler ran %d times, want 1", calls.Load())
	}
}

// TestConcurrentDuplicatesFreeRunning does the same without holding the
// handler: whatever the interleaving, the handler runs once and every
// response is either the original, an identical replay, or a 409.
func TestConcurrentDuplicatesFreeRunning(t *testing.T) {
	t.Parallel()
	const n = 64
	h := &counter{h: created}
	srv := httptest.NewServer(newMiddleware(t, idempotency.Config{}).Handler(h))
	t.Cleanup(srv.Close)
	results := make([]result, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			results[i] = post(t, srv.Client(), srv.URL, testKey)
		})
	}
	close(start)
	wg.Wait()
	if h.Calls() != 1 {
		t.Fatalf("handler ran %d times, want exactly 1", h.Calls())
	}
	var originals int
	for _, r := range results {
		switch {
		case r.status == http.StatusConflict:
		case r.status == http.StatusCreated && !r.replayed:
			originals++
		case r.status == http.StatusCreated && r.replayed:
			if r.body != `{"id":"pay_1","echo":{"amount":100}}` {
				t.Errorf("replay body = %q", r.body)
			}
		default:
			t.Errorf("unexpected result %+v", r)
		}
	}
	if originals != 1 {
		t.Errorf("%d non-replayed 201s, want 1", originals)
	}
}

// TestDistinctKeysRunConcurrently checks that keys do not serialise each
// other: every handler must be running at the same time. Inside a synctest
// bubble, a serialisation bug shows up as a reported deadlock instead of a
// hung test.
func TestDistinctKeysRunConcurrently(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const n = 16
		var inside sync.WaitGroup
		inside.Add(n)
		mw := newMiddleware(t, idempotency.Config{}).Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			inside.Done()
			inside.Wait()
			w.WriteHeader(http.StatusCreated)
		}))
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() {
				key, err := idempotency.FormatKey("key-" + strconv.Itoa(i))
				if err != nil {
					t.Error(err)
					return
				}
				if w := serve(mw, request{key: key}); w.Code != http.StatusCreated {
					t.Errorf("status %d", w.Code)
				}
			})
		}
		wg.Wait()
	})
}

// TestLeaseIsRenewedWhileHandlerRuns uses a virtual clock (testing/synctest)
// to run a handler for ten times its lease. The lease must be renewed in the
// background so that a duplicate still sees the request as in flight.
func TestLeaseIsRenewedWhileHandlerRuns(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const lease = 3 * time.Second
		store := memstore.New(memstore.WithCleanupInterval(0))
		h := &counter{h: func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(10 * lease)
			created(w, r)
		}}
		mw := newMiddleware(t, idempotency.Config{Store: store, Lease: lease}).Handler(h)

		first := make(chan *httptest.ResponseRecorder)
		go func() { first <- serve(mw, request{key: testKey}) }()
		for range 9 {
			time.Sleep(lease)
			wantProblem(t, serve(mw, request{key: testKey}), http.StatusConflict, idempotency.ProblemRequestInFlight)
		}
		original := <-first
		wantReplay(t, serve(mw, request{key: testKey}), http.StatusCreated, original.Body.String())
		if h.Calls() != 1 {
			t.Fatalf("handler ran %d times, want 1", h.Calls())
		}
	})
}

// TestCrashedHolderLeaseExpires simulates a process that claimed a key and
// died: its in-flight record stays in the shared store but nobody renews it.
// An injected clock moves past the lease, after which a retry runs.
func TestCrashedHolderLeaseExpires(t *testing.T) {
	t.Parallel()
	clock := &manualClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	store := memstore.New(memstore.WithClock(clock.Now), memstore.WithCleanupInterval(0))
	t.Cleanup(func() { _ = store.Close() })
	const lease = 10 * time.Second
	h := &counter{h: created}
	mw := newMiddleware(t, idempotency.Config{Store: store, Lease: lease}).Handler(h)

	// The crashed process saw the same request, so it left the same fingerprint.
	crashedReq := httptest.NewRequest(http.MethodPost, "/payments", http.NoBody)
	fp := idempotency.FingerprintOf(crashedReq, nil, nil)
	if _, err := store.Acquire(t.Context(), idempotency.StorageKey("", "k"), "crashed-process", fp, lease); err != nil {
		t.Fatal(err)
	}

	wantProblem(t, serve(mw, request{key: `"k"`}), http.StatusConflict, idempotency.ProblemRequestInFlight)
	clock.Advance(lease - time.Millisecond)
	wantProblem(t, serve(mw, request{key: `"k"`}), http.StatusConflict, idempotency.ProblemRequestInFlight)
	clock.Advance(time.Millisecond)
	if w := serve(mw, request{key: `"k"`}); w.Code != http.StatusCreated || w.Header().Get(idempotency.ReplayedHeader) != "" {
		t.Fatalf("after the lease expired: status %d, want a fresh 201", w.Code)
	}
	if h.Calls() != 1 {
		t.Fatalf("handler ran %d times, want 1", h.Calls())
	}
}

// TestLeaseLostWhileHandlerRuns covers the failure the lease exists for: the
// store reports that this request no longer owns the key (it was presumed
// dead). The response must reach the client but must not be stored, and the
// stale holder must not release the key from under its new owner.
func TestLeaseLostWhileHandlerRuns(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const lease = 3 * time.Second
		store, logger, logs := newFaulty(t, &faultyStore{
			extendErr: func(int) error { return idempotency.ErrLockLost },
		})
		h := &counter{h: func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(2 * time.Second) // one renewal attempt happens at 1s
			created(w, r)
		}}
		mw := newMiddleware(t, idempotency.Config{Store: store, Logger: logger, Lease: lease}).Handler(h)
		if w := serve(mw, request{key: testKey}); w.Code != http.StatusCreated {
			t.Fatalf("status %d, want 201 for the original client", w.Code)
		}
		if !strings.Contains(logs.String(), "lease lost") {
			t.Errorf("lease loss not logged: %s", logs)
		}
		// Nothing was completed or released: the record is still the
		// abandoned in-flight lock, until its lease runs out.
		wantProblem(t, serve(mw, request{key: testKey}), http.StatusConflict, idempotency.ProblemRequestInFlight)
		time.Sleep(lease)
		serve(mw, request{key: testKey})
		if h.Calls() != 2 {
			t.Fatalf("handler ran %d times, want 2", h.Calls())
		}
	})
}

// TestTransientRenewalErrorsAreRetried checks that a failed renewal is
// logged and retried rather than treated as a lost lock.
func TestTransientRenewalErrorsAreRetried(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const lease = 3 * time.Second
		store, logger, logs := newFaulty(t, &faultyStore{
			extendErr: func(n int) error {
				if n == 1 {
					return errors.New("i/o timeout")
				}
				return nil
			},
		})
		mw := newMiddleware(t, idempotency.Config{Store: store, Logger: logger, Lease: lease}).Handler(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(2500 * time.Millisecond) // renewals at 1s (fails) and 2s (succeeds)
				created(w, r)
			}))
		serve(mw, request{key: testKey})
		if !strings.Contains(logs.String(), "i/o timeout") {
			t.Errorf("renewal failure not logged: %s", logs)
		}
		if got := store.extendCalls.Load(); got != 2 {
			t.Errorf("Extend called %d times, want 2", got)
		}
		wantReplay(t, serve(mw, request{key: testKey}), http.StatusCreated, `{"id":"pay_1","echo":}`)
	})
}

// manualClock is a manually advanced clock, safe for concurrent use.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
