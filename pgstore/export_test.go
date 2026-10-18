package pgstore

// Test-only exports for the external pgstore_test package.

// The statements Acquire runs, so that tests can interpose between them.
const (
	AcquireSQL = acquireSQL
	SelectSQL  = selectSQL
)

// MaxAcquireAttempts exposes maxAcquireAttempts.
const MaxAcquireAttempts = maxAcquireAttempts
