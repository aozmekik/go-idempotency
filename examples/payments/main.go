// Command payments is a small payment-like API that demonstrates the
// idempotency middleware. It keeps everything in memory and fakes both
// authentication and the payment provider; it is a demo, not a template for
// handling real money.
//
//	go run ./examples/payments -addr :8080
//
// See README.md in this directory for a curl walkthrough.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	idempotency "github.com/aozmekik/go-idempotency"
	"github.com/aozmekik/go-idempotency/memstore"
)

func main() {
	addr := flag.String("addr", "localhost:8080", "listen address")
	delay := flag.Duration("charge-delay", 0, "simulated payment provider latency (makes 409 Conflict easy to observe)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(*addr, *delay, logger); err != nil {
		logger.Error("payments: exiting", "error", err)
		os.Exit(1)
	}
}

func run(addr string, delay time.Duration, logger *slog.Logger) error {
	store := memstore.New()
	defer func() { _ = store.Close() }()
	handler, err := newHandler(store, delay, logger)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	logger.Info("payments: listening", "addr", addr, "charge_delay", delay)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// newHandler wires the routes: authentication first, so that the
// idempotency middleware can scope keys by account.
func newHandler(store idempotency.Store, delay time.Duration, logger *slog.Logger) (http.Handler, error) {
	idem, err := idempotency.New(idempotency.Config{
		Store:  store,
		Scope:  func(r *http.Request) string { return accountFrom(r.Context()) },
		Logger: logger,
	})
	if err != nil {
		return nil, err
	}
	api := &paymentsAPI{ledger: newLedger(), delay: delay}
	mux := http.NewServeMux()
	mux.Handle("POST /payments", idem.Handler(http.HandlerFunc(api.create)))
	mux.HandleFunc("GET /payments", api.list)
	mux.HandleFunc("GET /payments/{id}", api.get)
	return authenticate(mux), nil
}

type accountKey struct{}

func accountFrom(ctx context.Context) string {
	account, _ := ctx.Value(accountKey{}).(string)
	return account
}

// authenticate is a stand-in for real authentication: the bearer token is
// taken as the account name.
func authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || account == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="payments-demo"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing bearer token"})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), accountKey{}, account)))
	})
}

type payment struct {
	ID       string `json:"id"`
	Account  string `json:"account"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Status   string `json:"status"`
	// IdempotencyKey is what a real integration would forward to the
	// payment provider, so that its side is idempotent too.
	IdempotencyKey string `json:"idempotency_key"`
}

// ledger stands in for the payment provider and the database.
type ledger struct {
	mu       sync.Mutex
	next     int
	payments map[string]payment
}

func newLedger() *ledger { return &ledger{payments: make(map[string]payment)} }

func (l *ledger) charge(p payment) payment {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	p.ID = fmt.Sprintf("pay_%d", l.next)
	p.Status = "succeeded"
	l.payments[p.ID] = p
	return p
}

type paymentsAPI struct {
	ledger *ledger
	delay  time.Duration
}

type createRequest struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

func (a *paymentsAPI) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if req.Amount <= 0 || len(req.Currency) != 3 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "amount must be positive and currency a 3-letter code"})
		return
	}
	if a.delay > 0 {
		select {
		case <-time.After(a.delay):
		case <-r.Context().Done():
		}
	}
	key, _ := idempotency.KeyFromContext(r.Context())
	p := a.ledger.charge(payment{
		Account:        accountFrom(r.Context()),
		Amount:         req.Amount,
		Currency:       strings.ToUpper(req.Currency),
		IdempotencyKey: key,
	})
	w.Header().Set("Location", "/payments/"+p.ID)
	writeJSON(w, http.StatusCreated, p)
}

func (a *paymentsAPI) get(w http.ResponseWriter, r *http.Request) {
	a.ledger.mu.Lock()
	p, ok := a.ledger.payments[r.PathValue("id")]
	a.ledger.mu.Unlock()
	if !ok || p.Account != accountFrom(r.Context()) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "payment not found"})
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *paymentsAPI) list(w http.ResponseWriter, r *http.Request) {
	account := accountFrom(r.Context())
	a.ledger.mu.Lock()
	out := make([]payment, 0, len(a.ledger.payments))
	for i := 1; i <= a.ledger.next; i++ {
		if p := a.ledger.payments[fmt.Sprintf("pay_%d", i)]; p.Account == account {
			out = append(out, p)
		}
	}
	a.ledger.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n')) // nothing useful to do if the client went away
}
