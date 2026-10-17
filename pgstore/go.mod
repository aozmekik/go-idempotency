module github.com/aozmekik/go-idempotency/pgstore

go 1.26

require (
	github.com/aozmekik/go-idempotency v0.1.0
	github.com/jackc/pgx/v5 v5.11.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/text v0.29.0 // indirect
)

// Development: build against the core module in this repository. Consumers
// ignore replace directives and resolve the required version instead.
replace github.com/aozmekik/go-idempotency => ../
