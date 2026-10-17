package pgstore_test

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	idempotency "github.com/aozmekik/go-idempotency"
	"github.com/aozmekik/go-idempotency/pgstore"
)

// This example needs a PostgreSQL server, so it is compiled but not run.
func ExampleNew() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "postgres://app@localhost:5432/app")
	if err != nil {
		log.Panic(err)
	}
	defer pool.Close()

	store := pgstore.New(pool)
	// Or apply schema.sql with your own migration tool.
	if err = store.Migrate(ctx); err != nil {
		log.Panic(err)
	}

	// Reclaim space from expired records every ten minutes.
	go func() {
		for range time.Tick(10 * time.Minute) {
			if _, cleanupErr := store.DeleteExpired(ctx); cleanupErr != nil {
				log.Print(cleanupErr)
			}
		}
	}()

	mw, err := idempotency.New(idempotency.Config{Store: store})
	if err != nil {
		log.Panic(err)
	}
	mux := http.NewServeMux()
	mux.Handle("POST /payments", mw.Handler(http.NotFoundHandler()))
	srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Print(err)
	}
}
