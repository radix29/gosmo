package gosmo

import "strings"

// CollationIgnoresCase reports whether collation compares names without
// regard to case: every collation but a case-sensitive (…_CS_…) or binary
// (…_BIN, …_BIN2) one. Matched by whole "_"-separated token, so a collation
// whose name merely contains the letters is not mistaken for one. An empty
// collation — not read — ignores case, the default every install ships with.
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
func SameName(collation, a, b string) bool {
	if CollationIgnoresCase(collation) {
		return strings.EqualFold(a, b)
	}
	return a == b
}
