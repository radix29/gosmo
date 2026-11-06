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
// collection — replays the script into an empty database, and compares the
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
		`GRANT SELECT ON app.v0 TO gosmo_objs_r`,
	)

	res, err := NewScripter(src, DefaultScriptOptions()).ScriptDatabaseObjectList(ctx, ObjectScriptOptions{IncludePermissions: true})
	if err != nil {
		t.Fatalf("ScriptDatabaseObjectList: %v", err)
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

	script := NewScripter(src, DefaultScriptOptions()).DatabaseContext() + res.String()
	script = strings.Replace(script, "USE [gosmo_objs_src_live]", "USE [gosmo_objs_dst_live]", 1)
	liveRunScript(t, dst, ctx, script)

	got, want := liveCatalogFingerprint(t, dst, ctx), liveCatalogFingerprint(t, src, ctx)
	if len(want) < 40 {
		t.Fatalf("fingerprint of the source has only %d lines", len(want))
	}
	if !slices.Equal(got, want) {
		for _, l := range want {
			if !slices.Contains(got, l) {
				t.Errorf("missing after replay: %s", l)
			}
		}
		for _, l := range got {
			if !slices.Contains(want, l) {
				t.Errorf("extra after replay: %s", l)
			}
		}
	}
}

// liveCatalogFingerprint lists the database's user objects, types, schemas,
// foreign keys, indexes, module definitions and grants as sorted lines.
func liveCatalogFingerprint(t *testing.T, d *Database, ctx context.Context) []string {
	t.Helper()
	rows, err := d.query(ctx, `
SELECT 'object ' + o.type COLLATE DATABASE_DEFAULT + ' ' + SCHEMA_NAME(o.schema_id) + '.' + o.name COLLATE DATABASE_DEFAULT + ' ' + COALESCE(CONVERT(varchar(64), HASHBYTES('SHA1', m.definition), 2), '')
FROM sys.objects o LEFT JOIN sys.sql_modules m ON m.object_id = o.object_id
WHERE o.is_ms_shipped = 0 AND o.type COLLATE DATABASE_DEFAULT NOT IN ('TT', 'IT', 'S')
UNION ALL SELECT 'type ' + SCHEMA_NAME(schema_id) + '.' + name COLLATE DATABASE_DEFAULT + ' ' + CAST(system_type_id AS varchar) + ' ' + CAST(max_length AS varchar) + ' ' + CAST(is_nullable AS varchar)
FROM sys.types WHERE is_user_defined = 1
UNION ALL SELECT 'xml ' + SCHEMA_NAME(schema_id) + '.' + name COLLATE DATABASE_DEFAULT FROM sys.xml_schema_collections WHERE schema_id <> 4
UNION ALL SELECT 'schema ' + name COLLATE DATABASE_DEFAULT FROM sys.schemas WHERE schema_id > 4 AND schema_id < 16384
UNION ALL SELECT 'index ' + OBJECT_NAME(i.object_id) + '.' + COALESCE(i.name, '') + ' ' + i.type_desc COLLATE DATABASE_DEFAULT + ' ' + COALESCE(ds.name COLLATE DATABASE_DEFAULT, '')
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
