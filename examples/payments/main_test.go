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

// TestWalkthrough follows the curl walkthrough in README.md.
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
		StatusCode int
		Header     http.Header
	}
	do := func(method, path, account, key, body string) (response, string) {
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
		return response{resp.StatusCode, resp.Header}, string(b)
	}

	const key = `"3f1c2a9e-0b7d-4c55-9e0e-5d6b8a7c4f21"`
	body := `{"amount":1999,"currency":"EUR"}`

	first, firstBody := do(http.MethodPost, "/payments", "alice", key, body)
	if first.StatusCode != http.StatusCreated || first.Header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("create: %d %s", first.StatusCode, firstBody)
	}
	if first.Header.Get("Location") != "/payments/pay_1" {
		t.Errorf("Location = %q", first.Header.Get("Location"))
	}

	retry, retryBody := do(http.MethodPost, "/payments", "alice", key, body)
	if retry.StatusCode != http.StatusCreated || retry.Header.Get("Idempotent-Replayed") != "true" || retryBody != firstBody {
		t.Fatalf("retry: %d replayed=%q %s", retry.StatusCode, retry.Header.Get("Idempotent-Replayed"), retryBody)
	}

	changed, changedBody := do(http.MethodPost, "/payments", "alice", key, `{"amount":2999,"currency":"EUR"}`)
	if changed.StatusCode != http.StatusUnprocessableEntity || changed.Header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("changed body: %d %s", changed.StatusCode, changedBody)
	}

	missing, _ := do(http.MethodPost, "/payments", "alice", "", body)
	if missing.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing key: %d", missing.StatusCode)
	}

	bare, _ := do(http.MethodPost, "/payments", "alice", "3f1c2a9e-0b7d-4c55-9e0e-5d6b8a7c4f21", body)
	if bare.StatusCode != http.StatusBadRequest {
		t.Fatalf("unquoted key: %d, want 400", bare.StatusCode)
	}

	// The same key from another account is a different key.
	bob, bobBody := do(http.MethodPost, "/payments", "bob", key, body)
	if bob.StatusCode != http.StatusCreated || bob.Header.Get("Idempotent-Replayed") != "" || !strings.Contains(bobBody, `"pay_2"`) {
		t.Fatalf("bob: %d %s", bob.StatusCode, bobBody)
	}

	invalid, _ := do(http.MethodPost, "/payments", "alice", `"bad-request-1"`, `{"amount":-5,"currency":"EUR"}`)
	invalidRetry, _ := do(http.MethodPost, "/payments", "alice", `"bad-request-1"`, `{"amount":-5,"currency":"EUR"}`)
	if invalid.StatusCode != http.StatusBadRequest || invalidRetry.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("4xx responses are stored and replayed by default: %d, replayed=%q", invalid.StatusCode, invalidRetry.Header.Get("Idempotent-Replayed"))
	}

	_, listBody := do(http.MethodGet, "/payments", "alice", "", "")
	var payments []payment
	if err := json.Unmarshal([]byte(listBody), &payments); err != nil {
		t.Fatal(err)
	}
	if len(payments) != 1 || payments[0].IdempotencyKey != "3f1c2a9e-0b7d-4c55-9e0e-5d6b8a7c4f21" {
		t.Fatalf("alice's payments = %+v, want exactly one", payments)
	}

	if got, _ := do(http.MethodGet, "/payments/pay_2", "alice", "", ""); got.StatusCode != http.StatusNotFound {
		t.Errorf("alice can read bob's payment: %d", got.StatusCode)
	}
	if got, _ := do(http.MethodGet, "/payments/pay_1", "alice", "", ""); got.StatusCode != http.StatusOK {
		t.Errorf("get own payment: %d", got.StatusCode)
	}
	if got, _ := do(http.MethodPost, "/payments", "", key, body); got.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated: %d", got.StatusCode)
	}
	if got, _ := do(http.MethodPost, "/payments", "alice", `"bad-json"`, `{"amount":`); got.StatusCode != http.StatusBadRequest {
		t.Errorf("bad JSON: %d", got.StatusCode)
	}
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
