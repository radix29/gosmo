package gosmo

import mssql "github.com/microsoft/go-mssqldb"

// quoting.go centralises T-SQL identifier and literal quoting on the driver's
// own TSQLQuoter, so gosmo, its callers, and gossms share one implementation
// instead of each hand-rolling bracket/quote escaping.

// QuoteName wraps a SQL Server identifier (schema, table, column, ...) in
// square brackets, doubling any embedded closing bracket — the equivalent of
// T-SQL's QUOTENAME(). Use it to build object names safely; note it quotes
// the whole string as one identifier, so pass each part of a multi-part name
// separately.
func QuoteName(name string) string {
	return mssql.TSQLQuoter{}.ID(name)
}

// QuoteLiteral renders s as a T-SQL string literal, including the surrounding
// single quotes and doubling any embedded quote — safe to embed in SQL text
// where a parameter placeholder is not accepted (DDL, dynamic SQL). Prefer a
// query parameter for ordinary values.
func QuoteLiteral(s string) string {
	return mssql.TSQLQuoter{}.Value(s)
}
