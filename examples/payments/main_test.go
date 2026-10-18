package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aozmekik/go-idempotency/memstore"
)

// TestWalkthrough repeats the curl walkthrough in README.md, except step 4
// (the in-flight 409, see TestInFlightConflict), whose payment it creates
// directly so that payment IDs match the README. It checks the status codes,
// the replay marker and the bodies shown there, then a few cases the README
// does not show.
func TestWalkthrough(t *testing.T) {
	t.Parallel()
	store := memstore.New(memstore.WithCleanupInterval(0))
	t.Cleanup(func() { _ = store.Close() })
	handler, err := newHandler(store, 0, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	type response struct {
		status   int
		replayed string
		problem  string // the problem type's slug, if any
		body     string
	}
	do := func(method, path, account, key, body string) response {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if account != "" {
			req.Header.Set("Authorization", "Bearer "+account)
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		r := response{status: resp.StatusCode, replayed: resp.Header.Get("Idempotent-Replayed"), body: string(b)}
		if resp.Header.Get("Content-Type") == "application/problem+json" {
			var p struct{ Type string }
			if err := json.Unmarshal(b, &p); err != nil {
				t.Fatal(err)
			}
			_, r.problem, _ = strings.Cut(p.Type, "#")
		}
		return r
	}
	want := func(step string, got, want response) {
		t.Helper()
		if want.body == "" {
			got.body = "" // not compared
		}
		if got != want {
			t.Errorf("%s: got %+v, want %+v", step, got, want)
		}
	}

	const (
		keyA  = `"3f1c2a9e-0b7d-4c55-9e0e-5d6b8a7c4f21"`
		keyB  = `"7a0d3c1e-2b4f-4e6a-8c9d-0f1e2d3c4b5a"`
		body  = `{"amount":1999,"currency":"EUR"}`
		pay1  = `{"id":"pay_1","account":"alice","amount":1999,"currency":"EUR","status":"succeeded","idempotency_key":"3f1c2a9e-0b7d-4c55-9e0e-5d6b8a7c4f21"}`
		pay2  = `{"id":"pay_2","account":"alice","amount":500,"currency":"EUR","status":"succeeded","idempotency_key":"7a0d3c1e-2b4f-4e6a-8c9d-0f1e2d3c4b5a"}`
		pay3  = `{"id":"pay_3","account":"bob","amount":1999,"currency":"EUR","status":"succeeded","idempotency_key":"3f1c2a9e-0b7d-4c55-9e0e-5d6b8a7c4f21"}`
		posts = "/payments"
	)
	want("1. create", do(http.MethodPost, posts, "alice", keyA, body), response{status: 201, body: pay1 + "\n"})
	want("2. retry", do(http.MethodPost, posts, "alice", keyA, body), response{status: 201, replayed: "true", body: pay1 + "\n"})
	want("3. reuse", do(http.MethodPost, posts, "alice", keyA, `{"amount":2999,"currency":"EUR"}`), response{status: 422, problem: "key-reused"})
	want("4. second payment", do(http.MethodPost, posts, "alice", keyB, `{"amount":500,"currency":"EUR"}`), response{status: 201, body: pay2 + "\n"})
	want("5. no key", do(http.MethodPost, posts, "alice", "", body), response{status: 400, problem: "key-missing"})
	want("6. unquoted key", do(http.MethodPost, posts, "alice", "3f1c2a9e-0b7d-4c55-9e0e-5d6b8a7c4f21", body), response{status: 400, problem: "key-invalid"})
	want("7. list", do(http.MethodGet, posts, "alice", "", ""), response{status: 200, body: "[" + pay1 + "," + pay2 + "]\n"})
	want("8. same key, other account", do(http.MethodPost, posts, "bob", keyA, body), response{status: 201, body: pay3 + "\n"})

	// Not in the README.
	invalid := `{"amount":-5,"currency":"EUR"}`
	want("validation error", do(http.MethodPost, posts, "alice", `"bad-request-1"`, invalid), response{status: 400})
	want("validation error is stored", do(http.MethodPost, posts, "alice", `"bad-request-1"`, invalid), response{status: 400, replayed: "true"})
	want("bad JSON", do(http.MethodPost, posts, "alice", `"bad-json"`, `{"amount":`), response{status: 400})
	want("other account's payment", do(http.MethodGet, "/payments/pay_3", "alice", "", ""), response{status: 404})
	want("own payment", do(http.MethodGet, "/payments/pay_1", "alice", "", ""), response{status: 200, body: pay1 + "\n"})
	want("unauthenticated", do(http.MethodPost, posts, "", keyA, body), response{status: 401})
}

// TestInFlightConflict runs on a virtual clock (testing/synctest), so the
// simulated provider latency costs no real time and the test is
// deterministic: synctest.Wait returns once the first request is blocked
// inside the handler, holding the key.
func TestInFlightConflict(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		store := memstore.New(memstore.WithCleanupInterval(0))
		handler, err := newHandler(store, 2*time.Second, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		send := func() *httptest.ResponseRecorder {
			r := httptest.NewRequest(http.MethodPost, "/payments", strings.NewReader(`{"amount":1,"currency":"EUR"}`))
			r.Header.Set("Authorization", "Bearer alice")
			r.Header.Set("Idempotency-Key", `"slow-1"`)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			return w
		}
		first := make(chan *httptest.ResponseRecorder)
		go func() { first <- send() }()
		synctest.Wait()

		if w := send(); w.Code != http.StatusConflict {
			t.Fatalf("duplicate while in flight: %d, want 409", w.Code)
		}
		if w := <-first; w.Code != http.StatusCreated {
			t.Fatalf("original request: %d, want 201", w.Code)
		}
		if w := send(); w.Code != http.StatusCreated || w.Header().Get("Idempotent-Replayed") != "true" {
			t.Fatalf("after completion: %d replayed=%q, want a 201 replay", w.Code, w.Header().Get("Idempotent-Replayed"))
		}
	})
}
