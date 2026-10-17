// Package sfv parses Structured Field Value Items as defined in RFC 9651,
// "Structured Field Values for HTTP".
//
// Only Items are supported, because that is the only top-level type the
// Idempotency-Key header uses. The parser follows the algorithms in RFC 9651
// Section 4.2 step by step, so every bare-item type (Integer, Decimal, String,
// Token, Byte Sequence, Boolean, Date and Display String) is recognised. This
// matters even though the header's value must be a String: parameters attached
// to the Item may carry any bare-item type and must be parsed (and then
// ignored) rather than rejected.
package sfv

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Kind identifies the type of a [BareItem].
type Kind uint8

// The bare-item types of RFC 9651 Section 3.3.
const (
	KindInteger Kind = iota + 1
	KindDecimal
	KindString
	KindToken
	KindByteSequence
	KindBoolean
	KindDate
	KindDisplayString
)

// String returns the RFC 9651 name of the kind.
func (k Kind) String() string {
	switch k {
	case KindInteger:
		return "Integer"
	case KindDecimal:
		return "Decimal"
	case KindString:
		return "String"
	case KindToken:
		return "Token"
	case KindByteSequence:
		return "Byte Sequence"
	case KindBoolean:
		return "Boolean"
	case KindDate:
		return "Date"
	case KindDisplayString:
		return "Display String"
	default:
		return "Kind(" + strconv.Itoa(int(k)) + ")"
	}
}

// BareItem is a parsed bare item. Exactly one value field is meaningful,
// selected by Kind.
type BareItem struct {
	// Kind selects which of the fields below holds the value.
	Kind Kind
	// Int holds Integer and Date values (a Date is seconds since the Unix epoch).
	Int int64
	// Decimal holds Decimal values.
	Decimal float64
	// Str holds String, Token and Display String values.
	Str string
	// Bytes holds Byte Sequence values.
	Bytes []byte
	// Bool holds Boolean values.
	Bool bool
}

// Param is a single key/value parameter attached to an Item.
type Param struct {
	// Key is the parameter name (lowercase, see RFC 9651 Section 3.1.2).
	Key string
	// Value is the parameter value; a parameter without "=" is Boolean true.
	Value BareItem
}

// Item is a parsed Structured Field Item: a bare item plus ordered parameters.
type Item struct {
	// Value is the bare item.
	Value BareItem
	// Params are the item's parameters in input order, with duplicate keys
	// collapsed as RFC 9651 Section 4.2.3.2 requires.
	Params []Param
}

// ErrSyntax is wrapped by every error returned by [ParseItem].
var ErrSyntax = errors.New("invalid structured field")

// ParseItem parses a complete field value as an Item (RFC 9651 Section 4.2
// with field type "item"). Callers that received several field lines must
// join them with "," first, as Section 4.2 requires; for an Item that join
// always produces a parse failure, which is the intended outcome.
func ParseItem(value string) (Item, error) {
	for i := range len(value) {
		if value[i] > 0x7f {
			return Item{}, syntaxError(i, "non-ASCII byte")
		}
	}
	p := parser{s: value}
	p.skipSP()
	item, err := p.item()
	if err != nil {
		return Item{}, err
	}
	p.skipSP()
	if !p.eof() {
		return Item{}, p.errorf("unexpected trailing characters")
	}
	return item, nil
}

func syntaxError(pos int, msg string) error {
	return fmt.Errorf("%w: %s at offset %d", ErrSyntax, msg, pos)
}

type parser struct {
	s   string
	pos int
}

func (p *parser) eof() bool { return p.pos >= len(p.s) }

// peek returns the next byte, or 0 at end of input. 0 never starts a valid
// production, so callers can treat it as "no match".
func (p *parser) peek() byte {
	if p.eof() {
		return 0
	}
	return p.s[p.pos]
}

func (p *parser) errorf(format string, args ...any) error {
	return syntaxError(p.pos, fmt.Sprintf(format, args...))
}

func (p *parser) skipSP() {
	for p.peek() == ' ' {
		p.pos++
	}
}

// item implements RFC 9651 Section 4.2.3.
func (p *parser) item() (Item, error) {
	bare, err := p.bareItem()
	if err != nil {
		return Item{}, err
	}
	params, err := p.parameters()
	if err != nil {
		return Item{}, err
	}
	return Item{Value: bare, Params: params}, nil
}

// bareItem implements RFC 9651 Section 4.2.3.1.
func (p *parser) bareItem() (BareItem, error) {
	c := p.peek()
	switch {
	case c == '-' || isDigit(c):
		return p.number()
	case c == '"':
		s, err := p.str()
		return BareItem{Kind: KindString, Str: s}, err
	case isAlpha(c) || c == '*':
		return BareItem{Kind: KindToken, Str: p.token()}, nil
	case c == ':':
		b, err := p.byteSequence()
		return BareItem{Kind: KindByteSequence, Bytes: b}, err
	case c == '?':
		b, err := p.boolean()
		return BareItem{Kind: KindBoolean, Bool: b}, err
	case c == '@':
		return p.date()
	case c == '%':
		s, err := p.displayString()
		return BareItem{Kind: KindDisplayString, Str: s}, err
	case p.eof():
		return BareItem{}, p.errorf("expected bare item, found end of input")
	default:
		return BareItem{}, p.errorf("unexpected character %q", c)
	}
}

// parameters implements RFC 9651 Section 4.2.3.2.
func (p *parser) parameters() ([]Param, error) {
	var params []Param
	for p.peek() == ';' {
		p.pos++
		p.skipSP()
		key, err := p.key()
		if err != nil {
			return nil, err
		}
		value := BareItem{Kind: KindBoolean, Bool: true}
		if p.peek() == '=' {
			p.pos++
			if value, err = p.bareItem(); err != nil {
				return nil, err
			}
		}
		params = setParam(params, key, value)
	}
	return params, nil
}

// setParam overwrites an existing key in place (keeping its position) or
// appends a new one, as Section 4.2.3.2 step 2.6-2.7 requires.
func setParam(params []Param, key string, value BareItem) []Param {
	for i := range params {
		if params[i].Key == key {
			params[i].Value = value
			return params
		}
	}
	return append(params, Param{Key: key, Value: value})
}

// key implements RFC 9651 Section 4.2.3.3.
func (p *parser) key() (string, error) {
	if c := p.peek(); !isLCAlpha(c) && c != '*' {
		return "", p.errorf("parameter key must start with a-z or '*'")
	}
	start := p.pos
	for isKeyChar(p.peek()) {
		p.pos++
	}
	return p.s[start:p.pos], nil
}

// number implements RFC 9651 Section 4.2.4 (Integer or Decimal).
func (p *parser) number() (BareItem, error) {
	const (
		maxIntegerDigits = 15
		maxDecimalChars  = 16 // 12 integer digits, ".", 3 fraction digits
		maxDecimalInt    = 12
		maxFractionChars = 3
	)
	negative := false
	if p.peek() == '-' {
		negative = true
		p.pos++
	}
	if !isDigit(p.peek()) {
		return BareItem{}, p.errorf("expected digit")
	}
	start := p.pos
	dot := -1
scan:
	for !p.eof() {
		switch c := p.s[p.pos]; {
		case isDigit(c):
		case c == '.' && dot < 0:
			if p.pos-start > maxDecimalInt {
				return BareItem{}, p.errorf("decimal has more than %d integer digits", maxDecimalInt)
			}
			dot = p.pos
		default:
			break scan
		}
		p.pos++
		if dot < 0 && p.pos-start > maxIntegerDigits {
			return BareItem{}, p.errorf("integer has more than %d digits", maxIntegerDigits)
		}
		if dot >= 0 && p.pos-start > maxDecimalChars {
			return BareItem{}, p.errorf("decimal is longer than %d characters", maxDecimalChars)
		}
	}
	digits := p.s[start:p.pos]
	if dot < 0 {
		n, err := strconv.ParseInt(digits, 10, 64)
		if err != nil { // unreachable: at most 15 ASCII digits
			return BareItem{}, p.errorf("integer: %v", err)
		}
		if negative {
			n = -n
		}
		return BareItem{Kind: KindInteger, Int: n}, nil
	}
	frac := p.pos - dot - 1
	if frac == 0 {
		return BareItem{}, p.errorf("decimal must not end with '.'")
	}
	if frac > maxFractionChars {
		return BareItem{}, p.errorf("decimal has more than %d fraction digits", maxFractionChars)
	}
	f, err := strconv.ParseFloat(digits, 64)
	if err != nil { // unreachable: validated above
		return BareItem{}, p.errorf("decimal: %v", err)
	}
	if negative {
		f = -f
	}
	return BareItem{Kind: KindDecimal, Decimal: f}, nil
}

// str implements RFC 9651 Section 4.2.5.
func (p *parser) str() (string, error) {
	p.pos++ // opening DQUOTE, checked by the caller
	var b strings.Builder
	for !p.eof() {
		c := p.s[p.pos]
		p.pos++
		switch {
		case c == '\\':
			if p.eof() {
				return "", p.errorf("unterminated escape in string")
			}
			next := p.s[p.pos]
			if next != '"' && next != '\\' {
				return "", p.errorf("invalid escape \\%c in string", next)
			}
			p.pos++
			b.WriteByte(next)
		case c == '"':
			return b.String(), nil
		case c < 0x20 || c > 0x7e:
			return "", syntaxError(p.pos-1, "control character in string")
		default:
			b.WriteByte(c)
		}
	}
	return "", p.errorf("unterminated string")
}

// token implements RFC 9651 Section 4.2.6. The caller has checked the first
// character, so a token always has at least one character.
func (p *parser) token() string {
	start := p.pos
	p.pos++
	for c := p.peek(); isTChar(c) || c == ':' || c == '/'; c = p.peek() {
		p.pos++
	}
	return p.s[start:p.pos]
}

// byteSequence implements RFC 9651 Section 4.2.7.
func (p *parser) byteSequence() ([]byte, error) {
	p.pos++ // opening ':'
	end := strings.IndexByte(p.s[p.pos:], ':')
	if end < 0 {
		return nil, p.errorf("unterminated byte sequence")
	}
	content := p.s[p.pos : p.pos+end]
	for i := range len(content) {
		if c := content[i]; !isAlpha(c) && !isDigit(c) && c != '+' && c != '/' && c != '=' {
			return nil, syntaxError(p.pos+i, "invalid base64 character")
		}
	}
	p.pos += end + 1
	// Section 4.2.7 says parsers SHOULD NOT fail on missing "=" padding or on
	// non-zero pad bits, so decode the unpadded form leniently.
	out, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(content, "="))
	if err != nil {
		return nil, p.errorf("invalid base64: %v", err)
	}
	return out, nil
}

// boolean implements RFC 9651 Section 4.2.8.
func (p *parser) boolean() (bool, error) {
	p.pos++ // '?'
	switch p.peek() {
	case '1':
		p.pos++
		return true, nil
	case '0':
		p.pos++
		return false, nil
	default:
		return false, p.errorf("boolean must be ?0 or ?1")
	}
}

// date implements RFC 9651 Section 4.2.9.
func (p *parser) date() (BareItem, error) {
	p.pos++ // '@'
	n, err := p.number()
	if err != nil {
		return BareItem{}, err
	}
	if n.Kind != KindInteger {
		return BareItem{}, p.errorf("date must be an integer")
	}
	return BareItem{Kind: KindDate, Int: n.Int}, nil
}

// displayString implements RFC 9651 Section 4.2.10.
func (p *parser) displayString() (string, error) {
	if !strings.HasPrefix(p.s[p.pos:], `%"`) {
		return "", p.errorf(`display string must start with %%"`)
	}
	p.pos += 2
	var buf []byte
	for !p.eof() {
		c := p.s[p.pos]
		p.pos++
		switch {
		case c < 0x20 || c > 0x7e:
			return "", syntaxError(p.pos-1, "control character in display string")
		case c == '%':
			if len(p.s)-p.pos < 2 {
				return "", p.errorf("truncated percent-encoding")
			}
			hi, ok1 := lowerHex(p.s[p.pos])
			lo, ok2 := lowerHex(p.s[p.pos+1])
			if !ok1 || !ok2 {
				return "", p.errorf("percent-encoding must use lowercase hex digits")
			}
			p.pos += 2
			buf = append(buf, hi<<4|lo)
		case c == '"':
			if !utf8.Valid(buf) {
				return "", p.errorf("display string is not valid UTF-8")
			}
			return string(buf), nil
		default:
			buf = append(buf, c)
		}
	}
	return "", p.errorf("unterminated display string")
}

func lowerHex(c byte) (byte, bool) {
	switch {
	case isDigit(c):
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	default:
		return 0, false
	}
}

func isDigit(c byte) bool   { return c >= '0' && c <= '9' }
func isLCAlpha(c byte) bool { return c >= 'a' && c <= 'z' }
func isAlpha(c byte) bool   { return isLCAlpha(c) || (c >= 'A' && c <= 'Z') }

func isKeyChar(c byte) bool {
	return isLCAlpha(c) || isDigit(c) || c == '_' || c == '-' || c == '.' || c == '*'
}

// isTChar reports whether c is an RFC 9110 tchar.
func isTChar(c byte) bool {
	if isAlpha(c) || isDigit(c) {
		return true
	}
	return strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}
