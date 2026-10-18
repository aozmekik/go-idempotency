package idempotency_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	idempotency "github.com/aozmekik/go-idempotency"
	"github.com/aozmekik/go-idempotency/memstore"
)

const testKey = `"5f0e6f6e-6c1b-4a7c-9a52-0b8f3c1d2e4a"`

// counter is an http.Handler that records how often it ran.
type counter struct {
	calls atomic.Int32
	h     http.HandlerFunc
}

func (c *counter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.calls.Add(1)
	c.h(w, r)
}

func (c *counter) Calls() int { return int(c.calls.Load()) }

// created is a typical "create a resource" handler.
func created(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Location", "/payments/pay_1")
	h.Set("X-Request-ID", "req-unique-per-response")
	http.SetCookie(w, &http.Cookie{Name: "session", Value: "secret"})
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"id":"pay_1","echo":` + string(body) + `}`))
}

func newMiddleware(t *testing.T, cfg idempotency.Config) *idempotency.Middleware {
	t.Helper()
	if cfg.Store == nil {
		s := memstore.New(memstore.WithCleanupInterval(0))
		t.Cleanup(func() { _ = s.Close() })
		cfg.Store = s
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	m, err := idempotency.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

type request struct {
	method string
	target string
	key    string // raw header value; empty means no header
	body   string
	header http.Header
	ctx    context.Context
}

func serve(h http.Handler, req request) *httptest.ResponseRecorder {
	method := req.method
	if method == "" {
		method = http.MethodPost
	}
	target := req.target
	if target == "" {
		target = "/payments"
	}
	r := httptest.NewRequest(method, target, strings.NewReader(req.body))
	if req.ctx != nil {
		r = r.WithContext(req.ctx)
	}
	for name, values := range req.header {
		for _, v := range values {
			r.Header.Add(name, v)
		}
	}
	if req.key != "" {
		r.Header.Set(idempotency.DefaultHeaderName, req.key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

type problemBody struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

func wantProblem(t *testing.T, w *httptest.ResponseRecorder, status int, slug string) problemBody {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d; body %s", w.Code, status, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	var p problemBody
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("problem body %q: %v", w.Body, err)
	}
	if want := idempotency.DefaultProblemTypeBase + slug; p.Type != want {
		t.Errorf("type = %q, want %q", p.Type, want)
	}
	if p.Status != status || p.Title == "" {
		t.Errorf("problem = %+v, want status %d and a title", p, status)
	}
	return p
}

func wantReplay(t *testing.T, w *httptest.ResponseRecorder, status int, body string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d; body %s", w.Code, status, w.Body)
	}
	if got := w.Header().Get(idempotency.ReplayedHeader); got != "true" {
		t.Errorf("%s = %q, want true", idempotency.ReplayedHeader, got)
	}
	if w.Body.String() != body {
		t.Errorf("body = %q, want %q", w.Body, body)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	t.Parallel()
	store := memstore.New(memstore.WithCleanupInterval(0))
	t.Cleanup(func() { _ = store.Close() })
	tests := []struct {
		name string
		cfg  idempotency.Config
		want string
	}{
		{"missing store", idempotency.Config{}, "Store is required"},
		{"bad header name", idempotency.Config{Store: store, HeaderName: "Idem Key"}, "invalid header name"},
		{"bad method", idempotency.Config{Store: store, Methods: []string{"PO ST"}}, "invalid method"},
		{"negative key length", idempotency.Config{Store: store, MaxKeyLength: -1}, "MaxKeyLength"},
		{"negative body limit", idempotency.Config{Store: store, MaxBodyBytes: -1}, "MaxBodyBytes"},
		{"negative response limit", idempotency.Config{Store: store, MaxResponseBytes: -1}, "MaxResponseBytes"},
		{"sub-millisecond lease", idempotency.Config{Store: store, Lease: time.Microsecond}, "Lease"},
		{"negative TTL", idempotency.Config{Store: store, TTL: -time.Second}, "TTL"},
		{"bad fingerprint header", idempotency.Config{Store: store, FingerprintHeaders: []string{"a b"}}, "fingerprint header"},
		{"bad replay header", idempotency.Config{Store: store, ReplayHeaders: []string{"a:b"}}, "invalid replay header"},
		{"hop-by-hop replay header", idempotency.Config{Store: store, ReplayHeaders: []string{"transfer-encoding"}}, "hop-by-hop"},
		{"content-length replay header", idempotency.Config{Store: store, ReplayHeaders: []string{"Content-Length"}}, "Content-Length"},
		{"replayed marker as replay header", idempotency.Config{Store: store, ReplayHeaders: []string{"Idempotent-Replayed"}}, "set by the middleware"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := idempotency.New(tt.cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("New() error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
	if _, err := idempotency.New(idempotency.Config{Store: store}); err != nil {
		t.Fatalf("New with only a store: %v", err)
	}
}

func TestFirstRequestRunsHandlerAndDuplicateIsReplayed(t *testing.T) {
	t.Parallel()
	h := &counter{h: created}
	mw := newMiddleware(t, idempotency.Config{}).Handler(h)

	first := serve(mw, request{key: testKey, body: `{"amount":100}`})
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", first.Code)
	}
	if first.Header().Get(idempotency.ReplayedHeader) != "" {
		t.Error("first response is marked as replayed")
	}
	if first.Header().Get("Set-Cookie") == "" {
		t.Error("first response lost its Set-Cookie header")
	}

	second := serve(mw, request{key: testKey, body: `{"amount":100}`})
	wantReplay(t, second, http.StatusCreated, first.Body.String())
	if h.Calls() != 1 {
		t.Errorf("handler ran %d times, want 1", h.Calls())
	}
	for _, name := range []string{"Content-Type", "Location"} {
		if got, want := second.Header().Get(name), first.Header().Get(name); got != want {
			t.Errorf("replayed %s = %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"Set-Cookie", "X-Request-ID"} {
		if got := second.Header().Get(name); got != "" {
			t.Errorf("replayed response carries %s: %q", name, got)
		}
	}
	if got, want := second.Header().Get("Content-Length"), strconv.Itoa(first.Body.Len()); got != want {
		t.Errorf("replayed Content-Length = %q, want %q", got, want)
	}
}

func TestReplayHeadersAllowlist(t *testing.T) {
	t.Parallel()
	mw := newMiddleware(t, idempotency.Config{ReplayHeaders: []string{"set-cookie", "x-request-id"}}).Handler(http.HandlerFunc(created))
	serve(mw, request{key: testKey})
	replayed := serve(mw, request{key: testKey})
	if got := replayed.Header().Get("Set-Cookie"); !strings.HasPrefix(got, "session=secret") {
		t.Errorf("Set-Cookie = %q, want the explicitly allowed cookie", got)
	}
	if got := replayed.Header().Get("X-Request-ID"); got == "" {
		t.Error("explicitly allowed X-Request-ID was not replayed")
	}
	if got := replayed.Header().Get("Location"); got != "" {
		t.Errorf("Location = %q, but it is not in the custom allowlist", got)
	}
}

func TestEmptyReplayHeadersReplaysNoHeaders(t *testing.T) {
	t.Parallel()
	mw := newMiddleware(t, idempotency.Config{ReplayHeaders: []string{}}).Handler(http.HandlerFunc(created))
	serve(mw, request{key: testKey})
	replayed := serve(mw, request{key: testKey})
	for name := range replayed.Header() {
		if name != idempotency.ReplayedHeader && name != "Content-Length" {
			t.Errorf("unexpected replayed header %s", name)
		}
	}
}

func TestConnectionNominatedHeadersAreNotReplayed(t *testing.T) {
	t.Parallel()
	mw := newMiddleware(t, idempotency.Config{ReplayHeaders: []string{"X-Hop", "X-Kept"}}).Handler(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Connection", "x-hop")
			w.Header().Set("X-Hop", "1")
			w.Header().Set("X-Kept", "1")
			w.WriteHeader(http.StatusOK)
		}))
	serve(mw, request{key: testKey})
	replayed := serve(mw, request{key: testKey})
	if replayed.Header().Get("X-Hop") != "" {
		t.Error("header nominated by Connection was replayed")
	}
	if replayed.Header().Get("X-Kept") != "1" {
		t.Error("ordinary allowlisted header was not replayed")
	}
}

func TestMethodsWithoutIdempotencyPassThrough(t *testing.T) {
	t.Parallel()
	h := &counter{h: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }}
	mw := newMiddleware(t, idempotency.Config{}).Handler(h)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		if w := serve(mw, request{method: method}); w.Code != http.StatusOK {
			t.Errorf("%s without key: status %d, want 200", method, w.Code)
		}
	}
	// Even with a key, a method outside Config.Methods is not deduplicated.
	serve(mw, request{method: http.MethodPut, key: testKey})
	serve(mw, request{method: http.MethodPut, key: testKey})
	if h.Calls() != 5 {
		t.Errorf("handler ran %d times, want 5", h.Calls())
	}
}

func TestCustomMethods(t *testing.T) {
	t.Parallel()
	h := &counter{h: created}
	mw := newMiddleware(t, idempotency.Config{Methods: []string{http.MethodPut}}).Handler(h)
	wantProblem(t, serve(mw, request{method: http.MethodPut}), http.StatusBadRequest, idempotency.ProblemKeyMissing)
	if w := serve(mw, request{method: http.MethodPost}); w.Code != http.StatusCreated {
		t.Errorf("POST is no longer covered, status = %d, want 201", w.Code)
	}
	serve(mw, request{method: http.MethodPut, key: testKey})
	wantReplay(t, serve(mw, request{method: http.MethodPut, key: testKey}), http.StatusCreated, `{"id":"pay_1","echo":}`)
}

func TestMissingKey(t *testing.T) {
	t.Parallel()
	h := &counter{h: created}
	mw := newMiddleware(t, idempotency.Config{}).Handler(h)
	p := wantProblem(t, serve(mw, request{body: "{}"}), http.StatusBadRequest, idempotency.ProblemKeyMissing)
	if !strings.Contains(p.Detail, "Idempotency-Key") {
		t.Errorf("detail %q does not name the header", p.Detail)
	}
	if h.Calls() != 0 {
		t.Error("handler ran for a request without a key")
	}
}

func TestKeyRequiredPredicate(t *testing.T) {
	t.Parallel()
	h := &counter{h: created}
	mw := newMiddleware(t, idempotency.Config{
		KeyRequired: func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/payments") },
	}).Handler(h)

	wantProblem(t, serve(mw, request{target: "/payments"}), http.StatusBadRequest, idempotency.ProblemKeyMissing)
	if w := serve(mw, request{target: "/notes"}); w.Code != http.StatusCreated {
		t.Fatalf("optional route without key: status %d, want 201", w.Code)
	}
	// A key on an optional route is still honoured.
	serve(mw, request{target: "/notes", key: testKey})
	wantReplay(t, serve(mw, request{target: "/notes", key: testKey}), http.StatusCreated, `{"id":"pay_1","echo":}`)
	if h.Calls() != 2 {
		t.Errorf("handler ran %d times, want 2", h.Calls())
	}
}

func TestCustomHeaderName(t *testing.T) {
	t.Parallel()
	h := &counter{h: created}
	mw := newMiddleware(t, idempotency.Config{HeaderName: "x-idempotency-key"}).Handler(h)
	req := request{header: http.Header{"X-Idempotency-Key": {testKey}}}
	serve(mw, req)
	wantReplay(t, serve(mw, req), http.StatusCreated, `{"id":"pay_1","echo":}`)
	wantProblem(t, serve(mw, request{key: testKey}), http.StatusBadRequest, idempotency.ProblemKeyMissing)
}

func TestInvalidKeys(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		values []string
		limit  int
	}{
		{"unquoted token", []string{"abc-123"}, 0},
		{"unquoted uuid", []string{"5f0e6f6e-6c1b-4a7c-9a52-0b8f3c1d2e4a"}, 0},
		{"integer", []string{"42"}, 0},
		{"empty field", []string{""}, 0},
		{"empty string", []string{`""`}, 0},
		{"unterminated", []string{`"abc`}, 0},
		{"two field lines", []string{`"a"`, `"b"`}, 0},
		{"list in one line", []string{`"a", "b"`}, 0},
		{"non-ASCII", []string{`"ключ"`}, 0},
		{"too long", []string{`"` + strings.Repeat("k", 9) + `"`}, 8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := &counter{h: created}
			mw := newMiddleware(t, idempotency.Config{MaxKeyLength: tt.limit}).Handler(h)
			w := serve(mw, request{header: http.Header{idempotency.DefaultHeaderName: tt.values}})
			wantProblem(t, w, http.StatusBadRequest, idempotency.ProblemKeyInvalid)
			if h.Calls() != 0 {
				t.Error("handler ran for an invalid key")
			}
		})
	}
}

func TestKeyAtLengthLimitIsAccepted(t *testing.T) {
	t.Parallel()
	mw := newMiddleware(t, idempotency.Config{MaxKeyLength: 8}).Handler(http.HandlerFunc(created))
	if w := serve(mw, request{key: `"12345678"`}); w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w.Code)
	}
}

func TestKeyParametersAreIgnored(t *testing.T) {
	t.Parallel()
	h := &counter{h: created}
	mw := newMiddleware(t, idempotency.Config{}).Handler(h)
	serve(mw, request{key: `"k-1";client=web`})
	wantReplay(t, serve(mw, request{key: `"k-1"`}), http.StatusCreated, `{"id":"pay_1","echo":}`)
	if h.Calls() != 1 {
		t.Errorf("handler ran %d times, want 1", h.Calls())
	}
}

func TestKeyReusedWithDifferentRequest(t *testing.T) {
	t.Parallel()
	original := request{
		method: http.MethodPost, target: "/payments?currency=EUR", key: testKey, body: `{"amount":100}`,
		header: http.Header{"X-Tenant": {"a"}, "User-Agent": {"curl/8"}},
	}
	tests := []struct {
		name   string
		change func(r *request)
	}{
		{"different body", func(r *request) { r.body = `{"amount":200}` }},
		{"different path", func(r *request) { r.target = "/refunds?currency=EUR" }},
		{"different query", func(r *request) { r.target = "/payments?currency=USD" }},
		{"different method", func(r *request) { r.method = http.MethodPatch }},
		{"different fingerprinted header", func(r *request) { r.header = http.Header{"X-Tenant": {"b"}} }},
		{"fingerprinted header removed", func(r *request) { r.header = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := &counter{h: created}
			mw := newMiddleware(t, idempotency.Config{FingerprintHeaders: []string{"x-tenant"}}).Handler(h)
			serve(mw, original)
			changed := original
			tt.change(&changed)
			wantProblem(t, serve(mw, changed), http.StatusUnprocessableEntity, idempotency.ProblemKeyReused)
			if h.Calls() != 1 {
				t.Errorf("handler ran %d times, want 1", h.Calls())
			}
		})
	}
	t.Run("headers outside the fingerprint do not matter", func(t *testing.T) {
		t.Parallel()
		mw := newMiddleware(t, idempotency.Config{FingerprintHeaders: []string{"X-Tenant"}}).Handler(http.HandlerFunc(created))
		first := serve(mw, original)
		changed := original
		changed.header = http.Header{"X-Tenant": {"a"}, "User-Agent": {"retry-client/2"}}
		wantReplay(t, serve(mw, changed), http.StatusCreated, first.Body.String())
	})
}

func TestReusedKeyWhileInFlightIsStillAMismatch(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	entered := make(chan struct{})
	mw := newMiddleware(t, idempotency.Config{}).Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusCreated)
	}))
	done := make(chan struct{})
	go func() {
		defer close(done)
		serve(mw, request{key: testKey, body: "a"})
	}()
	<-entered
	wantProblem(t, serve(mw, request{key: testKey, body: "b"}), http.StatusUnprocessableEntity, idempotency.ProblemKeyReused)
	close(release)
	<-done
}

func TestConcurrentDuplicateGetsConflict(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	entered := make(chan struct{})
	h := &counter{h: func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		created(w, r)
	}}
	mw := newMiddleware(t, idempotency.Config{}).Handler(h)
	firstDone := make(chan *httptest.ResponseRecorder)
	go func() { firstDone <- serve(mw, request{key: testKey}) }()
	<-entered

	p := wantProblem(t, serve(mw, request{key: testKey}), http.StatusConflict, idempotency.ProblemRequestInFlight)
	if !strings.Contains(p.Detail, "Retry") {
		t.Errorf("detail %q should tell the client to retry", p.Detail)
	}
	close(release)
	first := <-firstDone
	wantReplay(t, serve(mw, request{key: testKey}), http.StatusCreated, first.Body.String())
	if h.Calls() != 1 {
		t.Errorf("handler ran %d times, want 1", h.Calls())
	}
}

func TestStoragePolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		status      int
		shouldStore func(int) bool
		wantStored  bool
	}{
		{"2xx stored by default", http.StatusCreated, nil, true},
		{"4xx stored by default", http.StatusBadRequest, nil, true},
		{"500 not stored by default", http.StatusInternalServerError, nil, false},
		{"503 not stored by default", http.StatusServiceUnavailable, nil, false},
		{"custom policy rejects 4xx", http.StatusConflict, func(c int) bool { return c < 400 }, false},
		{"custom policy stores 5xx", http.StatusBadGateway, func(int) bool { return true }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := &counter{h: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte("result"))
			}}
			mw := newMiddleware(t, idempotency.Config{ShouldStore: tt.shouldStore}).Handler(h)
			serve(mw, request{key: testKey})
			second := serve(mw, request{key: testKey})
			if tt.wantStored {
				wantReplay(t, second, tt.status, "result")
				if h.Calls() != 1 {
					t.Errorf("handler ran %d times, want 1", h.Calls())
				}
				return
			}
			if second.Header().Get(idempotency.ReplayedHeader) != "" {
				t.Error("response that must not be stored was replayed")
			}
			if h.Calls() != 2 {
				t.Errorf("handler ran %d times, want 2 (retry after an unstored response)", h.Calls())
			}
		})
	}
}

func TestRetryAfterServerErrorCanSucceed(t *testing.T) {
	t.Parallel()
	var fail atomic.Bool
	fail.Store(true)
	mw := newMiddleware(t, idempotency.Config{}).Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Swap(false) {
			http.Error(w, "upstream timeout", http.StatusGatewayTimeout)
			return
		}
		created(w, r)
	}))
	if w := serve(mw, request{key: testKey}); w.Code != http.StatusGatewayTimeout {
		t.Fatalf("first status = %d, want 504", w.Code)
	}
	if w := serve(mw, request{key: testKey}); w.Code != http.StatusCreated || w.Header().Get(idempotency.ReplayedHeader) != "" {
		t.Fatalf("retry: status %d replayed %q, want a fresh 201", w.Code, w.Header().Get(idempotency.ReplayedHeader))
	}
	wantReplay(t, serve(mw, request{key: testKey}), http.StatusCreated, `{"id":"pay_1","echo":}`)
}

func TestRequestBodyLimit(t *testing.T) {
	t.Parallel()
	h := &counter{h: created}
	mw := newMiddleware(t, idempotency.Config{MaxBodyBytes: 10}).Handler(h)
	wantProblem(t, serve(mw, request{key: `"a"`, body: "12345678901"}), http.StatusRequestEntityTooLarge, idempotency.ProblemBodyTooLarge)
	if h.Calls() != 0 {
		t.Fatal("handler ran for an oversized body")
	}
	if w := serve(mw, request{key: `"b"`, body: "1234567890"}); w.Code != http.StatusCreated {
		t.Fatalf("body at the limit: status %d, want 201", w.Code)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestUnreadableBody(t *testing.T) {
	t.Parallel()
	h := &counter{h: created}
	mw := newMiddleware(t, idempotency.Config{}).Handler(h)
	r := httptest.NewRequest(http.MethodPost, "/payments", failingReader{})
	r.Header.Set(idempotency.DefaultHeaderName, testKey)
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, r)
	wantProblem(t, w, http.StatusBadRequest, idempotency.ProblemBodyUnreadable)
	if h.Calls() != 0 {
		t.Fatal("handler ran with an unreadable body")
	}
}

func TestHandlerSeesBodyAndKey(t *testing.T) {
	t.Parallel()
	const body = `{"amount":100,"currency":"EUR"}`
	mw := newMiddleware(t, idempotency.Config{}).Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil || string(got) != body {
			t.Errorf("handler read %q, %v; want %q", got, err, body)
		}
		if r.ContentLength != int64(len(body)) {
			t.Errorf("ContentLength = %d, want %d", r.ContentLength, len(body))
		}
		again, err := r.GetBody()
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := io.ReadAll(again); string(b) != body {
			t.Errorf("GetBody returned %q", b)
		}
		key, ok := idempotency.KeyFromContext(r.Context())
		if !ok || key != "order-42" {
			t.Errorf("KeyFromContext = %q, %v; want order-42, true", key, ok)
		}
		w.WriteHeader(http.StatusOK)
	}))
	serve(mw, request{key: `"order-42"`, body: body})
	if _, ok := idempotency.KeyFromContext(t.Context()); ok {
		t.Error("KeyFromContext found a key in an unrelated context")
	}
}

func TestOversizedResponseIsNotReplayedButNotRerun(t *testing.T) {
	t.Parallel()
	h := &counter{h: func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("0123456789"))
		_, _ = w.Write([]byte("abcdef"))
	}}
	mw := newMiddleware(t, idempotency.Config{MaxResponseBytes: 12}).Handler(h)
	first := serve(mw, request{key: testKey})
	if first.Body.String() != "0123456789abcdef" {
		t.Fatalf("original client got %q, want the full response", first.Body)
	}
	second := serve(mw, request{key: testKey})
	p := wantProblem(t, second, http.StatusInternalServerError, idempotency.ProblemResponseNotReplayable)
	if second.Header().Get(idempotency.ReplayedHeader) != "true" {
		t.Error("stored problem is not marked as replayed")
	}
	if !strings.Contains(p.Detail, "12-byte") {
		t.Errorf("detail %q does not mention the limit", p.Detail)
	}
	if h.Calls() != 1 {
		t.Errorf("handler ran %d times, want 1", h.Calls())
	}
}

func TestPanickingHandlerReleasesKey(t *testing.T) {
	t.Parallel()
	var panicked atomic.Bool
	h := &counter{h: func(w http.ResponseWriter, r *http.Request) {
		if !panicked.Swap(true) {
			panic("boom")
		}
		created(w, r)
	}}
	mw := newMiddleware(t, idempotency.Config{}).Handler(h)
	func() {
		defer func() {
			if p := recover(); p != "boom" {
				t.Fatalf("recovered %v, want the handler's panic to propagate", p)
			}
		}()
		serve(mw, request{key: testKey})
	}()
	if w := serve(mw, request{key: testKey}); w.Code != http.StatusCreated || w.Header().Get(idempotency.ReplayedHeader) != "" {
		t.Fatalf("retry after panic: status %d, want a fresh 201", w.Code)
	}
	if h.Calls() != 2 {
		t.Errorf("handler ran %d times, want 2", h.Calls())
	}
}

func TestPanicIsStoredWhenPolicyStoresServerErrors(t *testing.T) {
	t.Parallel()
	h := &counter{h: func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }}
	mw := newMiddleware(t, idempotency.Config{ShouldStore: func(int) bool { return true }}).Handler(h)
	func() {
		defer func() { _ = recover() }()
		serve(mw, request{key: testKey})
	}()
	w := serve(mw, request{key: testKey})
	wantProblem(t, w, http.StatusInternalServerError, idempotency.ProblemRequestFailed)
	if w.Header().Get(idempotency.ReplayedHeader) != "true" {
		t.Error("stored failure is not marked as replayed")
	}
	if h.Calls() != 1 {
		t.Errorf("handler ran %d times, want 1 (at-most-once policy)", h.Calls())
	}
}

func TestScopesAreIsolated(t *testing.T) {
	t.Parallel()
	h := &counter{h: func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("account " + r.Header.Get("X-Account")))
	}}
	mw := newMiddleware(t, idempotency.Config{
		Scope: func(r *http.Request) string { return r.Header.Get("X-Account") },
	}).Handler(h)
	alice := request{key: testKey, header: http.Header{"X-Account": {"alice"}}}
	bob := request{key: testKey, header: http.Header{"X-Account": {"bob"}}}
	serve(mw, alice)
	if w := serve(mw, bob); w.Body.String() != "account bob" || w.Header().Get(idempotency.ReplayedHeader) != "" {
		t.Fatalf("bob got %q (replayed %q); must not see alice's response", w.Body, w.Header().Get(idempotency.ReplayedHeader))
	}
	wantReplay(t, serve(mw, alice), http.StatusOK, "account alice")
	if h.Calls() != 2 {
		t.Errorf("handler ran %d times, want 2", h.Calls())
	}
}

func TestScopesCannotCollideThroughSeparators(t *testing.T) {
	t.Parallel()
	pairs := [][2]string{{"a:b", "c"}, {"a", "b:c"}, {`a"`, "b"}, {"a", `"b`}, {"", `"":x`}, {"\x00", "x"}}
	seen := map[string][2]string{}
	for _, p := range pairs {
		k := idempotency.StorageKey(p[0], p[1])
		if prev, dup := seen[k]; dup {
			t.Fatalf("scope/key %q and %q share storage key %q", prev, p, k)
		}
		seen[k] = p
		for i := range len(k) {
			if k[i] < 0x20 || k[i] > 0x7e {
				t.Fatalf("storage key %q is not printable ASCII", k)
			}
		}
	}
}

func TestImplicitStatusAndBodylessResponses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		handler http.HandlerFunc
		status  int
	}{
		{"handler writes nothing", func(http.ResponseWriter, *http.Request) {}, http.StatusOK},
		{"body without WriteHeader", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(nil) }, http.StatusOK},
		{"204 No Content", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }, http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mw := newMiddleware(t, idempotency.Config{}).Handler(tt.handler)
			serve(mw, request{key: testKey})
			w := serve(mw, request{key: testKey})
			wantReplay(t, w, tt.status, "")
			if tt.status == http.StatusNoContent && w.Header().Get("Content-Length") != "" {
				t.Error("204 replay carries Content-Length")
			}
		})
	}
}

func TestInformationalResponsesAreNotTheResult(t *testing.T) {
	t.Parallel()
	mw := newMiddleware(t, idempotency.Config{}).Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", "</style.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("queued"))
	}))
	serve(mw, request{key: testKey})
	wantReplay(t, serve(mw, request{key: testKey}), http.StatusAccepted, "queued")
}

func TestSuperfluousWriteHeaderKeepsFirstStatus(t *testing.T) {
	t.Parallel()
	mw := newMiddleware(t, idempotency.Config{}).Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		w.WriteHeader(http.StatusTeapot) // ignored by net/http, and by the recorder
		_, _ = w.Write([]byte("ok"))
	}))
	if w := serve(mw, request{key: testKey}); w.Code != http.StatusAccepted {
		t.Fatalf("original status %d, want 202", w.Code)
	}
	wantReplay(t, serve(mw, request{key: testKey}), http.StatusAccepted, "ok")
}

func TestHeadersChangedAfterWriteHeaderAreNotStored(t *testing.T) {
	t.Parallel()
	mw := newMiddleware(t, idempotency.Config{}).Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Header().Set("Content-Type", "application/json") // too late: not sent
	}))
	serve(mw, request{key: testKey})
	if got := serve(mw, request{key: testKey}).Header().Get("Content-Type"); got != "text/plain" {
		t.Errorf("replayed Content-Type = %q, want the one actually sent", got)
	}
}

func TestFlushingHandler(t *testing.T) {
	t.Parallel()
	mw := newMiddleware(t, idempotency.Config{}).Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("part1 "))
		w.(http.Flusher).Flush()
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("ResponseController.Flush: %v", err)
		}
		_, _ = w.Write([]byte("part2"))
	}))
	first := serve(mw, request{key: testKey})
	if !first.Flushed {
		t.Error("flush did not reach the underlying writer")
	}
	wantReplay(t, serve(mw, request{key: testKey}), http.StatusOK, "part1 part2")
}

func TestFlushBeforeWriteSetsImplicitStatus(t *testing.T) {
	t.Parallel()
	mw := newMiddleware(t, idempotency.Config{}).Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush()
	}))
	serve(mw, request{key: testKey})
	wantReplay(t, serve(mw, request{key: testKey}), http.StatusOK, "")
}

func TestClientDisconnectDoesNotPreventStoring(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	mw := newMiddleware(t, idempotency.Config{}).Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel() // the client goes away while the handler is still working
		<-r.Context().Done()
		w.WriteHeader(http.StatusCreated)
	}))
	serve(mw, request{key: testKey, ctx: ctx})
	wantReplay(t, serve(mw, request{key: testKey}), http.StatusCreated, "")
}

func TestHijackedResponseReleasesKey(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	mw := newMiddleware(t, idempotency.Config{}).Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("Hijack: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
		_ = buf.Flush()
	}))
	// The client sees the hijacked response before the middleware has
	// released the key, so wait for the server side to return each time.
	served := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { served <- struct{}{} }()
		mw.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	for range 2 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(idempotency.DefaultHeaderName, testKey)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		<-served
		if resp.StatusCode != http.StatusOK || resp.Header.Get(idempotency.ReplayedHeader) != "" {
			t.Fatalf("status %d replayed %q, want a fresh 200", resp.StatusCode, resp.Header.Get(idempotency.ReplayedHeader))
		}
	}
	if calls.Load() != 2 {
		t.Errorf("handler ran %d times, want 2 (hijacked responses are not stored)", calls.Load())
	}
}

func TestResponseControllerReachesUnderlyingWriter(t *testing.T) {
	t.Parallel()
	mw := newMiddleware(t, idempotency.Config{}).Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Deadlines are not implemented by the recorder itself; the
		// ResponseController must find them through Unwrap.
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
			t.Errorf("SetWriteDeadline through the middleware: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	srv := httptest.NewServer(mw)
	t.Cleanup(srv.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(idempotency.DefaultHeaderName, testKey)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestProblemCustomisation(t *testing.T) {
	t.Parallel()
	var gotSlug string
	mw := newMiddleware(t, idempotency.Config{
		ProblemTypeBase: "https://api.example.com/problems/",
		ProblemWriter: func(w http.ResponseWriter, r *http.Request, p *idempotency.Problem) {
			gotSlug = p.Slug
			if !strings.HasPrefix(p.Type, "https://api.example.com/problems/") {
				t.Errorf("type = %q, want the custom base", p.Type)
			}
			idempotency.WriteProblem(w, r, p)
		},
	}).Handler(http.HandlerFunc(created))
	w := serve(mw, request{})
	if gotSlug != idempotency.ProblemKeyMissing || w.Code != http.StatusBadRequest {
		t.Fatalf("slug = %q, status = %d", gotSlug, w.Code)
	}
}

// syncBuffer is a goroutine-safe bytes.Buffer for capturing logs.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// faultyStore wraps a Store and injects failures. The error fields make a
// method fail without reaching the store. The hooks, when set, run in place
// of a method and may call next to delegate; n counts the method's calls
// from 1.
type faultyStore struct {
	idempotency.Store
	acquireErr  error
	completeErr error
	releaseErr  error

	onAcquire  func(ctx context.Context, n int, next func() (idempotency.Acquisition, error)) (idempotency.Acquisition, error)
	onExtend   func(ctx context.Context, n int, next func() error) error
	onComplete func(ctx context.Context, next func() error) error

	acquireCalls, extendCalls, releaseCalls atomic.Int32
}

func (f *faultyStore) Acquire(ctx context.Context, key, token string, fp idempotency.Fingerprint, lease time.Duration) (idempotency.Acquisition, error) {
	n := int(f.acquireCalls.Add(1))
	if f.acquireErr != nil {
		return idempotency.Acquisition{}, f.acquireErr
	}
	next := func() (idempotency.Acquisition, error) { return f.Store.Acquire(ctx, key, token, fp, lease) }
	if f.onAcquire != nil {
		return f.onAcquire(ctx, n, next)
	}
	return next()
}

func (f *faultyStore) Complete(ctx context.Context, key, token string, resp *idempotency.Response, ttl time.Duration) error {
	if f.completeErr != nil {
		return f.completeErr
	}
	next := func() error { return f.Store.Complete(ctx, key, token, resp, ttl) }
	if f.onComplete != nil {
		return f.onComplete(ctx, next)
	}
	return next()
}

func (f *faultyStore) Release(ctx context.Context, key, token string) error {
	f.releaseCalls.Add(1)
	if f.releaseErr != nil {
		return f.releaseErr
	}
	return f.Store.Release(ctx, key, token)
}

func (f *faultyStore) Extend(ctx context.Context, key, token string, lease time.Duration) error {
	n := int(f.extendCalls.Add(1))
	next := func() error { return f.Store.Extend(ctx, key, token, lease) }
	if f.onExtend != nil {
		return f.onExtend(ctx, n, next)
	}
	return next()
}

// hang blocks until ctx is done, like a store call stuck on a dead
// connection, and returns the context's error.
func hang(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func newFaulty(t *testing.T, f *faultyStore) (*faultyStore, *slog.Logger, *syncBuffer) {
	t.Helper()
	s := memstore.New(memstore.WithCleanupInterval(0))
	t.Cleanup(func() { _ = s.Close() })
	f.Store = s
	logs := &syncBuffer{}
	return f, slog.New(slog.NewTextHandler(logs, nil)), logs
}

func TestStoreFailures(t *testing.T) {
	t.Parallel()
	t.Run("acquire fails closed with 503", func(t *testing.T) {
		t.Parallel()
		store, logger, logs := newFaulty(t, &faultyStore{acquireErr: errors.New("connection refused")})
		h := &counter{h: created}
		mw := newMiddleware(t, idempotency.Config{Store: store, Logger: logger}).Handler(h)
		wantProblem(t, serve(mw, request{key: testKey}), http.StatusServiceUnavailable, idempotency.ProblemStoreUnavailable)
		if h.Calls() != 0 {
			t.Error("handler ran although the store was unavailable")
		}
		if !strings.Contains(logs.String(), "connection refused") {
			t.Errorf("store error not logged: %s", logs)
		}
	})
	t.Run("acquire canceled by client is not logged as an error", func(t *testing.T) {
		t.Parallel()
		store, logger, logs := newFaulty(t, &faultyStore{})
		mw := newMiddleware(t, idempotency.Config{Store: store, Logger: logger}).Handler(http.HandlerFunc(created))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		wantProblem(t, serve(mw, request{key: testKey, ctx: ctx}), http.StatusServiceUnavailable, idempotency.ProblemStoreUnavailable)
		if logs.String() != "" {
			t.Errorf("unexpected log output: %s", logs)
		}
	})
	t.Run("complete failure is logged and the response still delivered", func(t *testing.T) {
		t.Parallel()
		store, logger, logs := newFaulty(t, &faultyStore{completeErr: errors.New("disk full")})
		mw := newMiddleware(t, idempotency.Config{Store: store, Logger: logger}).Handler(http.HandlerFunc(created))
		if w := serve(mw, request{key: testKey}); w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", w.Code)
		}
		if !strings.Contains(logs.String(), "disk full") {
			t.Errorf("complete error not logged: %s", logs)
		}
	})
	t.Run("lost lock at completion is logged as a warning", func(t *testing.T) {
		t.Parallel()
		store, logger, logs := newFaulty(t, &faultyStore{completeErr: idempotency.ErrLockLost})
		mw := newMiddleware(t, idempotency.Config{Store: store, Logger: logger}).Handler(http.HandlerFunc(created))
		serve(mw, request{key: testKey})
		if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "lock lost") {
			t.Errorf("want a lock-lost warning, got: %s", logs)
		}
	})
	t.Run("release failure is logged", func(t *testing.T) {
		t.Parallel()
		store, logger, logs := newFaulty(t, &faultyStore{releaseErr: errors.New("timeout")})
		mw := newMiddleware(t, idempotency.Config{Store: store, Logger: logger}).Handler(
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
		serve(mw, request{key: testKey})
		if !strings.Contains(logs.String(), "releasing key failed") {
			t.Errorf("release error not logged: %s", logs)
		}
	})
	t.Run("completed record without response", func(t *testing.T) {
		t.Parallel()
		mw := newMiddleware(t, idempotency.Config{Store: brokenStore{}}).Handler(http.HandlerFunc(created))
		wantProblem(t, serve(mw, request{key: testKey}), http.StatusServiceUnavailable, idempotency.ProblemStoreUnavailable)
	})
}

// brokenStore violates the Store contract by reporting a completed record
// without a response.
type brokenStore struct{ idempotency.Store }

func (brokenStore) Acquire(context.Context, string, string, idempotency.Fingerprint, time.Duration) (idempotency.Acquisition, error) {
	return idempotency.Acquisition{Existing: idempotency.Record{State: idempotency.StateCompleted, Fingerprint: idempotency.FingerprintOf(
		httptest.NewRequest(http.MethodPost, "/payments", http.NoBody), nil, nil)}}, nil
}

func TestWriteProblem(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	idempotency.WriteProblem(w, httptest.NewRequest(http.MethodPost, "/", http.NoBody), &idempotency.Problem{
		Type: "https://example.com/p", Title: "T", Status: http.StatusConflict, Detail: "D", Slug: "hidden",
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d", w.Code)
	}
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"type": "https://example.com/p", "title": "T", "status": float64(409), "detail": "D"}
	if len(raw) != len(want) {
		t.Fatalf("problem JSON = %v, want exactly %v", raw, want)
	}
	for k, v := range want {
		if raw[k] != v {
			t.Errorf("%s = %v, want %v", k, raw[k], v)
		}
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Error("problem responses must not be cached")
	}
}

func TestStateString(t *testing.T) {
	t.Parallel()
	for state, want := range map[idempotency.State]string{
		idempotency.StateInFlight: "in-flight", idempotency.StateCompleted: "completed", 0: "unknown",
	} {
		if got := state.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", state, got, want)
		}
	}
}

func TestFingerprintString(t *testing.T) {
	t.Parallel()
	var fp idempotency.Fingerprint
	fp[0], fp[31] = 0xab, 0x01
	if got := fp.String(); len(got) != 64 || got[:2] != "ab" || got[62:] != "01" {
		t.Errorf("Fingerprint.String() = %q", got)
	}
}
