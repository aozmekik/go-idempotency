package pgstore_test

import (
	"crypto/rand"
	"strconv"
	"testing"
	"time"

	idempotency "github.com/aozmekik/go-idempotency"
	"github.com/aozmekik/go-idempotency/pgstore"
)

// BenchmarkAcquireComplete measures the store work of one first request
// (Acquire, then Complete) against a real PostgreSQL server. It runs only
// when IDEMPOTENCY_TEST_PG_DSN is set.
func BenchmarkAcquireComplete(b *testing.B) {
	p := testPool(b)
	s := pgstore.New(p)
	ctx := b.Context()
	prefix := "bench/" + rand.Text() + "/"
	resp := &idempotency.Response{StatusCode: 201, Header: map[string][]string{"Content-Type": {"application/json"}}, Body: make([]byte, 256)}
	var fp idempotency.Fingerprint
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		key := prefix + strconv.Itoa(i)
		if _, err := s.Acquire(ctx, key, "tok", fp, 30*time.Second); err != nil {
			b.Fatal(err)
		}
		if err := s.Complete(ctx, key, "tok", resp, time.Minute); err != nil {
			b.Fatal(err)
		}
	}
}
