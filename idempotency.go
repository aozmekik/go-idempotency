package idempotency

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Defaults applied by [New] to zero-valued [Config] fields.
const (
	DefaultHeaderName       = "Idempotency-Key"
	DefaultMaxKeyLength     = 255
	DefaultMaxBodyBytes     = 1 << 20 // 1 MiB
	DefaultMaxResponseBytes = 1 << 20 // 1 MiB
	DefaultLease            = 30 * time.Second
	DefaultTTL              = 24 * time.Hour
)

// ReplayedHeader is set to "true" on every replayed response. It is not part
// of the draft; the name follows a widely used convention so that clients
// and logs can tell a replay from a fresh execution.
const ReplayedHeader = "Idempotent-Replayed"

// DefaultMethods returns the methods the middleware applies to by default:
// the non-idempotent methods the draft is written for.
func DefaultMethods() []string { return []string{http.MethodPost, http.MethodPatch} }

// DefaultReplayHeaders returns the response headers that are stored and
// replayed by default. Set-Cookie is deliberately absent: replaying a session
// cookie to whoever presents the key is rarely intended. Add it to
// Config.ReplayHeaders explicitly if you need it.
func DefaultReplayHeaders() []string {
	return []string{
		"Content-Type", "Content-Encoding", "Content-Language", "Content-Location",
		"Location", "ETag", "Last-Modified", "Link", "Vary",
	}
}

// DefaultShouldStore stores every response except server errors (5xx), so a
// client can retry a request that failed because of a transient fault.
func DefaultShouldStore(statusCode int) bool { return statusCode < http.StatusInternalServerError }

// Config configures a [Middleware]. Only Store is required.
type Config struct {
	// Store persists idempotency records. Required.
	Store Store

	// HeaderName is the request header that carries the key.
	// Default: [DefaultHeaderName].
	HeaderName string

	// Methods lists the request methods the middleware applies to; requests
	// with other methods pass straight through. Default: [DefaultMethods].
	Methods []string

	// KeyRequired reports whether a request without a key must be rejected
	// with 400. When it returns false the request passes through without
	// idempotency protection. Requests that do carry a key are always
	// processed. Default: every request with a matching method requires a key.
	KeyRequired func(*http.Request) bool

	// Scope returns the namespace a request's key lives in, typically the
	// authenticated account or API client. Keys in different scopes never
	// interact. Without a scope, any client that guesses another client's key
	// and request could receive that client's stored response, so set this
	// whenever the API has more than one caller. Default: one global scope.
	Scope func(*http.Request) string

	// FingerprintHeaders lists request header fields that are part of the
	// fingerprint in addition to the method, path, query and body.
	// Default: none.
	FingerprintHeaders []string

	// MaxKeyLength is the longest accepted key, in bytes.
	// Default: [DefaultMaxKeyLength].
	MaxKeyLength int

	// MaxBodyBytes bounds the request body that is read into memory to be
	// fingerprinted. Larger requests are rejected with 413.
	// Default: [DefaultMaxBodyBytes].
	MaxBodyBytes int64

	// MaxResponseBytes bounds the response body that is captured for replay.
	// A larger response is still sent to the original client, but cannot be
	// replayed: later requests with the key get a 500 problem
	// (response-not-replayable) rather than running the handler a second
	// time. Default: [DefaultMaxResponseBytes].
	MaxResponseBytes int64

	// Lease is how long an in-flight lock lives without renewal. While the
	// handler runs, the middleware renews it every Lease/3. A renewal that
	// fails or times out is retried at shorter and shorter intervals as the
	// lease runs down, so the lease lapses only if the process stalls or the
	// store stays unreachable for almost a whole lease (ADR 0001). Store
	// calls are bounded by the lease too: Acquire and each renewal attempt by
	// at most Lease/6, and the Complete or Release that records the outcome
	// by whatever remains of the lease. Default: [DefaultLease].
	Lease time.Duration

	// TTL is how long a stored response is kept for replay.
	// Default: [DefaultTTL].
	TTL time.Duration

	// ShouldStore decides from the final status code whether a response is
	// stored for replay. When it returns false, the key is released and a
	// retry runs the handler again. A handler that panics counts as a 500:
	// if ShouldStore(500) is true, later requests with the key get a stored
	// 500 problem (request-failed) instead of running the handler again.
	// Default: [DefaultShouldStore].
	ShouldStore func(statusCode int) bool

	// ReplayHeaders lists the response headers that are stored and replayed.
	// Headers not listed are never replayed. Hop-by-hop headers and
	// Content-Length are rejected; Content-Length is recomputed on replay.
	// A nil slice selects [DefaultReplayHeaders]; an empty non-nil slice
	// replays no headers.
	ReplayHeaders []string

	// ProblemTypeBase prefixes the problem type slugs to form the "type" URI
	// of error responses. Default: [DefaultProblemTypeBase].
	ProblemTypeBase string

	// ProblemWriter writes error responses. Default: [WriteProblem], which
	// writes RFC 9457 application/problem+json.
	ProblemWriter func(http.ResponseWriter, *http.Request, *Problem)

	// Logger receives store failures and lease losses. Default: slog.Default().
	Logger *slog.Logger
}

// Middleware enforces idempotency keys on an http.Handler. Create one with
// [New]; it is safe for concurrent use and may wrap any number of handlers
// (they share one key space per scope).
type Middleware struct {
	cfg           Config
	methods       map[string]struct{}
	fpHeaders     []string
	replayHeaders []string
}

// hopByHop lists the connection-specific header fields of RFC 9110 Section
// 7.6.1 plus legacy ones that proxies still treat as hop-by-hop.
var hopByHop = []string{
	"Connection", "Keep-Alive", "Proxy-Connection", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// New validates cfg, applies defaults and returns a Middleware.
func New(cfg Config) (*Middleware, error) {
	if cfg.Store == nil {
		return nil, errors.New("idempotency: Config.Store is required")
	}
	if cfg.HeaderName == "" {
		cfg.HeaderName = DefaultHeaderName
	}
	if !isToken(cfg.HeaderName) {
		return nil, fmt.Errorf("idempotency: invalid header name %q", cfg.HeaderName)
	}
	cfg.HeaderName = http.CanonicalHeaderKey(cfg.HeaderName)
	if cfg.Methods == nil {
		cfg.Methods = DefaultMethods()
	}
	m := &Middleware{methods: make(map[string]struct{}, len(cfg.Methods))}
	for _, method := range cfg.Methods {
		if !isToken(method) {
			return nil, fmt.Errorf("idempotency: invalid method %q", method)
		}
		m.methods[method] = struct{}{}
	}
	if err := applyLimits(&cfg); err != nil {
		return nil, err
	}
	if cfg.ShouldStore == nil {
		cfg.ShouldStore = DefaultShouldStore
	}
	if cfg.ProblemTypeBase == "" {
		cfg.ProblemTypeBase = DefaultProblemTypeBase
	}
	if cfg.ProblemWriter == nil {
		cfg.ProblemWriter = WriteProblem
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	for _, name := range cfg.FingerprintHeaders {
		if !isToken(name) {
			return nil, fmt.Errorf("idempotency: invalid fingerprint header %q", name)
		}
		m.fpHeaders = append(m.fpHeaders, http.CanonicalHeaderKey(name))
	}
	slices.Sort(m.fpHeaders)
	m.fpHeaders = slices.Compact(m.fpHeaders)

	if cfg.ReplayHeaders == nil {
		cfg.ReplayHeaders = DefaultReplayHeaders()
	}
	m.replayHeaders = []string{} // non-nil: the recorder relies on len only
	for _, name := range cfg.ReplayHeaders {
		canonical := http.CanonicalHeaderKey(name)
		switch {
		case !isToken(name):
			return nil, fmt.Errorf("idempotency: invalid replay header %q", name)
		case slices.Contains(hopByHop, canonical):
			return nil, fmt.Errorf("idempotency: hop-by-hop header %q cannot be replayed", name)
		case canonical == "Content-Length":
			return nil, errors.New("idempotency: Content-Length is recomputed on replay and cannot be listed")
		case canonical == ReplayedHeader:
			return nil, fmt.Errorf("idempotency: %s is set by the middleware and cannot be listed", ReplayedHeader)
		}
		if !slices.Contains(m.replayHeaders, canonical) {
			m.replayHeaders = append(m.replayHeaders, canonical)
		}
	}
	m.cfg = cfg
	return m, nil
}

func applyLimits(cfg *Config) error {
	switch {
	case cfg.MaxKeyLength < 0:
		return errors.New("idempotency: MaxKeyLength must not be negative")
	case cfg.MaxBodyBytes < 0:
		return errors.New("idempotency: MaxBodyBytes must not be negative")
	case cfg.MaxResponseBytes < 0:
		return errors.New("idempotency: MaxResponseBytes must not be negative")
	case cfg.Lease < 0 || (cfg.Lease > 0 && cfg.Lease < time.Millisecond):
		return errors.New("idempotency: Lease must be zero (default) or at least 1ms")
	case cfg.TTL < 0 || (cfg.TTL > 0 && cfg.TTL < time.Millisecond):
		return errors.New("idempotency: TTL must be zero (default) or at least 1ms")
	}
	cfg.MaxKeyLength = cmp.Or(cfg.MaxKeyLength, DefaultMaxKeyLength)
	cfg.MaxBodyBytes = cmp.Or(cfg.MaxBodyBytes, DefaultMaxBodyBytes)
	cfg.MaxResponseBytes = cmp.Or(cfg.MaxResponseBytes, DefaultMaxResponseBytes)
	cfg.Lease = cmp.Or(cfg.Lease, DefaultLease)
	cfg.TTL = cmp.Or(cfg.TTL, DefaultTTL)
	return nil
}

// Handler wraps next with idempotency-key enforcement.
func (m *Middleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.serve(w, r, next)
	})
}

type keyContextKey struct{}

// KeyFromContext returns the idempotency key of the request being handled,
// as sent by the client. It is set in the context of requests the middleware
// passes to the wrapped handler, which may forward the key to downstream
// services.
func KeyFromContext(ctx context.Context) (string, bool) {
	key, ok := ctx.Value(keyContextKey{}).(string)
	return key, ok
}

func (m *Middleware) serve(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if _, ok := m.methods[r.Method]; !ok {
		next.ServeHTTP(w, r)
		return
	}
	values := r.Header.Values(m.cfg.HeaderName)
	if len(values) == 0 {
		if m.cfg.KeyRequired != nil && !m.cfg.KeyRequired(r) {
			next.ServeHTTP(w, r)
			return
		}
		m.problem(w, r, ProblemKeyMissing, fmt.Sprintf("Send an %s header with a unique, quoted key for this request.", m.cfg.HeaderName))
		return
	}
	key, err := ParseKey(strings.Join(values, ", "))
	if err != nil {
		m.problem(w, r, ProblemKeyInvalid, fmt.Sprintf(`The %s header must be a single Structured Field String, for example %s: "8e03978e-40d5-43e8-bc93-6894a57f9324".`, m.cfg.HeaderName, m.cfg.HeaderName))
		return
	}
	if len(key) > m.cfg.MaxKeyLength {
		m.problem(w, r, ProblemKeyInvalid, fmt.Sprintf("The key is %d bytes long; the limit is %d.", len(key), m.cfg.MaxKeyLength))
		return
	}

	body, err := readBody(w, r, m.cfg.MaxBodyBytes)
	if err != nil {
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			m.problem(w, r, ProblemBodyTooLarge, fmt.Sprintf("Request bodies are limited to %d bytes.", m.cfg.MaxBodyBytes))
		} else {
			m.problem(w, r, ProblemBodyUnreadable, "")
		}
		return
	}

	var scope string
	if m.cfg.Scope != nil {
		scope = m.cfg.Scope(r)
	}
	storeKey := storageKey(scope, key)
	fp := fingerprint(r, m.fpHeaders, body)
	token := rand.Text()

	ctx := r.Context()
	acquired := time.Now() // the lease runs from here at the latest
	acq, err := m.acquire(ctx, storeKey, token, fp)
	if err != nil {
		if ctx.Err() == nil {
			m.cfg.Logger.ErrorContext(ctx, "idempotency: acquiring key failed", "error", err)
		}
		m.problem(w, r, ProblemStoreUnavailable, "")
		// The store may have applied the claim even though the call failed,
		// for example when the client disconnected after the write reached
		// the store. Release it so that a retry is not refused with 409 until
		// the lease runs out. The token is this request's own, so the release
		// cannot touch anyone else's claim. Flush first so that the client
		// does not wait for it.
		_ = http.NewResponseController(w).Flush() // optional: not every writer can flush
		m.releaseAbandoned(ctx, storeKey, token, acquired.Add(m.cfg.Lease))
		return
	}
	if !acq.Acquired {
		m.serveExisting(w, r, acq.Existing, fp)
		return
	}

	r = r.WithContext(context.WithValue(ctx, keyContextKey{}, key))
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	r.ContentLength = int64(len(body))
	m.run(w, r, next, storeKey, token, acquired)
}

// acquire claims key, bounding the call like a renewal attempt: a claim
// confirmed only after most of its lease has passed would be of little use.
func (m *Middleware) acquire(ctx context.Context, key, token string, fp Fingerprint) (Acquisition, error) {
	ctx, cancel := context.WithTimeout(ctx, m.attemptTimeout(m.cfg.Lease))
	defer cancel()
	return m.cfg.Store.Acquire(ctx, key, token, fp, m.cfg.Lease)
}

// releaseAbandoned makes a best-effort attempt to release a claim whose
// Acquire reported an error. Most of the time there is nothing to release,
// and the store answers ErrLockLost.
func (m *Middleware) releaseAbandoned(ctx context.Context, key, token string, deadline time.Time) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.attemptTimeout(time.Until(deadline)))
	defer cancel()
	if err := m.cfg.Store.Release(ctx, key, token); err != nil && !errors.Is(err, ErrLockLost) {
		m.cfg.Logger.DebugContext(ctx, "idempotency: releasing the claim of a failed acquire failed", "error", err)
	}
}

// storageKey joins scope and key unambiguously. Go quoting is injective
// (strconv.Unquote inverts it) and its output ends at the first unescaped
// quote, so no two (scope, key) pairs collide. The result is printable ASCII
// because ParseKey only admits printable ASCII keys, which keeps it safe for
// every store, including PostgreSQL text columns that reject NUL bytes.
func storageKey(scope, key string) string {
	return strconv.QuoteToASCII(scope) + ":" + key
}

// readBody reads the whole body, failing with *http.MaxBytesError beyond
// limit. A declared Content-Length within the limit sizes the buffer up
// front, so typical bodies are read without regrowing it.
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	var buf bytes.Buffer
	if r.ContentLength > 0 && r.ContentLength <= limit {
		buf.Grow(int(r.ContentLength) + bytes.MinRead)
	}
	_, err := buf.ReadFrom(http.MaxBytesReader(w, r.Body, limit))
	return buf.Bytes(), err
}

func (m *Middleware) serveExisting(w http.ResponseWriter, r *http.Request, rec Record, fp Fingerprint) {
	switch {
	case rec.Fingerprint != fp:
		m.problem(w, r, ProblemKeyReused, "This key was already used for a request with a different method, path, query, headers or body. Use a new key for a new request.")
	case rec.State == StateInFlight:
		m.problem(w, r, ProblemRequestInFlight, "The original request is still being processed. Retry later with the same key.")
	case rec.Response == nil:
		m.cfg.Logger.ErrorContext(r.Context(), "idempotency: store returned a completed record without a response")
		m.problem(w, r, ProblemStoreUnavailable, "")
	default:
		replay(w, rec.Response)
	}
}

func replay(w http.ResponseWriter, resp *Response) {
	h := w.Header()
	for name, values := range resp.Header {
		h[name] = slices.Clone(values)
	}
	h.Set(ReplayedHeader, "true")
	if !bodyAllowed(resp.StatusCode) {
		w.WriteHeader(resp.StatusCode)
		return
	}
	h.Set("Content-Length", strconv.Itoa(len(resp.Body)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(resp.Body) // nothing useful to do if the client went away
}

// bodyAllowed mirrors RFC 9110: 1xx, 204 and 304 responses have no content.
func bodyAllowed(status int) bool {
	return status >= http.StatusOK && status != http.StatusNoContent && status != http.StatusNotModified
}

// run executes the handler while holding the key's lock, then stores or
// releases the result.
func (m *Middleware) run(w http.ResponseWriter, r *http.Request, next http.Handler, key, token string, acquired time.Time) {
	// Store calls from here on must finish even if the client disconnects:
	// the handler's side effects have happened (or are happening) either way.
	ctx := context.WithoutCancel(r.Context())
	lease := m.keepAlive(ctx, key, token, acquired)
	rec := newRecorder(w, m.replayHeaders, m.cfg.MaxResponseBytes)

	finished := false
	defer func() {
		if finished {
			return
		}
		// The handler panicked or called runtime.Goexit, so the client gets
		// no response and the outcome is unknown. Treat it as a 500 for the
		// storage policy, then let the panic continue to net/http.
		deadline, lost := lease.stop()
		if lost {
			return
		}
		if m.cfg.ShouldStore(http.StatusInternalServerError) {
			m.complete(ctx, deadline, key, token, m.renderProblem(r, ProblemRequestFailed,
				"The original request failed with an internal error and its outcome is unknown. Use a new key only if you intend to repeat it."))
			return
		}
		m.release(ctx, deadline, key, token)
	}()
	next.ServeHTTP(rec, r)
	finished = true

	deadline, lost := lease.stop()
	if lost {
		m.cfg.Logger.ErrorContext(ctx, "idempotency: lease lost while the handler ran; response not stored",
			"method", r.Method, "path", r.URL.Path)
		return
	}
	resp, replayable := rec.result()
	switch {
	case !replayable || !m.cfg.ShouldStore(resp.StatusCode):
		m.release(ctx, deadline, key, token)
	case rec.overflow:
		m.complete(ctx, deadline, key, token, m.renderProblem(r, ProblemResponseNotReplayable,
			fmt.Sprintf("The original request was processed, but its response exceeded the %d-byte replay limit and was not stored. Do not retry it with a new key unless you intend to repeat it.", m.cfg.MaxResponseBytes)))
	default:
		m.complete(ctx, deadline, key, token, resp)
	}
}

// finishContext bounds the store call that records a request's outcome by
// what remains of its lease: once the lease has expired, the fenced write
// would be rejected anyway. The response usually waits in net/http's buffer
// until this call returns, so an unbounded call could hold it back forever.
// A short minimum gives a store whose clock runs slightly behind a chance.
func (m *Middleware) finishContext(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, max(time.Until(deadline), m.minStep()))
}

func (m *Middleware) complete(ctx context.Context, deadline time.Time, key, token string, resp *Response) {
	ctx, cancel := m.finishContext(ctx, deadline)
	defer cancel()
	err := m.cfg.Store.Complete(ctx, key, token, resp, m.cfg.TTL)
	switch {
	case errors.Is(err, ErrLockLost):
		m.cfg.Logger.WarnContext(ctx, "idempotency: lock lost before the response could be stored")
	case err != nil:
		m.cfg.Logger.ErrorContext(ctx, "idempotency: storing response failed", "error", err)
	}
}

func (m *Middleware) release(ctx context.Context, deadline time.Time, key, token string) {
	ctx, cancel := m.finishContext(ctx, deadline)
	defer cancel()
	if err := m.cfg.Store.Release(ctx, key, token); err != nil && !errors.Is(err, ErrLockLost) {
		m.cfg.Logger.ErrorContext(ctx, "idempotency: releasing key failed", "error", err)
	}
}

// renderProblem renders a problem through the configured ProblemWriter into
// a Response that can be stored and replayed. All headers the writer sets are
// kept (the middleware authored them), except Content-Length, which replay
// recomputes.
func (m *Middleware) renderProblem(r *http.Request, slug, detail string) *Response {
	bw := &bufferWriter{header: make(http.Header)}
	m.problem(bw, r, slug, detail)
	bw.header.Del("Content-Length")
	return &Response{StatusCode: cmp.Or(bw.status, http.StatusOK), Header: bw.header, Body: bw.body.Bytes()}
}

// Lease timing; ADR 0001 explains the numbers. The renewal interval leaves
// two thirds of the lease to retry in. Timeouts and retry delays shrink with
// the remaining lease, so that attempts keep fitting before the deadline,
// down to minStep, which keeps renewal from spinning.

func (m *Middleware) renewInterval() time.Duration { return m.cfg.Lease / 3 }

func (m *Middleware) minStep() time.Duration { return m.cfg.Lease / 100 }

// attemptTimeout bounds one Acquire or Extend call given the lease that
// remains: half of it, so that another attempt fits before the deadline, but
// at most Lease/6 and at least minStep.
func (m *Middleware) attemptTimeout(remaining time.Duration) time.Duration {
	ceiling := m.renewInterval() / 2
	if remaining <= 0 {
		return ceiling
	}
	return min(max(remaining/2, m.minStep()), ceiling)
}

// retryDelay is the pause after a failed renewal: a quarter of the lease
// that remains, so that attempts become more frequent as the deadline
// approaches, but at least minStep. Past the deadline the lease is presumed
// lost and attempts return to the normal interval: they can only confirm the
// loss (ErrLockLost) or find a store whose clock runs behind.
func (m *Middleware) retryDelay(remaining time.Duration) time.Duration {
	if remaining <= 0 {
		return m.renewInterval()
	}
	return max(remaining/4, m.minStep())
}

// keepAlive renews the lease of the claim made at acquired until stop is
// called. Renewal runs on a timer rather than a dedicated goroutine: most
// handlers finish before the first renewal is due, so they never pay for one.
func (m *Middleware) keepAlive(ctx context.Context, key, token string, acquired time.Time) *leaseKeeper {
	k := &leaseKeeper{m: m, ctx: ctx, key: key, token: token, deadline: acquired.Add(m.cfg.Lease)}
	k.mu.Lock()
	k.timer = time.AfterFunc(time.Until(acquired.Add(m.renewInterval())), k.renew)
	k.mu.Unlock()
	return k
}

// leaseKeeper renews one in-flight lock. Its mutex guards the fields below
// but is never held across a store call, so stop never waits for a renewal.
type leaseKeeper struct {
	m          *Middleware
	ctx        context.Context // detached from the request; lives exactly as long as the lock
	key, token string

	mu       sync.Mutex
	timer    *time.Timer
	deadline time.Time          // when the lease expires at the latest: start of the last confirmed claim or renewal + Lease
	cancel   context.CancelFunc // cancels the renewal in progress, if any
	stopped  bool
	lost     bool
}

func (k *leaseKeeper) renew() {
	k.mu.Lock()
	if k.stopped {
		k.mu.Unlock()
		return
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(k.ctx, k.m.attemptTimeout(k.deadline.Sub(start)))
	k.cancel = cancel
	k.mu.Unlock()

	err := k.m.cfg.Store.Extend(ctx, k.key, k.token, k.m.cfg.Lease)
	cancel()

	k.mu.Lock()
	defer k.mu.Unlock()
	k.cancel = nil
	if k.stopped {
		return
	}
	switch {
	case err == nil:
		// The store set the new expiry after start, so start + Lease is a
		// safe lower bound for it.
		k.deadline = start.Add(k.m.cfg.Lease)
		k.timer.Reset(time.Until(start.Add(k.m.renewInterval())))
	case errors.Is(err, ErrLockLost):
		k.lost = true
	default:
		remaining := time.Until(k.deadline)
		k.m.cfg.Logger.WarnContext(k.ctx, "idempotency: renewing lease failed; retrying",
			"error", err, "lease_remaining", remaining)
		k.timer.Reset(k.m.retryDelay(remaining))
	}
}

// stop ends renewal and reports the lease deadline and whether the lock is
// known to be lost, in which case the record must not be completed or
// released by this request. A renewal in progress is canceled rather than
// waited for: the Complete or Release that follows is fenced by the token,
// so it is safe whichever of the two calls the store sees first.
func (k *leaseKeeper) stop() (deadline time.Time, lost bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.stopped = true
	k.timer.Stop()
	if k.cancel != nil {
		k.cancel()
	}
	return k.deadline, k.lost
}

// isToken reports whether s is a non-empty RFC 9110 token.
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		if !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			return false
		}
	}
	return true
}
