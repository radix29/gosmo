package gosmo

import (
	"fmt"
	"strings"
)

// quoteIdent wraps a SQL Server identifier in square brackets, escaping any
// embedded closing brackets. Thin internal alias for the exported QuoteName
// (see quoting.go) so the many internal call sites stay terse.
func quoteIdent(name string) string {
	return QuoteName(name)
}

// escapeSingle escapes single quotes in a string literal for use in T-SQL.
// Prefer parameterised queries ($1 / ?) for values; use this only where
// parameters are not accepted (e.g. DDL statements, stored procedure names).
//
// It escapes only — the surrounding quotes come from the caller's format
// string. It does not bracket-quote: an identifier going into a literal needs
// qualifiedName/quoteIdent first. See QuoteLiteral (quoting.go) for when to
// reach for which.
func escapeSingle(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// nullableStr returns a T-SQL NULL literal or a quoted N'...' string.
func nullableStr(s string) string {
	if s == "" {
		return "NULL"
	}
	return fmt.Sprintf("N'%s'", escapeSingle(s))
}

// nStringLiteral quotes s as an N'...' T-SQL string literal, escaping any
// embedded quote. Unlike nullableStr, an empty s still quotes to N” rather
// than becoming NULL — callers use this for values (e.g. passwords) where
// NULL is never the intended result.
func nStringLiteral(s string) string {
	return fmt.Sprintf("N'%s'", escapeSingle(s))
}

// boolToInt converts a bool to 0/1 for T-SQL BIT parameters.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// qualifiedName returns [schema].[name].
func qualifiedName(schema, name string) string {
	return quoteIdent(schema) + "." + quoteIdent(name)
}

// likeEscape escapes T-SQL LIKE wildcard characters (%, _, [) in s so it can
// be embedded in a pattern (e.g. '%' + @p1 + '%') and matched literally.
// Pair with an ESCAPE '\' clause on the LIKE itself — without the clause the
// backslashes this adds are matched as themselves.
//
// The escaping is not cosmetic. _ and % are both legal in an identifier, so a
// user searching for one gets a wildcard match instead of the name they typed;
// and a name containing [ turns the pattern into a character class that
// silently matches nothing, so the search comes up empty with no explanation.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`, `[`, `\[`)
	return r.Replace(s)
}
