package idempotency_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"

	idempotency "github.com/aozmekik/go-idempotency"
	"github.com/aozmekik/go-idempotency/memstore"
)

// The benchmarks call ServeHTTP directly with httptest types, so they
// measure the middleware's CPU and allocation cost, not networking. All
// variants, including the bare handler, pay for the same request and
// recorder setup, so the difference between them is the middleware's
// overhead with the in-memory store.

var benchResponse = []byte(`{"id":"pay_1","amount":1999,"currency":"EUR","status":"succeeded"}`)

func benchHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", "/payments/pay_1")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(benchResponse)
	})
}

func benchMiddleware(b *testing.B) http.Handler {
	b.Helper()
	store := memstore.New(memstore.WithCleanupInterval(0))
	b.Cleanup(func() { _ = store.Close() })
	mw, err := idempotency.New(idempotency.Config{Store: store})
	if err != nil {
		b.Fatal(err)
	}
	return mw.Handler(benchHandler())
}

// resetEvery bounds the store's size in new-key benchmarks: without it the
// heap, and so ns/op, would grow with b.N and depend on -benchtime.
const resetEvery = 10_000

var benchURL = &url.URL{Path: "/payments"}

// benchRequest builds a request directly rather than with
// httptest.NewRequest, which parses wire format and allocates a 4 KiB
// buffer per call that would dominate the measurement.
func benchRequest(body []byte, key string) *http.Request {
	h := http.Header{"Content-Type": {"application/json"}}
	if key != "" {
		h[idempotency.DefaultHeaderName] = []string{key}
	}
	return &http.Request{
		Method:        http.MethodPost,
		URL:           benchURL,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        h,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Host:          "example.com",
		RequestURI:    "/payments",
	}
}

func benchBody(size int) []byte {
	return bytes.Repeat([]byte("a"), size)
}

// BenchmarkBareHandler is the baseline: the handler without middleware.
func BenchmarkBareHandler(b *testing.B) {
	h := benchHandler()
	body := benchBody(256)
	b.ReportAllocs()
	for b.Loop() {
		h.ServeHTTP(httptest.NewRecorder(), benchRequest(body, ""))
	}
}

// BenchmarkFirstRequest measures a request with a new key: parse the key,
// read and hash the body, acquire, run the handler while capturing the
// response, and store it.
func BenchmarkFirstRequest(b *testing.B) {
	for _, size := range []int{256, 16 << 10} {
		b.Run("body="+strconv.Itoa(size), func(b *testing.B) {
			h := benchMiddleware(b)
			body := benchBody(size)
			b.ReportAllocs()
			b.SetBytes(int64(size))
			i := 0
			for b.Loop() {
				i++
				if i%resetEvery == 0 {
					b.StopTimer()
					h = benchMiddleware(b)
					b.StartTimer()
				}
				h.ServeHTTP(httptest.NewRecorder(), benchRequest(body, `"k-`+strconv.Itoa(i)+`"`))
			}
		})
	}
}

// BenchmarkReplay measures a duplicate of a completed request: parse,
// hash, look up and write the stored response. The handler does not run.
func BenchmarkReplay(b *testing.B) {
	h := benchMiddleware(b)
	body := benchBody(256)
	h.ServeHTTP(httptest.NewRecorder(), benchRequest(body, `"k"`))
	b.ReportAllocs()
	for b.Loop() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, benchRequest(body, `"k"`))
		if w.Code != http.StatusCreated {
			b.Fatalf("status %d", w.Code)
		}
	}
}

// BenchmarkFirstRequestParallel runs new-key requests from GOMAXPROCS
// goroutines to show contention on the sharded store. The store is not
// reset here (resetting would need a pause across goroutines), so its heap
// grows with b.N; compare it at the same -benchtime only.
func BenchmarkFirstRequestParallel(b *testing.B) {
	h := benchMiddleware(b)
	body := benchBody(256)
	var n atomic.Int64
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			key := `"k-` + strconv.FormatInt(n.Add(1), 10) + `"`
			h.ServeHTTP(httptest.NewRecorder(), benchRequest(body, key))
		}
	})
}

// BenchmarkBareHandlerParallel is the baseline for the parallel benchmark.
func BenchmarkBareHandlerParallel(b *testing.B) {
	h := benchHandler()
	body := benchBody(256)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			h.ServeHTTP(httptest.NewRecorder(), benchRequest(body, ""))
		}
	})
}

// BenchmarkParseKey measures Structured Field parsing of a UUID key.
func BenchmarkParseKey(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := idempotency.ParseKey(`"8e03978e-40d5-43e8-bc93-6894a57f9324"`); err != nil {
			b.Fatal(err)
		}
	}
}
