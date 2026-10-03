//go:build livedb

// Live verification of T31: CLR functions are listed with their typed
// FuncType and a CLR scalar function's call template selects it as a value.
// TestLiveCLRModulesScriptRoundTrip scripts every CLR module kind — procedure,
// scalar and table function, DML, database and server trigger — drops it,
// replays the script and expects the same script back.
//
// The modules come from testdata/clr/w7clr.dll (source beside it), loaded
// from its bytes so no file has to exist on the server. From SQL Server 2017
// "clr strict security" refuses an unsigned SAFE assembly unless its hash is
// trusted, so the test trusts it for its own duration and removes the entry
// after. Creating a CLR function does not need "clr enabled"; nothing here
// calls one.
//
//	go test -tags livedb . -run TestLiveCLRFunctions -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops one throwaway database and one trusted-assembly entry;
// the round trip also creates and drops an assembly and a server trigger in
// master.
package gosmo

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLiveCLRFunctions(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 2*time.Minute)
	t.Cleanup(done)

	d, dropDB := liveScratchDB(t, db, ctx, "gosmo_clr_live")
	t.Cleanup(dropDB)
	liveCLRAssembly(t, db, ctx, d, "w7clr")
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
	// Schema Properties' Functions count stands in for this folder, so it
	// counts the CLR functions too (it once joined sys.sql_modules,
	// where a CLR module has no row).
	counts, err := d.SchemaRef("dbo").ObjectCountsByType(ctx)
	if err != nil {
		t.Fatalf("ObjectCountsByType: %v", err)
	}
	inDbo := 0
	for _, f := range fns {
		if f.Schema == "dbo" {
			inDbo++
		}
	}
	if counts.Functions != inDbo {
		t.Errorf("ObjectCountsByType(dbo).Functions = %d, UserDefinedFunctions lists %d in dbo", counts.Functions, inDbo)
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
		if s, err := sc.ScriptFunction(ctx, "dbo", name); err != nil || !strings.Contains(s, "AS EXTERNAL NAME [w7clr].[W7Clr].") {
			t.Errorf("ScriptFunction(%s) = %q, %v", name, s, err)
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

// liveCLRAssembly creates the test assembly as name in d. From SQL Server
// 2017 "clr strict security" refuses the unsigned SAFE assembly unless its
// hash is trusted, so the hash is trusted until the test ends.
func liveCLRAssembly(t *testing.T, db *sql.DB, ctx context.Context, d *Database, name string) {
	t.Helper()
	dll, err := os.ReadFile("testdata/clr/w7clr.dll")
	if err != nil {
		t.Fatal(err)
	}
	if major := d.serverMajorVersion(); major == 0 || major >= int(SQLServer2017) {
		const trust = `
DECLARE @h varbinary(64) = HASHBYTES('SHA2_512', @p1);
IF NOT EXISTS (SELECT 1 FROM sys.trusted_assemblies WHERE hash = @h)
    EXEC sys.sp_add_trusted_assembly @hash = @h, @description = N'gosmo live test w7clr'`
		if _, err := db.ExecContext(ctx, trust, dll); err != nil {
			t.Fatalf("trust the test assembly: %v", err)
		}
		t.Cleanup(func() {
			const untrust = `
DECLARE @h varbinary(64) = HASHBYTES('SHA2_512', @p1);
IF EXISTS (SELECT 1 FROM sys.trusted_assemblies WHERE hash = @h)
    EXEC sys.sp_drop_trusted_assembly @hash = @h`
			if _, err := db.ExecContext(context.Background(), untrust, dll); err != nil {
				t.Errorf("remove the trusted-assembly entry: %v", err)
			}
		})
	}
	if _, err := d.exec(ctx, `CREATE ASSEMBLY `+quoteIdent(name)+` FROM @p1 WITH PERMISSION_SET = SAFE`, dll); err != nil {
		t.Fatalf("CREATE ASSEMBLY: %v", err)
	}
}

func TestLiveCLRModulesScriptRoundTrip(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 3*time.Minute)
	t.Cleanup(done)

	d, dropDB := liveScratchDB(t, db, ctx, "gosmo_clr_script_live")
	t.Cleanup(dropDB)
	liveCLRAssembly(t, db, ctx, d, "w7clr")
	liveExecIn(t, d, ctx,
		`CREATE USER clr_runner WITHOUT LOGIN`,
		`CREATE TABLE dbo.clr_t (id int)`,
		`CREATE VIEW dbo.clr_v AS SELECT id FROM dbo.clr_t`,
		`CREATE FUNCTION dbo.clr_twice(@x int = 21) RETURNS int
		 WITH RETURNS NULL ON NULL INPUT, EXECUTE AS OWNER
		 AS EXTERNAL NAME w7clr.W7Clr.Twice`,
		`CREATE FUNCTION dbo.clr_seq(@c int) RETURNS TABLE (n int) ORDER (n DESC)
		 AS EXTERNAL NAME w7clr.W7Clr.Seq`,
		`CREATE PROCEDURE dbo.clr_echo @x int = NULL, @s nvarchar(20) = N'it''s', @y int OUTPUT
		 WITH EXECUTE AS N'clr_runner'
		 AS EXTERNAL NAME w7clr.W7Clr.Echo`,
		`CREATE TRIGGER dbo.clr_trg ON dbo.clr_t AFTER INSERT, DELETE NOT FOR REPLICATION
		 AS EXTERNAL NAME w7clr.W7Clr.Noop`,
		`CREATE TRIGGER dbo.clr_vtrg ON dbo.clr_v INSTEAD OF UPDATE
		 AS EXTERNAL NAME w7clr.W7Clr.Noop`,
		`DISABLE TRIGGER dbo.clr_trg ON dbo.clr_t`,
		// A DDL trigger last: "clr enabled" is off, so a CLR trigger that
		// fires fails its statement, and nothing below creates or drops a
		// table outside a replay of the trigger itself.
		`CREATE TRIGGER clr_ddl ON DATABASE WITH EXECUTE AS N'clr_runner' FOR CREATE_TABLE, DROP_TABLE
		 AS EXTERNAL NAME w7clr.W7Clr.Noop`,
		`DISABLE TRIGGER clr_ddl ON DATABASE`,
	)

	// A server trigger's assembly must be in master.
	srv := d.Server()
	master, err := srv.DatabaseByName(ctx, "master")
	if err != nil {
		t.Fatal(err)
	}
	const srvAsm, srvTrg = "gosmo_w7clr_live", "gosmo_clr_srv_live"
	liveExecIn(t, master, ctx,
		`IF EXISTS (SELECT 1 FROM sys.server_triggers WHERE name = N'`+srvTrg+`') DROP TRIGGER `+srvTrg+` ON ALL SERVER`,
		`IF EXISTS (SELECT 1 FROM sys.assemblies WHERE name = N'`+srvAsm+`') DROP ASSEMBLY `+srvAsm)
	liveCLRAssembly(t, db, ctx, master, srvAsm)
	t.Cleanup(func() {
		c := context.Background()
		master.exec(c, `IF EXISTS (SELECT 1 FROM sys.server_triggers WHERE name = N'`+srvTrg+`') DROP TRIGGER `+srvTrg+` ON ALL SERVER`)
		if _, err := master.exec(c, `DROP ASSEMBLY `+srvAsm); err != nil {
			t.Errorf("drop the master assembly: %v", err)
		}
	})
	// Created disabled straight away: an enabled CLR trigger with "clr
	// enabled" off fails the statement it fires on.
	liveExecIn(t, master, ctx,
		`CREATE TRIGGER `+srvTrg+` ON ALL SERVER FOR ALTER_SERVER_AUDIT AS EXTERNAL NAME `+srvAsm+`.W7Clr.Noop`,
		`DISABLE TRIGGER `+srvTrg+` ON ALL SERVER`)

	sc := func(v ScriptVerb) *Scripter { return NewScripter(d, ScriptOptions{Verb: v}) }
	ssc := func(v ScriptVerb) *ServerScripter { return NewServerScripter(srv, ScriptOptions{Verb: v}) }
	cases := []struct {
		name   string
		script func(ScriptVerb) (string, error)
		drop   string
		run    *Database
		want   []string
	}{
		{"scalar function", func(v ScriptVerb) (string, error) { return sc(v).ScriptFunction(ctx, "dbo", "clr_twice") },
			`DROP FUNCTION dbo.clr_twice`, d,
			[]string{"(@x int = 21)", "RETURNS int", "WITH RETURNS NULL ON NULL INPUT, EXECUTE AS OWNER", "EXTERNAL NAME [w7clr].[W7Clr].[Twice]"}},
		{"table function", func(v ScriptVerb) (string, error) { return sc(v).ScriptFunction(ctx, "dbo", "clr_seq") },
			`DROP FUNCTION dbo.clr_seq`, d,
			[]string{"(@c int)", "RETURNS TABLE (\n    [n] int\n)", "ORDER ([n] DESC)"}},
		{"procedure", func(v ScriptVerb) (string, error) { return sc(v).ScriptStoredProcedure(ctx, "dbo", "clr_echo") },
			`DROP PROCEDURE dbo.clr_echo`, d,
			[]string{"@x int = NULL", "@s nvarchar(20) = N'it''s'", "@y int OUTPUT", "WITH EXECUTE AS N'clr_runner'"}},
		{"DML trigger", func(v ScriptVerb) (string, error) { return sc(v).ScriptTrigger(ctx, "dbo", "clr_trg") },
			`DROP TRIGGER dbo.clr_trg`, d,
			[]string{"ON [dbo].[clr_t]", "AFTER INSERT, DELETE", "NOT FOR REPLICATION", "DISABLE TRIGGER [dbo].[clr_trg]"}},
		{"INSTEAD OF trigger", func(v ScriptVerb) (string, error) { return sc(v).ScriptTrigger(ctx, "dbo", "clr_vtrg") },
			`DROP TRIGGER dbo.clr_vtrg`, d,
			[]string{"ON [dbo].[clr_v]", "INSTEAD OF UPDATE"}},
		{"database trigger", func(v ScriptVerb) (string, error) { return sc(v).ScriptDatabaseTrigger(ctx, "clr_ddl") },
			`DROP TRIGGER clr_ddl ON DATABASE`, d,
			[]string{"ON DATABASE", "WITH EXECUTE AS N'clr_runner'", "AFTER CREATE_TABLE, DROP_TABLE", "DISABLE TRIGGER [clr_ddl] ON DATABASE"}},
		{"server trigger", func(v ScriptVerb) (string, error) { return ssc(v).ScriptServerTrigger(ctx, srvTrg) },
			`DROP TRIGGER ` + srvTrg + ` ON ALL SERVER`, master,
			[]string{"ON ALL SERVER", "AFTER ALTER_SERVER_AUDIT", "[" + srvAsm + "].[W7Clr].[Noop]", "DISABLE TRIGGER"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before, err := c.script(ScriptCreate)
			if err != nil {
				t.Fatalf("script: %v", err)
			}
			for _, w := range c.want {
				if !strings.Contains(before, w) {
					t.Errorf("script lacks %q:\n%s", w, before)
				}
			}
			alter, err := c.script(ScriptAlter)
			if err != nil {
				t.Fatalf("script ALTER: %v", err)
			}
			if !strings.Contains(alter, "ALTER ") {
				t.Errorf("ALTER script holds no ALTER:\n%s", alter)
			}
			liveRunScript(t, c.run, ctx, alter)
			liveExecIn(t, c.run, ctx, c.drop)
			liveRunScript(t, c.run, ctx, before)
			after, err := c.script(ScriptCreate)
			if err != nil {
				t.Fatalf("script the replayed module: %v", err)
			}
			if after != before {
				t.Errorf("the replayed module scripts differently\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
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
