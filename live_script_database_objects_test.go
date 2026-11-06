//go:build livedb

package gosmo

import (
	"context"
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
// succeeded) and, on 2022+, ledger tables (their ledger view failed the
// replay) — replays the script into an empty database, and compares the
// two catalogs.
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
	)
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
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "p_one") {
		t.Errorf("warnings = %v, want one broken p_one/p_two cycle", res.Warnings)
	}
	if _, ok := index["table app.temporal_hist"]; ok {
		t.Error("the history table is scripted on its own")
	}
	var droppedNotes []string
	for _, o := range res.Objects {
		switch {
		case o.Note == "":
		case strings.HasPrefix(o.Name, "MSSQL_DroppedLedger"):
			droppedNotes = append(droppedNotes, string(o.Kind))
		default:
			t.Errorf("%s %s.%s not scripted: %s", o.Kind, o.Schema, o.Name, o.Note)
		}
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
	// script recreates.
	want = slices.DeleteFunc(want, func(l string) bool { return strings.Contains(l, "MSSQL_DroppedLedger") })
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

// liveCatalogFingerprint lists the database's user objects, types, schemas,
// foreign keys, indexes, module definitions and grants as sorted lines. Names
// are QUOTENAMEd: joined bare, [a.b].[c] and [a].[b.c] print alike.
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
UNION ALL SELECT 'index ' + QUOTENAME(SCHEMA_NAME(o.schema_id)) + '.' + QUOTENAME(o.name) + '.' + COALESCE(QUOTENAME(i.name), '') + ' ' + i.type_desc COLLATE DATABASE_DEFAULT + ' ' + COALESCE(ds.name COLLATE DATABASE_DEFAULT, '')
FROM sys.indexes i JOIN sys.objects o ON o.object_id = i.object_id LEFT JOIN sys.data_spaces ds ON ds.data_space_id = i.data_space_id
WHERE o.is_ms_shipped = 0 AND o.type COLLATE DATABASE_DEFAULT IN ('U', 'V')
UNION ALL SELECT 'fk ' + fk.name COLLATE DATABASE_DEFAULT + ' ' + CAST(fk.is_disabled AS varchar) + ' ' + CAST(fk.is_not_trusted AS varchar) FROM sys.foreign_keys fk
UNION ALL SELECT 'perm ' + pr.name COLLATE DATABASE_DEFAULT + ' ' + p.permission_name COLLATE DATABASE_DEFAULT + ' ' + p.state_desc COLLATE DATABASE_DEFAULT + ' ' + COALESCE(OBJECT_NAME(p.major_id), '')
FROM sys.database_permissions p JOIN sys.database_principals pr ON pr.principal_id = p.grantee_principal_id
WHERE pr.name COLLATE DATABASE_DEFAULT LIKE 'gosmo_objs%'
UNION ALL SELECT 'member ' + r.name COLLATE DATABASE_DEFAULT + ' ' + m.name COLLATE DATABASE_DEFAULT FROM sys.database_role_members rm
JOIN sys.database_principals r ON r.principal_id = rm.role_principal_id JOIN sys.database_principals m ON m.principal_id = rm.member_principal_id
WHERE r.name COLLATE DATABASE_DEFAULT LIKE 'gosmo_objs%'`)
	out, err := scanRows(rows, err, "fingerprint", func(scan func(...any) error) (string, error) {
		var s string
		err := scan(&s)
		return s, err
	})
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	slices.Sort(out)
	return out
}
