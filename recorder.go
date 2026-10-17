package idempotency

import (
	"bufio"
	"bytes"
	"net"
	"net/http"
	"slices"
	"strings"
)

// recorder passes a handler's response through to the client while capturing
// what is needed to replay it: the final status, the allowlisted headers as
// they were when the header was written, and up to limit bytes of body.
type recorder struct {
	rw    http.ResponseWriter
	allow []string // canonical header names that may be replayed
	limit int64

	status      int
	header      http.Header
	body        bytes.Buffer
	wroteHeader bool
	overflow    bool // body exceeded limit; body is discarded
	hijacked    bool
}

func newRecorder(rw http.ResponseWriter, allow []string, limit int64) *recorder {
	return &recorder{rw: rw, allow: allow, limit: limit}
}

// Header returns the underlying writer's header map.
func (r *recorder) Header() http.Header { return r.rw.Header() }

// WriteHeader records the final status code and a snapshot of the
// replayable headers, then forwards the call.
func (r *recorder) WriteHeader(code int) {
	if r.wroteHeader {
		r.rw.WriteHeader(code) // let net/http report the superfluous call
		return
	}
	// Informational responses (e.g. 103 Early Hints) may precede the final
	// one; pass them through without treating them as the result. 101 ends
	// HTTP semantics on the connection, so it is final.
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		r.rw.WriteHeader(code)
		return
	}
	r.rw.WriteHeader(code)
	r.wroteHeader = true
	r.status = code
	r.header = r.snapshot()
}

// Write captures p (up to the limit) and forwards it to the client.
func (r *recorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	if !r.overflow {
		if int64(r.body.Len())+int64(len(p)) > r.limit {
			r.overflow = true
			r.body = bytes.Buffer{}
		} else {
			r.body.Write(p)
		}
	}
	return r.rw.Write(p)
}

// FlushError lets http.ResponseController flush through the recorder.
func (r *recorder) FlushError() error {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(r.rw).Flush()
}

// Flush implements http.Flusher for handlers that type-assert for it.
func (r *recorder) Flush() { _ = r.FlushError() }

// Hijack implements http.Hijacker. A hijacked connection's response cannot
// be captured, so the middleware releases the key instead of storing it.
func (r *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(r.rw).Hijack()
	if err == nil {
		r.hijacked = true
	}
	return conn, rw, err
}

// Unwrap exposes the underlying writer to http.ResponseController for the
// remaining optional methods (deadlines, full duplex).
func (r *recorder) Unwrap() http.ResponseWriter { return r.rw }

// result returns the captured response, or false if it cannot be replayed
// faithfully (hijacked connection or 101 Switching Protocols). A handler that
// wrote nothing produced an implicit 200 with an empty body.
func (r *recorder) result() (resp *Response, replayable bool) {
	if r.hijacked || r.status == http.StatusSwitchingProtocols {
		return nil, false
	}
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.header = r.snapshot()
	}
	// The recorder is discarded after this call, so its buffer can be
	// handed over without copying.
	return &Response{StatusCode: r.status, Header: r.header, Body: r.body.Bytes()}, true
}

// snapshot copies the allowlisted headers, minus any the response itself
// declares hop-by-hop through the Connection field (RFC 9110 Section 7.6.1).
func (r *recorder) snapshot() http.Header {
	src := r.rw.Header()
	var hopByHop []string
	for _, v := range src.Values("Connection") {
		for opt := range strings.SplitSeq(v, ",") {
			hopByHop = append(hopByHop, http.CanonicalHeaderKey(strings.TrimSpace(opt)))
		}
	}
	out := make(http.Header, len(r.allow))
	for _, name := range r.allow {
		if slices.Contains(hopByHop, name) {
			continue
		}
		if vs := src.Values(name); len(vs) > 0 {
			out[name] = append([]string(nil), vs...)
		}
	}
	return out
}

// bufferWriter is a minimal in-memory http.ResponseWriter, used to render a
// problem response through Config.ProblemWriter so that it can be stored.
type bufferWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

// Header returns the buffered header map.
func (b *bufferWriter) Header() http.Header { return b.header }

// WriteHeader records the first status code written.
func (b *bufferWriter) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}

// Write appends p to the buffered body.
func (b *bufferWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}
