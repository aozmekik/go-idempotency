package idempotency_test

import (
	"context"
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
			onExtend: func(context.Context, int, func() error) error { return idempotency.ErrLockLost },
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
			onExtend: func(_ context.Context, n int, next func() error) error {
				if n == 1 {
					return errors.New("i/o timeout")
				}
				return next()
			},
		})
		mw := newMiddleware(t, idempotency.Config{Store: store, Logger: logger, Lease: lease}).Handler(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Renewals at 1s (fails; a quarter of the remaining 2s is
				// 0.5s) and at 1.5s (succeeds); the next would be at 2.5s.
				time.Sleep(2200 * time.Millisecond)
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

// outage makes renewal fail while time.Since(start) is in [from, to): with
// an immediate error (connection refused) or, if hung, by blocking until the
// attempt's own timeout (a dead connection that is noticed only then). Only
// Extend is cut off; duplicates still reach the store, which is how the
// tests observe that the lock held.
func outage(start time.Time, from, to time.Duration, hung bool) func(context.Context, int, func() error) error {
	return func(ctx context.Context, _ int, next func() error) error {
		if since := time.Since(start); since < from || since >= to {
			return next()
		}
		if hung {
			return hang(ctx)
		}
		return errors.New("connection refused")
	}
}

// TestRenewalSurvivesStoreOutages cuts renewal off for part of a 60-second
// handler run with a 30-second lease. Duplicates arrive throughout: as long
// as the store answers again shortly before the lease would run out, each
// must get 409 and the handler must run once.
func TestRenewalSurvivesStoreOutages(t *testing.T) {
	t.Parallel()
	const lease = 30 * time.Second
	tests := []struct {
		name     string
		from, to time.Duration
		hung     bool
	}{
		{"one hung renewal", 10 * time.Second, 11 * time.Second, true},
		{"two failed renewals", 10 * time.Second, 20500 * time.Millisecond, false},
		{"failures until a second before the deadline", 10 * time.Second, 29 * time.Second, false},
		{"hung renewals until two seconds before the deadline", 10 * time.Second, 28 * time.Second, true},
		{"failures from just after a renewal until a second before the deadline", 10*time.Second + time.Millisecond, 39 * time.Second, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				store, logger, logs := newFaulty(t, &faultyStore{onExtend: outage(start, tt.from, tt.to, tt.hung)})
				h := &counter{h: func(w http.ResponseWriter, r *http.Request) {
					time.Sleep(60 * time.Second)
					created(w, r)
				}}
				mw := newMiddleware(t, idempotency.Config{Store: store, Logger: logger, Lease: lease}).Handler(h)
				first := make(chan *httptest.ResponseRecorder)
				go func() { first <- serve(mw, request{key: testKey}) }()
				for _, seconds := range []int{5, 12, 21, 28, 29, 30, 31, 40, 59} {
					time.Sleep(time.Duration(seconds)*time.Second - time.Since(start))
					w := serve(mw, request{key: testKey})
					if w.Code != http.StatusConflict {
						t.Fatalf("duplicate sent at %ds: status %d, want 409; logs:\n%s", seconds, w.Code, logs)
					}
				}
				original := <-first
				wantReplay(t, serve(mw, request{key: testKey}), http.StatusCreated, original.Body.String())
				if h.Calls() != 1 {
					t.Fatalf("handler ran %d times, want 1", h.Calls())
				}
				if !strings.Contains(logs.String(), "renewing lease failed") {
					t.Errorf("renewal failures not logged: %s", logs)
				}
				// Retries speed up near the deadline but must not spin.
				if n := store.extendCalls.Load(); n > 25 {
					t.Errorf("Extend called %d times in 60s, want a bounded number", n)
				}
			})
		})
	}
}

// TestRenewalOutageLongerThanLeaseLosesLease is the documented limit: if the
// store is unreachable until after the lease has run out, a duplicate can
// take the key over and run the handler a second time. The original request
// must notice and not overwrite its successor's record.
func TestRenewalOutageLongerThanLeaseLosesLease(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const lease = 30 * time.Second
		start := time.Now()
		store, logger, logs := newFaulty(t, &faultyStore{onExtend: outage(start, 10*time.Second, 32*time.Second, false)})
		h := &counter{}
		h.h = func(w http.ResponseWriter, _ *http.Request) {
			n := h.Calls()
			if n == 1 {
				time.Sleep(60 * time.Second)
			}
			_, _ = w.Write([]byte("call " + strconv.Itoa(n)))
		}
		mw := newMiddleware(t, idempotency.Config{Store: store, Logger: logger, Lease: lease}).Handler(h)
		first := make(chan *httptest.ResponseRecorder)
		go func() { first <- serve(mw, request{key: testKey}) }()
		time.Sleep(lease + 500*time.Millisecond)
		if w := serve(mw, request{key: testKey}); w.Code != http.StatusOK || w.Body.String() != "call 2" {
			t.Fatalf("duplicate after the lease ran out: %d %q, want the handler to run again", w.Code, w.Body)
		}
		if w := <-first; w.Body.String() != "call 1" {
			t.Fatalf("original client got %q", w.Body)
		}
		if !strings.Contains(logs.String(), "lease lost") {
			t.Errorf("lease loss not logged: %s", logs)
		}
		wantReplay(t, serve(mw, request{key: testKey}), http.StatusOK, "call 2")
	})
}

// TestStopDoesNotWaitForRenewal finishes the handler while a renewal hangs.
// The response must not wait for that renewal: it is abandoned, and the
// fenced Complete stores the response.
func TestStopDoesNotWaitForRenewal(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const lease = 3 * time.Second
		store, logger, _ := newFaulty(t, &faultyStore{
			onExtend: func(ctx context.Context, _ int, _ func() error) error { return hang(ctx) },
		})
		mw := newMiddleware(t, idempotency.Config{Store: store, Logger: logger, Lease: lease}).Handler(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(1200 * time.Millisecond) // the renewal at 1s is still hanging
				created(w, r)
			}))
		start := time.Now()
		first := serve(mw, request{key: testKey})
		if elapsed := time.Since(start); elapsed != 1200*time.Millisecond {
			t.Errorf("response took %v, want 1.2s: it waited for the hung renewal", elapsed)
		}
		wantReplay(t, serve(mw, request{key: testKey}), http.StatusCreated, first.Body.String())
	})
}

// TestCompleteIsBoundedByLease stalls the store's Complete. The original
// response must still reach the client once the lease runs out, rather than
// never, and the failure must be logged.
func TestCompleteIsBoundedByLease(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const lease = 3 * time.Second
		store, logger, logs := newFaulty(t, &faultyStore{
			onComplete: func(ctx context.Context, _ func() error) error { return hang(ctx) },
		})
		mw := newMiddleware(t, idempotency.Config{Store: store, Logger: logger, Lease: lease}).Handler(http.HandlerFunc(created))
		start := time.Now()
		first := make(chan *httptest.ResponseRecorder)
		go func() { first <- serve(mw, request{key: testKey}) }()
		time.Sleep(time.Second)
		wantProblem(t, serve(mw, request{key: testKey}), http.StatusConflict, idempotency.ProblemRequestInFlight)
		if w := <-first; w.Code != http.StatusCreated {
			t.Fatalf("original status %d, want 201", w.Code)
		}
		if elapsed := time.Since(start); elapsed != lease {
			t.Errorf("response took %v, want the remaining lease (%v)", elapsed, lease)
		}
		if !strings.Contains(logs.String(), "storing response failed") {
			t.Errorf("store failure not logged: %s", logs)
		}
	})
}

// TestAcquireIsBounded stalls the store's Acquire: the client must get a 503
// after Lease/6 instead of waiting for as long as the store hangs.
func TestAcquireIsBounded(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const lease = 3 * time.Second
		store, logger, _ := newFaulty(t, &faultyStore{
			onAcquire: func(ctx context.Context, _ int, _ func() (idempotency.Acquisition, error)) (idempotency.Acquisition, error) {
				return idempotency.Acquisition{}, hang(ctx)
			},
		})
		h := &counter{h: created}
		mw := newMiddleware(t, idempotency.Config{Store: store, Logger: logger, Lease: lease}).Handler(h)
		start := time.Now()
		w := serve(mw, request{key: testKey})
		if elapsed := time.Since(start); elapsed != lease/6 {
			t.Errorf("503 took %v, want Lease/6 = %v", elapsed, lease/6)
		}
		wantProblem(t, w, http.StatusServiceUnavailable, idempotency.ProblemStoreUnavailable)
		if h.Calls() != 0 {
			t.Error("handler ran without a claim")
		}
	})
}

// TestFailedAcquireReleasesItsClaim covers an Acquire that the store applied
// but whose reply was lost (a reset connection, or a client that went away
// mid-call). The middleware answers 503 and must release the claim it may
// hold, so that the client's retry runs instead of getting 409 until the
// lease runs out.
func TestFailedAcquireReleasesItsClaim(t *testing.T) {
	t.Parallel()
	store, logger, _ := newFaulty(t, &faultyStore{
		onAcquire: func(_ context.Context, n int, next func() (idempotency.Acquisition, error)) (idempotency.Acquisition, error) {
			acq, err := next()
			if n == 1 && err == nil {
				return idempotency.Acquisition{}, errors.New("connection reset by peer")
			}
			return acq, err
		},
	})
	h := &counter{h: created}
	mw := newMiddleware(t, idempotency.Config{Store: store, Logger: logger}).Handler(h)
	wantProblem(t, serve(mw, request{key: testKey}), http.StatusServiceUnavailable, idempotency.ProblemStoreUnavailable)
	if w := serve(mw, request{key: testKey}); w.Code != http.StatusCreated || w.Header().Get(idempotency.ReplayedHeader) != "" {
		t.Fatalf("retry: status %d, want a fresh 201", w.Code)
	}
	if h.Calls() != 1 || store.releaseCalls.Load() != 1 {
		t.Errorf("handler calls = %d, releases = %d; want 1 and 1", h.Calls(), store.releaseCalls.Load())
	}
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
