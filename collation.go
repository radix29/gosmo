package gosmo

import (
	"strings"
	"unicode"
)

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
// Case-insensitive equality is strings.EqualFold's (Unicode simple folding),
// so it is exactly NameKey(collation, a) == NameKey(collation, b).
//
// It compares case only. Accent, kana and width sensitivity are not
// modelled, so under an _AI collation (`café`/`cafe`), or the kana- and
// width-insensitive defaults (fullwidth `ａ`/`a`), two names it calls
// different can be one object to the server.
func SameName(collation, a, b string) bool {
	return NameKey(collation, a) == NameKey(collation, b)
}

// NameKey is the map key for name under collation: two names have the same
// key exactly when SameName calls them the same, so a map or set of
// server-supplied names keyed by it agrees with SameName. On a case-sensitive
// collation the key is name itself. Otherwise each rune is replaced by the
// smallest rune of its simple case-folding orbit (unicode.SimpleFold), the
// relation strings.EqualFold compares by. Lower-casing is not that relation:
// `ſ` (U+017F) and `s`, or final `ς` and `σ`, fold equal but lower apart, and
// ToLower(ToUpper(s)) joins `İ` (U+0130) to `i`, which EqualFold keeps apart.
//
// The key is for comparing only; it is not a display form.
func NameKey(collation, name string) string {
	if !CollationIgnoresCase(collation) {
		return name
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
