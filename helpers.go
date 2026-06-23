package gosmo

import (
	"fmt"
	"strings"
)

// quoteIdent wraps a SQL Server identifier in square brackets,
// escaping any embedded closing brackets.
func quoteIdent(name string) string {
	return "[" + strings.ReplaceAll(name, "]", "]]") + "]"
}

// escapeSingle escapes single quotes in a string literal for use in T-SQL.
// Prefer parameterised queries ($1 / ?) for values; use this only where
// parameters are not accepted (e.g. DDL statements, stored procedure names).
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
