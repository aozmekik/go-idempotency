package sfv

import (
	"bytes"
	"encoding/base32"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVectors runs the HTTP Working Group's structured-field test suite
// (https://github.com/httpwg/structured-field-tests) against ParseItem.
// The suite is not vendored; point SFV_TEST_VECTORS at a checkout to run it.
func TestVectors(t *testing.T) {
	dir := os.Getenv("SFV_TEST_VECTORS")
	if dir == "" {
		t.Skip("SFV_TEST_VECTORS not set; skipping structured-field-tests conformance run")
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no *.json test vectors in %s", dir)
	}
	var ran int
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var cases []vector
		if err := json.Unmarshal(data, &cases); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		for _, tc := range cases {
			if tc.HeaderType != "item" || tc.Raw == nil {
				continue
			}
			ran++
			t.Run(filepath.Base(file)+"/"+tc.Name, func(t *testing.T) {
				got, err := ParseItem(strings.Join(tc.Raw, ", "))
				if tc.MustFail {
					if err == nil {
						t.Fatalf("ParseItem(%q) = %+v, want error", tc.Raw, got)
					}
					return
				}
				if err != nil {
					if tc.CanFail {
						return
					}
					t.Fatalf("ParseItem(%q): %v", tc.Raw, err)
				}
				checkItem(t, got, tc.Expected)
			})
		}
	}
	t.Logf("ran %d item vectors from %d files", ran, len(files))
}

type vector struct {
	Name       string          `json:"name"`
	Raw        []string        `json:"raw"`
	HeaderType string          `json:"header_type"`
	MustFail   bool            `json:"must_fail"`
	CanFail    bool            `json:"can_fail"`
	Expected   json.RawMessage `json:"expected"`
}

func checkItem(t *testing.T, got Item, expected json.RawMessage) {
	t.Helper()
	var pair [2]json.RawMessage
	if err := json.Unmarshal(expected, &pair); err != nil {
		t.Fatalf("bad expected value %s: %v", expected, err)
	}
	checkBare(t, "item", got.Value, pair[0])
	var params [][2]json.RawMessage
	if err := json.Unmarshal(pair[1], &params); err != nil {
		t.Fatalf("bad expected params %s: %v", pair[1], err)
	}
	if len(params) != len(got.Params) {
		t.Fatalf("got %d params, want %d", len(got.Params), len(params))
	}
	for i, p := range params {
		var key string
		if err := json.Unmarshal(p[0], &key); err != nil {
			t.Fatal(err)
		}
		if got.Params[i].Key != key {
			t.Errorf("param %d key = %q, want %q", i, got.Params[i].Key, key)
		}
		checkBare(t, "param "+key, got.Params[i].Value, p[1])
	}
}

func checkBare(t *testing.T, what string, got BareItem, want json.RawMessage) {
	t.Helper()
	var typed struct {
		Type  string          `json:"__type"`
		Value json.RawMessage `json:"value"`
	}
	if len(want) > 0 && want[0] == '{' {
		if err := json.Unmarshal(want, &typed); err != nil {
			t.Fatal(err)
		}
		switch typed.Type {
		case "token":
			assertKind(t, what, got, KindToken)
			assertJSONString(t, what, got.Str, typed.Value)
		case "displaystring":
			assertKind(t, what, got, KindDisplayString)
			assertJSONString(t, what, got.Str, typed.Value)
		case "binary":
			assertKind(t, what, got, KindByteSequence)
			var b32 string
			if err := json.Unmarshal(typed.Value, &b32); err != nil {
				t.Fatal(err)
			}
			wantBytes, err := base32.StdEncoding.DecodeString(b32)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes, wantBytes) {
				t.Errorf("%s bytes = %x, want %x", what, got.Bytes, wantBytes)
			}
		case "date":
			assertKind(t, what, got, KindDate)
			var n int64
			if err := json.Unmarshal(typed.Value, &n); err != nil {
				t.Fatal(err)
			}
			if got.Int != n {
				t.Errorf("%s date = %d, want %d", what, got.Int, n)
			}
		default:
			t.Fatalf("unknown __type %q", typed.Type)
		}
		return
	}
	var v any
	if err := json.Unmarshal(want, &v); err != nil {
		t.Fatal(err)
	}
	switch v := v.(type) {
	case bool:
		assertKind(t, what, got, KindBoolean)
		if got.Bool != v {
			t.Errorf("%s = %v, want %v", what, got.Bool, v)
		}
	case string:
		assertKind(t, what, got, KindString)
		if got.Str != v {
			t.Errorf("%s = %q, want %q", what, got.Str, v)
		}
	case float64:
		switch got.Kind {
		case KindInteger:
			if float64(got.Int) != v {
				t.Errorf("%s = %d, want %v", what, got.Int, v)
			}
		case KindDecimal:
			if math.Abs(got.Decimal-v) > 1e-9*math.Max(1, math.Abs(v)) {
				t.Errorf("%s = %v, want %v", what, got.Decimal, v)
			}
		default:
			t.Errorf("%s kind = %v, want a number", what, got.Kind)
		}
	default:
		t.Fatalf("unexpected expected value %s", want)
	}
}

func assertKind(t *testing.T, what string, got BareItem, want Kind) {
	t.Helper()
	if got.Kind != want {
		t.Fatalf("%s kind = %v, want %v", what, got.Kind, want)
	}
}

func assertJSONString(t *testing.T, what, got string, raw json.RawMessage) {
	t.Helper()
	var want string
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("%s = %q, want %q", what, got, want)
	}
}
