package tsql

import "strings"

// QuoteName wraps a SQL Server identifier (schema, table, column, ...) in
// square brackets, doubling any embedded closing bracket — the equivalent of
// T-SQL's QUOTENAME(). Use it to build object names safely; note it quotes
// the whole string as one identifier, so pass each part of a multi-part name
// separately.
//
// It is the mssql driver's TSQLQuoter.ID, written out here so this package
// needs no driver; gosmo's TestTSQLQuoterParity pins the two together.
func QuoteName(name string) string {
	return "[" + strings.ReplaceAll(name, "]", "]]") + "]"
}

// QualifiedName returns [schema].[name], each part quoted with QuoteName, or
// just [name] when schema is empty. The empty case is the point: the naive
// QuoteName(schema)+"."+QuoteName(name) emits "[].[name]", which OBJECT_ID
// resolves to NULL, so a caller gets an empty result rather than an error.
func QualifiedName(schema, name string) string {
	if schema == "" {
		return QuoteName(name)
	}
	return QuoteName(schema) + "." + QuoteName(name)
}

// UnquoteName undoes QuoteName for one name part: "[a]]b]" is "a]b", and a
// double-quoted identifier, `"a""b"`, is `a"b`. Anything else — a bare name,
// or one whose brackets don't match — comes back unchanged. It does not split
// a multi-part name: "[dbo].[t]" is not one part, so it is returned as is.
//
// Use it where SQL Server hands back an identifier already quoted (showplan
// XML, sys.dm_exec_* text) and the bare name is wanted. strings.Trim(s, "[]")
// is not that: it leaves "]]" doubled and also strips a bracket that is part
// of the name.
func UnquoteName(name string) string {
	if len(name) < 2 {
		return name
	}
	var closer string
	switch {
	case name[0] == '[' && name[len(name)-1] == ']':
		closer = "]"
	case name[0] == '"' && name[len(name)-1] == '"':
		closer = `"`
	default:
		return name
	}
	inner := name[1 : len(name)-1]
	// Every closer inside must be doubled, or the outer pair is not one
	// quoted identifier ("[a]b]", "[a].[b]").
	if strings.Count(inner, closer+closer)*2 != strings.Count(inner, closer) {
		return name
	}
	return strings.ReplaceAll(inner, closer+closer, closer)
}

// QuoteLiteral renders s as a T-SQL Unicode string literal — N'…', doubling
// any embedded quote — safe to embed in SQL text where a parameter
// placeholder is not accepted (DDL, dynamic SQL). Prefer a query parameter
// for ordinary values.
//
// The N prefix is not optional. Without it the literal is varchar, converted
// through the current database's code page, and any character outside that
// code page becomes '?': a Cyrillic or CJK file path in CREATE DATABASE's
// FILENAME created a file somewhere else, or failed.
//
// It quotes a value, not an identifier. An identifier that ends up inside a
// string literal — the argument to OBJECT_ID, DBCC SHOW_STATISTICS and
// similar — needs QuoteName on each part first: without the brackets a name
// containing '.' parses as a multi-part name and resolves to the wrong
// object or to NULL.
func QuoteLiteral(s string) string {
	return "N" + QuoteAnsiLiteral(s)
}

// QuoteAnsiLiteral renders s as a T-SQL non-Unicode string literal — '…',
// doubling any embedded quote, with no N prefix. It is for the few places
// that want varchar and not nvarchar: an Extended Events predicate on an
// ansi_string field, whose like_i_sql_ansi_string comparator takes a
// varchar. Everywhere else use QuoteLiteral; a character outside the current
// code page becomes '?' here, which is the reason QuoteLiteral has its N.
//
// It is the mssql driver's TSQLQuoter.Value for a string, written out here
// so this package needs no driver; gosmo's TestTSQLQuoterParity pins the two
// together.
func QuoteAnsiLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
