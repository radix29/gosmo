package gosmo

import (
	"context"
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

	text, args, err := d.procRead(q, []any{"t", 1, int32(2), true, []byte{1}, nil, strings.Repeat("x", 4001), time.Time{}, uint32(3)})
	if err != nil {
		t.Fatalf("procRead declined arguments it declares: %v", err)
	}
	if want := "[App]]DB].sys.sp_executesql"; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	wantDecl := "@p1 nvarchar(4000), @p2 bigint, @p3 int, @p4 bit, @p5 varbinary(max), @p6 nvarchar(1), @p7 nvarchar(max), @p8 datetimeoffset(7), @p9 bigint"
	if len(args) != 11 || args[0] != q || args[1] != wantDecl || args[2] != "t" {
		t.Errorf("args = %q, want q, %q and the nine arguments", args[:2], wantDecl)
	}

	if _, args, err := d.procRead(q, nil); err != nil || len(args) != 1 || args[0] != q {
		t.Errorf("no arguments: args = %q, err = %v, want q alone", args, err)
	}
	if _, _, err := d.procRead(q, []any{"t", sql.NullString{}}); err == nil || !strings.Contains(err.Error(), "argument 2 is a sql.NullString") {
		t.Errorf("an undeclarable argument: err = %v, want it named", err)
	}
}

// A pooled read sends procRead's call and nothing else: no USE.
func TestPooledReadIsOneProcCall(t *testing.T) {
	d := useTestDB(t)
	if qErr, rowErr := readBoth(d); qErr != nil || rowErr != nil {
		t.Fatalf("query: %v, queryRow: %v", qErr, rowErr)
	}
	want := "[App]]DB].sys.sp_executesql"
	if !slices.Equal(useState.stmts, []string{want, want}) {
		t.Errorf("statements = %q, want two of %q", useState.stmts, want)
	}
}

// A read whose call fails with a can't-enter error asks d once more, with an
// empty call: d refusing that too is the USE failure every database-scoped
// read has always reported; d answering it leaves the read's own error, which
// was q reaching another database. Any other error is q's, with no second call.
func TestPooledReadTellsARefusalFromTheQuerysOwnError(t *testing.T) {
	const call = "[App]]DB].sys.sp_executesql"
	refused := mssql.Error{Number: 924, Message: "Database 'App]DB' is already open and can only have one user at a time."}
	other := mssql.Error{Number: 942, Message: "Database 'Other' cannot be opened because it is offline."}
	column := mssql.Error{Number: 207, Message: "Invalid column name 'x'."}
	cases := []struct {
		name             string
		failQuery, entry error
		wantUse          bool
		wantNum          int32
		wantStmts        int
	}{
		{"d refuses", refused, refused, true, 924, 4},
		{"q reaches a refusing database", other, nil, false, 942, 4},
		{"q's own fault", column, nil, false, 207, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := useTestDB(t)
			useState.failQuery, useState.failEntry = tc.failQuery, tc.entry
			qErr, rowErr := readBoth(d)
			for name, err := range map[string]error{"query": qErr, "queryRow": rowErr} {
				me, ok := errors.AsType[mssql.Error](err)
				if !ok || me.Number != tc.wantNum {
					t.Errorf("%s error = %v, want Msg %d underneath", name, err, tc.wantNum)
				}
				if got := strings.HasPrefix(err.Error(), "gosmo: USE App]DB: "); got != tc.wantUse {
					t.Errorf("%s error = %q, USE prefix %v, want %v", name, err, got, tc.wantUse)
				}
			}
			if len(useState.stmts) != tc.wantStmts {
				t.Errorf("statements = %q, want %d", useState.stmts, tc.wantStmts)
			}
			for _, s := range useState.stmts {
				if s != call {
					t.Errorf("sent %q, want only calls of %q", s, call)
				}
			}
		})
	}
}

// An argument procRead cannot declare fails the read before it takes a
// connection or sends anything.
func TestPooledReadRefusesAnUndeclarableArgument(t *testing.T) {
	d := useTestDB(t)
	ctx := context.Background()
	if _, err := d.query(ctx, "SELECT @p1", struct{}{}); err == nil {
		t.Error("query: no error")
	}
	var s string
	if err := d.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&s) }, "SELECT @p1", struct{}{}); err == nil {
		t.Error("queryRow: no error")
	}
	if len(useState.stmts) != 0 {
		t.Errorf("statements = %q, want none", useState.stmts)
	}
}
