-- Schema for github.com/aozmekik/go-idempotency/pgstore (version 1).
--
-- One row per idempotency key. A row is in flight while lock_token is set
-- and completed once status_code is set; the CHECK constraint keeps the two
-- states exclusive. Rows whose expires_at has passed are treated as absent
-- and are removed by Store.DeleteExpired.

CREATE TABLE IF NOT EXISTS idempotency_keys (
    key         text        PRIMARY KEY,
    fingerprint bytea       NOT NULL CHECK (octet_length(fingerprint) = 32),
    lock_token  text,
    status_code integer     CHECK (status_code BETWEEN 100 AND 999),
    header      jsonb,
    body        bytea,
    expires_at  timestamptz NOT NULL,
    CONSTRAINT idempotency_keys_state CHECK (
        (lock_token IS NOT NULL AND status_code IS NULL)
        OR (lock_token IS NULL AND status_code IS NOT NULL)
    )
);

CREATE INDEX IF NOT EXISTS idempotency_keys_expires_at_idx
    ON idempotency_keys (expires_at);
