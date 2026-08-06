//go:build livedb

// Live verification that ExecProc's WithScript form produces T-SQL SQL
// Server actually accepts. scriptExecProc picks a DECLARE type per output
// parameter from the Go pointee type (scriptDeclType); script_test.go pins
// that mapping, but only the server can say whether it binds — and it is the
// server that caught the sql.Null*/UniqueIdentifier gap these tests now
// cover, which no unit test could have.
//
//	go test -tags livedb . -run TestLiveExecProc -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway procedures; touches nothing else.
// Skipped entirely without -livedb.
package gosmo

import (
	"context"
	"database/sql"
	"flag"
	"strings"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

var liveDSN = flag.String("livedb", "", "SQL Server DSN for the live ExecProc script tests")

func liveDB(t *testing.T) (*sql.DB, context.Context, func()) {
	t.Helper()
	if *liveDSN == "" {
		t.Skip("no -livedb DSN given")
	}
	db, err := sql.Open("sqlserver", *liveDSN)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	if err := db.PingContext(ctx); err != nil {
		cancel()
		db.Close()
		t.Fatalf("ping: %v", err)
	}
	return db, ctx, func() { db.Close(); cancel() }
}

// liveProc creates a throwaway procedure and returns its name plus a
// dropper. body is the procedure's parameter list and body.
func liveProc(t *testing.T, db *sql.DB, ctx context.Context, name, body string) func() {
	t.Helper()
	if _, err := db.ExecContext(ctx, "IF OBJECT_ID('dbo."+name+"') IS NOT NULL DROP PROCEDURE dbo."+name); err != nil {
		t.Fatalf("pre-drop %s: %v", name, err)
	}
	if _, err := db.ExecContext(ctx, "CREATE PROCEDURE dbo."+name+" "+body); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return func() {
		db.ExecContext(context.Background(), "DROP PROCEDURE dbo."+name)
	}
}

// The plain-Go destination types, scripted and run, then read back — so a
// DECLARE that binds but truncates (a BIGINT value into an INT, a decimal
// into a FLOAT) fails on the value rather than passing on the exec.
func TestLiveExecProcScriptRunsOnTheServer(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	drop := liveProc(t, db, ctx, "gosmo_live_outparams", `
	@n        int            OUTPUT,
	@big      bigint         OUTPUT,
	@s        nvarchar(100)  OUTPUT,
	@f        float          OUTPUT,
	@b        bit            OUTPUT,
	@when     datetime2      OUTPUT,
	@money    decimal(18,4)  OUTPUT,
	@in       int
AS
BEGIN
	SET @n = @in * 2;
	SET @big = 9000000000;
	SET @s = N'written';
	SET @f = 1.5;
	SET @b = 1;
	SET @when = '2026-08-06T12:00:00';
	SET @money = 1234.5678;
	RETURN 7;
END`)
	defer drop()

	var (
		n     int32
		big   int64
		s     string
		f     float64
		b     bool
		when  time.Time
		money float64 // the natural Go choice for a DECIMAL OUTPUT
	)
	params := []ProcParam{
		Out("n", &n), Out("big", &big), Out("s", &s), Out("f", &f),
		Out("b", &b), Out("when", &when), Out("money", &money),
		In("in", 21),
	}

	stmt, err := scriptExecProc("[dbo].[gosmo_live_outparams]", params)
	if err != nil {
		t.Fatalf("scriptExecProc: %v", err)
	}
	t.Logf("scripted:\n%s", stmt)

	// The whole point: hand the scripted text to the server exactly as a
	// user would after pasting it into a query window.
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		t.Fatalf("SQL Server refused the scripted EXEC: %v\n%s", err, stmt)
	}

	// And confirm the scripted form is not silently a no-op: run it again
	// with the values selected back out, so a DECLARE type that truncates
	// or refuses a value shows up as a wrong value rather than passing.
	verify := stmt + "\nSELECT @n, @big, @s, @f, @b, @when, @money;"
	var (
		gotN     int32
		gotBig   int64
		gotS     string
		gotF     float64
		gotB     bool
		gotWhen  time.Time
		gotMoney float64
	)
	row := db.QueryRowContext(ctx, verify)
	if err := row.Scan(&gotN, &gotBig, &gotS, &gotF, &gotB, &gotWhen, &gotMoney); err != nil {
		t.Fatalf("scanning the scripted EXEC's outputs: %v", err)
	}
	if gotN != 42 {
		t.Errorf("@n = %d, want 42 (the IN parameter did not reach the procedure)", gotN)
	}
	if gotBig != 9000000000 {
		t.Errorf("@big = %d, want 9000000000 — BIGINT DECLARE truncated it", gotBig)
	}
	if gotS != "written" {
		t.Errorf("@s = %q, want %q", gotS, "written")
	}
	if gotF != 1.5 {
		t.Errorf("@f = %v, want 1.5", gotF)
	}
	if !gotB {
		t.Error("@b = false, want true")
	}
	if gotWhen.Year() != 2026 || gotWhen.Month() != time.August {
		t.Errorf("@when = %v, want 2026-08-06", gotWhen)
	}
	if gotMoney != 1234.5678 {
		t.Errorf("@money = %v, want 1234.5678 — the FLOAT DECLARE lost decimal precision", gotMoney)
	}
}

// Every declTypeByName entry, scripted and then run against a procedure
// whose OUTPUT parameter is the matching T-SQL type. Before the mapping
// existed, all eleven scripted as SQL_VARIANT and every one of them was
// refused — "Implicit conversion from data type sql_variant to <type> is not
// allowed" — so this is the test that would have caught it.
func TestLiveExecProcScriptDeclTypesAreAccepted(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	drop := liveProc(t, db, ctx, "gosmo_live_decltypes", `
	@i int = NULL OUTPUT, @b bigint = NULL OUTPUT, @s nvarchar(50) = NULL OUTPUT,
	@g uniqueidentifier = NULL OUTPUT, @d decimal(18,4) = NULL OUTPUT,
	@bit bit = NULL OUTPUT, @ti tinyint = NULL OUTPUT, @si smallint = NULL OUTPUT,
	@dt datetime2 = NULL OUTPUT
AS
BEGIN
	SET @i = 5; SET @b = 9000000000; SET @s = N'hi'; SET @g = NEWID();
	SET @d = 12.25; SET @bit = 1; SET @ti = 3; SET @si = 300;
	SET @dt = '2026-08-06T09:00:00';
END`)
	defer drop()

	var (
		ni  sql.NullInt64
		n32 sql.NullInt32
		n16 sql.NullInt16
		nby sql.NullByte
		ns  sql.NullString
		nb  sql.NullBool
		nf  sql.NullFloat64
		nt  sql.NullTime
		tt  time.Time
		g   mssql.UniqueIdentifier
		ng  mssql.NullUniqueIdentifier
	)
	for _, tc := range []struct {
		name string
		parm string
		dest any
	}{
		{"*sql.NullInt64", "b", &ni},
		{"*sql.NullInt32", "i", &n32},
		{"*sql.NullInt16", "si", &n16},
		{"*sql.NullByte", "ti", &nby},
		{"*sql.NullString", "s", &ns},
		{"*sql.NullBool", "bit", &nb},
		{"*sql.NullFloat64", "d", &nf},
		{"*sql.NullTime", "dt", &nt},
		{"*time.Time", "dt", &tt},
		{"*mssql.UniqueIdentifier", "g", &g},
		{"*mssql.NullUniqueIdentifier", "g", &ng},
	} {
		stmt, err := scriptExecProc("[dbo].[gosmo_live_decltypes]", []ProcParam{Out(tc.parm, tc.dest)})
		if err != nil {
			t.Errorf("%s: scriptExecProc: %v", tc.name, err)
			continue
		}
		if strings.Contains(stmt, "SQL_VARIANT") {
			t.Errorf("%s: still scripts as SQL_VARIANT:\n%s", tc.name, stmt)
			continue
		}
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Errorf("%s: SQL Server refused the scripted EXEC: %v\n%s", tc.name, err, stmt)
		}
	}
}

// SQL_VARIANT is still the fallthrough for an unmapped destination, and it
// must stay one rather than becoming an error: against a procedure whose
// parameter really is sql_variant it is the correct DECLARE and the server
// takes it. That is why the fix widened the mapping instead of refusing the
// default.
func TestLiveExecProcScriptSQLVariantIsValidForAVariantParameter(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	drop := liveProc(t, db, ctx, "gosmo_live_variant", `
	@v sql_variant = NULL OUTPUT
AS
BEGIN
	SET @v = CAST(5 AS int);
END`)
	defer drop()

	var unmapped struct{ X int }
	stmt, err := scriptExecProc("[dbo].[gosmo_live_variant]", []ProcParam{Out("v", &unmapped)})
	if err != nil {
		t.Fatalf("scriptExecProc: %v", err)
	}
	if !strings.Contains(stmt, "SQL_VARIANT") {
		t.Fatalf("expected the SQL_VARIANT fallthrough, got:\n%s", stmt)
	}
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		t.Errorf("SQL Server refused SQL_VARIANT against a sql_variant parameter: %v\n%s", err, stmt)
	}
}
