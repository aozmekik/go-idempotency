package redisstore_test

import (
	"crypto/rand"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	idempotency "github.com/aozmekik/go-idempotency"
	"github.com/aozmekik/go-idempotency/redisstore"
)

// BenchmarkAcquireComplete measures the store work of one first request
// (Acquire, then Complete) against a real Redis server. It runs only when
// IDEMPOTENCY_TEST_REDIS_ADDR is set; there is deliberately no miniredis
// variant, because an in-process emulator's speed says nothing about Redis.
func BenchmarkAcquireComplete(b *testing.B) {
	addr := os.Getenv("IDEMPOTENCY_TEST_REDIS_ADDR")
	if addr == "" {
		b.Skip("IDEMPOTENCY_TEST_REDIS_ADDR not set")
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	b.Cleanup(func() { _ = client.Close() })
	s := redisstore.New(client, redisstore.WithPrefix("idempotency-bench:"+rand.Text()+":"))
	ctx := b.Context()
	resp := &idempotency.Response{StatusCode: 201, Header: map[string][]string{"Content-Type": {"application/json"}}, Body: make([]byte, 256)}
	var fp idempotency.Fingerprint
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		key := strconv.Itoa(i)
		if _, err := s.Acquire(ctx, key, "tok", fp, 30*time.Second); err != nil {
			b.Fatal(err)
		}
		if err := s.Complete(ctx, key, "tok", resp, time.Minute); err != nil {
			b.Fatal(err)
		}
	}
}
