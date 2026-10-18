package idempotency_test

import (
	"context"
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

// Scope keys by the authenticated caller, so that callers cannot collide
// with, or read, each other's stored responses. Authentication runs first and
// puts the identity in the request context; Scope reads it from there, never
// from a header the client controls.
func ExampleConfig_scope() {
	type accountKey struct{}
	accountOf := func(r *http.Request) string {
		account, _ := r.Context().Value(accountKey{}).(string)
		return account
	}
	store := memstore.New()
	mw, err := idempotency.New(idempotency.Config{Store: store, Scope: accountOf})
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	var charges atomic.Int32
	createPayment := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, "pay_%d for %s", charges.Add(1), accountOf(r))
	})
	// A stand-in for real authentication: the bearer token is the account.
	authenticate := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			account := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), accountKey{}, account)))
		})
	}
	handler := authenticate(mw.Handler(createPayment))

	for _, account := range []string{"alice", "bob", "alice"} {
		req := httptest.NewRequest(http.MethodPost, "/payments", strings.NewReader(`{"amount":1000}`))
		req.Header.Set("Authorization", "Bearer "+account)
		req.Header.Set("Idempotency-Key", `"order-42"`) // the same key from both accounts
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		fmt.Println(account, rec.Code, rec.Header().Get("Idempotent-Replayed"), rec.Body.String())
	}
	// Output:
	// alice 201  pay_1 for alice
	// bob 201  pay_2 for bob
	// alice 201 true pay_1 for alice
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
