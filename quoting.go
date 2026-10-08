package gosmo

import (
	"strings"

	mssql "github.com/microsoft/go-mssqldb"
)

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

// QuoteNameIfNeeded returns name as it stands when SQL Server would read it
// bare as the same identifier, and QuoteName(name) otherwise — for text a
// person reads or edits (a completion, a generated query), where [brackets]
// on every name are noise. Code that only builds statements should call
// QuoteName: it is always right.
//
// A name stays bare only if it is an ASCII regular identifier — a letter or
// '_', then letters, digits, '_', '@', '#' or '$' — and not a reserved
// keyword (IsReservedKeyword). Two consequences worth knowing:
//
//   - Non-ASCII names are always bracketed. SQL Server's regular-identifier
//     letters are Unicode 3.2's; a letter added since would be a syntax error
//     bare, and bracketing one that needed none is harmless.
//   - The keyword check is not cosmetic. A column named User, read bare, is
//     the USER function: the query runs and returns the caller's user name in
//     every row instead of the column. CURRENT_USER, SESSION_USER,
//     SYSTEM_USER, CURRENT_TIMESTAMP, CURRENT_DATE and NULL do the same.
func QuoteNameIfNeeded(name string) string {
	if isRegularIdentifier(name) && !IsReservedKeyword(name) {
		return name
	}
	return QuoteName(name)
}

// isRegularIdentifier reports whether name is an ASCII regular identifier:
// see QuoteNameIfNeeded. A leading '@' (a variable) or '#' (a temporary
// object) is a different name bare, so neither may start one here.
func isRegularIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c == '_':
		case i > 0 && (c >= '0' && c <= '9' || c == '@' || c == '#' || c == '$'):
		default:
			return false
		}
	}
	return true
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

// SplitName splits a multi-part name the way T-SQL reads one — "dbo.t",
// "[my.db].dbo.[a]]b]", `"x"."y"`, "db..t", "srv.db.dbo.t" — into its parts,
// outermost first, each unquoted as UnquoteName would. A dot inside brackets
// or double quotes is part of the name, white space around a part or a dot is
// ignored ("dbo . t"), and an omitted middle part ("db..t") comes back as "".
//
// It returns an error for what is not one name of at most four parts: an
// unterminated quote, a bracket or quote inside a bare part, white space
// inside one ("dbo t"), an empty first or last part, or more than four parts.
// A bare part is otherwise taken as written — it need not be a regular
// identifier, since the caller quotes each part with QuoteName.
func SplitName(name string) ([]string, error) {
	fail := func(why string) ([]string, error) {
		return nil, invalidf("gosmo: %q is not a multi-part name: %s", name, why)
	}
	isSpace := func(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }
	var parts []string
	i := 0
	for {
		for i < len(name) && isSpace(name[i]) {
			i++
		}
		var part strings.Builder
		switch {
		case i < len(name) && (name[i] == '[' || name[i] == '"'):
			closer := name[i]
			if closer == '[' {
				closer = ']'
			}
			i++
			for {
				j := strings.IndexByte(name[i:], closer)
				if j < 0 {
					return fail("unterminated " + string(closer))
				}
				part.WriteString(name[i : i+j])
				i += j + 1
				if i < len(name) && name[i] == closer {
					part.WriteByte(closer)
					i++
					continue
				}
				break
			}
		default:
			start := i
			for i < len(name) && name[i] != '.' && !isSpace(name[i]) {
				if c := name[i]; c == '[' || c == ']' || c == '"' {
					return fail("a bracket or quote inside an unquoted part")
				}
				i++
			}
			part.WriteString(name[start:i])
		}
		parts = append(parts, part.String())
		for i < len(name) && isSpace(name[i]) {
			i++
		}
		if i == len(name) {
			break
		}
		if name[i] != '.' {
			return fail("text after a part that is not a '.'")
		}
		i++
	}
	switch {
	case len(parts) > 4:
		return fail("more than four parts")
	case parts[0] == "" || parts[len(parts)-1] == "":
		return fail("an empty first or last part")
	}
	return parts, nil
}

// IsReservedKeyword reports whether word, in any case, is one of SQL Server's
// reserved keywords — the words that cannot stand as an identifier without
// [brackets] or "double quotes". It is the list Microsoft documents
// ("Reserved keywords (Transact-SQL)"), probed on 13 and 17: the server
// refuses each one bare as a column alias except DISK, DUMP, LOAD, PRECISION
// and SECURITYAUDIT, which are kept because they are documented. ODBC and
// "future" keywords are not included: they are accepted bare. The two-word
// WITHIN GROUP is not a single word and is left out.
func IsReservedKeyword(word string) bool {
	_, ok := reservedKeywords[strings.ToUpper(word)]
	return ok
}

var reservedKeywords = func() map[string]struct{} {
	words := strings.Fields(`
ADD ALL ALTER AND ANY AS ASC AUTHORIZATION BACKUP BEGIN BETWEEN BREAK BROWSE
BULK BY CASCADE CASE CHECK CHECKPOINT CLOSE CLUSTERED COALESCE COLLATE COLUMN
COMMIT COMPUTE CONSTRAINT CONTAINS CONTAINSTABLE CONTINUE CONVERT CREATE CROSS
CURRENT CURRENT_DATE CURRENT_TIME CURRENT_TIMESTAMP CURRENT_USER CURSOR
DATABASE DBCC DEALLOCATE DECLARE DEFAULT DELETE DENY DESC DISK DISTINCT
DISTRIBUTED DOUBLE DROP DUMP ELSE END ERRLVL ESCAPE EXCEPT EXEC EXECUTE EXISTS
EXIT EXTERNAL FETCH FILE FILLFACTOR FOR FOREIGN FREETEXT FREETEXTTABLE FROM
FULL FUNCTION GOTO GRANT GROUP HAVING HOLDLOCK IDENTITY IDENTITY_INSERT
IDENTITYCOL IF IN INDEX INNER INSERT INTERSECT INTO IS JOIN KEY KILL LEFT LIKE
LINENO LOAD MERGE NATIONAL NOCHECK NONCLUSTERED NOT NULL NULLIF OF OFF OFFSETS
ON OPEN OPENDATASOURCE OPENQUERY OPENROWSET OPENXML OPTION OR ORDER OUTER OVER
PERCENT PIVOT PLAN PRECISION PRIMARY PRINT PROC PROCEDURE PUBLIC RAISERROR
READ READTEXT RECONFIGURE REFERENCES REPLICATION RESTORE RESTRICT RETURN
REVERT REVOKE RIGHT ROLLBACK ROWCOUNT ROWGUIDCOL RULE SAVE SCHEMA
SECURITYAUDIT SELECT SEMANTICKEYPHRASETABLE SEMANTICSIMILARITYDETAILSTABLE
SEMANTICSIMILARITYTABLE SESSION_USER SET SETUSER SHUTDOWN SOME STATISTICS
SYSTEM_USER TABLE TABLESAMPLE TEXTSIZE THEN TO TOP TRAN TRANSACTION TRIGGER
TRUNCATE TRY_CONVERT TSEQUAL UNION UNIQUE UNPIVOT UPDATE UPDATETEXT USE USER
VALUES VARYING VIEW WAITFOR WHEN WHERE WHILE WITH WRITETEXT`)
	m := make(map[string]struct{}, len(words))
	for _, w := range words {
		m[w] = struct{}{}
	}
	return m
}()

// QuoteLiteral renders s as a T-SQL Unicode string literal — N'…', doubling
// any embedded quote — safe to embed in SQL text where a parameter
// placeholder is not accepted (DDL, dynamic SQL). Prefer a query parameter
// for ordinary values.
//
// The N prefix is not optional. Without it the literal is varchar, converted
// through the current database's code page, and any character outside that
// code page becomes '?': a Cyrillic or CJK file path in CREATE DATABASE's
// FILENAME created a file somewhere else, or failed. Until 2026-09-23 this
// returned the driver's bare '…' and every caller that knew added the N by
// hand; the ones that did not were the bug (S9).
//
// Use QuoteLiteral where the whole literal is being produced. Where the
// quotes are already part of a format string — the common shape in this
// package, e.g. "@name = N'%s'" — use the unexported escapeSingle (helpers.go)
// instead, which escapes without adding quotes of its own.
//
// Neither one quotes an *identifier*. An identifier that ends up inside a
// string literal — the argument to OBJECT_ID, DBCC SHOW_STATISTICS,
// fn_listextendedproperty, and similar — needs QuoteName/qualifiedName
// applied first and escapeSingle on top of that:
//
//	escapeSingle(qualifiedName(schema, name))  // -> [dbo].[Sales.Archive]
//	escapeSingle(t.FullName())                 // same, for a *Table
//
// Skipping the bracket-quoting is not cosmetic. A name containing '.' then
// parses as a multi-part name and resolves to the wrong object or to NULL,
// and a NULL object_id means "every object in the database" to
// sys.dm_db_index_physical_stats — so the wrong form returns plausible stats
// for the wrong tables instead of failing. See
// identifier_quoting_test.go, which pins all of this.
func QuoteLiteral(s string) string {
	return "N" + mssql.TSQLQuoter{}.Value(s)
}

// QuoteAnsiLiteral renders s as a T-SQL non-Unicode string literal — '…',
// doubling any embedded quote, with no N prefix. It is for the few places
// that want varchar and not nvarchar: an Extended Events predicate on an
// ansi_string field, whose like_i_sql_ansi_string comparator takes a
// varchar. Everywhere else use QuoteLiteral; a character outside the current
// code page becomes '?' here, which is the reason QuoteLiteral has its N.
func QuoteAnsiLiteral(s string) string {
	return mssql.TSQLQuoter{}.Value(s)
}
