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

// Under a collation the case tables do not list — the empty one (not read),
// or one newer than they are — SameName and NameKey agree with
// strings.EqualFold rune for rune: a key that lower-cases instead splits
// `ſ`/`s` and `ς`/`σ`, and one that round-trips through upper case joins
// `İ`/`i`.
func TestNameKeyMatchesEqualFoldOverEveryRune(t *testing.T) {
	for _, ci := range []string{"", "Klingon_200_CI_AS"} {
		if caseFoldOf(ci) != nil {
			t.Fatalf("%q is in the case tables", ci)
		}
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
}

// A listed collation pairs letters as the server does, which is not
// Unicode's: answers taken from the server's = (TestLiveCaseTables checks
// every pair under every collation).
func TestSameNameFoldsBeyondASCII(t *testing.T) {
	for _, tt := range []struct {
		collation, a, b string
		same            bool
	}{
		{"", "ſa", "sa", true},
		{"", "ς", "σ", true},
		{"", "Kelvin", "\u212Aelvin", true},
		{"", "İ", "i", false},
		{"", "café", "cafe", false},
		{"Latin1_General_CI_AS", "ſa", "sa", false},
		{"Latin1_General_CI_AS", "ς", "σ", true},
		{"Latin1_General_CI_AS", "Kelvin", "\u212Aelvin", false},
		{"Latin1_General_CI_AS", "µs", "μs", false},
		{"Latin1_General_100_CI_AS", "Kelvin", "\u212Aelvin", false},
		{"Japanese_XJIS_140_CI_AS", "Kelvin", "\u212Aelvin", true},
		{"Latin1_General_100_CI_AS", "Ёлка", "ёлка", true},
		{"Kazakh_100_CI_AS", "Ёлка", "ёлка", false},
		{"Latin1_General_100_CI_AS_WS", "Ā", "ā", true},
		{"Maori_100_CI_AS_WS", "Ā", "ā", false},
		{"Maori_100_CI_AS", "Ā", "ā", true},
		{"latin1_general_ci_as", "Sales", "sales", true},
	} {
		if got := SameName(tt.collation, tt.a, tt.b); got != tt.same {
			t.Errorf("SameName(%q, %q, %q) = %v, want %v", tt.collation, tt.a, tt.b, got, tt.same)
		}
		if tt.a != tt.b && SameName("Latin1_General_CS_AS", tt.a, tt.b) {
			t.Errorf("SameName(_CS_, %q, %q) = true", tt.a, tt.b)
		}
	}
	if NameKey("Latin1_General_BIN2", "Sales") != "Sales" {
		t.Error("NameKey under a binary collation changed the name")
	}
}

// An _AI collation pairs case as its _AS sibling: the keys drop both.
func TestCaseClassIgnoresAccentAndCaseTokens(t *testing.T) {
	for _, c := range []string{"Turkish_100_CI_AI", "turkish_100_ci_as_ks_sc_utf8"} {
		if caseFoldOf(c) != caseFoldOf("Turkish_100_CI_AS") {
			t.Errorf("%s is not in Turkish_100_CI_AS's case class", c)
		}
	}
	if caseFoldOf("Turkish_100_CI_AS") == caseFoldOf("Latin1_General_100_CI_AS") {
		t.Error("Turkish_100 and Latin1_General_100 share a case class")
	}
}

// Every case class's key is its own key, so a key stored in a map can be
// looked up by keying it again, and each class keys a rune to a rune it
// actually pairs with (or to itself).
func TestCaseClassKeysAreIdempotent(t *testing.T) {
	for i, f := range caseFolds() {
		for r := rune(0); r <= unicode.MaxRune; r++ {
			if k := f.key(r); f.key(k) != k {
				t.Fatalf("class %d: key(%U) = %U, whose own key is %U", i, r, k, f.key(k))
			}
		}
	}
	if len(caseFolds()) == 0 || len(caseClassByCollation) == 0 {
		t.Fatal("the case tables are empty")
	}
}

// The Turkic collations, by family, as the server's = answers on SQL Server
// 2025 (17.0): every Turkish_* and Azeri_* collation and
// SQL_Latin1_General_CP1254_CI_AS pair `i`/`İ` and keep `i`/`I` apart;
// Turkmen_* and the other SQL_ code pages pair `i`/`I`.
func TestTurkicCaseCollations(t *testing.T) {
	for _, tt := range []struct {
		collation string
		turkic    bool
	}{
		{"Turkish_CI_AS", true},
		{"Turkish_100_CI_AS", true},
		{"Turkish_100_CI_AS_KS_WS_SC_UTF8", true},
		{"turkish_100_ci_as", true},
		{"Azeri_Latin_100_CI_AS", true},
		{"Azeri_Cyrillic_100_CI_AI_SC", true},
		{"SQL_Latin1_General_CP1254_CI_AS", true},
		{"Turkmen_100_CI_AS", false},
		{"SQL_Latin1_General_CP1_CI_AS", false},
		{"SQL_Latin1_General_CP1257_CI_AS", false},
		{"Latin1_General_100_CI_AS", false},
		{"", false},
	} {
		if got := SameName(tt.collation, "id", "İD"); got != tt.turkic {
			t.Errorf("SameName(%q, id, İD) = %v, want %v", tt.collation, got, tt.turkic)
		}
		if got := SameName(tt.collation, "id", "ID"); got == tt.turkic {
			t.Errorf("SameName(%q, id, ID) = %v, want %v", tt.collation, got, !tt.turkic)
		}
	}
}

// The server's answers (`=` under each collation, SQL Server 2025): Turkish
// and Azeri pair `i` with `İ` and `ı` with `I`; Latin1 and Turkmen pair `i`
// with `I` and leave `İ` and `ı` on their own. The Turkic ones also keep the
// ligature `Ĳ`/`ĳ` apart. TestLiveSameNameMatchesServerCollation
// asks the server the same pairs. Kelvin sign/`K` and `ſ`/`s` are not here:
// the server keeps them apart under every collation, while NameKey's
// Unicode orbit joins them (TestSameNameFoldsBeyondASCII pins gosmo's side).
var turkicNamePairs = []struct{ a, b string }{
	{"id", "ID"},
	{"id", "İD"},
	{"ı", "I"},
	{"İ", "I"},
	{"İ", "i"},
	{"ı", "i"},
	{"IX_ID", "IX_id"},
	{"İX_İD", "ix_id"},
	{"Kelvin", "KELVİN"},
	{"ς", "Σ"},
	{"Ĳ", "ĳ"},
}

var turkicNameCases = []struct {
	collation string
	same      []bool // one per turkicNamePairs entry
}{
	{"Turkish_100_CI_AS", []bool{false, true, true, false, true, false, false, true, true, true, false}},
	{"Azeri_Latin_100_CI_AS", []bool{false, true, true, false, true, false, false, true, true, true, false}},
	{"SQL_Latin1_General_CP1254_CI_AS", []bool{false, true, true, false, true, false, false, true, true, true, false}},
	{"Latin1_General_CI_AS", []bool{true, false, false, false, false, false, true, false, false, true, true}},
	{"Turkmen_100_CI_AS", []bool{true, false, false, false, false, false, true, false, false, true, true}},
	{"Turkish_100_CS_AS", []bool{false, false, false, false, false, false, false, false, false, false, false}},
}

func TestSameNameTurkicCase(t *testing.T) {
	for _, c := range turkicNameCases {
		for i, p := range turkicNamePairs {
			if got := SameName(c.collation, p.a, p.b); got != c.same[i] {
				t.Errorf("SameName(%q, %q, %q) = %v, want %v", c.collation, p.a, p.b, got, c.same[i])
			}
			if got := NameKey(c.collation, p.a) == NameKey(c.collation, p.b); got != c.same[i] {
				t.Errorf("NameKey(%q) of %q and %q equal = %v, want %v", c.collation, p.a, p.b, got, c.same[i])
			}
		}
	}
}
