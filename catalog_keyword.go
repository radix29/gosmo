package gosmo

import (
	"regexp"
	"strings"
)

// The helpers here put a catalog value into a generated script only once it
// has the shape its clause allows. Each reads a *_desc keyword, a numeric
// sql_variant or an already-quoted name the server wrote, so a mismatch means
// a server newer than this code (or a catalog read gone wrong), not an
// attacker. The script then carries a placeholder the server refuses to
// parse, as endpointAuthClause's "<certificate name>" does, rather than text
// nothing here checked. TestNoRawStringFieldIsInterpolatedIntoSQL is what
// keeps new interpolations going through one of these.

// unrecognized is the placeholder for a value of what that is not one the
// clause takes. The value itself stays out: it is the text that did not pass.
func unrecognized(what string) string {
	return "<unrecognized " + what + ">"
}

// scriptKeyword returns the allowed keyword v matches, case-insensitively,
// spelled as allowed spells it, or unrecognized(what).
func scriptKeyword[T ~string](what string, v T, allowed ...T) string {
	for _, a := range allowed {
		if strings.EqualFold(string(v), string(a)) {
			return string(a)
		}
	}
	return unrecognized(what)
}

var integerLiteral = regexp.MustCompile(`^-?[0-9]+$`)

// scriptInteger returns s when it is a plain integer literal (a sequence's
// bounds, read through sql_variant as text), or unrecognized(what).
func scriptInteger(what, s string) string {
	if integerLiteral.MatchString(s) {
		return s
	}
	return unrecognized(what)
}

// scriptObjectName returns s when it is a well-formed multi-part name — the
// already-bracketed form the catalog stores for a synonym's base object or a
// queue's activation procedure — or unrecognized(what).
func scriptObjectName(what, s string) string {
	if validQualifiedObjectName(s) {
		return s
	}
	return unrecognized(what)
}
