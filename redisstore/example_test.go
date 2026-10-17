package redisstore_test

import (
	"log"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	idempotency "github.com/aozmekik/go-idempotency"
	"github.com/aozmekik/go-idempotency/redisstore"
)

// This example needs a Redis server, so it is compiled but not run.
func ExampleNew() {
	client := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	defer func() { _ = client.Close() }()

	mw, err := idempotency.New(idempotency.Config{
		Store: redisstore.New(client, redisstore.WithPrefix("payments-api:idem:")),
	})
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
