package gosmo

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// rowSource is what a list read iterates: *sql.Rows from Server.query, or the
// *dbRows from Database.query that also releases its pinned connection.
type rowSource interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// scanRows drains a list read into a slice. rows and err are the query's two
// results, passed straight through; scan reads one row, given the Scan to
// call. rows is closed, and a failure of the query, of any scan or of
// rows.Err is wrapped "gosmo: <what>: %w" — the same message for all three,
// which is the rule CLAUDE.md § Conventions states and this makes true by
// construction. An empty what returns the error bare, for the shared readers
// whose callers name the operation. An empty result is a nil slice.
//
// scan runs inside the rows.Next() loop, so it must not query: the
// connection the read pinned is still held.
func scanRows[T any, R rowSource](rows R, err error, what string, scan func(scan func(...any) error) (T, error)) ([]T, error) {
	wrap := func(err error) error {
		if what == "" {
			return err
		}
		return fmt.Errorf("gosmo: %s: %w", what, err)
	}
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows.Scan)
		if err != nil {
			return nil, wrap(err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap(err)
	}
	return out, nil
}

// foundRow is the tail of a single-row read by name: sql.ErrNoRows becomes
// notFound (built with notFoundf, so errors.Is ErrNotFound), any other error
// is wrapped "gosmo: <what>: %w", and otherwise v is the result.
func foundRow[T any](v T, err error, notFound error, what string) (T, error) {
	var zero T
	if errors.Is(err, sql.ErrNoRows) {
		return zero, notFound
	}
	if err != nil {
		return zero, fmt.Errorf("gosmo: %s: %w", what, err)
	}
	return v, nil
}

// createdObject is the tail of every Create*: it returns what was created.
// Under Scripting(ctx) the statement was only collected, so there is nothing
// to read and handle — the family's name-only Ref form — is the answer.
// Otherwise read fetches the object back from the catalog.
//
// A read-back that finds nothing returns handle too, not the not-found error.
// The create succeeded; what failed is visibility — SQL Server hides a
// catalog row from a principal with no permission on it, and the one that
// created an object is not always one that can see it afterwards. Reporting
// that as an error would tell the caller a create that happened had failed.
// Any other read error is returned as is.
func createdObject[T any](ctx context.Context, handle T, read func() (T, error)) (T, error) {
	if Scripting(ctx) {
		return handle, nil
	}
	v, err := read()
	if errors.Is(err, ErrNotFound) {
		return handle, nil
	}
	return v, err
}

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

// binaryLiteral renders b as a T-SQL 0x… binary literal in uppercase hex.
// Empty (or nil) b renders as 0x, the empty binary string — DATALENGTH(0x)
// is 0, where 0x00 would be one zero byte. A caller for which nil means NULL
// checks for it first.
func binaryLiteral(b []byte) string {
	return "0x" + strings.ToUpper(hex.EncodeToString(b))
}

// Ptr returns a pointer to v. It exists for the batched *Changes structs
// (JobChanges, AlertChanges, OperatorChanges, ScheduleChanges), whose fields
// are pointers so that nil can mean "leave this property alone" — a
// distinction 0, "" and false cannot make, since msdb accepts all three as
// real values. Go has no way to take the address of a literal inside a
// struct literal, so without this every call site needs a named variable per
// field.
func Ptr[T any](v T) *T { return &v }

// boolToInt converts a bool to 0/1 for T-SQL BIT parameters.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// requireSchema is the one rule for a caller-supplied schema: an empty one is
// refused, by every call that takes a schema-scoped name. It used to mean two
// things — dbo to DropSequence and ~20 like it, the caller's own default
// schema to DropTable, which passed it to qualifiedName unqualified — so a
// login whose default schema was sales got sales.t from DropTable("", "t")
// and dbo.s from DropSequence("", "s"). Defaulting either way addresses the
// wrong object for some caller; refusing cannot.
//
// what is the operation, for the message ("drop view"); name the object.
func requireSchema(what, schema, name string) error {
	if schema == "" {
		return fmt.Errorf("gosmo: %s %q: %w", what, name, ErrSchemaRequired)
	}
	return nil
}

// qualifiedName returns [schema].[name], or just [name] when schema is empty.
// Most callers get schema from an exported method's parameter, and the naive
// form emits "[].[name]" for an empty one — which OBJECT_ID resolves to NULL,
// so the caller gets an empty result set rather than an error.
func qualifiedName(schema, name string) string {
	if schema == "" {
		return quoteIdent(name)
	}
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
func likeEscape(s string) string { return likeEscaper.Replace(s) }

// likeEscaper is built once: a strings.Replacer is safe for concurrent use and
// compiles its lookup on first use, which a per-call one paid every time.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`, `[`, `\[`)
