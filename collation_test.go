package gosmo

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestCollationIgnoresCase(t *testing.T) {
	for _, tt := range []struct {
		collation string
		ignores   bool
	}{
		{"", true},
		{"SQL_Latin1_General_CP1_CI_AS", true},
		{"Latin1_General_100_CI_AS_SC_UTF8", true},
		{"Latin1_General_CS_AS", false},
		{"latin1_general_cs_as", false},
		{"Latin1_General_BIN", false},
		{"Latin1_General_100_BIN2_UTF8", false},
		// Token match, not substring: "CS" inside another token is not one.
		{"Hebrew_CSX_CI_AS", true},
	} {
		if got := CollationIgnoresCase(tt.collation); got != tt.ignores {
			t.Errorf("CollationIgnoresCase(%q) = %v, want %v", tt.collation, got, tt.ignores)
		}
		if got := SameName(tt.collation, "Sales", "sales"); got != tt.ignores {
			t.Errorf("SameName(%q, Sales, sales) = %v, want %v", tt.collation, got, tt.ignores)
		}
		if !SameName(tt.collation, "Sales", "Sales") {
			t.Errorf("SameName(%q, Sales, Sales) = false", tt.collation)
		}
	}
}

// SameName and NameKey must agree with strings.EqualFold rune for rune: a
// key that lower-cases instead splits `ſ`/`s` and `ς`/`σ`, and one that
// round-trips through upper case joins `İ`/`i`.
func TestNameKeyMatchesEqualFoldOverEveryRune(t *testing.T) {
	const ci = "SQL_Latin1_General_CP1_CI_AS"
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		key := NameKey(ci, string(r))
		if !strings.EqualFold(key, string(r)) {
			t.Fatalf("NameKey(%U) = %q, which EqualFold calls different", r, key)
		}
		if NameKey(ci, key) != key {
			t.Fatalf("NameKey(%U) = %q is not its own key", r, key)
		}
		// Every rune EqualFold calls equal to r is in r's orbit.
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if NameKey(ci, string(f)) != key {
				t.Fatalf("NameKey(%U) = %q, NameKey(%U) = %q; EqualFold calls them equal", r, key, f, NameKey(ci, string(f)))
			}
		}
	}
}

func TestSameNameFoldsBeyondASCII(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		same bool
	}{
		{"ſa", "sa", true},
		{"ς", "σ", true},
		{"Kelvin", "\u212Aelvin", true},
		{"İ", "i", false},
		{"café", "cafe", false},
	} {
		for _, c := range []string{"", "Latin1_General_CI_AS"} {
			if got := SameName(c, tt.a, tt.b); got != tt.same {
				t.Errorf("SameName(%q, %q, %q) = %v, want %v", c, tt.a, tt.b, got, tt.same)
			}
			if got := strings.EqualFold(tt.a, tt.b); got != tt.same {
				t.Errorf("EqualFold(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.same)
			}
		}
		if tt.a != tt.b && SameName("Latin1_General_CS_AS", tt.a, tt.b) {
			t.Errorf("SameName(_CS_, %q, %q) = true", tt.a, tt.b)
		}
	}
	if NameKey("Latin1_General_BIN2", "Sales") != "Sales" {
		t.Error("NameKey under a binary collation changed the name")
	}
}
