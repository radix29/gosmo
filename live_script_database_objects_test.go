//go:build livedb

package gosmo

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestLiveScriptDatabaseObjectsReplay scripts a fixture database holding the
// shapes dependency order exists for — a SCHEMABINDING view chain, a view on
// a function on a table, an FK cycle and a self-FK, a default calling a
// function, a procedure taking a table type, mutually calling procedures, a
// synonym, a trigger, a temporal table, partitioning, an XML schema
// collection, two tables whose dotted names once shared a key ([a.b].[c] and
// [a].[b.c]: one was scripted twice, the other never, and the replay still
// succeeded), on 2019+ an edge table named before one of its node tables
// (its inline CONNECTION failed the replay, Msg 13931) and, on 2022+, ledger
// tables (their ledger view failed the replay) and encrypted modules with
// what needs them (a view on one, a trigger on one, a GRANT on one: each
// failed the replay), and a rule bound to an alias type and a default bound
// to a column (each binding failed the replay: neither was scripted) —
// replays the script into an empty database, and compares the two catalogs,
// bindings included.
//
//	go test -tags livedb . -run TestLiveScriptDatabaseObjectsReplay -v -livedb '...'
func TestLiveScriptDatabaseObjectsReplay(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 120*time.Second)
	defer done()

	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_objs_src_live")
	defer dropSrc()
	dst, dropDst := liveScratchDB(t, db, ctx, "gosmo_objs_dst_live")
	defer dropDst()

	liveExecIn(t, src, ctx,
		`CREATE SCHEMA app`,
		`CREATE USER gosmo_objs_u WITHOUT LOGIN`,
		`CREATE ROLE gosmo_objs_r`,
		`ALTER ROLE gosmo_objs_r ADD MEMBER gosmo_objs_u`,
		`CREATE PARTITION FUNCTION objs_pf (INT) AS RANGE RIGHT FOR VALUES (100, 200)`,
		`CREATE PARTITION SCHEME objs_ps AS PARTITION objs_pf ALL TO ([PRIMARY])`,
		`CREATE XML SCHEMA COLLECTION app.xc AS N'<xs:schema xmlns:xs="http://www.w3.org/2001/XMLSchema"><xs:element name="a" type="xs:string"/></xs:schema>'`,
		`CREATE TYPE app.code FROM varchar(10) NOT NULL`,
		`CREATE TYPE app.ids AS TABLE (id INT, c app.code)`,
		`CREATE SEQUENCE app.seq AS INT START WITH 5`,
		`CREATE FUNCTION app.fn_default() RETURNS INT AS BEGIN RETURN 7 END`,
		`CREATE TABLE app.a (id INT NOT NULL CONSTRAINT pk_a PRIMARY KEY, b_id INT NULL, c app.code NOT NULL DEFAULT 'x', x XML(app.xc) NULL,
		   d INT NOT NULL CONSTRAINT df_a_d DEFAULT (app.fn_default()), parent INT NULL)`,
		`CREATE TABLE app.b (id INT NOT NULL CONSTRAINT pk_b PRIMARY KEY, a_id INT NULL)`,
		`ALTER TABLE app.a ADD CONSTRAINT fk_a_b FOREIGN KEY (b_id) REFERENCES app.b (id)`,
		`ALTER TABLE app.b ADD CONSTRAINT fk_b_a FOREIGN KEY (a_id) REFERENCES app.a (id)`,
		`ALTER TABLE app.a ADD CONSTRAINT fk_a_self FOREIGN KEY (parent) REFERENCES app.a (id)`,
		`EXEC sys.sp_addextendedproperty N'MS_Description', N'a to b', N'SCHEMA', N'app', N'TABLE', N'a', N'CONSTRAINT', N'fk_a_b'`,
		`CREATE TABLE app.part (id INT NOT NULL) ON objs_ps (id)`,
		`CREATE TABLE app.temporal (id INT NOT NULL CONSTRAINT pk_temporal PRIMARY KEY,
		   s datetime2 GENERATED ALWAYS AS ROW START NOT NULL, e datetime2 GENERATED ALWAYS AS ROW END NOT NULL,
		   PERIOD FOR SYSTEM_TIME (s, e)) WITH (SYSTEM_VERSIONING = ON (HISTORY_TABLE = app.temporal_hist))`,
		`CREATE VIEW app.v1 WITH SCHEMABINDING AS SELECT id, b_id FROM app.a`,
		`CREATE VIEW app.v2 WITH SCHEMABINDING AS SELECT id FROM app.v1`,
		`CREATE FUNCTION app.fn_ids() RETURNS TABLE WITH SCHEMABINDING AS RETURN SELECT id FROM app.v2`,
		`CREATE VIEW app.v0 AS SELECT id FROM app.fn_ids()`,
		`CREATE PROCEDURE app.p_use @t app.ids READONLY AS SELECT id FROM @t`,
		`CREATE PROCEDURE app.p_one AS EXEC app.p_two`,
		`CREATE PROCEDURE app.p_two AS EXEC app.p_one`,
		`CREATE SYNONYM app.syn FOR app.a`,
		`CREATE PROCEDURE app.p_syn AS SELECT id FROM app.syn`,
		`CREATE TRIGGER app.tr_a ON app.a AFTER INSERT AS SET NOCOUNT ON`,
		`CREATE SCHEMA [a.b]`,
		`CREATE SCHEMA [a]`,
		`CREATE TABLE [a.b].[c] (id INT NOT NULL)`,
		`CREATE TABLE [a].[b.c] (id INT NOT NULL)`,
		`GRANT SELECT ON app.v0 TO gosmo_objs_r`,
		// Encrypted, so noted: what needs them is noted too, the GRANTs on
		// them are comments, and p_late, whose reference SQL Server resolves
		// when it runs, is scripted with a warning.
		`CREATE PROCEDURE app.p_enc WITH ENCRYPTION AS SELECT 1 AS one`,
		`GRANT EXECUTE ON app.p_enc TO gosmo_objs_r`,
		`CREATE VIEW app.v_enc WITH ENCRYPTION AS SELECT id FROM app.b`,
		`CREATE VIEW app.v_on_enc AS SELECT id FROM app.v_enc`,
		`CREATE TRIGGER app.tr_enc ON app.v_enc INSTEAD OF INSERT AS SET NOCOUNT ON`,
		`GRANT SELECT ON app.v_enc TO gosmo_objs_r`,
		`CREATE PROCEDURE app.p_late AS SELECT id FROM app.v_enc`,
		// Legacy bindings: the type's and the column's scripts call
		// sp_bindrule / sp_bindefault, so the rule and the default must be
		// created first. Left out, the replay failed on each binding.
		`CREATE RULE app.r_pos AS @v > 0`,
		`CREATE DEFAULT app.d_zero AS 0`,
		`CREATE TYPE app.qty FROM int NOT NULL`,
		`EXEC sp_bindrule N'app.r_pos', N'app.qty'`,
		`CREATE TABLE app.t (id INT NOT NULL CONSTRAINT pk_t PRIMARY KEY, n INT NULL, q app.qty)`,
		`EXEC sp_bindefault N'app.d_zero', N'app.t.n'`,
	)
	graph := src.atLeast(SQLServer2019)
	if graph {
		// By name the edge table sorts between its node tables, and the
		// catalog records no dependency from it to either.
		liveExecIn(t, src, ctx,
			`CREATE TABLE dbo.Zperson (id INT NOT NULL) AS NODE`,
			`CREATE TABLE dbo.Acity (id INT NOT NULL) AS NODE`,
			`CREATE TABLE dbo.AlivesIn (since INT NULL, CONSTRAINT EC_lives CONNECTION (dbo.Zperson TO dbo.Acity)) AS EDGE`,
		)
	}
	ledger := src.atLeast(SQLServer2022)
	if ledger {
		// An updatable and an append-only ledger table, each creating its
		// ledger view (and the updatable one its history table): scripted on
		// their own, the view failed the replay with Msg 2714 and the history
		// table was a "not scripted" note. A dropped one leaves a table, view
		// and history the script can only note.
		liveExecIn(t, src, ctx,
			`CREATE TABLE app.l_upd (id INT NOT NULL CONSTRAINT pk_l_upd PRIMARY KEY, v INT NULL)
			   WITH (SYSTEM_VERSIONING = ON (HISTORY_TABLE = app.l_upd_hist), LEDGER = ON (LEDGER_VIEW = app.l_upd_ledger))`,
			`CREATE TABLE app.l_app (id INT NOT NULL, v INT NULL) WITH (LEDGER = ON (APPEND_ONLY = ON))`,
			`CREATE TABLE app.l_gone (id INT NOT NULL CONSTRAINT pk_l_gone PRIMARY KEY)
			   WITH (SYSTEM_VERSIONING = ON, LEDGER = ON)`,
			`DROP TABLE app.l_gone`,
			// A GRANT on the dropped table and on its history table, which
			// the script skips as its parent's: both would fail Msg 15151.
			`DECLARE @q nvarchar(max)
			 SELECT @q = N'GRANT SELECT ON app.' + QUOTENAME(t.name) + N' TO gosmo_objs_r; GRANT SELECT ON app.' + QUOTENAME(h.name) + N' TO gosmo_objs_r'
			 FROM sys.tables t JOIN sys.tables h ON h.object_id = t.history_table_id
			 WHERE t.is_dropped_ledger_table = 1
			 EXEC (@q)`,
		)
	}

	var progress [][2]int
	srcScripter := NewScripter(src, DefaultScriptOptions())
	res, err := srcScripter.ScriptDatabaseObjectList(ctx, ObjectScriptOptions{IncludePermissions: true,
		Progress: func(done, total int) { progress = append(progress, [2]int{done, total}) }})
	if err != nil {
		t.Fatalf("ScriptDatabaseObjectList: %v", err)
	}
	if n := len(res.Objects); len(progress) != n+1 || progress[0] != [2]int{0, n} || progress[n] != [2]int{n, n} {
		t.Errorf("progress = %v, want 0..%d of %d", progress, n, n)
	}
	// The foreign keys render from the table prefetch; each must be what
	// ScriptForeignKey, reading the table and its keys itself, gives.
	fkProps := false
	for _, o := range res.Objects {
		if o.Kind != ScriptedForeignKey {
			continue
		}
		want, err := srcScripter.ScriptForeignKey(ctx, o.Schema, o.Table, o.Name)
		if err != nil {
			t.Fatalf("ScriptForeignKey %s: %v", o.Name, err)
		}
		if o.SQL != want {
			t.Errorf("foreign key %s:\n%s\nwant (ScriptForeignKey):\n%s", o.Name, o.SQL, want)
		}
		fkProps = fkProps || strings.Contains(o.SQL, "a to b")
	}
	if !fkProps {
		t.Error("fk_a_b's extended property is not scripted")
	}
	if graph {
		for _, o := range res.Objects {
			switch {
			case o.Kind == ScriptedTable && o.Name == "AlivesIn" && strings.Contains(o.SQL, "CONNECTION"):
				t.Errorf("the edge table's script holds its deferred edge constraint:\n%s", o.SQL)
			case o.Kind == ScriptedEdgeConstraint && o.Name == "EC_lives" &&
				!strings.Contains(o.SQL, "CONNECTION ([dbo].[Zperson] TO [dbo].[Acity])"):
				t.Errorf("EC_lives is scripted without its connection:\n%s", o.SQL)
			}
		}
	}
	index := map[string]int{}
	for i, o := range res.Objects {
		index[string(o.Kind)+" "+o.Schema+"."+o.Name] = i
	}
	before := func(a, b string) {
		t.Helper()
		ia, oka := index[a]
		ib, okb := index[b]
		if !oka || !okb || ia >= ib {
			t.Errorf("%q (%d, %v) should come before %q (%d, %v)", a, ia, oka, b, ib, okb)
		}
	}
	before("view app.v1", "view app.v2")
	before("view app.v2", "function app.fn_ids")
	before("function app.fn_ids", "view app.v0")
	before("function app.fn_default", "table app.a")
	before("type app.code", "table app.a")
	before("type app.code", "table type app.ids")
	before("table type app.ids", "procedure app.p_use")
	before("table app.a", "foreign key app.fk_a_b")
	before("table app.b", "foreign key app.fk_a_b")
	if graph {
		before("table dbo.Zperson", "edge constraint dbo.EC_lives")
		before("table dbo.AlivesIn", "edge constraint dbo.EC_lives")
		before("foreign key app.fk_a_b", "edge constraint dbo.EC_lives")
	}
	if len(res.Warnings) != 2 || !strings.Contains(res.Warnings[0], "p_one") ||
		res.Warnings[1] != "procedure [app].[p_late] references [app].[v_enc], which is not scripted" {
		t.Errorf("warnings = %q, want one broken p_one/p_two cycle and p_late's reference to v_enc", res.Warnings)
	}
	if _, ok := index["table app.temporal_hist"]; ok {
		t.Error("the history table is scripted on its own")
	}
	var droppedNotes, encNotes []string
	var perms string
	for _, o := range res.Objects {
		if o.Kind == ScriptedPermissions {
			perms = o.SQL
		}
		switch {
		case o.Note == "":
		case strings.HasPrefix(o.Name, "MSSQL_DroppedLedger"):
			droppedNotes = append(droppedNotes, string(o.Kind))
		case strings.HasSuffix(o.Name, "_enc"):
			encNotes = append(encNotes, string(o.Kind)+" "+o.Name)
		default:
			t.Errorf("%s %s.%s not scripted: %s", o.Kind, o.Schema, o.Name, o.Note)
		}
	}
	if slices.Sort(encNotes); !slices.Equal(encNotes, []string{"procedure p_enc", "trigger tr_enc", "view v_enc", "view v_on_enc"}) {
		t.Errorf("notes on the encrypted modules and what needs them = %v", encNotes)
	}
	for _, want := range []string{
		"-- not scripted: GRANT EXECUTE ON OBJECT::[app].[p_enc] TO [gosmo_objs_r]: [app].[p_enc] is not scripted",
		"-- not scripted: GRANT SELECT ON OBJECT::[app].[v_enc] TO [gosmo_objs_r]: [app].[v_enc] is not scripted",
	} {
		if !strings.Contains(perms, want) {
			t.Errorf("permissions lack %q:\n%s", want, perms)
		}
	}
	if ledger && strings.Count(perms, "-- not scripted: GRANT SELECT ON OBJECT::[app].[MSSQL_DroppedLedger") != 2 {
		t.Errorf("the GRANTs on the dropped ledger table and its history are not both comments:\n%s", perms)
	}
	if ledger {
		for _, k := range []string{"table app.l_upd_hist", "view app.l_upd_ledger", "view app.l_app_Ledger"} {
			if _, ok := index[k]; ok {
				t.Errorf("%s is scripted on its own; its ledger table creates it", k)
			}
		}
		if slices.Sort(droppedNotes); !slices.Equal(droppedNotes, []string{"table", "view"}) {
			t.Errorf("notes on the dropped ledger table's objects = %v, want its table and its view", droppedNotes)
		}
	}

	// Kinds keeping the schemas but not the principals block, which is what
	// creates them under IncludePermissions: they must still come out, bare.
	// Before, the IncludePermissions branch swallowed them and none did.
	only, err := srcScripter.ScriptDatabaseObjectList(ctx, ObjectScriptOptions{IncludePermissions: true,
		Kinds: []ScriptedKind{ScriptedSchema, ScriptedPermissions}})
	if err != nil {
		t.Fatalf("ScriptDatabaseObjectList (schemas and permissions): %v", err)
	}
	var kinds []string
	for _, o := range only.Objects {
		kinds = append(kinds, string(o.Kind)+" "+o.Name)
	}
	for _, k := range []string{"schema app", "schema a.b", "schema a", "permissions gosmo_objs_src_live"} {
		if !slices.Contains(kinds, k) {
			t.Errorf("Kinds {schema, permissions} with IncludePermissions: %q missing from %v", k, kinds)
		}
	}

	script := NewScripter(src, DefaultScriptOptions()).DatabaseContext() + res.String()
	script = strings.Replace(script, "USE [gosmo_objs_src_live]", "USE [gosmo_objs_dst_live]", 1)
	liveRunScript(t, dst, ctx, script)

	got, want := liveCatalogFingerprint(t, dst, ctx), liveCatalogFingerprint(t, src, ctx)
	// The dropped ledger table's objects are the ledger's record, which no
	// script recreates; the encrypted modules and what needs them are noted.
	want = slices.DeleteFunc(want, func(l string) bool {
		return strings.Contains(l, "MSSQL_DroppedLedger") || strings.Contains(l, "_enc]") || strings.HasSuffix(l, "_enc")
	})
	if len(want) < 40 {
		t.Fatalf("fingerprint of the source has only %d lines", len(want))
	}
	if !slices.Equal(got, want) {
		// Count, not Contains: a line present once where it should be twice
		// is a lost object.
		count := map[string]int{}
		for _, l := range want {
			count[l]++
		}
		for _, l := range got {
			count[l]--
		}
		for _, l := range want {
			if count[l] > 0 {
				t.Errorf("missing after replay: %s", l)
				count[l]--
			}
		}
		for _, l := range got {
			if count[l] < 0 {
				t.Errorf("extra after replay: %s", l)
				count[l]++
			}
		}
	}
}

// TestLiveScriptDatabaseObjectsCascadeAsRestrictedLogin scripts, as a login
// with SELECT on the schema but no VIEW DEFINITION, a table whose default it
// cannot read (refused, so noted) and another table with a foreign key to
// it. The key was scripted, and the replay failed Msg 1767; it must be
// noted, and the replay succeed. sys.sql_expression_dependencies refuses
// this login (Msg 229), so the order comes from sys.sql_dependencies.
func TestLiveScriptDatabaseObjectsCascadeAsRestrictedLogin(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 120*time.Second)
	t.Cleanup(done) // not defer: the drops below run in t.Cleanup

	const login = "gosmo_objs_restricted"
	const pass = "P@ssw0rd_gosmo_live"
	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_objs_rsrc_live")
	t.Cleanup(dropSrc)
	dst, dropDst := liveScratchDB(t, db, ctx, "gosmo_objs_rdst_live")
	t.Cleanup(dropDst)

	db.ExecContext(ctx, "IF SUSER_ID('"+login+"') IS NOT NULL DROP LOGIN ["+login+"]")
	if _, err := db.ExecContext(ctx, "CREATE LOGIN ["+login+"] WITH PASSWORD = '"+pass+"', CHECK_POLICY = OFF"); err != nil {
		t.Fatalf("create login: %v", err)
	}
	t.Cleanup(func() { db.ExecContext(context.Background(), "DROP LOGIN ["+login+"]") })
	// NewServer's loadInfo reads server-scope DMVs.
	if _, err := db.ExecContext(ctx, "GRANT VIEW SERVER STATE TO ["+login+"]"); err != nil {
		t.Fatalf("grant VIEW SERVER STATE: %v", err)
	}
	liveExecIn(t, src, ctx,
		`CREATE TABLE dbo.Q (id INT NOT NULL CONSTRAINT pk_q PRIMARY KEY, d INT NOT NULL CONSTRAINT df_q DEFAULT (1))`,
		`CREATE TABLE dbo.C (id INT NOT NULL CONSTRAINT pk_c PRIMARY KEY, q_id INT NULL CONSTRAINT FK_C_Q REFERENCES dbo.Q (id))`,
		`CREATE TRIGGER dbo.tr_q ON dbo.Q AFTER INSERT AS SET NOCOUNT ON`,
		`CREATE USER [`+login+`] FOR LOGIN [`+login+`]`,
		`GRANT SELECT ON SCHEMA::dbo TO [`+login+`]`,
	)

	pool, err := sql.Open("sqlserver", liveRestrictedDSN(t, login, pass))
	if err != nil {
		t.Fatalf("open as %s: %v", login, err)
	}
	t.Cleanup(func() { pool.Close() })
	srv, err := NewServer(ctx, pool)
	if err != nil {
		t.Fatalf("NewServer as %s: %v", login, err)
	}
	rsrc, err := srv.DatabaseByName(ctx, src.Name)
	if err != nil {
		t.Fatalf("DatabaseByName as %s: %v", login, err)
	}
	res, err := NewScripter(rsrc, DefaultScriptOptions()).ScriptDatabaseObjectList(ctx, ObjectScriptOptions{})
	if err != nil {
		t.Fatalf("ScriptDatabaseObjectList as %s: %v", login, err)
	}
	notes := map[string]string{}
	for _, o := range res.Objects {
		if o.Note != "" {
			notes[string(o.Kind)+" "+o.Name] = o.Note
		}
	}
	if notes["table Q"] == "" {
		t.Fatalf("table Q, whose default the login cannot read, is scripted; notes = %v", notes)
	}
	for k, want := range map[string]string{
		"foreign key FK_C_Q": "needs [dbo].[Q], which is not scripted",
		"trigger tr_q":       "needs [dbo].[Q], which is not scripted",
	} {
		if !strings.Contains(notes[k], want) {
			t.Errorf("%s note = %q, want %q", k, notes[k], want)
		}
	}
	if len(notes) != 3 {
		t.Errorf("notes = %v, want Q, FK_C_Q and tr_q", notes)
	}
	if !slices.Contains(res.Warnings, legacyDependencyWarning) {
		t.Errorf("warnings = %q, want the sys.sql_dependencies fallback named", res.Warnings)
	}

	script := NewScripter(src, DefaultScriptOptions()).DatabaseContext() + res.String()
	script = strings.Replace(script, "USE [gosmo_objs_rsrc_live]", "USE [gosmo_objs_rdst_live]", 1)
	liveRunScript(t, dst, ctx, script)
	if got := liveScalar(t, dst, ctx, `SELECT CAST(COUNT(*) AS varchar) FROM sys.tables WHERE name = 'C'`); got != "1" {
		t.Errorf("table C after replay: count %s, want 1", got)
	}
}

// TestLiveScriptDatabaseObjectsOrderWithoutDatabaseViewDefinition scripts
// dbo.vA over dbo.vZ and schema-bound dbo.fA() calling dbo.fZ() — name order
// is the wrong order for both — as two logins without the full
// sys.sql_expression_dependencies: db_datareader with VIEW DEFINITION on the
// schema (the view answers no rows) and VIEW DEFINITION on the database
// without db_datareader (it is refused, Msg 229). The first scripted fA, fZ,
// vA, vZ with no warning and the replay failed Msg 4121 and 208; the second
// failed outright. Both must order from sys.sql_dependencies, say so in
// Warnings, and replay.
func TestLiveScriptDatabaseObjectsOrderWithoutDatabaseViewDefinition(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 120*time.Second)
	t.Cleanup(done) // not defer: the drops below run in t.Cleanup

	const pass = "P@ssw0rd_gosmo_live"
	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_objs_vdsrc_live")
	t.Cleanup(dropSrc)
	liveExecIn(t, src, ctx,
		`CREATE TABLE dbo.T (id INT NOT NULL CONSTRAINT pk_t PRIMARY KEY)`,
		`CREATE VIEW dbo.vZ AS SELECT id FROM dbo.T`,
		`CREATE VIEW dbo.vA AS SELECT id FROM dbo.vZ`,
		`CREATE FUNCTION dbo.fZ() RETURNS INT WITH SCHEMABINDING AS BEGIN RETURN 1 END`,
		`CREATE FUNCTION dbo.fA() RETURNS INT WITH SCHEMABINDING AS BEGIN RETURN dbo.fZ() END`,
	)
	if res, err := NewScripter(src, DefaultScriptOptions()).ScriptDatabaseObjectList(ctx, ObjectScriptOptions{}); err != nil {
		t.Fatalf("ScriptDatabaseObjectList as the test login: %v", err)
	} else if slices.Contains(res.Warnings, legacyDependencyWarning) {
		t.Errorf("the test login's script fell back: warnings = %q", res.Warnings)
	}

	for _, c := range []struct {
		login, dst string
		grants     []string
	}{
		{"gosmo_objs_vd_schema", "gosmo_objs_vddst1_live", []string{
			`ALTER ROLE db_datareader ADD MEMBER [gosmo_objs_vd_schema]`,
			`GRANT VIEW DEFINITION ON SCHEMA::dbo TO [gosmo_objs_vd_schema]`}},
		{"gosmo_objs_vd_db", "gosmo_objs_vddst2_live", []string{
			`GRANT VIEW DEFINITION TO [gosmo_objs_vd_db]`}},
	} {
		t.Run(c.login, func(t *testing.T) {
			db.ExecContext(ctx, "IF SUSER_ID('"+c.login+"') IS NOT NULL DROP LOGIN ["+c.login+"]")
			if _, err := db.ExecContext(ctx, "CREATE LOGIN ["+c.login+"] WITH PASSWORD = '"+pass+"', CHECK_POLICY = OFF"); err != nil {
				t.Fatalf("create login: %v", err)
			}
			t.Cleanup(func() { db.ExecContext(context.Background(), "DROP LOGIN ["+c.login+"]") })
			// NewServer's loadInfo reads server-scope DMVs.
			if _, err := db.ExecContext(ctx, "GRANT VIEW SERVER STATE TO ["+c.login+"]"); err != nil {
				t.Fatalf("grant VIEW SERVER STATE: %v", err)
			}
			liveExecIn(t, src, ctx, append([]string{`CREATE USER [` + c.login + `] FOR LOGIN [` + c.login + `]`}, c.grants...)...)
			t.Cleanup(func() { src.exec(context.Background(), "DROP USER ["+c.login+"]") })

			pool, err := sql.Open("sqlserver", liveRestrictedDSN(t, c.login, pass))
			if err != nil {
				t.Fatalf("open as %s: %v", c.login, err)
			}
			t.Cleanup(func() { pool.Close() })
			srv, err := NewServer(ctx, pool)
			if err != nil {
				t.Fatalf("NewServer as %s: %v", c.login, err)
			}
			rsrc, err := srv.DatabaseByName(ctx, src.Name)
			if err != nil {
				t.Fatalf("DatabaseByName as %s: %v", c.login, err)
			}
			res, err := NewScripter(rsrc, DefaultScriptOptions()).ScriptDatabaseObjectList(ctx, ObjectScriptOptions{})
			if err != nil {
				t.Fatalf("ScriptDatabaseObjectList as %s: %v", c.login, err)
			}
			if !slices.Contains(res.Warnings, legacyDependencyWarning) {
				t.Errorf("warnings = %q, want the sys.sql_dependencies fallback named", res.Warnings)
			}
			pos := map[string]int{}
			for i, o := range res.Objects {
				if o.Note != "" {
					t.Errorf("%s %s not scripted: %s", o.Kind, o.Name, o.Note)
				}
				pos[o.Name] = i
			}
			if pos["vZ"] > pos["vA"] || pos["fZ"] > pos["fA"] {
				t.Errorf("order = %v, want vZ before vA and fZ before fA", pos)
			}

			dst, dropDst := liveScratchDB(t, db, ctx, c.dst)
			t.Cleanup(dropDst)
			script := NewScripter(src, DefaultScriptOptions()).DatabaseContext() + res.String()
			script = strings.Replace(script, "USE ["+src.Name+"]", "USE ["+c.dst+"]", 1)
			liveRunScript(t, dst, ctx, script)
			if got := liveScalar(t, dst, ctx, `SELECT CAST(COUNT(*) AS varchar) FROM sys.objects WHERE name IN ('T', 'vZ', 'vA', 'fZ', 'fA')`); got != "5" {
				t.Errorf("objects after replay: %s, want 5", got)
			}
		})
	}
}

// liveCatalogFingerprint lists the database's user objects, types, schemas,
// foreign keys, edge constraints (2019+), indexes, module definitions,
// grants, and the rules and defaults bound to columns and types as sorted
// lines. Names are QUOTENAMEd: joined bare, [a.b].[c] and [a].[b.c] print
// alike. A graph table's GRAPH_UNIQUE_INDEX_<guid> is named
// by the server afresh on every CREATE TABLE, so its suffix is dropped.
func liveCatalogFingerprint(t *testing.T, d *Database, ctx context.Context) []string {
	t.Helper()
	rows, err := d.query(ctx, `
SELECT 'object ' + o.type COLLATE DATABASE_DEFAULT + ' ' + QUOTENAME(SCHEMA_NAME(o.schema_id)) + '.' + QUOTENAME(o.name) COLLATE DATABASE_DEFAULT + ' ' + COALESCE(CONVERT(varchar(64), HASHBYTES('SHA1', m.definition), 2), '')
FROM sys.objects o LEFT JOIN sys.sql_modules m ON m.object_id = o.object_id
WHERE o.is_ms_shipped = 0 AND o.type COLLATE DATABASE_DEFAULT NOT IN ('TT', 'IT', 'S')
UNION ALL SELECT 'type ' + QUOTENAME(SCHEMA_NAME(schema_id)) + '.' + QUOTENAME(name) COLLATE DATABASE_DEFAULT + ' ' + CAST(system_type_id AS varchar) + ' ' + CAST(max_length AS varchar) + ' ' + CAST(is_nullable AS varchar)
FROM sys.types WHERE is_user_defined = 1
UNION ALL SELECT 'xml ' + QUOTENAME(SCHEMA_NAME(schema_id)) + '.' + QUOTENAME(name) COLLATE DATABASE_DEFAULT FROM sys.xml_schema_collections WHERE schema_id <> 4
UNION ALL SELECT 'schema ' + QUOTENAME(name) COLLATE DATABASE_DEFAULT FROM sys.schemas WHERE schema_id > 4 AND schema_id < 16384
UNION ALL SELECT 'index ' + QUOTENAME(SCHEMA_NAME(o.schema_id)) + '.' + QUOTENAME(o.name) + '.' + COALESCE(QUOTENAME(
       CASE WHEN i.name LIKE 'GRAPH[_]UNIQUE[_]INDEX[_]%' THEN 'GRAPH_UNIQUE_INDEX' ELSE i.name END), '') + ' ' + i.type_desc COLLATE DATABASE_DEFAULT + ' ' + COALESCE(ds.name COLLATE DATABASE_DEFAULT, '')
FROM sys.indexes i JOIN sys.objects o ON o.object_id = i.object_id LEFT JOIN sys.data_spaces ds ON ds.data_space_id = i.data_space_id
WHERE o.is_ms_shipped = 0 AND o.type COLLATE DATABASE_DEFAULT IN ('U', 'V')
UNION ALL SELECT 'fk ' + fk.name COLLATE DATABASE_DEFAULT + ' ' + CAST(fk.is_disabled AS varchar) + ' ' + CAST(fk.is_not_trusted AS varchar) FROM sys.foreign_keys fk
UNION ALL SELECT 'perm ' + pr.name COLLATE DATABASE_DEFAULT + ' ' + p.permission_name COLLATE DATABASE_DEFAULT + ' ' + p.state_desc COLLATE DATABASE_DEFAULT + ' ' + COALESCE(OBJECT_NAME(p.major_id), '')
FROM sys.database_permissions p JOIN sys.database_principals pr ON pr.principal_id = p.grantee_principal_id
WHERE pr.name COLLATE DATABASE_DEFAULT LIKE 'gosmo_objs%'
UNION ALL SELECT 'member ' + r.name COLLATE DATABASE_DEFAULT + ' ' + m.name COLLATE DATABASE_DEFAULT FROM sys.database_role_members rm
JOIN sys.database_principals r ON r.principal_id = rm.role_principal_id JOIN sys.database_principals m ON m.principal_id = rm.member_principal_id
WHERE r.name COLLATE DATABASE_DEFAULT LIKE 'gosmo_objs%'
UNION ALL SELECT 'bind column ' + QUOTENAME(SCHEMA_NAME(o.schema_id)) + '.' + QUOTENAME(o.name) + '.' + QUOTENAME(col.name) COLLATE DATABASE_DEFAULT
       + ' ' + QUOTENAME(SCHEMA_NAME(r.schema_id)) + '.' + QUOTENAME(r.name) COLLATE DATABASE_DEFAULT
FROM sys.columns col JOIN sys.objects o ON o.object_id = col.object_id
JOIN sys.objects r ON r.object_id IN (col.rule_object_id, col.default_object_id) AND r.parent_object_id = 0
WHERE o.is_ms_shipped = 0
UNION ALL SELECT 'bind type ' + QUOTENAME(SCHEMA_NAME(t.schema_id)) + '.' + QUOTENAME(t.name) COLLATE DATABASE_DEFAULT
       + ' ' + QUOTENAME(SCHEMA_NAME(r.schema_id)) + '.' + QUOTENAME(r.name) COLLATE DATABASE_DEFAULT
FROM sys.types t JOIN sys.objects r ON r.object_id IN (t.rule_object_id, t.default_object_id) AND r.parent_object_id = 0
WHERE t.is_user_defined = 1`)
	out, err := scanRows(rows, err, "fingerprint", func(scan func(...any) error) (string, error) {
		var s string
		err := scan(&s)
		return s, err
	})
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if d.atLeast(SQLServer2019) {
		rows, err := d.query(ctx, `
SELECT 'ec ' + QUOTENAME(OBJECT_SCHEMA_NAME(ec.parent_object_id)) + '.' + QUOTENAME(OBJECT_NAME(ec.parent_object_id)) + '.' + QUOTENAME(ec.name) COLLATE DATABASE_DEFAULT
       + ' ' + CAST(ecc.clause_number AS varchar) + ' ' + QUOTENAME(OBJECT_NAME(ecc.from_object_id)) + ' TO ' + QUOTENAME(OBJECT_NAME(ecc.to_object_id))
       + ' ' + ec.delete_referential_action_desc COLLATE DATABASE_DEFAULT + ' ' + CAST(ec.is_disabled AS varchar) + ' ' + CAST(ec.is_not_trusted AS varchar)
FROM sys.edge_constraints ec JOIN sys.edge_constraint_clauses ecc ON ecc.object_id = ec.object_id`)
		ecs, err := scanRows(rows, err, "fingerprint edge constraints", func(scan func(...any) error) (string, error) {
			var s string
			err := scan(&s)
			return s, err
		})
		if err != nil {
			t.Fatalf("fingerprint: %v", err)
		}
		out = append(out, ecs...)
	}
	slices.Sort(out)
	return out
}
