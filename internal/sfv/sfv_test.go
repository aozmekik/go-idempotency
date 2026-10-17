package sfv

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseItem(t *testing.T) {
	t.Parallel()
	str := func(s string) BareItem { return BareItem{Kind: KindString, Str: s} }
	tests := []struct {
		name  string
		input string
		want  Item
	}{
		{"string", `"abc"`, Item{Value: str("abc")}},
		{"empty string", `""`, Item{Value: str("")}},
		{"escaped quote and backslash", `"a\"b\\c"`, Item{Value: str(`a"b\c`)}},
		{"percent is a plain character", `"100%"`, Item{Value: str("100%")}},
		{"surrounding spaces", `  "k"  `, Item{Value: str("k")}},
		{"uuid", `"8e03978e-40d5-43e8-bc93-6894a57f9324"`, Item{Value: str("8e03978e-40d5-43e8-bc93-6894a57f9324")}},
		{"integer", `-42`, Item{Value: BareItem{Kind: KindInteger, Int: -42}}},
		{"max integer", `999999999999999`, Item{Value: BareItem{Kind: KindInteger, Int: 999999999999999}}},
		{"decimal", `3.141`, Item{Value: BareItem{Kind: KindDecimal, Decimal: 3.141}}},
		{"token", `foo/bar:baz`, Item{Value: BareItem{Kind: KindToken, Str: "foo/bar:baz"}}},
		{"byte sequence", `:aGVsbG8=:`, Item{Value: BareItem{Kind: KindByteSequence, Bytes: []byte("hello")}}},
		{"byte sequence without padding", `:aGVsbG8:`, Item{Value: BareItem{Kind: KindByteSequence, Bytes: []byte("hello")}}},
		{"boolean", `?0`, Item{Value: BareItem{Kind: KindBoolean, Bool: false}}},
		{"date", `@1659578233`, Item{Value: BareItem{Kind: KindDate, Int: 1659578233}}},
		{"display string", `%"f%c3%bc%c3%bc"`, Item{Value: BareItem{Kind: KindDisplayString, Str: "füü"}}},
		{
			"parameters of every type",
			`"k";a;b=?0;c=1;d=1.5;e="x";f=tok;g=:AA==:;h=@0;i=%"z"`,
			Item{Value: str("k"), Params: []Param{
				{"a", BareItem{Kind: KindBoolean, Bool: true}},
				{"b", BareItem{Kind: KindBoolean, Bool: false}},
				{"c", BareItem{Kind: KindInteger, Int: 1}},
				{"d", BareItem{Kind: KindDecimal, Decimal: 1.5}},
				{"e", str("x")},
				{"f", BareItem{Kind: KindToken, Str: "tok"}},
				{"g", BareItem{Kind: KindByteSequence, Bytes: []byte{0}}},
				{"h", BareItem{Kind: KindDate, Int: 0}},
				{"i", BareItem{Kind: KindDisplayString, Str: "z"}},
			}},
		},
		{"space after semicolon", `"k"; a=1`, Item{Value: str("k"), Params: []Param{{"a", BareItem{Kind: KindInteger, Int: 1}}}}},
		{
			"duplicate parameter keeps first position, last value",
			`"k";a=1;b=2;a=3`,
			Item{Value: str("k"), Params: []Param{
				{"a", BareItem{Kind: KindInteger, Int: 3}},
				{"b", BareItem{Kind: KindInteger, Int: 2}},
			}},
		},
		{"key characters", `"k";*a-b.c_d9`, Item{Value: str("k"), Params: []Param{{"*a-b.c_d9", BareItem{Kind: KindBoolean, Bool: true}}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseItem(tt.input)
			if err != nil {
				t.Fatalf("ParseItem(%q): %v", tt.input, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseItem(%q) = %+v, want %+v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseItemErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"only spaces", "   "},
		{"leading tab", "\t\"k\""},
		{"unterminated string", `"abc`},
		{"invalid escape", `"a\nb"`},
		{"trailing backslash", `"a\`},
		{"control character", "\"a\x01b\""},
		{"tab in string", "\"a\tb\""},
		{"DEL in string", "\"a\x7fb\""},
		{"non-ASCII in string", `"é"`},
		{"two items (combined field lines)", `"a", "b"`},
		{"trailing garbage", `"a"b`},
		{"space before semicolon", `"a" ;b`},
		{"uppercase parameter key", `"a";B=1`},
		{"parameter key starting with digit", `"a";1=1`},
		{"empty parameter key", `"a";=1`},
		{"missing parameter value", `"a";b=`},
		{"integer too long", "1234567890123456"},
		{"decimal too many fraction digits", "1.2345"},
		{"decimal too many integer digits", "1234567890123.1"},
		{"decimal ending in dot", "1."},
		{"bare minus", "-"},
		{"minus then letter", "-a"},
		{"boolean other value", "?2"},
		{"boolean missing value", "?"},
		{"date as decimal", "@1.5"},
		{"unterminated byte sequence", ":aGVsbG8="},
		{"invalid base64 character", ":aGV*bG8=:"},
		{"display string uppercase hex", `%"%C3%BC"`},
		{"display string truncated escape", `%"%c"`},
		{"display string invalid UTF-8", `%"%ff"`},
		{"display string without quote", `%abc`},
		{"unterminated display string", `%"abc`},
		{"unexpected character", "(a)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseItem(tt.input)
			if err == nil {
				t.Fatalf("ParseItem(%q) = %+v, want error", tt.input, got)
			}
			if !errors.Is(err, ErrSyntax) {
				t.Errorf("error %v does not wrap ErrSyntax", err)
			}
		})
	}
}

func TestKindString(t *testing.T) {
	t.Parallel()
	for k := KindInteger; k <= KindDisplayString; k++ {
		if s := k.String(); s == "" || strings.HasPrefix(s, "Kind(") {
			t.Errorf("Kind(%d).String() = %q", k, s)
		}
	}
	if got := Kind(0).String(); got != "Kind(0)" {
		t.Errorf("Kind(0).String() = %q", got)
	}
}

// FuzzParseItem checks that ParseItem never panics, that every error wraps
// ErrSyntax, and that a successfully parsed String survives re-serialisation.
func FuzzParseItem(f *testing.F) {
	for _, seed := range []string{
		`"abc"`, `"a\"b"`, `"k";a=1;b=?0`, `1.5`, `-0`, `:AA==:`, `@1`, `%"%c3%bc"`,
		`tok/en:x`, `"a", "b"`, "", `"`, `"\`, `"k";`, `?1;a=:Zm9v:`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		item, err := ParseItem(input)
		if err != nil {
			if !errors.Is(err, ErrSyntax) {
				t.Fatalf("error %v does not wrap ErrSyntax", err)
			}
			return
		}
		if item.Value.Kind != KindString {
			return
		}
		quoted := `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(item.Value.Str) + `"`
		again, err := ParseItem(quoted)
		if err != nil {
			t.Fatalf("re-parsing %q (from %q): %v", quoted, input, err)
		}
		if again.Value.Str != item.Value.Str {
			t.Fatalf("round trip changed %q to %q", item.Value.Str, again.Value.Str)
		}
	})
}
