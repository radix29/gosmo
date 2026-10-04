package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// -- capture driver: records every statement it is handed, so a test can
// assert on the SQL gosmo generates without a real server ------------------

type captureDriver struct{}

func (captureDriver) Open(name string) (driver.Conn, error) { return &captureConn{}, nil }

type captureConn struct{}

func (c *captureConn) Prepare(query string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *captureConn) Close() error                              { return nil }
func (c *captureConn) Begin() (driver.Tx, error)                 { return nil, driver.ErrSkip }

func (c *captureConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	captured.add(query, args)
	return driver.ResultNoRows, nil
}

func (c *captureConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	captured.add(query, args)
	return captured.replyFor(query), nil
}

// captureRows yields the canned row registered for the statement, or no rows
// at all. Zero rows is the useful default: a queryRow then lands on
// sql.ErrNoRows and a query iterates zero times, which is enough for a test
// that only cares about the statement text. A canned row is needed only where
// gosmo would otherwise bail out before reaching the SQL under test.
type captureRows struct {
	cols []string
	rows [][]driver.Value
	next int
}

func (r *captureRows) Columns() []string {
	if r.cols == nil {
		return []string{"c"}
	}
	return r.cols
}
func (r *captureRows) Close() error { return nil }
func (r *captureRows) Next(dest []driver.Value) error {
	if r.next >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.next])
	r.next++
	return nil
}

// cannedRow is a reply the capture driver hands back for any statement
// containing match. Set row for a single reply row, rows for several — a
// test that cares about ordering or grouping across rows needs the latter.
type cannedRow struct {
	match string
	cols  []string
	row   []driver.Value
	rows  [][]driver.Value
}

func (c cannedRow) reply() *captureRows {
	rows := c.rows
	if rows == nil && c.row != nil {
		rows = [][]driver.Value{c.row}
	}
	return &captureRows{cols: c.cols, rows: rows}
}

// tableMetadataRow satisfies Database.TableByName's sys.tables lookup,
// so a Scripter test gets past it to the SQL it actually wants to inspect.
// The schema/name it reports are what the scripter then scripts.
func tableMetadataRow(schema, name string) cannedRow {
	return cannedRow{
		match: "FROM   sys.tables t",
		cols: []string{"object_id", "schema", "name", "create_date", "modify_date", "repl", "memopt",
			"ms_shipped", "filetable", "external", "node", "edge"},
		row: []driver.Value{int64(1), schema, name, time.Time{}, time.Time{}, false, false,
			false, false, false, false, false},
	}
}

type captureLog struct {
	mu     sync.Mutex
	qs     []string
	args   [][]any // args[i] is what qs[i] was sent with
	canned []cannedRow
}

func (l *captureLog) add(q string, args []driver.NamedValue) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.qs = append(l.qs, q)
	vals := make([]any, len(args))
	for i, a := range args {
		vals[i] = a.Value
	}
	l.args = append(l.args, vals)
}

// replyFor returns the canned reply for q, or an empty result set.
func (l *captureLog) replyFor(q string) *captureRows {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.canned {
		if strings.Contains(q, c.match) {
			return c.reply()
		}
	}
	return &captureRows{}
}

func (l *captureLog) reset(canned ...cannedRow) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.qs, l.args = nil, nil
	l.canned = canned
}

// find returns the first captured statement containing needle.
func (l *captureLog) find(needle string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, q := range l.qs {
		if strings.Contains(q, needle) {
			return q
		}
	}
	return ""
}

// findArgs returns the first captured statement containing needle and the
// arguments it was sent with.
func (l *captureLog) findArgs(needle string) (string, []any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, q := range l.qs {
		if strings.Contains(q, needle) {
			return q, l.args[i]
		}
	}
	return "", nil
}

// count returns how many captured statements contain needle.
func (l *captureLog) count(needle string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, q := range l.qs {
		if strings.Contains(q, needle) {
			n++
		}
	}
	return n
}

var captured captureLog

func init() { sql.Register("capture", captureDriver{}) }

// captureTable returns a Table wired to the capture driver, named so that
// both its schema and its name contain a '.' — the case that distinguishes a
// bracket-quoted qualified name from a raw one. It stands for a table read
// from the catalog, so it carries an ObjectID: a TableRef's reads are refused
// (ErrHandleNotLoaded) before they reach the driver.
func captureTable(t *testing.T) *Table {
	t.Helper()
	db, err := sql.Open("capture", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	captured.reset()
	srv := &Server{db: db}
	return &Table{db: &Database{server: srv, Name: "testdb"}, ObjectID: 1, Schema: "my.schema", Name: "Sales.Archive"}
}

// A qualified name handed to OBJECT_ID must be bracket-quoted:
// [dbo].[Sales.Archive], never dbo.Sales.Archive. Unbracketed, SQL Server
// reads a name containing '.' as a multi-part name and resolves it to the
// wrong object or to NULL — and a NULL object_id means "every object in the
// database" to sys.dm_db_index_physical_stats, so the unbracketed form
// returns plausible stats for the wrong tables rather than failing. Verified
// against SQL Server 17.0.4055.5: the Table.FragmentationStats shape below
// silently returned every index in the database, the Index.Fragmentation
// shape errored with 2561 ("Parameter 3 is incorrect for this statement"),
// and the scripter's OBJECT_ID existence guards inverted.
//
// The fragmentation and storage reads bind the name (H7, review plan
// 2026-10-04): one plan for every table, not one per name. Statistic.Columns
// still builds it into a literal, which then needs escapeSingle as well.
func TestFragmentationQueriesBracketQuoteTheObjectName(t *testing.T) {
	const (
		wantArg  = `[my.schema].[Sales.Archive]`
		wantName = `OBJECT_ID(N'[my.schema].[Sales.Archive]')`
		badName  = `OBJECT_ID(N'my.schema.Sales.Archive')`
	)

	// bound checks that the statement containing needle passes the table to
	// OBJECT_ID as parameter n, bracket-quoted, and has it nowhere in its text.
	bound := func(t *testing.T, needle string, n int) {
		t.Helper()
		q, args := captured.findArgs(needle)
		if q == "" {
			t.Fatalf("no %s statement was generated", needle)
		}
		if want := fmt.Sprintf("OBJECT_ID(@p%d)", n); !strings.Contains(q, want) {
			t.Errorf("generated SQL does not contain %s:\n%s", want, q)
		}
		if strings.Contains(q, "Sales.Archive") {
			t.Errorf("generated SQL interpolates the table name:\n%s", q)
		}
		if len(args) < n || args[n-1] != wantArg {
			t.Errorf("args = %q, want @p%d = %q", args, n, wantArg)
		}
	}

	t.Run("Index.Fragmentation", func(t *testing.T) {
		tbl := captureTable(t)
		// The capture driver returns no rows, so this errors; the statement it
		// generated on the way is what's under test.
		_, _ = tbl.IndexRef("IX_pad").Fragmentation(context.Background(), FragmentationSampled)
		bound(t, "dm_db_index_physical_stats", 2)
	})

	t.Run("Index.StorageInfo", func(t *testing.T) {
		tbl := captureTable(t)
		// The header read finds no row and stops there; it is the one the
		// other two share their target with.
		_, _ = tbl.IndexRef("IX_pad").StorageInfo(context.Background())
		bound(t, "sys.data_spaces", 2)
	})

	// Not a fragmentation query, but the same name: Columns finds its
	// statistic by name so that it works from a StatisticRef on a TableRef.
	t.Run("Statistic.Columns", func(t *testing.T) {
		tbl := captureTable(t)
		_, _ = tbl.StatisticRef("st_pad").Columns(context.Background())

		q := captured.find("stats_columns")
		if q == "" {
			t.Fatal("no stats_columns statement was generated")
		}
		if strings.Contains(q, badName) {
			t.Errorf("generated SQL uses the unbracketed name %s:\n%s", badName, q)
		}
		if !strings.Contains(q, wantName) {
			t.Errorf("generated SQL does not contain %s:\n%s", wantName, q)
		}
	})

	t.Run("Table.FragmentationStats", func(t *testing.T) {
		tbl := captureTable(t)
		_, _ = tbl.FragmentationStats(context.Background(), "LIMITED")
		bound(t, "dm_db_index_physical_stats", 1)
	})
}

// A name containing a single quote is bound as it is: escaping belongs to a
// literal, and a bound name has none — an escaped one would name another table.
func TestFragmentationQueryBindsQuoteInObjectNameUnescaped(t *testing.T) {
	db, err := sql.Open("capture", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	captured.reset()

	tbl := &Table{db: &Database{server: &Server{db: db}, Name: "testdb"}, Schema: "dbo", Name: "O'Brien.Log"}
	_, _ = tbl.FragmentationStats(context.Background(), "LIMITED")

	q, args := captured.findArgs("dm_db_index_physical_stats")
	if q == "" {
		t.Fatal("no dm_db_index_physical_stats statement was generated")
	}
	const want = `[dbo].[O'Brien.Log]`
	if len(args) != 1 || args[0] != want {
		t.Errorf("args = %q, want [%q]", args, want)
	}
}

// The scripter's OBJECT_ID existence guards need the same bracket-quoting as
// the fragmentation queries, and get it wrong in the more damaging direction:
// unbracketed, OBJECT_ID returns NULL for a dotted name, so the ScriptDrops
// guard ("IS NOT NULL" -> DROP) reads the table as absent and emits a script
// whose DROP never fires, while the create guard ("IS NULL" -> CREATE) always
// takes the branch and defeats its own IF-NOT-EXISTS. Both were confirmed
// against SQL Server 17.0.4055.5.
func TestScripterExistenceGuardsBracketQuoteTheObjectName(t *testing.T) {
	const (
		schema   = "my.schema"
		name     = "Sales.Archive"
		wantName = `OBJECT_ID(N'[my.schema].[Sales.Archive]', N'U')`
		badName  = `OBJECT_ID(N'my.schema.Sales.Archive', N'U')`
	)

	scriptWith := func(t *testing.T, opts ScriptOptions) string {
		t.Helper()
		db, err := sql.Open("capture", "")
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		captured.reset(tableMetadataRow(schema, name))

		d := &Database{server: &Server{db: db}, Name: "testdb"}
		out, err := NewScripter(d, opts).ScriptTable(context.Background(), schema, name)
		if err != nil {
			t.Fatalf("ScriptTable: %v", err)
		}
		return out
	}

	for _, c := range []struct {
		label string
		opts  ScriptOptions
	}{
		{"drop guard", ScriptOptions{ScriptDrops: true, IncludeIfNotExists: true}},
		{"create guard", ScriptOptions{IncludeIfNotExists: true}},
	} {
		t.Run(c.label, func(t *testing.T) {
			out := scriptWith(t, c.opts)
			if strings.Contains(out, badName) {
				t.Errorf("script uses the unbracketed name %s:\n%s", badName, out)
			}
			if !strings.Contains(out, wantName) {
				t.Errorf("script does not contain %s:\n%s", wantName, out)
			}
		})
	}
}

// TestQuoteLiteralIsUnicode pins S9: a literal without the N prefix is
// varchar, converted through the database's code page, and a Cyrillic or CJK
// path outside it turns into '?' — a file created somewhere else, or not at
// all. Every FILENAME gosmo writes goes through QuoteLiteral.
func TestQuoteLiteralIsUnicode(t *testing.T) {
	if got, want := QuoteLiteral(`C:\Данные\日本's.mdf`), `N'C:\Данные\日本''s.mdf'`; got != want {
		t.Errorf("QuoteLiteral = %s, want %s", got, want)
	}
	spec := DatabaseFileSpec{Name: "d", Path: `C:\Данные\d.mdf`}
	add, err := buildAddFileStatement("appdb", spec)
	if err != nil {
		t.Fatal(err)
	}
	for name, stmt := range map[string]string{"CREATE DATABASE file": buildFileDefClause(spec), "ADD FILE": add} {
		if !strings.Contains(stmt, `FILENAME = N'C:\Данные\d.mdf'`) {
			t.Errorf("%s: %s — the path is not an N'…' literal", name, stmt)
		}
	}
	ctx, script := WithScript(context.Background())
	if err := (&Server{}).RestoreFromSnapshot(ctx, "appdb", "снимок"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(script.Statements(), ""); !strings.Contains(got, "DATABASE_SNAPSHOT = N'снимок'") {
		t.Errorf("RestoreFromSnapshot: %s — the snapshot name is not an N'…' literal", got)
	}
}

// The fragmentation mode is formatted into the DMV call, not bound, so
// normalize is the only thing between a caller's string and the query text.
func TestFragmentationModeNormalize(t *testing.T) {
	for in, want := range map[FragmentationMode]FragmentationMode{
		"":                    FragmentationLimited,
		FragmentationLimited:  FragmentationLimited,
		FragmentationSampled:  FragmentationSampled,
		FragmentationDetailed: FragmentationDetailed,
	} {
		if got, err := in.normalize(); err != nil || got != want {
			t.Errorf("normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []FragmentationMode{"limited", "FAST", "LIMITED') --"} {
		if got, err := in.normalize(); err == nil {
			t.Errorf("normalize(%q) = %q, want an error", in, got)
		}
	}
	tbl := &Table{db: &Database{Name: "d"}, Schema: "dbo", Name: "t"}
	if _, err := tbl.FragmentationStats(context.Background(), "x"); err == nil ||
		err.Error() != `gosmo: fragmentation stats: invalid mode "x" (must be LIMITED, SAMPLED, or DETAILED)` {
		t.Errorf("FragmentationStats with a bad mode: err = %v", err)
	}
}
