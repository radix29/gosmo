package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"testing"
)

// maskRowDriver answers any query with maskRow, once (oneRow,
// server_backuppath_test.go).
type maskRowDriver struct{}

func (maskRowDriver) Open(string) (driver.Conn, error) { return maskRowConn{}, nil }

func init() { sql.Register("maskrow", maskRowDriver{}) }

var maskRow []driver.Value

type maskRowConn struct{}

func (maskRowConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (maskRowConn) Close() error                        { return nil }
func (maskRowConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (maskRowConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &oneRow{vals: maskRow}, nil
}

// TestMaskingFunctionReadsUnescaped pins H3: sys.masked_columns stores the
// function with its quotes doubled, and the read undoes it — the scripter
// escapes the field into its literal, so an escaped field was doubled again
// on every round trip and the replay masked with two quotes instead of one.
func TestMaskingFunctionReadsUnescaped(t *testing.T) {
	const stored = `partial(1, "X''X", 2)`
	maskRow = []driver.Value{
		"Email", int64(1), "nvarchar", int64(100), int64(0), int64(0), true, false, false,
		nil, nil, nil,
		false, nil,
		nil, nil,
		false,
		"sys", false,
		false, false,
		false, false,
		stored,
		int64(0), false, false,
		int64(0), false,
		"", "", "",
		"", "", false,
		int64(0), "",
		"", "", "", "",
		false,
	}
	db, err := sql.Open("maskrow", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(context.Background(), "SELECT")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := scanColumns(rows)
	if err != nil || len(cols) != 1 {
		t.Fatalf("scanColumns = %v, %v", cols, err)
	}
	if got, want := cols[0].MaskingFunction, `partial(1, "X'X", 2)`; got != want {
		t.Errorf("MaskingFunction = %q, want %q (as written, unescaped)", got, want)
	}
	if got, want := tableColumnDefinition(cols[0], ""), `MASKED WITH (FUNCTION = '`+stored+`')`; !strings.Contains(got, want) {
		t.Errorf("column definition = %q, want it to contain %q — the stored text, escaped once", got, want)
	}
}
