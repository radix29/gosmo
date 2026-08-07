package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"sync"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

type scriptCtxKey struct{}

// ScriptCollector accumulates the SQL statements a write method would have
// executed, instead of running them. See WithScript. Statements is guarded
// by mu since nothing stops a caller from reusing one collector/context
// across write calls issued from multiple goroutines concurrently.
type ScriptCollector struct {
	mu         sync.Mutex
	Statements []string
}

// append adds stmt under mu — the only way execContext/exec should touch
// Statements.
func (c *ScriptCollector) append(stmt string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Statements = append(c.Statements, stmt)
}

// WithScript returns a derived context carrying a *ScriptCollector. Every
// gosmo write method invoked with the returned context appends its
// statement to the collector and returns as if it had succeeded, without
// touching the server — callers use this to preview or hand off the exact
// SQL a set of pending edits would run (e.g. an "Script Changes" action
// that opens the statements in a query editor instead of executing them).
//
// Read methods are unaffected: only the exec chokepoints
// (Server.execContext, Database.exec) consult the collector.
func WithScript(ctx context.Context) (context.Context, *ScriptCollector) {
	c := &ScriptCollector{}
	return context.WithValue(ctx, scriptCtxKey{}, c), c
}

// Scripting reports whether ctx came from WithScript — that is, whether
// write methods invoked with it record their statement instead of running
// it.
//
// A caller that mirrors a write into its own state needs to know: under
// WithScript a write returns success without the server ever seeing it, so
// state derived from "it worked" is wrong. The case this exists for is a
// rename — an editor that renames an object and then re-reads it by the new
// name finds nothing, because the old name is still what the server has.
// Reads are unaffected by WithScript and go to the real server either way.
func Scripting(ctx context.Context) bool {
	_, ok := scriptFrom(ctx)
	return ok
}

// setIfApplied assigns v to *dst unless ctx is a WithScript context, where
// the write that just "succeeded" was only recorded and the server still
// holds the old value.
//
// Write methods that mirror their change back onto the receiver — a rename
// updating .Name, an enable updating .Enabled — must go through this rather
// than assigning directly. Otherwise a scripted write leaves the object
// claiming state the server does not have, and the next call built from that
// object (a second rename, a delete by name) targets an object that doesn't
// exist. Scripting(ctx) documents the same hazard for callers keeping their
// own mirrored state; this is gosmo honouring it for its own.
func setIfApplied[T any](ctx context.Context, dst *T, v T) {
	if !Scripting(ctx) {
		*dst = v
	}
}

func scriptFrom(ctx context.Context) (*ScriptCollector, bool) {
	c, ok := ctx.Value(scriptCtxKey{}).(*ScriptCollector)
	return c, ok
}

// execContext is the chokepoint every server-scoped write method (and, via
// Database.exec, every database-scoped one) funnels through. stmt must
// already be a complete, self-contained statement — every write method in
// this package builds one via QuoteName/QuoteLiteral/escapeSingle before
// reaching here, since none of these are parameterizable DDL/EXEC calls.
func (s *Server) execContext(ctx context.Context, stmt string) error {
	if c, ok := scriptFrom(ctx); ok {
		c.append(stmt)
		return nil
	}
	_, err := s.db.ExecContext(ctx, stmt)
	return err
}

// placeholderPat matches the driver's positional parameter placeholders
// (@p1, @p2, ...). The digits are matched greedily so "@p10" is one
// placeholder rather than "@p1" followed by a stray 0.
var placeholderPat = regexp.MustCompile(`@p([0-9]+)`)

// bindScriptArgs substitutes the literal form of each arg for its @pN
// placeholder in q, so a statement captured under WithScript is runnable on
// its own.
//
// A captured statement is handed to a query editor and run by hand, where
// nothing binds parameters — so recording the placeholder text alone produced
// a script that fails with "Must declare the scalar variable '@p1'". That was
// a real bug: Index.Rename, Database.RenameTable and
// Database.DropTable(cascade=true) are the parameterised write methods, and
// all three are reachable from a Script Changes button.
//
// Declaring the parameters in a preamble instead would collide, not compose:
// a collector's statements are concatenated into one batch, and a second
// DECLARE @p1 in the same batch is an error.
//
// q is always one of this package's own statement constants, never caller
// text, so a "@pN" appearing inside a string literal isn't a case that arises
// here — substitution is textual and does not attempt to parse around one.
func bindScriptArgs(q string, args []any) (string, error) {
	if len(args) == 0 {
		return q, nil
	}
	// Checked up front rather than from inside the substitution below: a
	// named argument's placeholder is "@name", which placeholderPat does not
	// match, so a statement parameterised purely by name would otherwise
	// script with every parameter left unbound and no error to say so. No
	// method in this package binds one today (ExecProc renders its own EXEC
	// form — see scriptExecProc), which is exactly why the guard has to be
	// here and not in scriptLiteral's type switch.
	for _, a := range args {
		if na, ok := a.(sql.NamedArg); ok {
			return "", fmt.Errorf("gosmo: script: named argument %q cannot be scripted positionally", na.Name)
		}
	}
	var bindErr error
	out := placeholderPat.ReplaceAllStringFunc(q, func(m string) string {
		n, err := strconv.Atoi(m[2:])
		if err != nil || n < 1 || n > len(args) {
			if bindErr == nil {
				bindErr = fmt.Errorf("gosmo: script: %s has no argument (%d given)", m, len(args))
			}
			return m
		}
		lit, err := scriptLiteral(args[n-1])
		if err != nil {
			if bindErr == nil {
				bindErr = fmt.Errorf("gosmo: script: %s: %w", m, err)
			}
			return m
		}
		return lit
	})
	if bindErr != nil {
		return "", bindErr
	}
	return out, nil
}

// scriptLiteral renders one bound argument as the T-SQL literal that would
// have been sent for it. Only the types this package actually binds are
// supported; anything else is an error rather than a %v guess, since a
// silently wrong literal in a script the user is about to run by hand is
// worse than a refusal to produce one.
//
// Its errors carry no "gosmo:" prefix of their own — both call sites
// (bindScriptArgs, scriptExecProc) wrap them with one plus the parameter
// they belong to, and prefixing here would put it in the middle.
func scriptLiteral(v any) (string, error) {
	if v == nil {
		return "NULL", nil
	}
	switch x := v.(type) {
	case string:
		return nStringLiteral(x), nil
	case []byte:
		// A bare "0x" is not a valid T-SQL binary literal, so an empty slice
		// scripts as the zero-length one, 0x00.
		if len(x) == 0 {
			return "0x00", nil
		}
		return fmt.Sprintf("0x%X", x), nil
	case bool:
		return strconv.Itoa(boolToInt(x)), nil
	case int:
		return strconv.Itoa(x), nil
	case int8:
		return strconv.FormatInt(int64(x), 10), nil
	case int16:
		return strconv.FormatInt(int64(x), 10), nil
	case int32:
		return strconv.FormatInt(int64(x), 10), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case uint:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint8:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint16:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint32:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint64:
		return strconv.FormatUint(x, 10), nil
	case float32:
		return strconv.FormatFloat(float64(x), 'g', -1, 32), nil
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), nil
	case time.Time:
		// The ISO 8601 form SQL Server parses unambiguously regardless of the
		// session's DATEFORMAT/language.
		return "'" + x.Format("2006-01-02T15:04:05.9999999") + "'", nil
	case driver.Valuer:
		dv, err := x.Value()
		if err != nil {
			return "", err
		}
		return scriptLiteral(dv)
	case sql.NamedArg:
		return "", fmt.Errorf("named argument %q cannot be scripted positionally", x.Name)
	}
	return "", fmt.Errorf("cannot script a %T argument", v)
}

// dereference reads through a pointer to the value it points at, so an
// output/in-out parameter's current value can be rendered as a literal. A nil
// pointer (or a non-pointer) reads as nil, which scripts as NULL.
func dereference(p any) any {
	rv := reflect.ValueOf(p)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return nil
	}
	return rv.Elem().Interface()
}

// declTypeByName maps the named destination types whose T-SQL type their Go
// kind cannot give away. Checked ahead of scriptDeclType's kind switch,
// which would otherwise send every one of them to SQL_VARIANT: the sql.Null*
// family and NullUniqueIdentifier are structs, and UniqueIdentifier is a
// [16]byte array. All of them are ordinary things to point an OUTPUT
// parameter at — sql.Null* is *the* way to receive a nullable one — and all
// of them were scripted as SQL_VARIANT, which SQL Server then refused:
// "Implicit conversion from data type sql_variant to int is not allowed."
// Verified live 2026-08-06 against every entry here.
var declTypeByName = map[reflect.Type]string{
	reflect.TypeOf(time.Time{}):                  "DATETIME2",
	reflect.TypeOf(sql.NullTime{}):               "DATETIME2",
	reflect.TypeOf(sql.NullBool{}):               "BIT",
	reflect.TypeOf(sql.NullByte{}):               "TINYINT",
	reflect.TypeOf(sql.NullInt16{}):              "SMALLINT",
	reflect.TypeOf(sql.NullInt32{}):              "INT",
	reflect.TypeOf(sql.NullInt64{}):              "BIGINT",
	reflect.TypeOf(sql.NullFloat64{}):            "FLOAT",
	reflect.TypeOf(sql.NullString{}):             "NVARCHAR(MAX)",
	reflect.TypeOf(mssql.UniqueIdentifier{}):     "UNIQUEIDENTIFIER",
	reflect.TypeOf(mssql.NullUniqueIdentifier{}): "UNIQUEIDENTIFIER",
}

// scriptDeclType names the T-SQL type to DECLARE for an output parameter
// whose value will be written back into dest. The declared type has to be
// assignment-compatible with the procedure's own parameter, so it is derived
// from what the caller intends to read the value into rather than defaulting
// to SQL_VARIANT, which SQL Server will not accept for most typed OUTPUT
// parameters.
func scriptDeclType(dest any) string {
	rv := reflect.ValueOf(dest)
	if rv.Kind() != reflect.Pointer {
		return "SQL_VARIANT"
	}
	t := rv.Type().Elem()
	if decl, ok := declTypeByName[t]; ok {
		return decl
	}
	switch t.Kind() {
	case reflect.Bool:
		return "BIT"
	case reflect.Int8, reflect.Uint8:
		return "TINYINT"
	case reflect.Int16, reflect.Uint16:
		return "SMALLINT"
	case reflect.Int32:
		return "INT"
	case reflect.Int, reflect.Int64, reflect.Uint, reflect.Uint32, reflect.Uint64:
		return "BIGINT"
	case reflect.Float32, reflect.Float64:
		return "FLOAT"
	case reflect.String:
		return "NVARCHAR(MAX)"
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return "VARBINARY(MAX)"
		}
	case reflect.Struct:
		// This arm is the kind-based fallback for a struct type that
		// declTypeByName has no entry for. Its time.Time test cannot fire
		// today — the map above is consulted first and answers for time.Time
		// and every sql.Null* — and is kept as the fallback's own statement
		// of what it would do, not as a live branch. Anything else struct-
		// shaped falls through to SQL_VARIANT below.
		if t == reflect.TypeOf(time.Time{}) {
			return "DATETIME2"
		}
	}
	return "SQL_VARIANT"
}
