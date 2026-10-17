package redisstore_test

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	idempotency "github.com/aozmekik/go-idempotency"
	"github.com/aozmekik/go-idempotency/redisstore"
	"github.com/aozmekik/go-idempotency/storetest"
)

// newMiniredis starts an in-process Redis emulator. It is used for
// correctness tests only; it says nothing about real Redis performance.
func newMiniredis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return mr, client
}

func TestConformanceMiniredis(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T) (idempotency.Store, func(time.Duration)) {
		t.Helper()
		mr, client := newMiniredis(t)
		return redisstore.New(client), mr.FastForward
	})
}

// TestConformanceRedis runs the suite against a real Redis server when
// IDEMPOTENCY_TEST_REDIS_ADDR is set (CI provides one). Expiry tests are
// skipped because a real server's clock cannot be moved; they run against
// miniredis above.
func TestConformanceRedis(t *testing.T) {
	addr := os.Getenv("IDEMPOTENCY_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("IDEMPOTENCY_TEST_REDIS_ADDR not set")
	}
	t.Parallel()
	client := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(t.Context()).Err(); err != nil {
		t.Fatalf("ping %s: %v", addr, err)
	}
	storetest.Run(t, func(t *testing.T) (idempotency.Store, func(time.Duration)) {
		t.Helper()
		return redisstore.New(client, redisstore.WithPrefix("idempotency-test:")), nil
	})
}

func TestPrefixAndTTL(t *testing.T) {
	t.Parallel()
	mr, client := newMiniredis(t)
	s := redisstore.New(client, redisstore.WithPrefix("app:idem:"))
	ctx := t.Context()
	fp := idempotency.Fingerprint(sha256.Sum256([]byte("r")))
	if _, err := s.Acquire(ctx, "k", "tok", fp, 1500*time.Microsecond); err != nil {
		t.Fatal(err)
	}
	if !mr.Exists("app:idem:k") {
		t.Fatalf("record not stored under the prefix; keys = %v", mr.Keys())
	}
	if got := mr.TTL("app:idem:k"); got != 2*time.Millisecond {
		t.Errorf("lease TTL = %v, want 1.5ms rounded up to 2ms", got)
	}
	if got := mr.HGet("app:idem:k", "t"); got != "tok" {
		t.Errorf("token field = %q", got)
	}
	if err := s.Complete(ctx, "k", "tok", &idempotency.Response{StatusCode: 201}, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := mr.TTL("app:idem:k"); got != 24*time.Hour {
		t.Errorf("completed TTL = %v, want 24h", got)
	}
	if mr.HGet("app:idem:k", "t") != "" {
		t.Error("token survived completion")
	}
}

func TestCorruptRecords(t *testing.T) {
	t.Parallel()
	valid := string(make([]byte, 32))
	tests := []struct {
		name   string
		fields map[string]string
	}{
		{"short fingerprint", map[string]string{"s": "c", "f": "abc"}},
		{"unknown state", map[string]string{"s": "x", "f": valid}},
		{"bad status", map[string]string{"s": "c", "f": valid, "c": "abc", "h": "{}"}},
		{"bad headers", map[string]string{"s": "c", "f": valid, "c": "200", "h": "{"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mr, client := newMiniredis(t)
			for f, v := range tt.fields {
				mr.HSet(redisstore.DefaultPrefix+"k", f, v)
			}
			_, err := redisstore.New(client).Acquire(t.Context(), "k", "tok", idempotency.Fingerprint{}, time.Second)
			if err == nil || !strings.Contains(err.Error(), "corrupt record") {
				t.Fatalf("err = %v, want a corrupt-record error", err)
			}
		})
	}
}

func TestServerErrorsAreWrapped(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	// No retries: the server is gone and backoff would only slow the test.
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1, DialerRetries: 1})
	t.Cleanup(func() { _ = client.Close() })
	s := redisstore.New(client)
	mr.Close()
	ctx := t.Context()
	_, err := s.Acquire(ctx, "k", "tok", idempotency.Fingerprint{}, time.Second)
	if err == nil || !strings.HasPrefix(err.Error(), "redisstore: acquire:") {
		t.Errorf("Acquire err = %v", err)
	}
	for name, err := range map[string]error{
		"extend":   s.Extend(ctx, "k", "tok", time.Second),
		"complete": s.Complete(ctx, "k", "tok", &idempotency.Response{StatusCode: 200}, time.Second),
		"release":  s.Release(ctx, "k", "tok"),
	} {
		if err == nil || errors.Is(err, idempotency.ErrLockLost) || !strings.HasPrefix(err.Error(), "redisstore: "+name+":") {
			t.Errorf("%s err = %v, want a wrapped connection error", name, err)
		}
	}
}

// TestMiddlewareWithRedisStore is an end-to-end check that the middleware
// and this store work together.
func TestMiddlewareWithRedisStore(t *testing.T) {
	t.Parallel()
	_, client := newMiniredis(t)
	mw, err := idempotency.New(idempotency.Config{Store: redisstore.New(client)})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	h := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"pay_1"}`))
	}))
	for i := range 3 {
		r := httptest.NewRequest(http.MethodPost, "/payments", strings.NewReader(`{"amount":1}`))
		r.Header.Set("Idempotency-Key", `"k-1"`)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusCreated || w.Body.String() != `{"id":"pay_1"}` {
			t.Fatalf("request %d: %d %s", i, w.Code, w.Body)
		}
		if replayed := w.Header().Get(idempotency.ReplayedHeader) == "true"; replayed != (i > 0) {
			t.Fatalf("request %d: replayed = %v", i, replayed)
		}
	}
	if calls != 1 {
		t.Fatalf("handler ran %d times, want 1", calls)
	}
}
