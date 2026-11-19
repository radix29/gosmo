package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"strings"
	"sync"
	"unicode"
)

// CollationCodePage returns the code page of collation's non-Unicode types
// (char, varchar, text) — COLLATIONPROPERTY(collation, 'CodePage'): 1252 for
// Latin1_General_CI_AS, 65001 for a _UTF8 collation, and 0 for a
// Unicode-only collation (Indic and others), which no char column can take.
// A name the server does not know is ErrNotFound.
//
// Column.Collation and Database.Collation name the collation; a bulk load or
// a client encoding char data itself needs its code page.
func (s *Server) CollationCodePage(ctx context.Context, collation string) (int, error) {
	var cp sql.NullInt32
	if err := s.queryRowScan(ctx, "SELECT CAST(COLLATIONPROPERTY(@p1, 'CodePage') AS int)",
		[]any{collation}, &cp); err != nil {
		return 0, fmt.Errorf("gosmo: code page of collation %q: %w", collation, err)
	}
	if !cp.Valid {
		return 0, notFoundf("gosmo: code page of collation %q: no such collation", collation)
	}
	return int(cp.Int32), nil
}

// CollationIgnoresCase reports whether collation compares names without
// regard to case: every collation but a case-sensitive (…_CS_…) or binary
// (…_BIN, …_BIN2) one. Matched by whole "_"-separated token, so a collation
// whose name merely contains the letters is not mistaken for one. An empty
// collation — not read — ignores case, the default every install ships with.
// It answers for case alone; see SameName for what it does not model.
//
// The collation to pass is the one of the scope a name lives in: the
// server's (ServerInfo.Collation) for logins, databases and other server
// objects, the database's (Database.Collation) for everything inside it.
func CollationIgnoresCase(collation string) bool {
	for tok := range strings.SplitSeq(strings.ToUpper(collation), "_") {
		switch tok {
		case "CS", "BIN", "BIN2":
			return false
		}
	}
	return true
}

// SameName reports whether a and b name the same object under collation:
// equal ignoring case when CollationIgnoresCase(collation), byte-equal
// otherwise. On a case-sensitive collation `Sales` and `sales` are two
// objects, and folding them would treat a new one as the existing other.
// It is exactly NameKey(collation, a) == NameKey(collation, b); NameKey says
// which letters pair.
//
// It compares case only. Accent, kana and width sensitivity are not
// modelled, so under an _AI collation (`café`/`cafe`), or the kana- and
// width-insensitive defaults (fullwidth `ａ`/`a`), two names it calls
// different can be one object to the server. Neither are a collation's
// ignorable characters (an older collation gives many code points no weight
// at all) or its contractions.
func SameName(collation, a, b string) bool {
	return NameKey(collation, a) == NameKey(collation, b)
}

// NameKey is the map key for name under collation: two names have the same
// key exactly when SameName calls them the same, so a map or set of
// server-supplied names keyed by it agrees with SameName. On a case-sensitive
// collation the key is name itself.
//
// Otherwise each rune is replaced by the smallest rune the collation pairs
// it with ignoring case. Which letters pair is the server's, not Unicode's,
// and differs by collation version and locale: under Turkish and Azeri `i`
// pairs with `İ` (U+0130) and `I` with `ı` (U+0131), under Kazakh `Ё` and `ё`
// stay apart, and the Kelvin sign pairs with `K` only from the 140
// collations on. collation_case_tables.go records the pairing of every
// collation SQL Server 2025 lists (TestLiveCaseTables generates it and
// checks it against the server).
//
// A collation it does not list — the empty one (not read) among them — folds
// by Unicode simple case folding (unicode.SimpleFold), the relation
// strings.EqualFold compares by. Lower-casing is not that relation: `ſ`
// (U+017F) and `s`, or final `ς` and `σ`, fold equal but lower apart, and
// ToLower(ToUpper(s)) joins `İ` to `i`, which EqualFold keeps apart.
//
// The key is for comparing only; it is not a display form.
func NameKey(collation, name string) string {
	if !CollationIgnoresCase(collation) {
		return name
	}
	if f := caseFoldOf(collation); f != nil {
		return strings.Map(f.key, name)
	}
	return strings.Map(foldKeyRune, name)
}

// foldKeyRune is the smallest rune in r's simple case-folding orbit.
func foldKeyRune(r rune) rune {
	if r < 0x80 {
		if 'a' <= r && r <= 'z' {
			return r - ('a' - 'A')
		}
		return r
	}
	least := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		least = min(least, f)
	}
	return least
}

// caseFold is one case class's pairing: each rune's key, the smallest rune
// the class pairs it with. A rune it does not list pairs with no other.
type caseFold struct {
	ascii [0x80]rune
	other map[rune]rune
}

func (f *caseFold) key(r rune) rune {
	if 0 <= r && r < 0x80 {
		return f.ascii[r]
	}
	if k, ok := f.other[r]; ok {
		return k
	}
	return r
}

// caseFolds builds every case class from caseClassGroups: class 0's groups
// in full, each other class as class 0 with its own groups laid over.
var caseFolds = sync.OnceValue(func() []*caseFold {
	apply := func(m map[rune]rune, groups []string) {
		for _, g := range groups {
			k := []rune(g)[0] // a group is written smallest rune first
			for _, r := range g {
				m[r] = k
			}
		}
	}
	base := map[rune]rune{}
	if len(caseClassGroups) > 0 {
		apply(base, caseClassGroups[0])
	}
	folds := make([]*caseFold, len(caseClassGroups))
	for i, groups := range caseClassGroups {
		m := maps.Clone(base)
		if i > 0 {
			apply(m, groups)
		}
		f := &caseFold{other: map[rune]rune{}}
		for r := range rune(0x80) {
			f.ascii[r] = r
		}
		for r, k := range m {
			if r < 0x80 {
				f.ascii[r] = k
			} else if r != k {
				f.other[r] = k
			}
		}
		folds[i] = f
	}
	return folds
})

// caseFlagTokens are the collation-name tokens that set options rather than
// name the collation: what is left is its base (Turkish_100, SQL_Latin1_
// General_CP1). Of the options, only those caseOptionTokens lists can change
// which letters pair.
var caseFlagTokens = map[string]bool{
	"CI": true, "CS": true, "AI": true, "AS": true, "KS": true, "WS": true,
	"SC": true, "UTF8": true, "VSS": true, "BIN": true, "BIN2": true,
}

var caseOptionTokens = []string{"KS", "WS", "VSS", "SC", "UTF8"}

// caseClassKeys is the keys caseClassByCollation may list collation under,
// most specific first: its base with every option it sets that
// caseOptionTokens lists, its base with WS when it sets WS, and its base.
func caseClassKeys(collation string) []string {
	var base []string
	opts := map[string]bool{}
	for tok := range strings.SplitSeq(strings.ToUpper(collation), "_") {
		if caseFlagTokens[tok] {
			opts[tok] = true
		} else {
			base = append(base, tok)
		}
	}
	b := strings.Join(base, "_")
	full := b
	for _, o := range caseOptionTokens {
		if opts[o] {
			full += "_" + o
		}
	}
	keys := []string{full}
	if opts["WS"] && full != b+"_WS" {
		keys = append(keys, b+"_WS")
	}
	if full != b {
		keys = append(keys, b)
	}
	return keys
}

// caseFoldOf is collation's case class, or nil when caseClassByCollation
// does not list it. Answers are cached by collation name: NameKey runs once
// per name a set or map holds.
func caseFoldOf(collation string) *caseFold {
	if f, ok := caseFoldCache.Load(collation); ok {
		return f.(*caseFold)
	}
	var f *caseFold
	for _, k := range caseClassKeys(collation) {
		if c, ok := caseClassByCollation[k]; ok {
			f = caseFolds()[c]
			break
		}
	}
	caseFoldCache.Store(collation, f)
	return f
}

var caseFoldCache sync.Map // collation name → *caseFold (nil: not listed)
