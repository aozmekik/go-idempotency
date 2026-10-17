package idempotency

import "net/http"

// Test-only exports for the external idempotency_test package.

// StorageKey exposes storageKey.
func StorageKey(scope, key string) string { return storageKey(scope, key) }

// FingerprintOf exposes fingerprint; headers must be canonical and sorted.
func FingerprintOf(r *http.Request, headers []string, body []byte) Fingerprint {
	return fingerprint(r, headers, body)
}
