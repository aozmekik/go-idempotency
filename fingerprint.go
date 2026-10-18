package idempotency

import (
	"crypto/sha256"
	"encoding/binary"
	"net/http"
	"strings"
)

// fingerprintVersion is mixed into every fingerprint so that a future change
// to the encoding can never collide with records written by this version.
const fingerprintVersion = "go-idempotency/fingerprint/v1"

// fingerprint computes the request fingerprint described in ADR 0001: a
// SHA-256 over an unambiguous, length-prefixed encoding of the method, the
// host (lowercased), the escaped path, the raw query, the selected header
// fields (names must be canonical and sorted) and the SHA-256 of the body.
//
// testdata/fingerprint.py is an independent implementation of the encoding.
func fingerprint(r *http.Request, headers []string, body []byte) Fingerprint {
	bodySum := sha256.Sum256(body)
	// Typical requests fit the fixed-size buffer, which stays on the stack.
	buf := make([]byte, 0, 512)
	buf = appendField(buf, fingerprintVersion)
	buf = appendField(buf, r.Method)
	// net/http keeps the Host header (or the authority of an absolute
	// request target) in r.Host, not in r.Header. Host names are
	// case-insensitive (RFC 9110 Section 4.2.3); ToLower does not allocate
	// for a host that is already lowercase.
	buf = appendField(buf, strings.ToLower(r.Host))
	buf = appendField(buf, r.URL.EscapedPath())
	buf = appendField(buf, r.URL.RawQuery)
	buf = binary.AppendUvarint(buf, uint64(len(headers)))
	for _, name := range headers {
		values := r.Header.Values(name)
		buf = appendField(buf, name)
		buf = binary.AppendUvarint(buf, uint64(len(values)))
		for _, v := range values {
			buf = appendField(buf, v)
		}
	}
	buf = binary.AppendUvarint(buf, uint64(len(bodySum)))
	buf = append(buf, bodySum[:]...)
	return sha256.Sum256(buf)
}

// appendField appends s with a uvarint length prefix.
func appendField(buf []byte, s string) []byte {
	buf = binary.AppendUvarint(buf, uint64(len(s)))
	return append(buf, s...)
}
