# payments example

A small payment-like API that shows the middleware end to end. Everything is
in memory, the "payment provider" is a map, and authentication is a stand-in
(the bearer token is taken as the account name). It is a demo, not a template
for handling real money.

Routes:

| Route                | Idempotency                     |
| -------------------- | ------------------------------- |
| `POST /payments`     | key required, scoped by account |
| `GET /payments`      | none (safe method)              |
| `GET /payments/{id}` | none (safe method)              |

## Run it

From the repository root:

```sh
go run ./examples/payments -addr localhost:18080 -charge-delay 2s
```

`-charge-delay` makes every charge take that long, which makes the in-flight
`409 Conflict` easy to see. The walkthrough below was captured from a real run
of the server with these flags; `Date` headers are left out.
`TestWalkthrough` in `main_test.go` repeats steps 1 to 3 and 5 to 8 and checks
the status codes, the replay marker and the bodies shown here;
`TestInFlightConflict` covers step 4 on a virtual clock.

## Walkthrough

### 1. Create a payment

The key is a Structured Field String, so it is sent in double quotes.

```console
$ curl -si -X POST localhost:18080/payments \
    -H 'Authorization: Bearer alice' \
    -H 'Content-Type: application/json' \
    -H 'Idempotency-Key: "3f1c2a9e-0b7d-4c55-9e0e-5d6b8a7c4f21"' \
    -d '{"amount":1999,"currency":"EUR"}'
HTTP/1.1 201 Created
Content-Type: application/json
Location: /payments/pay_1
Content-Length: 142

{"id":"pay_1","account":"alice","amount":1999,"currency":"EUR","status":"succeeded","idempotency_key":"3f1c2a9e-0b7d-4c55-9e0e-5d6b8a7c4f21"}
```

### 2. Retry it (say the client timed out)

Same key, same request: the stored response comes back immediately, marked
`Idempotent-Replayed: true`. No second charge is made.

```console
$ curl -si -X POST localhost:18080/payments \
    -H 'Authorization: Bearer alice' \
    -H 'Content-Type: application/json' \
    -H 'Idempotency-Key: "3f1c2a9e-0b7d-4c55-9e0e-5d6b8a7c4f21"' \
    -d '{"amount":1999,"currency":"EUR"}'
HTTP/1.1 201 Created
Content-Length: 142
Content-Type: application/json
Idempotent-Replayed: true
Location: /payments/pay_1

{"id":"pay_1","account":"alice","amount":1999,"currency":"EUR","status":"succeeded","idempotency_key":"3f1c2a9e-0b7d-4c55-9e0e-5d6b8a7c4f21"}
```

### 3. Reuse the key for a different request

```console
$ curl -si -X POST localhost:18080/payments \
    -H 'Authorization: Bearer alice' \
    -H 'Content-Type: application/json' \
    -H 'Idempotency-Key: "3f1c2a9e-0b7d-4c55-9e0e-5d6b8a7c4f21"' \
    -d '{"amount":2999,"currency":"EUR"}'
HTTP/1.1 422 Unprocessable Entity
Cache-Control: no-store
Content-Length: 302
Content-Type: application/problem+json

{"type":"https://github.com/aozmekik/go-idempotency/blob/main/docs/problems.md#key-reused","title":"Idempotency key reused with a different request","status":422,"detail":"This key was already used for a request with a different method, path, query, headers or body. Use a new key for a new request."}
```

### 4. Send a duplicate while the original is still running

The first request is started in the background; the duplicate arrives half a
second later, while the 2-second charge is still in progress. `Retry-After`
is a hint; the original usually finishes well within it.

```console
$ curl -s -o /dev/null -w 'first:  %{http_code}\n' -X POST localhost:18080/payments \
    -H 'Authorization: Bearer alice' \
    -H 'Idempotency-Key: "7a0d3c1e-2b4f-4e6a-8c9d-0f1e2d3c4b5a"' \
    -d '{"amount":500,"currency":"EUR"}' &
$ sleep 0.5
$ curl -si -X POST localhost:18080/payments \
    -H 'Authorization: Bearer alice' \
    -H 'Idempotency-Key: "7a0d3c1e-2b4f-4e6a-8c9d-0f1e2d3c4b5a"' \
    -d '{"amount":500,"currency":"EUR"}'
HTTP/1.1 409 Conflict
Cache-Control: no-store
Content-Length: 266
Content-Type: application/problem+json
Retry-After: 1

{"type":"https://github.com/aozmekik/go-idempotency/blob/main/docs/problems.md#request-in-flight","title":"A request with this idempotency key is being processed","status":409,"detail":"The original request is still being processed. Retry later with the same key."}
first:  201
```

### 5. Forget the key

```console
$ curl -si -X POST localhost:18080/payments \
    -H 'Authorization: Bearer alice' \
    -d '{"amount":1999,"currency":"EUR"}'
HTTP/1.1 400 Bad Request
Cache-Control: no-store
Content-Length: 230
Content-Type: application/problem+json

{"type":"https://github.com/aozmekik/go-idempotency/blob/main/docs/problems.md#key-missing","title":"Idempotency key is required","status":400,"detail":"Send an Idempotency-Key header with a unique, quoted key for this request."}
```

### 6. Send the key without quotes

Many existing clients send bare UUIDs. The draft defines the value as a
Structured Field String, so this middleware rejects them (see the main
README's limitations).

```console
$ curl -si -X POST localhost:18080/payments \
    -H 'Authorization: Bearer alice' \
    -H 'Idempotency-Key: 3f1c2a9e-0b7d-4c55-9e0e-5d6b8a7c4f21' \
    -d '{"amount":1999,"currency":"EUR"}'
HTTP/1.1 400 Bad Request
Cache-Control: no-store
Content-Length: 296
Content-Type: application/problem+json

{"type":"https://github.com/aozmekik/go-idempotency/blob/main/docs/problems.md#key-invalid","title":"Idempotency key is malformed","status":400,"detail":"The Idempotency-Key header must be a single Structured Field String, for example Idempotency-Key: \"8e03978e-40d5-43e8-bc93-6894a57f9324\"."}
```

### 7. Count the payments

Two retries and a conflicting reuse later, alice has exactly the two payments
she meant to make.

```console
$ curl -s localhost:18080/payments -H 'Authorization: Bearer alice'
[{"id":"pay_1","account":"alice","amount":1999,"currency":"EUR","status":"succeeded","idempotency_key":"3f1c2a9e-0b7d-4c55-9e0e-5d6b8a7c4f21"},{"id":"pay_2","account":"alice","amount":500,"currency":"EUR","status":"succeeded","idempotency_key":"7a0d3c1e-2b4f-4e6a-8c9d-0f1e2d3c4b5a"}]
```

### 8. Send alice's first key from another account

`Config.Scope` returns the authenticated account, so bob's request with the
key from step 1 is a new request in bob's own key space: a fresh `201`, not a
replay of alice's payment and not a `422`.

```console
$ curl -si -X POST localhost:18080/payments \
    -H 'Authorization: Bearer bob' \
    -H 'Content-Type: application/json' \
    -H 'Idempotency-Key: "3f1c2a9e-0b7d-4c55-9e0e-5d6b8a7c4f21"' \
    -d '{"amount":1999,"currency":"EUR"}'
HTTP/1.1 201 Created
Content-Type: application/json
Location: /payments/pay_3
Content-Length: 140

{"id":"pay_3","account":"bob","amount":1999,"currency":"EUR","status":"succeeded","idempotency_key":"3f1c2a9e-0b7d-4c55-9e0e-5d6b8a7c4f21"}
```
