package memstore_test

import (
	"log"
	"net/http"
	"time"

	idempotency "github.com/aozmekik/go-idempotency"
	"github.com/aozmekik/go-idempotency/memstore"
)

func ExampleNew() {
	store := memstore.New(
		memstore.WithShards(64),
		memstore.WithCleanupInterval(30*time.Second),
	)
	mw, err := idempotency.New(idempotency.Config{Store: store})
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	mux := http.NewServeMux()
	mux.Handle("POST /payments", mw.Handler(http.NotFoundHandler()))
	// Output:
}
