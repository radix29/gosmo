package gosmo

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

// procRead calls the database's own sp_executesql by its bracketed name — a
// text the driver sends as a procedure call — with q, the declarations and the
// arguments positionally after it.
func TestProcReadCallsTheDatabasesSpExecutesql(t *testing.T) {
	d := &Database{Name: "App]DB"}
	const q = "SELECT name FROM sys.tables WHERE name = @p1 AND schema_id = @p2"

	text, args, ok := d.procRead(q, []any{"t", 1, int32(2), true, []byte{1}, nil, strings.Repeat("x", 4001)})
	if !ok {
		t.Fatal("procRead declined arguments it declares")
	}
	if want := "[App]]DB].sys.sp_executesql"; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	wantDecl := "@p1 nvarchar(4000), @p2 bigint, @p3 int, @p4 bit, @p5 varbinary(max), @p6 nvarchar(1), @p7 nvarchar(max)"
	if len(args) != 9 || args[0] != q || args[1] != wantDecl || args[2] != "t" {
		t.Errorf("args = %q, want q, %q and the seven arguments", args[:2], wantDecl)
	}

	if _, args, ok := d.procRead(q, nil); !ok || len(args) != 1 || args[0] != q {
		t.Errorf("no arguments: args = %q, ok = %v, want q alone", args, ok)
	}
	if _, _, ok := d.procRead(q, []any{time.Now()}); ok {
		t.Error("procRead declared a time.Time, which it cannot match to the driver's type")
	}
}

// The switch is off by default; on, a read sends procRead's call and nothing
// else — no USE, and no recheckUse on failure, since its error is the read's
// own. An argument procRead cannot declare keeps the read on useBatch.
func TestProcReadsSwitch(t *testing.T) {
	if procReads {
		t.Fatal("procReads is on by default")
	}
	d := useTestDB(t)
	procReads = true
	t.Cleanup(func() { procReads = false })

	if qErr, rowErr := readBoth(d); qErr != nil || rowErr != nil {
		t.Fatalf("query: %v, queryRow: %v", qErr, rowErr)
	}
	want := "[App]]DB].sys.sp_executesql"
	if !slices.Equal(useState.stmts, []string{want, want}) {
		t.Errorf("statements = %q, want two of %q", useState.stmts, want)
	}

	useState.stmts = nil
	useState.failQuery = mssql.Error{Number: 924, Message: "Database 'App]DB' is already open and can only have one user at a time."}
	qErr, rowErr := readBoth(d)
	for name, err := range map[string]error{"query": qErr, "queryRow": rowErr} {
		if me, ok := errors.AsType[mssql.Error](err); !ok || me.Number != 924 {
			t.Errorf("%s error = %v, want the call's 924", name, err)
		}
	}
	if !slices.Equal(useState.stmts, []string{want, want}) {
		t.Errorf("statements = %q, want one call per read and no USE", useState.stmts)
	}

	useState.stmts, useState.failQuery = nil, nil
	var name string
	if err := d.queryRow(t.Context(), func(r *sql.Row) error { return r.Scan(&name) }, "SELECT @p1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(useState.stmts) != 1 || !strings.HasPrefix(useState.stmts[0], "USE ") {
		t.Errorf("statements = %q, want the useBatch fallback", useState.stmts)
	}
}
