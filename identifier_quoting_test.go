package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
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
	captured.add(query)
	return driver.ResultNoRows, nil
}

func (c *captureConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	captured.add(query)
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

// tableMetadataRow satisfies Database.TableByNameContext's sys.tables lookup,
// so a Scripter test gets past it to the SQL it actually wants to inspect.
// The schema/name it reports are what the scripter then scripts.
func tableMetadataRow(schema, name string) cannedRow {
	return cannedRow{
		match: "FROM   sys.tables t",
		cols:  []string{"object_id", "schema", "name", "create_date", "modify_date", "repl", "memopt"},
		row:   []driver.Value{int64(1), schema, name, time.Time{}, time.Time{}, false, false},
	}
}

type captureLog struct {
	mu     sync.Mutex
	qs     []string
	canned []cannedRow
}

func (l *captureLog) add(q string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.qs = append(l.qs, q)
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
	l.qs = nil
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
// bracket-quoted qualified name from a raw one.
func captureTable(t *testing.T) *Table {
	t.Helper()
	db, err := sql.Open("capture", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	captured.reset()
	srv := &Server{db: db}
	return &Table{db: &Database{server: srv, name: "testdb"}, Schema: "my.schema", Name: "Sales.Archive"}
}

// A qualified name embedded in a T-SQL string literal must be bracket-quoted
// inside the literal: OBJECT_ID(N'[dbo].[Sales.Archive]'), never
// OBJECT_ID(N'dbo.Sales.Archive'). Unbracketed, SQL Server reads a name
// containing '.' as a multi-part name and resolves it to the wrong object or
// to NULL — and a NULL object_id means "every object in the database" to
// sys.dm_db_index_physical_stats, so the unbracketed form returns plausible
// stats for the wrong tables rather than failing. Verified against SQL Server
// 17.0.4055.5: the Table.FragmentationStats shape below silently returned
// every index in the database, the Index.Fragmentation shape errored with
// 2561 ("Parameter 3 is incorrect for this statement"), and the scripter's
// OBJECT_ID existence guards inverted.
func TestFragmentationQueriesBracketQuoteTheObjectName(t *testing.T) {
	const (
		wantName = `OBJECT_ID(N'[my.schema].[Sales.Archive]')`
		badName  = `OBJECT_ID(N'my.schema.Sales.Archive')`
	)

	t.Run("Index.Fragmentation", func(t *testing.T) {
		tbl := captureTable(t)
		idx := &Index{Name: "IX_pad", IndexID: 2}
		// The capture driver returns no rows, so this errors; the statement it
		// generated on the way is what's under test.
		_, _ = idx.FragmentationContext(context.Background(), tbl, "SAMPLED")

		q := captured.find("dm_db_index_physical_stats")
		if q == "" {
			t.Fatal("no dm_db_index_physical_stats statement was generated")
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
		_, _ = tbl.FragmentationStatsContext(context.Background(), "LIMITED")

		q := captured.find("dm_db_index_physical_stats")
		if q == "" {
			t.Fatal("no dm_db_index_physical_stats statement was generated")
		}
		if strings.Contains(q, badName) {
			t.Errorf("generated SQL uses the unbracketed name %s:\n%s", badName, q)
		}
		if !strings.Contains(q, wantName) {
			t.Errorf("generated SQL does not contain %s:\n%s", wantName, q)
		}
	})
}

// A name containing a single quote must still be escaped for the literal it
// sits in, on top of being bracket-quoted — the two are independent.
func TestFragmentationQueryEscapesQuoteInObjectName(t *testing.T) {
	db, err := sql.Open("capture", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	captured.reset()

	tbl := &Table{db: &Database{server: &Server{db: db}, name: "testdb"}, Schema: "dbo", Name: "O'Brien.Log"}
	_, _ = tbl.FragmentationStatsContext(context.Background(), "LIMITED")

	q := captured.find("dm_db_index_physical_stats")
	if q == "" {
		t.Fatal("no dm_db_index_physical_stats statement was generated")
	}
	const want = `OBJECT_ID(N'[dbo].[O''Brien.Log]')`
	if !strings.Contains(q, want) {
		t.Errorf("generated SQL does not contain %s:\n%s", want, q)
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

		d := &Database{server: &Server{db: db}, name: "testdb"}
		out, err := NewScripter(d, opts).ScriptTableContext(context.Background(), schema, name)
		if err != nil {
			t.Fatalf("ScriptTableContext: %v", err)
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
