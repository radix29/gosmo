//go:build livedb

// Live verification of the state a script carries beyond an object's
// definition — a foreign key's trust, a trigger's order and disabled flag —
// and of the scripts that must refuse rather than emit something wrong: an
// encrypted module, and the DML templates' server-filled columns.
//
// Each assertion is on the catalog after a replay, not on the script's text.
//
//	go test -tags livedb . -run TestLiveScriptState -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops two throwaway databases; touches nothing else.
package gosmo

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestLiveScriptStateSurvivesAReplay(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_state_src")
	defer dropSrc()
	dst, dropDst := liveScratchDB(t, db, ctx, "gosmo_state_dst")
	defer dropDst()

	// The child row violates every key it is checked against, so a key
	// replayed WITH CHECK fails with Msg 547 rather than passing quietly.
	for _, d := range []*Database{src, dst} {
		liveExecIn(t, d, ctx,
			"CREATE TABLE dbo.P (id int PRIMARY KEY)",
			"CREATE TABLE dbo.C (id int, p1 int, p2 int, p3 int)",
			"INSERT dbo.C VALUES (1, 99, 99, NULL)",
		)
	}
	liveExecIn(t, src, ctx,
		"ALTER TABLE dbo.C WITH NOCHECK ADD CONSTRAINT FK_dis FOREIGN KEY (p1) REFERENCES dbo.P (id)",
		"ALTER TABLE dbo.C NOCHECK CONSTRAINT FK_dis",
		"ALTER TABLE dbo.C WITH NOCHECK ADD CONSTRAINT FK_untr FOREIGN KEY (p2) REFERENCES dbo.P (id)",
		"ALTER TABLE dbo.C ADD CONSTRAINT FK_nfr FOREIGN KEY (p3) REFERENCES dbo.P (id) NOT FOR REPLICATION",
	)
	sc := NewScripter(src, DefaultScriptOptions())

	t.Run("foreign keys", func(t *testing.T) {
		for _, fk := range []string{"FK_dis", "FK_untr", "FK_nfr"} {
			s, err := sc.ScriptForeignKey(ctx, "dbo", "C", fk)
			if err != nil {
				t.Fatalf("ScriptForeignKey %s: %v", fk, err)
			}
			liveRunScript(t, dst, ctx, s)
		}
		const q = `SELECT name, is_disabled, is_not_trusted, is_not_for_replication
FROM sys.foreign_keys ORDER BY name`
		if a, b := liveRowsAsStrings(t, src, ctx, q), liveRowsAsStrings(t, dst, ctx, q); !slices.Equal(a, b) {
			t.Errorf("foreign keys differ after replay:\nsource: %v\nreplay: %v", a, b)
		}
	})

	t.Run("trigger order and state", func(t *testing.T) {
		livePinnedRun(t, db, ctx, src.Name,
			"CREATE TRIGGER dbo.trA ON dbo.C AFTER INSERT, UPDATE AS SET NOCOUNT ON\nGO\n"+
				"CREATE TRIGGER dbo.trB ON dbo.C AFTER INSERT AS SET NOCOUNT ON\nGO\n"+
				"EXEC sp_settriggerorder N'dbo.trA', N'First', N'INSERT'\nGO\n"+
				"EXEC sp_settriggerorder N'dbo.trA', N'Last', N'UPDATE'\nGO\n"+
				"EXEC sp_settriggerorder N'dbo.trB', N'Last', N'INSERT'\nGO\n"+
				"DISABLE TRIGGER dbo.trA ON dbo.C\nGO\n")
		const q = `SELECT tr.name, tr.is_disabled, te.type_desc, te.is_first, te.is_last
FROM sys.triggers tr JOIN sys.trigger_events te ON te.object_id = tr.object_id
ORDER BY tr.name, te.type_desc`
		want := liveRowsAsStrings(t, src, ctx, q)

		for _, tr := range []string{"trA", "trB"} {
			s, err := sc.ScriptTrigger(ctx, "dbo", tr)
			if err != nil {
				t.Fatalf("ScriptTrigger %s: %v", tr, err)
			}
			livePinnedRun(t, db, ctx, dst.Name, s)
		}
		if got := liveRowsAsStrings(t, dst, ctx, q); !slices.Equal(want, got) {
			t.Errorf("triggers differ after CREATE replay:\nsource: %v\nreplay: %v", want, got)
		}

		// ALTER TRIGGER resets a First/Last order to None, so the ALTER script
		// has to put it back as well.
		alter := NewScripter(src, ScriptOptions{Verb: ScriptAlter, SchemaQualify: true})
		for _, tr := range []string{"trA", "trB"} {
			s, err := alter.ScriptTrigger(ctx, "dbo", tr)
			if err != nil {
				t.Fatalf("ScriptTrigger ALTER %s: %v", tr, err)
			}
			livePinnedRun(t, db, ctx, src.Name, s)
		}
		if got := liveRowsAsStrings(t, src, ctx, q); !slices.Equal(want, got) {
			t.Errorf("triggers differ after ALTER replay:\nbefore: %v\nafter:  %v", want, got)
		}
	})

	t.Run("encrypted module", func(t *testing.T) {
		liveExecIn(t, src, ctx, "CREATE PROCEDURE dbo.pEnc WITH ENCRYPTION AS SELECT 1")
		for _, v := range []ScriptVerb{ScriptCreate, ScriptAlter, ScriptDropAndCreate} {
			s, err := NewScripter(src, ScriptOptions{Verb: v}).ScriptStoredProcedure(ctx, "dbo", "pEnc")
			if err == nil || s != "" {
				t.Errorf("verb %v: an encrypted procedure scripted as %q, err %v; want an error and no text", v, s, err)
			} else if !strings.Contains(err.Error(), "encrypted") {
				t.Errorf("verb %v: error does not say why: %v", v, err)
			}
		}
		// The trigger listing reads the same NULL definition: one encrypted
		// trigger used to fail Database.Triggers for every trigger.
		liveExecIn(t, src, ctx, "CREATE TRIGGER dbo.trEnc ON dbo.P WITH ENCRYPTION AFTER INSERT AS SET NOCOUNT ON")
		if trs, err := src.Triggers(ctx); err != nil || !slices.ContainsFunc(trs, func(tr *Trigger) bool { return tr.Name == "trEnc" }) {
			t.Errorf("Triggers with an encrypted trigger present: %v, %v", trs, err)
		}
		if _, err := sc.ScriptTrigger(ctx, "dbo", "trEnc"); err == nil || !strings.Contains(err.Error(), "encrypted") {
			t.Errorf("ScriptTrigger on an encrypted trigger: %v; want an encrypted error", err)
		}
		liveExecIn(t, src, ctx, "DROP TRIGGER dbo.trEnc")

		s, err := NewScripter(src, ScriptOptions{Verb: ScriptDrop}).ScriptStoredProcedure(ctx, "dbo", "pEnc")
		if err != nil || !strings.Contains(s, "DROP PROCEDURE IF EXISTS [dbo].[pEnc]") {
			t.Errorf("DROP of an encrypted procedure: %q, %v", s, err)
		}
	})

	t.Run("user named with WITH", func(t *testing.T) {
		liveExecIn(t, src, ctx, "CREATE USER [x WITH y] WITHOUT LOGIN WITH DEFAULT_SCHEMA = dbo")
		s, err := sc.ScriptUser(ctx, "x WITH y")
		if err != nil {
			t.Fatalf("ScriptUser: %v", err)
		}
		liveRunScript(t, dst, ctx, s)
		const q = `SELECT name, type, default_schema_name FROM sys.database_principals WHERE name = N'x WITH y'`
		if a, b := liveRowsAsStrings(t, src, ctx, q), liveRowsAsStrings(t, dst, ctx, q); !slices.Equal(a, b) || len(a) != 1 {
			t.Errorf("user differs after replay:\nsource: %v\nreplay: %v", a, b)
		}
	})

	t.Run("DML templates", func(t *testing.T) {
		liveExecIn(t, src, ctx, `CREATE TABLE dbo.T (
    id int IDENTITY PRIMARY KEY, rv rowversion, a int,
    vf datetime2 GENERATED ALWAYS AS ROW START NOT NULL,
    vt datetime2 GENERATED ALWAYS AS ROW END NOT NULL,
    PERIOD FOR SYSTEM_TIME (vf, vt))`)
		fill := func(template string, values map[string]string) string {
			return regexp.MustCompile(`<([^,<>]*),[^<>]*,>`).ReplaceAllStringFunc(template, func(m string) string {
				name := regexp.MustCompile(`^<([^,]*),`).FindStringSubmatch(m)[1]
				if v, ok := values[name]; ok {
					return v
				}
				if name == "Search Conditions" {
					return "1 = 1"
				}
				t.Fatalf("no value for placeholder %s in:\n%s", m, template)
				return ""
			})
		}
		run := func(table string, values map[string]string) {
			ins, err := sc.ScriptInsert(ctx, "dbo", table)
			if err != nil {
				t.Fatalf("ScriptInsert %s: %v", table, err)
			}
			liveRunScript(t, src, ctx, fill(ins, values))
			upd, err := sc.ScriptUpdate(ctx, "dbo", table)
			if err != nil {
				t.Fatalf("ScriptUpdate %s: %v", table, err)
			}
			liveRunScript(t, src, ctx, fill(upd, values))
		}
		run("T", map[string]string{"a": "1"})

		if src.serverMajorVersion() < int(SQLServer2017) {
			t.Log("graph tables are 2017+; edge template not checked")
			return
		}
		liveExecIn(t, src, ctx,
			"CREATE TABLE dbo.N (id int, rv rowversion) AS NODE",
			"CREATE TABLE dbo.E (w int) AS EDGE")
		// Before seeding: the UPDATE template's WHERE 1 = 1 rewrites every row.
		run("N", map[string]string{"id": "3"})
		liveExecIn(t, src, ctx, "INSERT dbo.N (id) VALUES (1), (2)")
		run("E", map[string]string{
			"$from_id": "(SELECT $node_id FROM dbo.N WHERE id = 1)",
			"$to_id":   "(SELECT $node_id FROM dbo.N WHERE id = 2)",
			"w":        "5",
		})
		if got := liveRowsAsStrings(t, src, ctx, "SELECT COUNT(*) AS n FROM dbo.E"); !slices.Equal(got, []string{"n=1"}) {
			t.Errorf("edge rows after the INSERT template: %v, want one", got)
		}
	})
}
