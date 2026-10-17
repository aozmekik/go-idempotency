// Package storetest is a conformance test suite for implementations of
// [idempotency.Store].
//
// A store package runs it from an ordinary test:
//
//	func TestConformance(t *testing.T) {
//		storetest.Run(t, func(t *testing.T) (idempotency.Store, func(time.Duration)) {
//			clock := newFakeClock()
//			return mystore.New(mystore.WithClock(clock.Now)), clock.Advance
//		})
//	}
//
// The suite generates unique keys for every run, so a factory may hand out
// stores that share one backend (a database table, a Redis instance).
package storetest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	idempotency "github.com/aozmekik/go-idempotency"
)

// Factory returns an empty store for one subtest and a function that moves
// the store's clock forward. Return a nil advance function when the store's
// clock cannot be controlled; the tests that depend on expiry are then
// skipped.
type Factory func(t *testing.T) (store idempotency.Store, advance func(time.Duration))

const (
	lease = 10 * time.Second
	ttl   = time.Hour
)

var (
	fpA = idempotency.Fingerprint(sha256.Sum256([]byte("request A")))
	fpB = idempotency.Fingerprint(sha256.Sum256([]byte("request B")))
)

// Run runs the conformance suite as parallel subtests of t.
func Run(t *testing.T, newStore Factory) {
	t.Helper()
	nonce := rand.Text()
	tests := []struct {
		name         string
		needsAdvance bool
		run          func(t *testing.T, s idempotency.Store, advance func(time.Duration), key string)
	}{
		{"AcquireNewKey", false, testAcquireNewKey},
		{"AcquireInFlight", false, testAcquireInFlight},
		{"AcquireReportsOriginalFingerprint", false, testAcquireReportsOriginalFingerprint},
		{"CompleteThenAcquireReturnsResponse", false, testCompleteThenAcquire},
		{"ResponseRoundTrip", false, testResponseRoundTrip},
		{"TokensFenceOperations", false, testTokensFence},
		{"ReleaseFreesKey", false, testReleaseFreesKey},
		{"OperationsAfterRelease", false, testOperationsAfterRelease},
		{"OperationsAfterComplete", false, testOperationsAfterComplete},
		{"KeysAreIndependent", false, testKeysAreIndependent},
		{"UnusualKeyBytes", false, testUnusualKeyBytes},
		{"ConcurrentAcquire", false, testConcurrentAcquire},
		{"CanceledContext", false, testCanceledContext},
		{"LeaseExpiry", true, testLeaseExpiry},
		{"ExtendRenewsLease", true, testExtendRenewsLease},
		{"OperationsAfterLeaseExpiry", true, testOperationsAfterLeaseExpiry},
		{"TTLExpiry", true, testTTLExpiry},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, advance := newStore(t)
			if tt.needsAdvance && advance == nil {
				t.Skip("store clock cannot be advanced")
			}
			tt.run(t, s, advance, "storetest/"+nonce+"/"+tt.name)
		})
	}
}

func acquire(t *testing.T, s idempotency.Store, key, token string, fp idempotency.Fingerprint) idempotency.Acquisition {
	t.Helper()
	acq, err := s.Acquire(t.Context(), key, token, fp, lease)
	if err != nil {
		t.Fatalf("Acquire(%q, %q): %v", key, token, err)
	}
	return acq
}

func mustAcquire(t *testing.T, s idempotency.Store, key, token string, fp idempotency.Fingerprint) {
	t.Helper()
	if acq := acquire(t, s, key, token, fp); !acq.Acquired {
		t.Fatalf("Acquire(%q, %q) = not acquired (existing %v), want acquired", key, token, acq.Existing.State)
	}
}

func wantExisting(t *testing.T, s idempotency.Store, key string, state idempotency.State) idempotency.Record {
	t.Helper()
	acq := acquire(t, s, key, "probe-"+rand.Text(), fpA)
	if acq.Acquired {
		t.Fatalf("Acquire(%q) acquired the key, want existing %v record", key, state)
	}
	if acq.Existing.State != state {
		t.Fatalf("existing record state = %v, want %v", acq.Existing.State, state)
	}
	return acq.Existing
}

func wantLockLost(t *testing.T, op string, err error) {
	t.Helper()
	if !errors.Is(err, idempotency.ErrLockLost) {
		t.Fatalf("%s: err = %v, want ErrLockLost", op, err)
	}
}

func mustNil(t *testing.T, op string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
}

func sampleResponse() *idempotency.Response {
	return &idempotency.Response{
		StatusCode: http.StatusCreated,
		Header: http.Header{
			"Content-Type": {"application/json"},
			"Link":         {"</a>; rel=a", "</b>; rel=b"},
		},
		Body: []byte("{\"id\":\"pay_1\"}\x00\xff binary-safe"),
	}
}

func equalResponse(t *testing.T, got, want *idempotency.Response) {
	t.Helper()
	if got == nil {
		t.Fatal("response is nil")
	}
	if got.StatusCode != want.StatusCode {
		t.Errorf("StatusCode = %d, want %d", got.StatusCode, want.StatusCode)
	}
	if !bytes.Equal(got.Body, want.Body) {
		t.Errorf("Body = %q, want %q", got.Body, want.Body)
	}
	if len(got.Header) != len(want.Header) {
		t.Errorf("Header = %v, want %v", got.Header, want.Header)
	}
	for name, values := range want.Header {
		gotValues := got.Header[name]
		if len(gotValues) != len(values) {
			t.Errorf("Header[%q] = %q, want %q", name, gotValues, values)
			continue
		}
		for i := range values {
			if gotValues[i] != values[i] {
				t.Errorf("Header[%q] = %q, want %q", name, gotValues, values)
			}
		}
	}
}

func testAcquireNewKey(t *testing.T, s idempotency.Store, _ func(time.Duration), key string) {
	mustAcquire(t, s, key, "t1", fpA)
}

func testAcquireInFlight(t *testing.T, s idempotency.Store, _ func(time.Duration), key string) {
	mustAcquire(t, s, key, "t1", fpA)
	rec := wantExisting(t, s, key, idempotency.StateInFlight)
	if rec.Fingerprint != fpA {
		t.Errorf("Fingerprint = %v, want %v", rec.Fingerprint, fpA)
	}
	if rec.Response != nil {
		t.Errorf("in-flight record has a response: %+v", rec.Response)
	}
}

func testAcquireReportsOriginalFingerprint(t *testing.T, s idempotency.Store, _ func(time.Duration), key string) {
	mustAcquire(t, s, key, "t1", fpA)
	acq := acquire(t, s, key, "t2", fpB)
	if acq.Acquired {
		t.Fatal("second Acquire with another fingerprint acquired the key")
	}
	if acq.Existing.Fingerprint != fpA {
		t.Errorf("Fingerprint = %v, want the original %v", acq.Existing.Fingerprint, fpA)
	}
}

func testCompleteThenAcquire(t *testing.T, s idempotency.Store, _ func(time.Duration), key string) {
	mustAcquire(t, s, key, "t1", fpA)
	want := sampleResponse()
	mustNil(t, "Complete", s.Complete(t.Context(), key, "t1", want, ttl))
	rec := wantExisting(t, s, key, idempotency.StateCompleted)
	if rec.Fingerprint != fpA {
		t.Errorf("Fingerprint = %v, want %v", rec.Fingerprint, fpA)
	}
	equalResponse(t, rec.Response, want)
}

func testResponseRoundTrip(t *testing.T, s idempotency.Store, _ func(time.Duration), key string) {
	large := make([]byte, 256<<10)
	_, _ = rand.Read(large)
	cases := map[string]*idempotency.Response{
		"empty":        {StatusCode: http.StatusNoContent},
		"emptyHeader":  {StatusCode: http.StatusOK, Header: http.Header{}, Body: []byte{}},
		"large":        {StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: large},
		"clientError":  {StatusCode: http.StatusBadRequest, Body: []byte(`{"error":"bad"}`)},
		"manyHeaders":  {StatusCode: http.StatusAccepted, Header: http.Header{"A": {"1"}, "B": {"2", "3"}, "C": {""}}},
		"utf8AndSpace": {StatusCode: http.StatusOK, Body: []byte("  ünïcödé\n\t ")},
	}
	for name, want := range cases {
		k := key + "/" + name
		mustAcquire(t, s, k, "t1", fpA)
		mustNil(t, "Complete", s.Complete(t.Context(), k, "t1", want, ttl))
		equalResponse(t, wantExisting(t, s, k, idempotency.StateCompleted).Response, want)
	}
}

func testTokensFence(t *testing.T, s idempotency.Store, _ func(time.Duration), key string) {
	ctx := t.Context()
	mustAcquire(t, s, key, "t1", fpA)
	wantLockLost(t, "Complete with foreign token", s.Complete(ctx, key, "t2", sampleResponse(), ttl))
	wantLockLost(t, "Extend with foreign token", s.Extend(ctx, key, "t2", lease))
	wantLockLost(t, "Release with foreign token", s.Release(ctx, key, "t2"))
	wantExisting(t, s, key, idempotency.StateInFlight)
	mustNil(t, "Complete with owner token", s.Complete(ctx, key, "t1", sampleResponse(), ttl))
}

func testReleaseFreesKey(t *testing.T, s idempotency.Store, _ func(time.Duration), key string) {
	mustAcquire(t, s, key, "t1", fpA)
	mustNil(t, "Release", s.Release(t.Context(), key, "t1"))
	mustAcquire(t, s, key, "t2", fpB)
}

func testOperationsAfterRelease(t *testing.T, s idempotency.Store, _ func(time.Duration), key string) {
	ctx := t.Context()
	mustAcquire(t, s, key, "t1", fpA)
	mustNil(t, "Release", s.Release(ctx, key, "t1"))
	wantLockLost(t, "Complete after Release", s.Complete(ctx, key, "t1", sampleResponse(), ttl))
	wantLockLost(t, "Extend after Release", s.Extend(ctx, key, "t1", lease))
	wantLockLost(t, "Release after Release", s.Release(ctx, key, "t1"))
}

func testOperationsAfterComplete(t *testing.T, s idempotency.Store, _ func(time.Duration), key string) {
	ctx := t.Context()
	mustAcquire(t, s, key, "t1", fpA)
	want := sampleResponse()
	mustNil(t, "Complete", s.Complete(ctx, key, "t1", want, ttl))
	other := &idempotency.Response{StatusCode: http.StatusTeapot}
	wantLockLost(t, "second Complete", s.Complete(ctx, key, "t1", other, ttl))
	wantLockLost(t, "Extend after Complete", s.Extend(ctx, key, "t1", lease))
	wantLockLost(t, "Release after Complete", s.Release(ctx, key, "t1"))
	equalResponse(t, wantExisting(t, s, key, idempotency.StateCompleted).Response, want)
}

func testKeysAreIndependent(t *testing.T, s idempotency.Store, _ func(time.Duration), key string) {
	ctx := t.Context()
	k1, k2 := key+"/1", key+"/2"
	mustAcquire(t, s, k1, "t1", fpA)
	mustAcquire(t, s, k2, "t2", fpB)
	mustNil(t, "Complete k1", s.Complete(ctx, k1, "t1", sampleResponse(), ttl))
	wantExisting(t, s, k1, idempotency.StateCompleted)
	wantExisting(t, s, k2, idempotency.StateInFlight)
	mustNil(t, "Release k2", s.Release(ctx, k2, "t2"))
	wantExisting(t, s, k1, idempotency.StateCompleted)
}

func testUnusualKeyBytes(t *testing.T, s idempotency.Store, _ func(time.Duration), key string) {
	// Keys are printable ASCII (see idempotency.Store); exercise the
	// characters most likely to trip quoting, globbing or escaping.
	for _, suffix := range []string{` with spaces `, `"quoted"\`, "{braces}:colon*glob?[x]", "%_like_%", "'; DROP TABLE x; --"} {
		k := key + "/" + suffix
		mustAcquire(t, s, k, "t1", fpA)
		mustNil(t, "Complete", s.Complete(t.Context(), k, "t1", sampleResponse(), ttl))
		wantExisting(t, s, k, idempotency.StateCompleted)
	}
}

func testConcurrentAcquire(t *testing.T, s idempotency.Store, _ func(time.Duration), key string) {
	const n = 32
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		acquired int
		inFlight int
		errs     []error
	)
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			acq, err := s.Acquire(t.Context(), key, "t"+strconv.Itoa(i), fpA, lease)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				errs = append(errs, err)
			case acq.Acquired:
				acquired++
			case acq.Existing.State == idempotency.StateInFlight && acq.Existing.Fingerprint == fpA:
				inFlight++
			}
		})
	}
	close(start)
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("Acquire errors: %v", errs)
	}
	if acquired != 1 || inFlight != n-1 {
		t.Fatalf("acquired = %d, in flight = %d; want 1 and %d", acquired, inFlight, n-1)
	}
}

func testCanceledContext(t *testing.T, s idempotency.Store, _ func(time.Duration), key string) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Acquire(ctx, key, "t1", fpA, lease); !errors.Is(err, context.Canceled) {
		t.Errorf("Acquire: err = %v, want context.Canceled", err)
	}
	if err := s.Extend(ctx, key, "t1", lease); !errors.Is(err, context.Canceled) {
		t.Errorf("Extend: err = %v, want context.Canceled", err)
	}
	if err := s.Complete(ctx, key, "t1", sampleResponse(), ttl); !errors.Is(err, context.Canceled) {
		t.Errorf("Complete: err = %v, want context.Canceled", err)
	}
	if err := s.Release(ctx, key, "t1"); !errors.Is(err, context.Canceled) {
		t.Errorf("Release: err = %v, want context.Canceled", err)
	}
	// Nothing may have been stored by the canceled calls.
	mustAcquire(t, s, key, "t2", fpA)
}

func testLeaseExpiry(t *testing.T, s idempotency.Store, advance func(time.Duration), key string) {
	mustAcquire(t, s, key, "t1", fpA)
	advance(lease - time.Second)
	wantExisting(t, s, key, idempotency.StateInFlight)
	advance(2 * time.Second)
	// The holder is presumed dead: a new request may claim the key, even
	// with a different fingerprint.
	mustAcquire(t, s, key, "t2", fpB)
	wantLockLost(t, "Complete by the expired holder", s.Complete(t.Context(), key, "t1", sampleResponse(), ttl))
	if rec := wantExisting(t, s, key, idempotency.StateInFlight); rec.Fingerprint != fpB {
		t.Errorf("Fingerprint = %v, want the new holder's %v", rec.Fingerprint, fpB)
	}
}

func testExtendRenewsLease(t *testing.T, s idempotency.Store, advance func(time.Duration), key string) {
	mustAcquire(t, s, key, "t1", fpA)
	advance(lease * 6 / 10)
	mustNil(t, "Extend", s.Extend(t.Context(), key, "t1", lease))
	advance(lease * 6 / 10) // past the original lease, within the renewed one
	wantExisting(t, s, key, idempotency.StateInFlight)
	advance(lease * 6 / 10) // past the renewed lease
	mustAcquire(t, s, key, "t2", fpA)
}

func testOperationsAfterLeaseExpiry(t *testing.T, s idempotency.Store, advance func(time.Duration), key string) {
	ctx := t.Context()
	mustAcquire(t, s, key, "t1", fpA)
	advance(lease + time.Second)
	wantLockLost(t, "Extend after expiry", s.Extend(ctx, key, "t1", lease))
	wantLockLost(t, "Complete after expiry", s.Complete(ctx, key, "t1", sampleResponse(), ttl))
	wantLockLost(t, "Release after expiry", s.Release(ctx, key, "t1"))
	mustAcquire(t, s, key, "t2", fpA)
}

func testTTLExpiry(t *testing.T, s idempotency.Store, advance func(time.Duration), key string) {
	mustAcquire(t, s, key, "t1", fpA)
	mustNil(t, "Complete", s.Complete(t.Context(), key, "t1", sampleResponse(), ttl))
	advance(ttl - time.Minute)
	wantExisting(t, s, key, idempotency.StateCompleted)
	advance(2 * time.Minute)
	mustAcquire(t, s, key, "t2", fpB)
}
