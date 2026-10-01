//go:build livedb

// Live verification that a table's DROP and DROP AND CREATE scripts name the
// foreign keys on other tables that reference it, which make DROP TABLE fail
// with Msg 3726 (gossms fix plan 2026-10-01 item 4). The script is run as the
// note says it behaves: refused while they exist, leaving the table in place
// (a temporal table with versioning switched off), then replayed once they
// are dropped. A self-reference is not named: DROP TABLE takes it along.
//
//	go test -tags livedb . -run TestLiveTableDropNamesReferencingForeignKeys -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops one throwaway database; touches nothing else.
package gosmo

import (
	"strings"
	"testing"
	"time"
)

func TestLiveTableDropNamesReferencingForeignKeys(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 3*time.Minute)
	t.Cleanup(done)

	d, drop := liveScratchDB(t, db, ctx, "gosmo_referencing_fk")
	t.Cleanup(drop)
	liveExecIn(t, d, ctx,
		`CREATE SCHEMA s`,
		`CREATE TABLE dbo.p (id int NOT NULL PRIMARY KEY, parent int NULL CONSTRAINT fk_p_self REFERENCES dbo.p (id))`,
		`CREATE TABLE dbo.c (id int NOT NULL PRIMARY KEY, p int NULL CONSTRAINT fk_c_p REFERENCES dbo.p (id))`,
		`CREATE TABLE s.c2 (id int NOT NULL PRIMARY KEY, p int NULL CONSTRAINT fk_c2_p REFERENCES dbo.p (id))`,
		`CREATE TABLE dbo.tt (id int NOT NULL PRIMARY KEY,
  vf datetime2 GENERATED ALWAYS AS ROW START NOT NULL, vt datetime2 GENERATED ALWAYS AS ROW END NOT NULL,
  PERIOD FOR SYSTEM_TIME (vf, vt)) WITH (SYSTEM_VERSIONING = ON (HISTORY_TABLE = dbo.tt_history))`,
		`CREATE TABLE dbo.ct (id int NOT NULL PRIMARY KEY, tt int NULL CONSTRAINT fk_ct_tt REFERENCES dbo.tt (id))`,
	)

	opts := DefaultScriptOptions()
	create, err := NewScripter(d, opts).ScriptTable(ctx, "dbo", "p")
	if err != nil {
		t.Fatalf("ScriptTable CREATE: %v", err)
	}
	if strings.Contains(create, "Msg 3726") {
		t.Errorf("a CREATE script carries the drop note:\n%s", create)
	}

	opts.Verb = ScriptDropAndCreate
	sc := NewScripter(d, opts)
	script, err := sc.ScriptTable(ctx, "dbo", "p")
	if err != nil {
		t.Fatalf("ScriptTable DROP AND CREATE: %v", err)
	}
	want := "-- This DROP fails with Msg 3726 while these foreign keys reference [dbo].[p]:\n" +
		"--   [dbo].[c].[fk_c_p] (FOREIGN KEY)\n" +
		"--   [s].[c2].[fk_c2_p] (FOREIGN KEY)\n" +
		"-- Drop them first, and recreate them after.\n"
	if !strings.HasPrefix(script, want) {
		t.Fatalf("want the script to open with\n%s\ngot:\n%s", want, script)
	}

	objectID := func(name string) string {
		return strings.Join(liveRowsAsStrings(t, d, ctx, `SELECT OBJECT_ID(@p1) AS id`, name), "")
	}

	// As the note says: refused, and nothing changes.
	before := objectID("dbo.p")
	if nums := liveRunScriptErrors(t, d, ctx, script); len(nums) != 1 || nums[0] != 3726 {
		t.Fatalf("run with the foreign keys in place: errors %v, want just Msg 3726", nums)
	}
	if got := objectID("dbo.p"); got != before {
		t.Fatalf("the refused script changed the table: %s -> %s", before, got)
	}

	// With them dropped it runs, self-reference and all.
	liveExecIn(t, d, ctx, `ALTER TABLE dbo.c DROP CONSTRAINT fk_c_p`, `ALTER TABLE s.c2 DROP CONSTRAINT fk_c2_p`)
	liveRunScript(t, d, ctx, script)
	if got := objectID("dbo.p"); got == before || got == "id=<nil>" {
		t.Fatalf("the table was not recreated: %s -> %s", before, got)
	}
	if got := strings.Join(liveRowsAsStrings(t, d, ctx, `SELECT name FROM sys.foreign_keys WHERE referenced_object_id = OBJECT_ID(N'dbo.p')`), ""); got != "name=fk_p_self" {
		t.Errorf("foreign keys on the recreated table: %s, want just its self-reference", got)
	}

	// A temporal table's SET (SYSTEM_VERSIONING = OFF) is not blocked by a
	// foreign key: it runs, the DROP is refused, and versioning stays off.
	script, err = sc.ScriptTable(ctx, "dbo", "tt")
	if err != nil {
		t.Fatalf("ScriptTable tt: %v", err)
	}
	if want := "-- This DROP fails with Msg 3726 while these foreign keys reference [dbo].[tt]:\n" +
		"--   [dbo].[ct].[fk_ct_tt] (FOREIGN KEY)\n" +
		"-- Drop them first, and recreate them after.\n" +
		"-- They do not block SET (SYSTEM_VERSIONING = OFF): it runs, and stays off.\n"; !strings.HasPrefix(script, want) {
		t.Fatalf("the temporal table's note:\n%s", script)
	}
	if nums := liveRunScriptErrors(t, d, ctx, script); len(nums) != 1 || nums[0] != 3726 {
		t.Fatalf("temporal run: errors %v, want just Msg 3726\n%s", nums, script)
	}
	if got := strings.Join(liveRowsAsStrings(t, d, ctx, `SELECT temporal_type FROM sys.tables WHERE name = N'tt'`), ""); got != "temporal_type=0" {
		t.Errorf("after the refused script the table reads %s, want versioning off as the note says", got)
	}
}
