package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

type scriptCtxKey struct{}

// scriptServerKey carries the instance name WithScriptServer overrides.
type scriptServerKey struct{}

// ScriptEntry is one statement a ScriptCollector captured, with where it
// would have run.
type ScriptEntry struct {
	// Server is the instance the statement would have run on: the
	// connected server's @@SERVERNAME, or the name WithScriptServer gave.
	// It is "" for a Server whose info was never loaded.
	Server string
	// Database is the database a database-scoped write would have run in,
	// and "" for a server-scoped one — which runs in whatever database the
	// session is in.
	Database string
	// SQL is the statement alone, with any bound parameters substituted and
	// no USE in front of it: Database says where it goes.
	SQL string
}

// ScriptCollector accumulates the SQL statements a write method would have
// executed, instead of running them. See WithScript. Entries is guarded
// by mu since nothing stops a caller from reusing one collector/context
// across write calls issued from multiple goroutines concurrently.
//
// String renders the whole capture as one runnable script, which is what a
// caller handing it to a person wants. Joining the entries by hand is the
// mistake it exists to prevent: some statements must open their batch
// (CREATE SCHEMA, CREATE PROCEDURE, CREATE VIEW …), and two entries sharing a
// batch collide on anything batch-scoped, such as a second DECLARE of the
// same variable.
type ScriptCollector struct {
	mu      sync.Mutex
	Entries []ScriptEntry
}

// append adds e under mu — the only way exec/exec should touch
// Entries.
func (c *ScriptCollector) append(e ScriptEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Entries = append(c.Entries, e)
}

// snapshot copies Entries under mu.
func (c *ScriptCollector) snapshot() []ScriptEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.Entries)
}

// Statements returns each captured entry as a script runnable on its own: a
// database-scoped entry is preceded by "USE [db];" and a GO, so it opens its
// own batch; a server-scoped one is its SQL alone. Unlike String, nothing
// says which instance an entry belongs to.
func (c *ScriptCollector) Statements() []string {
	entries := c.snapshot()
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.SQL
		if e.Database != "" {
			out[i] = scriptUse(e.Database) + e.SQL
		}
	}
	return out
}

// String renders every captured entry as one script, each statement in a
// batch of its own (followed by GO), in capture order:
//
//   - a database-scoped entry is preceded by USE whenever the database
//     differs from the one the script is already in;
//   - a server-scoped entry that follows a database-scoped one is preceded by
//     USE [master], so it does not run inside that database — DROP DATABASE
//     of the database a session is using fails;
//   - when the entries span more than one instance, a "-- on <server>" line
//     opens each run of entries for the same instance, and each run starts
//     with no database assumed, since it is a different session.
//
// It returns "" when nothing was captured.
func (c *ScriptCollector) String() string {
	entries := c.snapshot()
	multi := slices.ContainsFunc(entries, func(e ScriptEntry) bool { return e.Server != entries[0].Server })
	var b strings.Builder
	cur := "" // the database the session is known to be in; "" = its default
	for i, e := range entries {
		if i > 0 {
			b.WriteString("\n")
		}
		if multi && (i == 0 || e.Server != entries[i-1].Server) {
			name := e.Server
			if name == "" {
				name = "(unknown instance)"
			}
			fmt.Fprintf(&b, "-- on %s\n", name)
			cur = ""
		}
		switch {
		case e.Database != "" && e.Database != cur:
			b.WriteString(scriptUse(e.Database))
			cur = e.Database
		case e.Database == "" && cur != "":
			b.WriteString(scriptUse("master"))
			cur = ""
		}
		b.WriteString(e.SQL)
		b.WriteString("\nGO\n")
	}
	return b.String()
}

// scriptUse is the batch that switches a script into db. The GO is what lets
// the statement after it open a batch — CREATE SCHEMA and CREATE PROCEDURE
// refuse to run after a USE in the same one (Msg 111).
func scriptUse(db string) string {
	return "USE " + quoteIdent(db) + ";\nGO\n"
}

// WithScript returns a derived context carrying a *ScriptCollector. Every
// gosmo write method invoked with the returned context appends its
// statement to the collector and returns as if it had succeeded, without
// touching the server — callers use this to preview or hand off the exact
// SQL a set of pending edits would run (e.g. an "Script Changes" action
// that opens the statements in a query editor instead of executing them).
//
// Read methods are unaffected: only the exec chokepoints
// (Server.exec, Database.exec) consult the collector.
func WithScript(ctx context.Context) (context.Context, *ScriptCollector) {
	c := &ScriptCollector{}
	return context.WithValue(ctx, scriptCtxKey{}, c), c
}

// WithScriptServer returns a derived context under which captured entries
// record server as their ScriptEntry.Server, in place of the name of the
// Server the write was issued through. Outside WithScript it has no effect.
//
// It is for a script that spans instances the caller has not connected to:
// scripting the JOIN an availability group secondary must run needs no
// connection to the secondary, so the write is issued through a handle on
// the instance that is connected, and without this the entry would claim to
// belong there.
func WithScriptServer(ctx context.Context, server string) context.Context {
	return context.WithValue(ctx, scriptServerKey{}, server)
}

// scriptServerName is the ScriptEntry.Server for a write issued through s.
func scriptServerName(ctx context.Context, s *Server) string {
	if name, ok := ctx.Value(scriptServerKey{}).(string); ok {
		return name
	}
	if s == nil || s.info == nil {
		return ""
	}
	return s.info.Name
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

// setPtrIfApplied is setIfApplied for the batched *Changes structs, where a
// nil field means "not part of this change" and so must not be mirrored
// back onto the receiver at all.
func setPtrIfApplied[T any](ctx context.Context, dst *T, v *T) {
	if v != nil {
		setIfApplied(ctx, dst, *v)
	}
}

// observerCtxKey carries the func(ScriptEntry) WithStatementObserver installs.
// A nil value is how unobserved hides one from a bracket's own statements.
type observerCtxKey struct{}

// WithStatementObserver returns a derived context under which fn is called
// once for every statement a write method invoked with it executed
// successfully, after the server accepted it. It is the executing-side twin
// of WithScript, hooked at the same chokepoints: a caller running several
// writes in a row learns which of them reached the server before one failed,
// which the error alone cannot say. An audit trail is the other obvious use.
//
// fn never fires under WithScript, where nothing is executed, nor for a
// statement that failed — a statement that failed part-way through a
// non-atomic batch is reported by its error, not here. It is called on the
// goroutine that issued the write, so a caller writing from several at once
// synchronises fn itself. An observer already on ctx keeps firing, before fn.
//
// The entry's SQL is the statement as sent, with bound parameters substituted
// as WithScript would render them (the unbound text if they cannot be).
//
// The two halves of a window a write opens and closes around itself — an
// audit disabled for an ALTER and re-enabled after it, a database put back to
// MULTI_USER after a failed exclusive operation — are not reported: they
// leave the server as it was, and reporting them would make a write that
// changed nothing look as if it had. A window that fails to close is the
// write's error to report.
func WithStatementObserver(ctx context.Context, fn func(ScriptEntry)) context.Context {
	if parent := observerFrom(ctx); parent != nil {
		own := fn
		fn = func(e ScriptEntry) {
			parent(e)
			own(e)
		}
	}
	return context.WithValue(ctx, observerCtxKey{}, fn)
}

func observerFrom(ctx context.Context) func(ScriptEntry) {
	fn, _ := ctx.Value(observerCtxKey{}).(func(ScriptEntry))
	return fn
}

// unobserved is ctx with any statement observer hidden, for the statements
// that open and close a window — see WithStatementObserver. The writes a
// window wraps must still get the observed context, not this one.
func unobserved(ctx context.Context) context.Context {
	if observerFrom(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, observerCtxKey{}, (func(ScriptEntry))(nil))
}

// observe reports e to ctx's statement observer, if there is one.
func observe(ctx context.Context, e ScriptEntry) {
	if fn := observerFrom(ctx); fn != nil {
		fn(e)
	}
}

func scriptFrom(ctx context.Context) (*ScriptCollector, bool) {
	c, ok := ctx.Value(scriptCtxKey{}).(*ScriptCollector)
	return c, ok
}

// exec is the chokepoint every server-scoped write method (and, via
// Database.exec, every database-scoped one) funnels through. stmt must
// already be a complete, self-contained statement — every write method in
// this package builds one via QuoteName/QuoteLiteral/escapeSingle before
// reaching here, since none of these are parameterizable DDL/EXEC calls.
func (s *Server) exec(ctx context.Context, stmt string) error {
	if c, ok := scriptFrom(ctx); ok {
		c.append(ScriptEntry{Server: scriptServerName(ctx, s), SQL: stmt})
		return nil
	}
	if _, err := s.db.ExecContext(ctx, stmt); err != nil {
		return withAllMessages(err)
	}
	observe(ctx, ScriptEntry{Server: scriptServerName(ctx, s), SQL: stmt})
	return nil
}

// atomicBatch renders stmts as one batch that applies all of them or none.
// It is for a multi-statement write with no single procedure behind it, whose
// half-done state is one nobody asked for. Job step reordering is the case it
// was written for: msdb cannot renumber a step in place, so a move is a
// delete followed by an insert, and a failure between the two loses the step
// outright — the delete has already committed and the step's definition only
// ever existed in gosmo's memory.
//
// Three choices here are load-bearing, each of which a plausible
// simplification removes:
//
// TRY/CATCH rather than checked return codes. msdb's job procedures raise
// before they return non-zero, so the CATCH sees the failure either way — and
// the checked form needs a batch-scoped DECLARE, which collides with itself
// as soon as a ScriptCollector concatenates two of these into one batch. That
// is the same collision bindScriptArgs describes for a second DECLARE @p1.
//
// SET XACT_ABORT ON is what rolls the transaction back when the *client* goes
// away rather than the server failing. A cancelled context sends an attention,
// which aborts the running statement and, with XACT_ABORT off, leaves the
// transaction open — and a write here runs on a deadline (see
// objectWriteTimeout on the gossms side), so that is not a remote case. It
// does not leak onto the next user of the pooled connection: go-mssqldb sets
// the TDS reset-connection bit on the first packet after database/sql hands
// the connection back out, which restores every SET option to its login
// default and rolls back anything still open.
//
// THROW, not a RAISERROR of our own, so the caller is given msdb's message
// about what actually went wrong instead of a generic one.
func atomicBatch(stmts []string) string {
	var b strings.Builder
	b.WriteString("SET XACT_ABORT ON;\nBEGIN TRY\nBEGIN TRANSACTION;\n")
	for _, stmt := range stmts {
		b.WriteString(stmt)
		b.WriteString(";\n")
	}
	b.WriteString("COMMIT TRANSACTION;\nEND TRY\nBEGIN CATCH\n" +
		"IF @@TRANCOUNT > 0 ROLLBACK TRANSACTION;\nTHROW;\nEND CATCH;")
	return b.String()
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
// a real bug: Index.Rename, Table.Rename and
// Table.Drop(cascade=true) are the parameterised write methods, and
// all three are reachable from a Script Changes button.
//
// Declaring the parameters in a preamble instead would collide, not compose:
// a collector's statements are concatenated into one batch, and a second
// DECLARE @p1 in the same batch is an error.
//
// Substitution skips string literals, quoted identifiers and comments (see
// scriptCodeSpans). Textual replacement over the whole statement was correct
// for every statement this package binds today, and silently wrong for the
// first one to both interpolate a name and bind a parameter: an object
// actually named "@p1" reaches the statement inside an escapeSingle'd literal,
// and rewriting it there changes which object the DDL names — in a script the
// user is about to run by hand, which is the failure scriptLiteral refuses to
// risk for a literal it cannot render.
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
	bound := make([]bool, len(args))
	var out strings.Builder
	spans := scriptCodeSpans(q)
	for _, span := range spans {
		out.WriteString(q[span.prev:span.start])
		out.WriteString(placeholderPat.ReplaceAllStringFunc(q[span.start:span.end], func(m string) string {
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
			bound[n-1] = true
			return lit
		}))
	}
	if bindErr != nil {
		return "", bindErr
	}
	// A placeholder that occurs only inside skipped text is the one case the
	// skipping cannot decide: either the statement really does mention @pN in
	// a literal, or the scan mis-parsed and swallowed a live placeholder. The
	// second leaves "@p1" in a script that then fails by hand with "Must
	// declare the scalar variable" — the original bug — so it is an error here
	// rather than a statement that looks fine until it is run.
	for _, span := range spans {
		for _, m := range placeholderPat.FindAllString(q[span.prev:span.start], -1) {
			n, err := strconv.Atoi(m[2:])
			if err != nil || n < 1 || n > len(args) || bound[n-1] {
				continue
			}
			return "", fmt.Errorf("gosmo: script: %s appears only inside a literal or comment", m)
		}
	}
	return out.String(), nil
}

// codeSpan is one run of q that substitution applies to, together with the
// span of skipped text immediately before it.
type codeSpan struct{ prev, start, end int }

// scriptCodeSpans splits q into the runs where an @pN is a parameter
// placeholder, skipping the four places it is text the server never
// interprets: a 'string literal', a "quoted identifier", a [quoted
// identifier], a -- line comment and a /* block comment */.
//
// A doubled quote is how each of the quoted forms escapes its own delimiter,
// and it needs no special case: the first of the pair closes the span and the
// second opens a new one of the same kind, so the same bytes are skipped
// either way. T-SQL block comments nest, so those are counted, not matched.
func scriptCodeSpans(q string) []codeSpan {
	var spans []codeSpan
	prev, start := 0, 0
	skip := func(from, to int) {
		spans = append(spans, codeSpan{prev: prev, start: start, end: from})
		prev, start = from, to
	}
	for i := 0; i < len(q); {
		switch {
		case q[i] == '\'' || q[i] == '"' || q[i] == '[':
			closer := q[i]
			if closer == '[' {
				closer = ']'
			}
			j := i + 1
			for j < len(q) && q[j] != closer {
				j++
			}
			if j < len(q) {
				j++
			}
			skip(i, j)
			i = j
		case strings.HasPrefix(q[i:], "--"):
			j := strings.IndexByte(q[i:], '\n')
			if j < 0 {
				j = len(q)
			} else {
				j += i
			}
			skip(i, j)
			i = j
		case strings.HasPrefix(q[i:], "/*"):
			depth, j := 1, i+2
			for j < len(q) && depth > 0 {
				switch {
				case strings.HasPrefix(q[j:], "/*"):
					depth, j = depth+1, j+2
				case strings.HasPrefix(q[j:], "*/"):
					depth, j = depth-1, j+2
				default:
					j++
				}
			}
			skip(i, j)
			i = j
		default:
			i++
		}
	}
	return append(spans, codeSpan{prev: prev, start: start, end: len(q)})
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
		return QuoteLiteral(x), nil
	case []byte:
		// go-mssqldb sends a nil slice as NULL and an empty non-nil one as
		// the zero-length value, so the two script differently.
		if x == nil {
			return "NULL", nil
		}
		return binaryLiteral(x), nil
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
	reflect.TypeFor[time.Time]():                  "DATETIME2",
	reflect.TypeFor[sql.NullTime]():               "DATETIME2",
	reflect.TypeFor[sql.NullBool]():               "BIT",
	reflect.TypeFor[sql.NullByte]():               "TINYINT",
	reflect.TypeFor[sql.NullInt16]():              "SMALLINT",
	reflect.TypeFor[sql.NullInt32]():              "INT",
	reflect.TypeFor[sql.NullInt64]():              "BIGINT",
	reflect.TypeFor[sql.NullFloat64]():            "FLOAT",
	reflect.TypeFor[sql.NullString]():             "NVARCHAR(MAX)",
	reflect.TypeFor[mssql.UniqueIdentifier]():     "UNIQUEIDENTIFIER",
	reflect.TypeFor[mssql.NullUniqueIdentifier](): "UNIQUEIDENTIFIER",
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
		if t == reflect.TypeFor[time.Time]() {
			return "DATETIME2"
		}
	}
	return "SQL_VARIANT"
}
