package idempotency_test

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"

	idempotency "github.com/aozmekik/go-idempotency"
	"github.com/aozmekik/go-idempotency/memstore"
)

// Wrap a handler that creates a payment, then send the same request twice.
// The second response is replayed from the store; the handler runs once.
func Example() {
	store := memstore.New()
	mw, err := idempotency.New(idempotency.Config{Store: store})
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	var charges atomic.Int32
	createPayment := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := charges.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"id":"pay_%d"}`, n)
	})
	handler := mw.Handler(createPayment)

	for range 2 {
		req := httptest.NewRequest(http.MethodPost, "/payments", strings.NewReader(`{"amount":1000}`))
		req.Header.Set("Idempotency-Key", `"8e03978e-40d5-43e8-bc93-6894a57f9324"`)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		fmt.Println(rec.Code, rec.Header().Get("Idempotent-Replayed"), rec.Body.String())
	}
	fmt.Println("charges:", charges.Load())
	// Output:
	// 201  {"id":"pay_1"}
	// 201 true {"id":"pay_1"}
	// charges: 1
}

// Reusing a key for a different request is rejected with 422 and an
// RFC 9457 problem document.
func Example_keyReuse() {
	store := memstore.New()
	mw, err := idempotency.New(idempotency.Config{Store: store})
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
	}))

	for _, amount := range []string{"1000", "2000"} {
		req := httptest.NewRequest(http.MethodPost, "/payments", strings.NewReader(`{"amount":`+amount+`}`))
		req.Header.Set("Idempotency-Key", `"order-42"`)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		fmt.Println(rec.Code, rec.Header().Get("Content-Type"))
	}
	// Output:
	// 201 application/json
	// 422 application/problem+json
}

// Scope keys by the authenticated caller so that clients cannot collide
// with, or read, each other's stored responses.
func ExampleConfig_scope() {
	store := memstore.New()
	mw, err := idempotency.New(idempotency.Config{
		Store: store,
		Scope: func(r *http.Request) string {
			// In a real service, use the identity your auth middleware
			// established, not a raw header.
			return r.Header.Get("X-Account-Id")
		},
		KeyRequired: func(r *http.Request) bool {
			return strings.HasPrefix(r.URL.Path, "/payments")
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	_ = mw.Handler(http.NotFoundHandler())
	fmt.Println("configured")
	// Output: configured
}

// Clients serialise keys as Structured Field Strings with FormatKey.
func ExampleFormatKey() {
	v, err := idempotency.FormatKey(`order-42 "retry"`)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(v)
	// Output: "order-42 \"retry\""
}

func ExampleParseKey() {
	key, err := idempotency.ParseKey(`"8e03978e-40d5-43e8-bc93-6894a57f9324"`)
	fmt.Println(key, err)

	_, err = idempotency.ParseKey(`8e03978e-40d5-43e8-bc93-6894a57f9324`)
	fmt.Println(err != nil)
	// Output:
	// 8e03978e-40d5-43e8-bc93-6894a57f9324 <nil>
	// true
}

func ExampleKeyFromContext() {
	store := memstore.New()
	mw, err := idempotency.New(idempotency.Config{Store: store})
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Forward the key to a downstream payment provider.
		key, _ := idempotency.KeyFromContext(r.Context())
		fmt.Println("forwarding key", key)
		w.WriteHeader(http.StatusAccepted)
	}))
	req := httptest.NewRequest(http.MethodPost, "/payments", http.NoBody)
	req.Header.Set("Idempotency-Key", `"order-42"`)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	// Output: forwarding key order-42
}
