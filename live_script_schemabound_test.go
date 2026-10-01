//go:build livedb

// Live verification that a table's DROP and DROP AND CREATE scripts name the
// modules schema-bound to it — natively compiled modules on a
// memory-optimized table, and a SCHEMABINDING view — which make DROP TABLE
// fail with Msg 3729 (gossms fix plan 2026-10-01 item 16). The script is run
// as the note says it behaves: refused while they exist, leaving the table
// as it was (a temporal table still versioned), then replayed
// once they are dropped, with the modules' own scripts recreating them.
//
//	go test -tags livedb . -run TestLiveTableDropNamesSchemaBoundModules -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops one throwaway database; touches nothing else.
package gosmo

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

// liveRunScriptErrors runs every batch of script against d, carrying on past
// a failure as sqlcmd does, and returns the error numbers raised.
func liveRunScriptErrors(t *testing.T, d *Database, ctx context.Context, script string) []int32 {
	t.Helper()
	var nums []int32
	for _, batch := range splitGoBatches(script) {
		if _, err := d.exec(ctx, batch); err != nil {
			msErr, ok := errors.AsType[mssql.Error](err)
			if !ok {
				t.Fatalf("batch:\n%s\n%v", batch, err)
			}
			nums = append(nums, msErr.Number)
		}
	}
	return nums
}

func TestLiveTableDropNamesSchemaBoundModules(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 3*time.Minute)
	t.Cleanup(done)

	var dataPath string
	if err := db.QueryRowContext(ctx, `SELECT CONVERT(nvarchar(260), SERVERPROPERTY('InstanceDefaultDataPath'))`).Scan(&dataPath); err != nil {
		t.Fatalf("data path: %v", err)
	}
	d, drop := liveScratchDB(t, db, ctx, "gosmo_schemabound")
	t.Cleanup(drop)
	liveAddFileGroups(t, db, ctx, d, dataPath, false)

	const atomic = `BEGIN ATOMIC WITH (TRANSACTION ISOLATION LEVEL = SNAPSHOT, LANGUAGE = N'us_english')`
	liveExecIn(t, d, ctx,
		`CREATE TABLE dbo.mo (id int NOT NULL PRIMARY KEY NONCLUSTERED, v nvarchar(20) NULL) WITH (MEMORY_OPTIMIZED = ON)`,
		`CREATE PROCEDURE dbo.p_nc @id int WITH NATIVE_COMPILATION, SCHEMABINDING AS `+atomic+`
  SELECT id, v FROM dbo.mo WHERE id = @id;
END`,
		`CREATE FUNCTION dbo.f_nc (@id int) RETURNS int WITH NATIVE_COMPILATION, SCHEMABINDING AS `+atomic+`
  DECLARE @r int = (SELECT COUNT(*) FROM dbo.mo WHERE id = @id); RETURN @r;
END`,
		// A natively compiled trigger is schema-bound to its own table, and
		// DROP TABLE takes it along: not a blocker, not named.
		`CREATE TRIGGER dbo.tr_mo ON dbo.mo WITH NATIVE_COMPILATION, SCHEMABINDING AFTER INSERT AS `+atomic+`
  UPDATE dbo.mo SET v = v WHERE 1 = 0;
END`,
		`CREATE VIEW dbo.vsb WITH SCHEMABINDING AS SELECT id FROM dbo.mo`,
		`CREATE TABLE dbo.tt (id int NOT NULL PRIMARY KEY,
  vf datetime2 GENERATED ALWAYS AS ROW START NOT NULL, vt datetime2 GENERATED ALWAYS AS ROW END NOT NULL,
  PERIOD FOR SYSTEM_TIME (vf, vt)) WITH (SYSTEM_VERSIONING = ON (HISTORY_TABLE = dbo.tt_history))`,
		`CREATE VIEW dbo.vtt WITH SCHEMABINDING AS SELECT id FROM dbo.tt`,
	)

	opts := DefaultScriptOptions()
	sc := NewScripter(d, opts)
	modules := map[string]func(context.Context, string, string) (string, error){
		"p_nc": sc.ScriptStoredProcedure, "f_nc": sc.ScriptFunction, "vsb": sc.ScriptView,
	}
	moduleScripts := map[string]string{}
	for name, script := range modules {
		s, err := script(ctx, "dbo", name)
		if err != nil {
			t.Fatalf("script %s: %v", name, err)
		}
		moduleScripts[name] = s
	}

	create, err := sc.ScriptTable(ctx, "dbo", "mo")
	if err != nil {
		t.Fatalf("ScriptTable CREATE: %v", err)
	}
	if strings.Contains(create, "Msg 3729") {
		t.Errorf("a CREATE script carries the drop note:\n%s", create)
	}

	opts.Verb = ScriptDropAndCreate
	sc = NewScripter(d, opts)
	script, err := sc.ScriptTable(ctx, "dbo", "mo")
	if err != nil {
		t.Fatalf("ScriptTable DROP AND CREATE: %v", err)
	}
	want := "-- This DROP fails with Msg 3729 while these are schema-bound to [dbo].[mo]:\n" +
		"--   [dbo].[f_nc] (SQL_SCALAR_FUNCTION, natively compiled)\n" +
		"--   [dbo].[p_nc] (SQL_STORED_PROCEDURE, natively compiled)\n" +
		"--   [dbo].[vsb] (VIEW)\n" +
		"-- Drop them first, or ALTER them without SCHEMABINDING.\n" +
		"-- A natively compiled module requires SCHEMABINDING: drop it and recreate it after.\n"
	if !strings.HasPrefix(script, want) {
		t.Fatalf("want the script to open with\n%s\ngot:\n%s", want, script)
	}

	objectID := func(name string) string {
		return strings.Join(liveRowsAsStrings(t, d, ctx, `SELECT OBJECT_ID(@p1) AS id`, name), "")
	}

	// As the note says: refused, and nothing changes.
	before := objectID("dbo.mo")
	if nums := liveRunScriptErrors(t, d, ctx, script); len(nums) != 1 || nums[0] != 3729 {
		t.Fatalf("run with the modules in place: errors %v, want just Msg 3729", nums)
	}
	if got := objectID("dbo.mo"); got != before {
		t.Fatalf("the refused script changed the table: %s -> %s", before, got)
	}

	// With them dropped it runs, and their own scripts put them back.
	liveExecIn(t, d, ctx, `DROP VIEW dbo.vsb`, `DROP PROCEDURE dbo.p_nc`, `DROP FUNCTION dbo.f_nc`)
	liveRunScript(t, d, ctx, script)
	if got := objectID("dbo.mo"); got == before || got == "id=<nil>" {
		t.Fatalf("the table was not recreated: %s -> %s", before, got)
	}
	for _, name := range []string{"p_nc", "f_nc", "vsb"} {
		liveRunScript(t, d, ctx, moduleScripts[name])
	}
	got := liveRowsAsStrings(t, d, ctx, `
SELECT o.name, m.uses_native_compilation AS native, m.is_schema_bound AS bound
FROM   sys.sql_modules m JOIN sys.objects o ON o.object_id = m.object_id
ORDER  BY o.name`)
	if j := strings.Join(got, "; "); j != "name=f_nc native=true bound=true; name=p_nc native=true bound=true; "+
		"name=vsb native=false bound=true; name=vtt native=false bound=true" {
		t.Errorf("modules after the replay: %s", j)
	}

	// A temporal table's DROP first switches versioning off, and that ALTER
	// is refused with Msg 3729 too, so the DROP then fails with 13552 and the
	// table is left versioned.
	script, err = sc.ScriptTable(ctx, "dbo", "tt")
	if err != nil {
		t.Fatalf("ScriptTable tt: %v", err)
	}
	if !strings.HasPrefix(script, "-- This DROP fails with Msg 3729 while these are schema-bound to [dbo].[tt]:\n--   [dbo].[vtt] (VIEW)\n") {
		t.Fatalf("the temporal table's note:\n%s", script)
	}
	if nums := liveRunScriptErrors(t, d, ctx, script); len(nums) != 2 || nums[0] != 3729 || nums[1] != 13552 {
		t.Fatalf("temporal run: errors %v, want Msg 3729 then 13552\n%s", nums, script)
	}
	if got := strings.Join(liveRowsAsStrings(t, d, ctx, `SELECT temporal_type FROM sys.tables WHERE name = N'tt'`), ""); got != "temporal_type=2" {
		t.Errorf("after the refused script the table reads %s, want still system-versioned", got)
	}
}
