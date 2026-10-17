package idempotency

import (
	"errors"
	"fmt"
	"strings"

	"github.com/aozmekik/go-idempotency/internal/sfv"
)

// ErrInvalidKey is wrapped by the errors [ParseKey] and [FormatKey] return.
var ErrInvalidKey = errors.New("idempotency: invalid idempotency key")

// ParseKey parses an Idempotency-Key field value and returns the key.
//
// The draft defines the field as a Structured Field Item whose value must be
// a String (RFC 9651 Section 3.3.3), so the key must be sent in double quotes:
//
//	Idempotency-Key: "8e03978e-40d5-43e8-bc93-6894a57f9324"
//
// Parameters on the Item are parsed and ignored, as RFC 9651 Section 2.3
// recommends for forward compatibility. The empty string is rejected because
// it cannot identify a request.
func ParseKey(fieldValue string) (string, error) {
	item, err := sfv.ParseItem(fieldValue)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidKey, err)
	}
	if item.Value.Kind != sfv.KindString {
		return "", fmt.Errorf("%w: value is a Structured Field %s, want a String (quote the key)", ErrInvalidKey, item.Value.Kind)
	}
	if item.Value.Str == "" {
		return "", fmt.Errorf("%w: key is empty", ErrInvalidKey)
	}
	return item.Value.Str, nil
}

// FormatKey returns key serialized as a Structured Field String, ready to be
// used as an Idempotency-Key field value. It fails if key is empty or contains
// characters a String cannot carry (anything outside printable ASCII).
func FormatKey(key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("%w: key is empty", ErrInvalidKey)
	}
	var b strings.Builder
	b.Grow(len(key) + 2)
	b.WriteByte('"')
	for i := range len(key) {
		c := key[i]
		if c < 0x20 || c > 0x7e {
			return "", fmt.Errorf("%w: byte 0x%02x at offset %d is not printable ASCII", ErrInvalidKey, c, i)
		}
		if c == '"' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	b.WriteByte('"')
	return b.String(), nil
}
