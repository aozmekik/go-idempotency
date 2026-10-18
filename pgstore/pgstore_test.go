package pgstore_test

import (
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	idempotency "github.com/aozmekik/go-idempotency"
	"github.com/aozmekik/go-idempotency/pgstore"
	"github.com/aozmekik/go-idempotency/storetest"
)

// These are integration tests: they need a PostgreSQL server and run only
// when IDEMPOTENCY_TEST_PG_DSN is set (CI runs them against a postgres
// service container), for example:
//
//	IDEMPOTENCY_TEST_PG_DSN=postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable

// shared is the connection pool all tests share, created on first use.
var shared struct {
	once sync.Once
	pool *pgxpool.Pool
	err  error
}

func testPool(tb testing.TB) *pgxpool.Pool {
	tb.Helper()
	dsn := os.Getenv("IDEMPOTENCY_TEST_PG_DSN")
	if dsn == "" {
		tb.Skip("IDEMPOTENCY_TEST_PG_DSN not set; skipping PostgreSQL integration tests")
	}
	shared.once.Do(func() {
		ctx := context.Background()
		shared.pool, shared.err = pgxpool.New(ctx, dsn)
		if shared.err == nil {
			shared.err = pgstore.New(shared.pool).Migrate(ctx)
		}
	})
	if shared.err != nil {
		tb.Fatalf("connecting to PostgreSQL: %v", shared.err)
	}
	return shared.pool
}

func TestMain(m *testing.M) {
	code := m.Run()
	if shared.pool != nil {
		shared.pool.Close()
	}
	os.Exit(code)
}

// fakeClock is a manually advanced clock, safe for concurrent use.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestConformance runs the full suite with an application clock, so that
// lease and TTL expiry can be tested without sleeping.
func TestConformance(t *testing.T) {
	p := testPool(t)
	t.Parallel()
	storetest.Run(t, func(t *testing.T) (idempotency.Store, func(time.Duration)) {
		t.Helper()
		clock := &fakeClock{now: time.Now()}
		return pgstore.New(p, pgstore.WithClock(clock.Now)), clock.Advance
	})
}

// TestConformanceDatabaseClock runs the suite with the default database
// clock; the expiry tests are skipped because now() cannot be moved.
func TestConformanceDatabaseClock(t *testing.T) {
	p := testPool(t)
	t.Parallel()
	storetest.Run(t, func(t *testing.T) (idempotency.Store, func(time.Duration)) {
		t.Helper()
		return pgstore.New(p), nil
	})
}

func TestMigrateIsIdempotent(t *testing.T) {
	p := testPool(t)
	t.Parallel()
	s := pgstore.New(p)
	for range 2 {
		if err := s.Migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}

// TestMigrateConcurrently starts several instances at once on a fresh
// schema, as a deployment of several replicas does. CREATE TABLE IF NOT
// EXISTS alone is not safe to run concurrently: without Migrate's advisory
// lock, some calls fail with a unique violation on pg_type.
func TestMigrateConcurrently(t *testing.T) {
	p := testPool(t)
	t.Parallel()
	ctx := t.Context()
	const rounds, instances = 10, 8
	for round := range rounds {
		schema := "migrate_test_" + strings.ToLower(rand.Text())
		if _, err := p.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = p.Exec(context.WithoutCancel(ctx), "DROP SCHEMA "+schema+" CASCADE") })
		cfg := p.Config()
		cfg.ConnConfig.RuntimeParams["search_path"] = schema
		cfg.MaxConns = instances
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		// Open every connection first, so that the Migrate calls overlap.
		conns := make([]*pgxpool.Conn, instances)
		for i := range conns {
			if conns[i], err = pool.Acquire(ctx); err != nil {
				t.Fatal(err)
			}
		}
		for _, c := range conns {
			c.Release()
		}
		errs := make(chan error, instances)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range instances {
			wg.Go(func() {
				<-start
				errs <- pgstore.New(pool).Migrate(ctx)
			})
		}
		close(start)
		wg.Wait()
		close(errs)
		pool.Close()
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
	}
}

// TestDeleteExpired is deliberately not parallel: DeleteExpired affects the
// whole table, and Go starts parallel tests only after sequential ones end.
func TestDeleteExpired(t *testing.T) {
	p := testPool(t)
	clock := &fakeClock{now: time.Now()}
	s := pgstore.New(p, pgstore.WithClock(clock.Now))
	ctx := t.Context()
	prefix := "delete-expired/" + rand.Text() + "/"
	var fp idempotency.Fingerprint
	for _, k := range []string{"a", "b", "c"} {
		if _, err := s.Acquire(ctx, prefix+k, "tok", fp, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Complete(ctx, prefix+"c", "tok", &idempotency.Response{StatusCode: 200}, 10*time.Hour); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Minute)
	if _, err := s.DeleteExpired(ctx); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE key LIKE $1`, prefix+"%").Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Fatalf("%d of our rows left, want 1 (the completed record)", left)
	}
}

func TestErrorsAreWrapped(t *testing.T) {
	p := testPool(t)
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s := pgstore.New(p)
	_, err := s.Acquire(ctx, "k", "tok", idempotency.Fingerprint{}, time.Second)
	if err == nil || !strings.HasPrefix(err.Error(), "pgstore: acquire:") {
		t.Errorf("Acquire err = %v", err)
	}
	if _, err := s.DeleteExpired(ctx); err == nil || !strings.HasPrefix(err.Error(), "pgstore: delete expired:") {
		t.Errorf("DeleteExpired err = %v", err)
	}
	if err := s.Migrate(ctx); err == nil || !strings.HasPrefix(err.Error(), "pgstore: migrate:") {
		t.Errorf("Migrate err = %v", err)
	}
}

// TestCorruptHeaderIsReported stores a header document of the wrong shape,
// which the table's constraints allow (it is valid JSON), and expects
// Acquire to report it rather than replay a response without headers.
func TestCorruptHeaderIsReported(t *testing.T) {
	p := testPool(t)
	t.Parallel()
	key := "corrupt-header/" + rand.Text()
	if _, err := p.Exec(t.Context(), `
INSERT INTO idempotency_keys (key, fingerprint, status_code, header, body, expires_at)
VALUES ($1, $2, 200, '{"Content-Type": "not an array"}', '', now() + interval '1 hour')`, key, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	_, err := pgstore.New(p).Acquire(t.Context(), key, "tok", idempotency.Fingerprint{}, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "decoding headers") {
		t.Fatalf("Acquire err = %v, want a header decoding error", err)
	}
}

// racingDB runs hooks just before Acquire's two statements, to reproduce
// what concurrent requests can do between them.
type racingDB struct {
	pgstore.DB
	beforeInsert, beforeSelect func(ctx context.Context)
}

func (d *racingDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	switch {
	case sql == pgstore.AcquireSQL && d.beforeInsert != nil:
		d.beforeInsert(ctx)
	case sql == pgstore.SelectSQL && d.beforeSelect != nil:
		d.beforeSelect(ctx)
	}
	return d.DB.QueryRow(ctx, sql, args...)
}

// TestAcquireRetriesWhenTheRowVanishes covers Acquire's insert-then-select
// race: the INSERT finds a live row, which is released or expires before
// the SELECT can read it. Acquire must try again, and give up with an error
// if that keeps happening.
func TestAcquireRetriesWhenTheRowVanishes(t *testing.T) {
	p := testPool(t)
	t.Parallel()
	direct := pgstore.New(p)
	fp := idempotency.Fingerprint{1}
	// hold makes another request claim key; vanish releases that claim.
	hold := func(t *testing.T, ctx context.Context, key string) {
		t.Helper()
		if _, err := direct.Acquire(ctx, key, "other", fp, time.Minute); err != nil {
			t.Error(err)
		}
	}
	vanish := func(t *testing.T, ctx context.Context, key string) {
		t.Helper()
		if err := direct.Release(ctx, key, "other"); err != nil {
			t.Error(err)
		}
	}

	t.Run("once", func(t *testing.T) {
		t.Parallel()
		key := "vanish-once/" + rand.Text()
		hold(t, t.Context(), key)
		selects := 0
		s := pgstore.New(&racingDB{DB: p, beforeSelect: func(ctx context.Context) {
			selects++
			if selects == 1 {
				vanish(t, ctx, key)
			}
		}})
		acq, err := s.Acquire(t.Context(), key, "mine", fp, time.Minute)
		if err != nil || !acq.Acquired {
			t.Fatalf("Acquire = %+v, %v; want acquired after one retry", acq, err)
		}
	})
	t.Run("every time", func(t *testing.T) {
		t.Parallel()
		key := "vanish-always/" + rand.Text()
		inserts := 0
		s := pgstore.New(&racingDB{
			DB:           p,
			beforeInsert: func(ctx context.Context) { inserts++; hold(t, ctx, key) },
			beforeSelect: func(ctx context.Context) { vanish(t, ctx, key) },
		})
		_, err := s.Acquire(t.Context(), key, "mine", fp, time.Minute)
		if err == nil || !strings.Contains(err.Error(), "changed state") {
			t.Fatalf("Acquire err = %v, want the retries to give up", err)
		}
		if inserts != pgstore.MaxAcquireAttempts {
			t.Errorf("%d attempts, want %d", inserts, pgstore.MaxAcquireAttempts)
		}
	})
	t.Run("select fails", func(t *testing.T) {
		t.Parallel()
		key := "select-fails/" + rand.Text()
		hold(t, t.Context(), key)
		ctx, cancel := context.WithCancel(t.Context())
		s := pgstore.New(&racingDB{DB: p, beforeSelect: func(context.Context) { cancel() }})
		_, err := s.Acquire(ctx, key, "mine", fp, time.Minute)
		if err == nil || !strings.Contains(err.Error(), "reading existing record") {
			t.Fatalf("Acquire err = %v, want the failed read to be reported", err)
		}
	})
}

func TestMiddlewareWithPostgresStore(t *testing.T) {
	p := testPool(t)
	t.Parallel()
	mw, err := idempotency.New(idempotency.Config{Store: pgstore.New(p)})
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	h := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"pay_1"}`))
	}))
	key, err := idempotency.FormatKey("pg-e2e-" + rand.Text())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		r := httptest.NewRequest(http.MethodPost, "/payments", strings.NewReader(`{"amount":1}`))
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusCreated || w.Body.String() != `{"id":"pay_1"}` {
			t.Fatalf("request %d: %d %s", i, w.Code, w.Body)
		}
		if replayed := w.Header().Get(idempotency.ReplayedHeader) == "true"; replayed != (i > 0) {
			t.Fatalf("request %d: replayed = %v", i, replayed)
		}
	}
	if calls != 1 {
		t.Fatalf("handler ran %d times, want 1", calls)
	}
}

func TestSchemaIsEmbedded(t *testing.T) {
	t.Parallel()
	if !strings.Contains(pgstore.Schema, "CREATE TABLE IF NOT EXISTS idempotency_keys") {
		t.Fatalf("Schema does not contain the table definition:\n%s", pgstore.Schema)
	}
}
