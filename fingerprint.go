package idempotency

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"io"
	"net/http"
)

// fingerprintVersion is mixed into every fingerprint so that a future change
// to the encoding can never collide with records written by this version.
const fingerprintVersion = "go-idempotency/fingerprint/v1"

// fingerprint computes the request fingerprint described in ADR 0001: a
// SHA-256 over an unambiguous, length-prefixed encoding of the method, the
// escaped path, the raw query, the selected header fields (names must be
// canonical and sorted) and the SHA-256 of the body.
func fingerprint(r *http.Request, headers []string, body []byte) Fingerprint {
	bodySum := sha256.Sum256(body)
	h := sha256.New()
	writeField(h, fingerprintVersion)
	writeField(h, r.Method)
	writeField(h, r.URL.EscapedPath())
	writeField(h, r.URL.RawQuery)
	writeUvarint(h, uint64(len(headers)))
	for _, name := range headers {
		values := r.Header.Values(name)
		writeField(h, name)
		writeUvarint(h, uint64(len(values)))
		for _, v := range values {
			writeField(h, v)
		}
	}
	writeField(h, string(bodySum[:]))
	var fp Fingerprint
	h.Sum(fp[:0])
	return fp
}

func writeField(h hash.Hash, s string) {
	writeUvarint(h, uint64(len(s)))
	_, _ = io.WriteString(h, s) // hash.Hash.Write never returns an error
}

func writeUvarint(h hash.Hash, n uint64) {
	var buf [binary.MaxVarintLen64]byte
	_, _ = h.Write(binary.AppendUvarint(buf[:0], n))
}
