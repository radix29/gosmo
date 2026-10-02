//go:build livedb

// Live verification of T31: CLR functions are listed with their typed
// FuncType, a CLR scalar function's call template selects it as a value, and
// scripting a CLR function's CREATE is refused with ErrUnsupported rather
// than reported as not found.
//
// The functions come from testdata/clr/w7clr.dll (source beside it), loaded
// from its bytes so no file has to exist on the server. From SQL Server 2017
// "clr strict security" refuses an unsigned SAFE assembly unless its hash is
// trusted, so the test trusts it for its own duration and removes the entry
// after. Creating a CLR function does not need "clr enabled"; nothing here
// calls one.
//
//	go test -tags livedb . -run TestLiveCLRFunctions -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops one throwaway database and one trusted-assembly entry.
package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLiveCLRFunctions(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 2*time.Minute)
	defer done()

	dll, err := os.ReadFile("testdata/clr/w7clr.dll")
	if err != nil {
		t.Fatal(err)
	}
	d, dropDB := liveScratchDB(t, db, ctx, "gosmo_clr_live")
	defer dropDB()

	if major := d.serverMajorVersion(); major == 0 || major >= int(SQLServer2017) {
		const trust = `
DECLARE @h varbinary(64) = HASHBYTES('SHA2_512', @p1);
IF NOT EXISTS (SELECT 1 FROM sys.trusted_assemblies WHERE hash = @h)
    EXEC sys.sp_add_trusted_assembly @hash = @h, @description = N'gosmo live test w7clr'`
		if _, err := db.ExecContext(ctx, trust, dll); err != nil {
			t.Fatalf("trust the test assembly: %v", err)
		}
		defer func() {
			const untrust = `
DECLARE @h varbinary(64) = HASHBYTES('SHA2_512', @p1);
IF EXISTS (SELECT 1 FROM sys.trusted_assemblies WHERE hash = @h)
    EXEC sys.sp_drop_trusted_assembly @hash = @h`
			if _, err := db.ExecContext(ctx, untrust, dll); err != nil {
				t.Errorf("remove the trusted-assembly entry: %v", err)
			}
		}()
	}
	if _, err := d.exec(ctx, `CREATE ASSEMBLY w7clr FROM @p1 WITH PERMISSION_SET = SAFE`, dll); err != nil {
		t.Fatalf("CREATE ASSEMBLY: %v", err)
	}
	liveExecIn(t, d, ctx,
		`CREATE FUNCTION dbo.clr_twice(@x int) RETURNS int AS EXTERNAL NAME w7clr.W7Clr.Twice`,
		`CREATE FUNCTION dbo.clr_seq(@c int) RETURNS TABLE (n int) AS EXTERNAL NAME w7clr.W7Clr.Seq`,
		`CREATE FUNCTION dbo.tsql_fn(@x int) RETURNS int AS BEGIN RETURN @x END`,
	)

	fns, err := d.UserDefinedFunctions(ctx)
	if err != nil {
		t.Fatalf("UserDefinedFunctions: %v", err)
	}
	got := map[string]FunctionType{}
	for _, f := range fns {
		got[f.Name] = f.FuncType
	}
	want := map[string]FunctionType{"clr_twice": FunctionTypeCLRScalar, "clr_seq": FunctionTypeCLRTable, "tsql_fn": FunctionTypeScalar}
	for name, ft := range want {
		if got[name] != ft {
			t.Errorf("%s listed as %q, want %q (all: %v)", name, got[name], ft, got)
		}
	}

	sc := NewScripter(d, DefaultScriptOptions())
	call, err := sc.ScriptFunctionCall(ctx, "dbo", "clr_twice", got["clr_twice"])
	if err != nil {
		t.Fatalf("ScriptFunctionCall: %v", err)
	}
	if strings.Contains(call, "FROM") {
		t.Errorf("a CLR scalar function is selected from:\n%s", call)
	}
	// The template's placeholder filled in, the server binds the call. SET
	// PARSEONLY would not resolve the name, so it is compiled under
	// SHOWPLAN_XML. With "clr enabled" off — the default, and left alone —
	// compiling a bound CLR call stops at Msg 6263, after name binding; the
	// old template's SELECT * FROM a scalar function fails binding, Msg 208.
	stmt := strings.Replace(strings.TrimSuffix(strings.TrimSpace(call), "GO"), "<x, int,>", "21", 1)
	if n := liveCompileError(ctx, db, d.Name, stmt); n != 0 && n != 6263 {
		t.Errorf("the call template does not bind: Msg %d\n%s", n, stmt)
	}
	if n := liveCompileError(ctx, db, d.Name, "SELECT * FROM [dbo].[clr_twice](21)"); n != 208 {
		t.Errorf("selecting FROM a CLR scalar function: Msg %d, want 208 — the premise of T31", n)
	}

	for _, name := range []string{"clr_twice", "clr_seq"} {
		if _, err := sc.ScriptFunction(ctx, "dbo", name); !errors.Is(err, ErrUnsupported) {
			t.Errorf("ScriptFunction(%s) = %v, want ErrUnsupported", name, err)
		}
	}
	if s, err := sc.ScriptFunction(ctx, "dbo", "tsql_fn"); err != nil || !strings.Contains(s, "RETURN @x") {
		t.Errorf("ScriptFunction(tsql_fn) = %q, %v", s, err)
	}
	drops := NewScripter(d, ScriptOptions{Verb: ScriptDrop})
	if s, err := drops.ScriptFunction(ctx, "dbo", "clr_seq"); err != nil || !strings.Contains(s, "DROP FUNCTION IF EXISTS [dbo].[clr_seq]") {
		t.Errorf("a CLR function's DROP is still scriptable: %q, %v", s, err)
	}

	for _, name := range []string{"clr_twice", "clr_seq"} {
		if err := d.UserDefinedFunctionRef("dbo", name).Drop(ctx); err != nil {
			t.Errorf("Drop %s: %v", name, err)
		}
	}
}

// liveCompileError compiles stmt without running it, under SHOWPLAN_XML, and
// returns the SQL Server error number it fails with, 0 for none. SET
// SHOWPLAN_XML must be alone in its batch, so the statements run on one
// pinned connection; a failure outside stmt itself is -1.
func liveCompileError(ctx context.Context, db *sql.DB, dbName, stmt string) int32 {
	conn, err := db.Conn(ctx)
	if err != nil {
		return -1
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE "+quoteIdent(dbName)); err != nil {
		return -1
	}
	if _, err := conn.ExecContext(ctx, "SET SHOWPLAN_XML ON"); err != nil {
		return -1
	}
	defer conn.ExecContext(ctx, "SET SHOWPLAN_XML OFF")
	rows, err := conn.QueryContext(ctx, stmt)
	if err == nil {
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
	}
	if err == nil {
		return 0
	}
	if se, ok := AsSQLError(err); ok {
		return se.Number
	}
	return -1
}
