package idempotency_test

import (
	"errors"
	"testing"

	idempotency "github.com/aozmekik/go-idempotency"
)

func TestParseKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{`"8e03978e-40d5-43e8-bc93-6894a57f9324"`, "8e03978e-40d5-43e8-bc93-6894a57f9324"},
		{`"clCjS7s5"`, "clCjS7s5"},
		{`  "padded"  `, "padded"},
		{`"with spaces and 'quotes'"`, "with spaces and 'quotes'"},
		{`"esc\"aped\\"`, `esc"aped\`},
		{`"k";v=1;alg=sha-256`, "k"},
		{`"~!@#$%^&*()_+-={}[]|:;<>,.?/"`, "~!@#$%^&*()_+-={}[]|:;<>,.?/"},
	}
	for _, tt := range tests {
		got, err := idempotency.ParseKey(tt.in)
		if err != nil {
			t.Errorf("ParseKey(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseKey(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseKeyErrors(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"", " ", `""`, "token", "42", "4.2", "?1", ":YQ==:", "@1700000000", `%"x"`,
		`"unterminated`, `"a" "b"`, `"a", "b"`, `"a";B`, "\"tab\there\"", `"é"`,
	} {
		key, err := idempotency.ParseKey(in)
		if err == nil {
			t.Errorf("ParseKey(%q) = %q, want error", in, key)
			continue
		}
		if !errors.Is(err, idempotency.ErrInvalidKey) {
			t.Errorf("ParseKey(%q) error %v does not wrap ErrInvalidKey", in, err)
		}
	}
}

func TestFormatKey(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"abc", `"abc"`},
		{`a"b`, `"a\"b"`},
		{`a\b`, `"a\\b"`},
		{" ", `" "`},
	}
	for _, tt := range tests {
		got, err := idempotency.FormatKey(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("FormatKey(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
	for _, in := range []string{"", "tab\t", "newline\n", "é", "\x7f"} {
		if got, err := idempotency.FormatKey(in); !errors.Is(err, idempotency.ErrInvalidKey) {
			t.Errorf("FormatKey(%q) = %q, %v; want ErrInvalidKey", in, got, err)
		}
	}
}

// FuzzParseKey checks that ParseKey never panics, only returns printable
// ASCII keys, and that FormatKey and ParseKey are inverses.
func FuzzParseKey(f *testing.F) {
	for _, seed := range []string{
		`"8e03978e-40d5-43e8-bc93-6894a57f9324"`, `"a\"b\\c"`, `"k";p=1`, `""`, `"`, "token", "1", `"a", "b"`,
		`  "x"  `, `"k";a=:Zm9v:;b=?0;c=@1;d=%"%c3%bc"`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, fieldValue string) {
		key, err := idempotency.ParseKey(fieldValue)
		if err != nil {
			if !errors.Is(err, idempotency.ErrInvalidKey) {
				t.Fatalf("ParseKey(%q): error %v does not wrap ErrInvalidKey", fieldValue, err)
			}
			return
		}
		if key == "" {
			t.Fatalf("ParseKey(%q) returned an empty key", fieldValue)
		}
		for i := range len(key) {
			if key[i] < 0x20 || key[i] > 0x7e {
				t.Fatalf("ParseKey(%q) returned non-printable byte 0x%02x", fieldValue, key[i])
			}
		}
		formatted, err := idempotency.FormatKey(key)
		if err != nil {
			t.Fatalf("FormatKey(%q) of a parsed key: %v", key, err)
		}
		again, err := idempotency.ParseKey(formatted)
		if err != nil || again != key {
			t.Fatalf("ParseKey(FormatKey(%q)) = %q, %v", key, again, err)
		}
	})
}

// FuzzFormatKey checks that FormatKey accepts exactly the non-empty
// printable-ASCII strings and that its output always parses back.
func FuzzFormatKey(f *testing.F) {
	for _, seed := range []string{"abc", `a"b\c`, "", "\x00", "ü", " "} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, key string) {
		formatted, err := idempotency.FormatKey(key)
		printable := key != ""
		for i := range len(key) {
			if key[i] < 0x20 || key[i] > 0x7e {
				printable = false
			}
		}
		if printable != (err == nil) {
			t.Fatalf("FormatKey(%q) error = %v, printable = %v", key, err, printable)
		}
		if err != nil {
			return
		}
		got, err := idempotency.ParseKey(formatted)
		if err != nil || got != key {
			t.Fatalf("ParseKey(%q) = %q, %v; want %q", formatted, got, err, key)
		}
	})
}
