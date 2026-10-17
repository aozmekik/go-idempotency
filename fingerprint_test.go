package idempotency

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFingerprint(t *testing.T) {
	t.Parallel()
	base := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/payments?currency=EUR", http.NoBody)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Add("X-Tenant", "a")
		return r
	}
	headers := []string{"Content-Type", "X-Tenant"}
	ref := fingerprint(base(), headers, []byte(`{"amount":1}`))

	tests := []struct {
		name   string
		mutate func(*http.Request) []byte
		equal  bool
	}{
		{"identical", func(*http.Request) []byte { return []byte(`{"amount":1}`) }, true},
		{"unselected header", func(r *http.Request) []byte { r.Header.Set("User-Agent", "x"); return []byte(`{"amount":1}`) }, true},
		{"header name case", func(r *http.Request) []byte {
			r.Header = http.Header{"Content-Type": {"application/json"}, "X-Tenant": {"a"}}
			return []byte(`{"amount":1}`)
		}, true},
		{"body", func(*http.Request) []byte { return []byte(`{"amount":2}`) }, false},
		{"method", func(r *http.Request) []byte { r.Method = http.MethodPatch; return []byte(`{"amount":1}`) }, false},
		{"path", func(r *http.Request) []byte { r.URL.Path = "/refunds"; return []byte(`{"amount":1}`) }, false},
		{"query", func(r *http.Request) []byte { r.URL.RawQuery = "currency=USD"; return []byte(`{"amount":1}`) }, false},
		{"selected header value", func(r *http.Request) []byte { r.Header.Set("X-Tenant", "b"); return []byte(`{"amount":1}`) }, false},
		{"selected header repeated", func(r *http.Request) []byte { r.Header.Add("X-Tenant", "a"); return []byte(`{"amount":1}`) }, false},
		{"selected header removed", func(r *http.Request) []byte { r.Header.Del("X-Tenant"); return []byte(`{"amount":1}`) }, false},
		{"selected header empty value", func(r *http.Request) []byte { r.Header.Set("X-Tenant", ""); return []byte(`{"amount":1}`) }, false},
		{"value split across lines", func(r *http.Request) []byte {
			r.Header["X-Tenant"] = []string{"a", ""}
			return []byte(`{"amount":1}`)
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := base()
			body := tt.mutate(r)
			got := fingerprint(r, headers, body)
			if (got == ref) != tt.equal {
				t.Errorf("fingerprint equal = %v, want %v", got == ref, tt.equal)
			}
		})
	}
}

// TestFingerprintFieldBoundaries checks that the length-prefixed encoding
// keeps field boundaries: moving bytes between adjacent fields must change
// the fingerprint.
func TestFingerprintFieldBoundaries(t *testing.T) {
	t.Parallel()
	a := httptest.NewRequest(http.MethodPost, "/ab?c", http.NoBody)
	b := httptest.NewRequest(http.MethodPost, "/a?bc", http.NoBody)
	if fingerprint(a, nil, nil) == fingerprint(b, nil, nil) {
		t.Error("path/query boundary is ambiguous")
	}
	c := httptest.NewRequest(http.MethodPost, "/", http.NoBody)
	c.Header["X-A"] = []string{"1,2"}
	d := httptest.NewRequest(http.MethodPost, "/", http.NoBody)
	d.Header["X-A"] = []string{"1", "2"}
	if fingerprint(c, []string{"X-A"}, nil) == fingerprint(d, []string{"X-A"}, nil) {
		t.Error("header value boundary is ambiguous")
	}
}

func TestFingerprintIsStable(t *testing.T) {
	t.Parallel()
	// Records written by one version must still match after an upgrade, so
	// the encoding is pinned. Changing it requires a new fingerprintVersion.
	r := httptest.NewRequest(http.MethodPost, "/payments?x=1", http.NoBody)
	r.Header.Set("Content-Type", "application/json")
	got := fingerprint(r, []string{"Content-Type"}, []byte(`{"amount":100}`)).String()
	// Computed with an independent Python implementation of the encoding.
	const want = "2667d7253a2b0e50e2b600942a8dd099c0268ce9e46afeff65a1262fa5134b1f"
	if got != want {
		t.Errorf("fingerprint = %s, want %s", got, want)
	}
}
