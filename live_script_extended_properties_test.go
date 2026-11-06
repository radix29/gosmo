//go:build livedb

// Live round trips of what a table or module script carries beyond the
// object itself: an indexed view's indexes, and the extended properties of
// the object, its columns, parameters, indexes and constraints, each value
// with its own base type; of a database's and a schema's own properties; and
// of every other securable's a single-object script restates them for.
//
//	go test -tags livedb . -run TestLiveScript.*ExtendedProperties -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops throwaway databases; touches nothing else.
package gosmo

import (
	"context"
	"encoding/hex"
	"os"
	"slices"
	"strings"
	"testing"
)

// liveExtPropsOracle lists every extended property in the database with its
// value's base type, precision, scale, length and bytes, keyed by names
// rather than ids — an oracle written apart from objectExtendedProperties.
const liveExtPropsOracle = `
SELECT ep.class, ISNULL(OBJECT_SCHEMA_NAME(ep.major_id), '') AS sch, ISNULL(OBJECT_NAME(ep.major_id), '') AS obj,
       CASE ep.class WHEN 1 THEN ISNULL(COL_NAME(ep.major_id, ep.minor_id), '')
                     WHEN 2 THEN ISNULL((SELECT p.name FROM sys.parameters p WHERE p.object_id = ep.major_id AND p.parameter_id = ep.minor_id), '')
                     WHEN 7 THEN ISNULL((SELECT i.name FROM sys.indexes i WHERE i.object_id = ep.major_id AND i.index_id = ep.minor_id), '')
       END AS sub,
       ep.name,
       ISNULL(CONVERT(sysname, SQL_VARIANT_PROPERTY(ep.value, 'BaseType')), '') AS base,
       ISNULL(CONVERT(int, SQL_VARIANT_PROPERTY(ep.value, 'Precision')), -1) AS prec,
       ISNULL(CONVERT(int, SQL_VARIANT_PROPERTY(ep.value, 'Scale')), -1) AS scale,
       ISNULL(CONVERT(int, SQL_VARIANT_PROPERTY(ep.value, 'MaxLength')), -1) AS len,
       ISNULL(CONVERT(varchar(max), CONVERT(varbinary(max), ep.value), 1), 'NULL') AS bytes
FROM   sys.extended_properties ep
WHERE  ep.class IN (1, 2, 7)`

// liveViewIndexesOracle is every index on a view, with its key columns.
const liveViewIndexesOracle = `
SELECT OBJECT_NAME(i.object_id) AS v, i.name, i.type_desc, i.is_unique, i.is_disabled,
       STUFF((SELECT ',' + COL_NAME(ic.object_id, ic.column_id) FROM sys.index_columns ic
              WHERE ic.object_id = i.object_id AND ic.index_id = i.index_id
              ORDER BY ic.key_ordinal FOR XML PATH('')), 1, 1, '') AS cols
FROM   sys.indexes i JOIN sys.views v ON v.object_id = i.object_id
WHERE  i.type > 0`

func TestLiveScriptIndexedViewAndExtendedProperties(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_extprop_src")
	defer dropSrc()
	dst, dropDst := liveScratchDB(t, db, ctx, "gosmo_extprop_dst")
	defer dropDst()

	add := func(name, value, l1type, l1name, l2type, l2name string) string {
		l2 := ""
		if l2type != "" {
			l2 = ", @level2type = N'" + l2type + "', @level2name = N'" + l2name + "'"
		}
		return "DECLARE @v sql_variant = " + value + "; EXEC sys.sp_addextendedproperty @name = N'" + name +
			"', @value = @v, @level0type = N'SCHEMA', @level0name = N'dbo', @level1type = N'" + l1type +
			"', @level1name = N'" + l1name + "'" + l2
	}
	liveExecIn(t, src, ctx,
		`CREATE TABLE dbo.B (ID int NOT NULL CONSTRAINT PK_B PRIMARY KEY, Cat int NOT NULL,
		    Amt money NULL CONSTRAINT DF_B_Amt DEFAULT 0, CONSTRAINT CK_B_Cat CHECK (Cat > 0))`,
		`CREATE INDEX IX_B_Cat ON dbo.B (Cat)`,
		`CREATE VIEW dbo.vB WITH SCHEMABINDING AS SELECT Cat, COUNT_BIG(*) AS n, SUM(ISNULL(Amt, 0)) AS total FROM dbo.B GROUP BY Cat`,
		`CREATE UNIQUE CLUSTERED INDEX CIX_vB ON dbo.vB (Cat)`,
		`CREATE INDEX IX_vB_n ON dbo.vB (n)`,
		`CREATE PROCEDURE dbo.pB @cat int, @label nvarchar(20) = NULL AS SELECT * FROM dbo.B WHERE Cat = @cat`,
		`CREATE FUNCTION dbo.fB (@x int) RETURNS int AS BEGIN RETURN @x + 1 END`,
		`CREATE TRIGGER dbo.trB ON dbo.B AFTER INSERT AS SET NOCOUNT ON`,
		add("MS_Description", "N'The B table, it''s here'", "TABLE", "B", "", ""),
		add("MS_Description", "N'Category'", "TABLE", "B", "COLUMN", "Cat"),
		add("Version", "CAST(42 AS int)", "TABLE", "B", "", ""),
		add("Ratio", "CAST(0.1 AS float)", "TABLE", "B", "", ""),
		add("Small", "CAST(0.1 AS real)", "TABLE", "B", "", ""),
		add("Price", "CAST(12.3456 AS money)", "TABLE", "B", "COLUMN", "Amt"),
		add("Exact", "CAST(12.345 AS decimal(9,4))", "TABLE", "B", "COLUMN", "ID"),
		add("Stamp", "CAST('2026-10-13T12:34:56.997' AS datetime)", "TABLE", "B", "", ""),
		add("Stamp2", "CAST('2026-10-13 12:34:56.1234567' AS datetime2(7))", "TABLE", "B", "", ""),
		add("Offset", "CAST('2026-10-13 12:34:56.12 +02:00' AS datetimeoffset(2))", "TABLE", "B", "", ""),
		add("Day", "CAST('2026-10-13' AS date)", "TABLE", "B", "", ""),
		add("Clock", "CAST('12:34:56.1234567' AS time(7))", "TABLE", "B", "", ""),
		add("Short", "CAST('2026-10-13T12:34:00' AS smalldatetime)", "TABLE", "B", "", ""),
		add("Cents", "CAST(-3.5 AS smallmoney)", "TABLE", "B", "", ""),
		add("Num", "CAST(-7 AS numeric(5,0))", "TABLE", "B", "", ""),
		add("Char", "CAST('ab' AS char(4))", "TABLE", "B", "", ""),
		add("Bin", "CAST(0x01 AS binary(3))", "TABLE", "B", "", ""),
		add("Big", "CAST(-9223372036854775808 AS bigint)", "TABLE", "B", "", ""),
		add("Tiny", "CAST(255 AS tinyint)", "TABLE", "B", "", ""),
		add("Unicode", "N'Ünïcödé ✓ it''s'", "TABLE", "B", "", ""),
		add("Blob", "CAST(0x0A0BFF AS varbinary(10))", "TABLE", "B", "", ""),
		add("Ansi", "CAST('ascii' AS varchar(30))", "TABLE", "B", "", ""),
		add("Fixed", "CAST(N'fix' AS nchar(5))", "TABLE", "B", "", ""),
		add("Guid", "CAST('6F9619FF-8B86-D011-B42D-00C04FC964FF' AS uniqueidentifier)", "TABLE", "B", "", ""),
		add("Flag", "CAST(1 AS bit)", "TABLE", "B", "", ""),
		add("Empty", "NULL", "TABLE", "B", "", ""),
		add("MS_Description", "N'pk'", "TABLE", "B", "CONSTRAINT", "PK_B"),
		add("MS_Description", "N'default'", "TABLE", "B", "CONSTRAINT", "DF_B_Amt"),
		add("MS_Description", "N'check'", "TABLE", "B", "CONSTRAINT", "CK_B_Cat"),
		add("MS_Description", "N'index'", "TABLE", "B", "INDEX", "IX_B_Cat"),
		add("MS_Description", "N'view'", "VIEW", "vB", "", ""),
		add("MS_Description", "N'view column'", "VIEW", "vB", "COLUMN", "total"),
		add("MS_Description", "N'view clustered'", "VIEW", "vB", "INDEX", "CIX_vB"),
		add("MS_Description", "N'view nonclustered'", "VIEW", "vB", "INDEX", "IX_vB_n"),
		add("MS_Description", "N'proc'", "PROCEDURE", "pB", "", ""),
		add("MS_Description", "N'param'", "PROCEDURE", "pB", "PARAMETER", "@label"),
		add("MS_Description", "N'func'", "FUNCTION", "fB", "", ""),
		add("MS_Description", "N'func param'", "FUNCTION", "fB", "PARAMETER", "@x"),
		add("MS_Description", "N'trigger'", "TABLE", "B", "TRIGGER", "trB"),
	)

	sc := NewScripter(src, DefaultScriptOptions())
	scriptAll := func(sc *Scripter) []string {
		t.Helper()
		var out []string
		for _, f := range []func() (string, error){
			func() (string, error) { return sc.ScriptTable(ctx, "dbo", "B") },
			func() (string, error) { return sc.ScriptView(ctx, "dbo", "vB") },
			func() (string, error) { return sc.ScriptStoredProcedure(ctx, "dbo", "pB") },
			func() (string, error) { return sc.ScriptFunction(ctx, "dbo", "fB") },
			func() (string, error) { return sc.ScriptTrigger(ctx, "dbo", "trB") },
		} {
			s, err := f()
			if err != nil {
				t.Fatalf("script: %v", err)
			}
			out = append(out, s)
		}
		return out
	}
	compare := func(what string) {
		t.Helper()
		for _, q := range []string{liveExtPropsOracle, liveViewIndexesOracle} {
			want := liveRowsAsStrings(t, src, ctx, q)
			got := liveRowsAsStrings(t, dst, ctx, q)
			if len(want) == 0 {
				t.Fatalf("%s: the source has nothing to compare", what)
			}
			for _, w := range want {
				if !slices.Contains(got, w) {
					t.Errorf("%s: missing or different after the round trip:\n  %s", what, w)
				}
			}
			for _, g := range got {
				if !slices.Contains(want, g) {
					t.Errorf("%s: not in the source:\n  %s", what, g)
				}
			}
		}
	}

	creates := scriptAll(sc)
	for _, s := range creates {
		livePinnedRun(t, db, ctx, dst.Name, s)
	}
	compare("CREATE")

	// ALTER VIEW drops the view's indexes and with them their properties;
	// the script puts both back, and its guards skip what ALTER kept.
	alterOpts := DefaultScriptOptions()
	alterOpts.Verb = ScriptAlter
	alterOpts.IncludeIfNotExists = false
	for i, s := range scriptAll(NewScripter(src, alterOpts)) {
		if i == 0 {
			continue // a table has no ALTER form; its script is the CREATE
		}
		livePinnedRun(t, db, ctx, dst.Name, s)
	}
	compare("ALTER")

	// DROP AND CREATE over the copy, guards on. The view first, alone: its
	// indexes and their properties go with the DROP and come back. Then the
	// table, which the schema-bound view blocks until it is dropped, and
	// whose DROP takes the trigger along.
	dc := DefaultScriptOptions()
	dc.Verb = ScriptDropAndCreate
	scripts := scriptAll(NewScripter(src, dc))
	livePinnedRun(t, db, ctx, dst.Name, scripts[1])
	compare("DROP AND CREATE of the view")
	drop := DefaultScriptOptions()
	drop.Verb = ScriptDrop
	dropView, err := NewScripter(src, drop).ScriptView(ctx, "dbo", "vB")
	if err != nil {
		t.Fatal(err)
	}
	livePinnedRun(t, db, ctx, dst.Name, dropView)
	livePinnedRun(t, db, ctx, dst.Name, scripts[0])
	livePinnedRun(t, db, ctx, dst.Name, creates[4])
	livePinnedRun(t, db, ctx, dst.Name, creates[1])
	compare("DROP AND CREATE")

	// Off, nothing is restated.
	off := DefaultScriptOptions()
	off.ExtendedProperties = false
	for _, s := range scriptAll(NewScripter(src, off)) {
		if strings.Contains(s, "sp_addextendedproperty") {
			t.Errorf("ExtendedProperties off, but:\n%s", s)
		}
	}
}

// liveDBSchemaExtPropsOracle is liveExtPropsOracle for the database's own
// properties (class 0) and its schemas' (class 3).
const liveDBSchemaExtPropsOracle = `
SELECT ep.class,
       CASE ep.class WHEN 3  THEN ISNULL(SCHEMA_NAME(ep.major_id), '?')
                     WHEN 20 THEN ISNULL((SELECT fg.name FROM sys.filegroups fg WHERE fg.data_space_id = ep.major_id), '?')
                     WHEN 22 THEN ISNULL((SELECT FILEGROUP_NAME(f.data_space_id) + '.' + f.name FROM sys.database_files f
                                          WHERE f.file_id = ep.major_id), '?')
                     ELSE '' END AS target, ep.minor_id,
       ep.name,
       ISNULL(CONVERT(sysname, SQL_VARIANT_PROPERTY(ep.value, 'BaseType')), '') AS base,
       ISNULL(CONVERT(int, SQL_VARIANT_PROPERTY(ep.value, 'Precision')), -1) AS prec,
       ISNULL(CONVERT(int, SQL_VARIANT_PROPERTY(ep.value, 'Scale')), -1) AS scale,
       ISNULL(CONVERT(int, SQL_VARIANT_PROPERTY(ep.value, 'MaxLength')), -1) AS len,
       ISNULL(CONVERT(varchar(max), CONVERT(varbinary(max), ep.value), 1), 'NULL') AS bytes
FROM   sys.extended_properties ep
WHERE  ep.class IN (0, 3, 20, 22)`

// A database's, its filegroups', its files' and a schema's own properties
// round-trip through ScriptDatabase and ScriptSchema — a read-only
// filegroup's and an empty one's among them. The source is READ_ONLY, so the replay
// fails unless the database's properties are added before its READ_ONLY.
func TestLiveScriptDatabaseAndSchemaExtendedProperties(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	const name, copyName = "gosmo_extprop_db", "gosmo_extprop_db_copy"
	dropDBs := func() {
		c := context.Background()
		for _, n := range []string{copyName, name} {
			db.ExecContext(c, "IF DB_ID('"+n+"') IS NOT NULL ALTER DATABASE ["+n+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE")
			db.ExecContext(c, "IF DB_ID('"+n+"') IS NOT NULL DROP DATABASE ["+n+"]")
		}
	}
	dropDBs()
	t.Cleanup(dropDBs)
	src, _ := liveScratchDB(t, db, ctx, name)
	var dataPath string
	if err := db.QueryRowContext(ctx, `SELECT CONVERT(nvarchar(260), SERVERPROPERTY('InstanceDefaultDataPath'))`).Scan(&dataPath); err != nil {
		t.Fatal(err)
	}
	// The file names carry the database's, so the replay's rename moves
	// them too.
	liveExecIn(t, src, ctx,
		`ALTER DATABASE [`+name+`] ADD FILEGROUP [fg'2]`,
		`ALTER DATABASE [`+name+`] ADD FILE (NAME = N'`+name+`_f2', FILENAME = N'`+dataPath+name+`_f2.ndf') TO FILEGROUP [fg'2]`,
		`ALTER DATABASE [`+name+`] ADD FILEGROUP fg3`,
		`EXEC sys.sp_addextendedproperty @name = N'MS_Description', @value = N'primary', @level0type = N'FILEGROUP', @level0name = N'PRIMARY'`,
		`EXEC sys.sp_addextendedproperty @name = N'MS_Description', @value = N'primary file', @level0type = N'FILEGROUP', @level0name = N'PRIMARY',
		     @level1type = N'LOGICAL FILE NAME', @level1name = N'`+name+`'`,
		`EXEC sys.sp_addextendedproperty @name = N'MS_Description', @value = N'fg2', @level0type = N'FILEGROUP', @level0name = N'fg''2'`,
		`DECLARE @v sql_variant = CAST(7 AS int);
		 EXEC sys.sp_addextendedproperty @name = N'Disk', @value = @v, @level0type = N'FILEGROUP', @level0name = N'fg''2',
		     @level1type = N'LOGICAL FILE NAME', @level1name = N'`+name+`_f2'`,
		`EXEC sys.sp_addextendedproperty @name = N'MS_Description', @value = N'empty', @level0type = N'FILEGROUP', @level0name = N'fg3'`,
		`ALTER DATABASE [`+name+`] MODIFY FILEGROUP [fg'2] READ_ONLY`,
	)
	liveExecIn(t, src, ctx,
		`CREATE SCHEMA [Sa'les]`,
		`CREATE SCHEMA bare`,
		`EXEC sys.sp_addextendedproperty @name = N'MS_Description', @value = N'The database, it''s here'`,
		`DECLARE @v sql_variant = CAST(3 AS int); EXEC sys.sp_addextendedproperty @name = N'Version', @value = @v`,
		`EXEC sys.sp_addextendedproperty @name = N'Empty', @value = NULL`,
		`EXEC sys.sp_addextendedproperty @name = N'MS_Description', @value = N'sales', @level0type = N'SCHEMA', @level0name = N'Sa''les'`,
		`DECLARE @v sql_variant = CAST('2026-10-14T01:02:03.997' AS datetime);
		 EXEC sys.sp_addextendedproperty @name = N'Since', @value = @v, @level0type = N'SCHEMA', @level0name = N'Sa''les'`,
	)
	if _, err := db.ExecContext(ctx, "ALTER DATABASE ["+name+"] SET READ_ONLY"); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	opts := DefaultScriptOptions()
	opts.IncludeHeaders = false
	script, err := NewScripter(srv.DatabaseRef(name), opts).ScriptDatabase(ctx)
	if err != nil {
		t.Fatalf("ScriptDatabase: %v", err)
	}
	t.Logf("script:\n%s", script)
	replayScript(t, ctx, db, strings.ReplaceAll(script, name, copyName))
	// Writable again for the schemas.
	if _, err := db.ExecContext(ctx, "ALTER DATABASE ["+copyName+"] SET READ_WRITE"); err != nil {
		t.Fatal(err)
	}
	cp, err := srv.DatabaseByName(ctx, copyName)
	if err != nil {
		t.Fatal(err)
	}
	sc := NewScripter(src, opts)
	for _, schema := range []string{"Sa'les", "bare"} {
		s, err := sc.ScriptSchema(ctx, schema)
		if err != nil {
			t.Fatalf("ScriptSchema %s: %v", schema, err)
		}
		livePinnedRun(t, db, ctx, copyName, s)
		// Guarded, a second run changes nothing.
		livePinnedRun(t, db, ctx, copyName, s)
	}
	compare := func(what string) {
		t.Helper()
		want := liveRowsAsStrings(t, src, ctx, liveDBSchemaExtPropsOracle)
		got := liveRowsAsStrings(t, cp, ctx, liveDBSchemaExtPropsOracle)
		// The copy's files are named for it.
		for i, r := range got {
			got[i] = strings.ReplaceAll(r, copyName, name)
		}
		if len(want) != 10 {
			t.Fatalf("%s: the source has %d properties, want 10", what, len(want))
		}
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(want, got) {
			t.Errorf("%s: properties differ.\nsource:\n  %s\nreplay:\n  %s", what, strings.Join(want, "\n  "), strings.Join(got, "\n  "))
		}
	}
	compare("CREATE")

	// DROP AND CREATE of a schema takes its properties along and puts them
	// back.
	dc := opts
	dc.Verb = ScriptDropAndCreate
	s, err := NewScripter(src, dc).ScriptSchema(ctx, "Sa'les")
	if err != nil {
		t.Fatal(err)
	}
	livePinnedRun(t, db, ctx, copyName, s)
	compare("DROP AND CREATE")

	off := opts
	off.ExtendedProperties = false
	for _, f := range []func(*Scripter) (string, error){
		func(sc *Scripter) (string, error) { return sc.ScriptDatabase(ctx) },
		func(sc *Scripter) (string, error) { return sc.ScriptSchema(ctx, "Sa'les") },
	} {
		s, err := f(NewScripter(src, off))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(s, "sp_addextendedproperty") {
			t.Errorf("ExtendedProperties off, but:\n%s", s)
		}
	}
}

// liveOtherExtPropsOracle is liveExtPropsOracle for the securables outside
// tables and modules, each named rather than numbered: users and roles
// (class 4), types and table-type columns (6, 8), sequences, synonyms, DDL
// triggers and constraints (1), indexes (7), XML schema collections (10),
// partition schemes (20) and functions (21).
const liveOtherExtPropsOracle = `
SELECT ep.class,
       CASE ep.class
           WHEN 1  THEN ISNULL(OBJECT_SCHEMA_NAME(ep.major_id) + '.' + OBJECT_NAME(ep.major_id),
                               (SELECT 'ddl.' + tr.name FROM sys.triggers tr WHERE tr.object_id = ep.major_id AND tr.parent_class = 0))
           WHEN 4  THEN USER_NAME(ep.major_id)
           WHEN 6  THEN (SELECT SCHEMA_NAME(ty.schema_id) + '.' + ty.name FROM sys.types ty WHERE ty.user_type_id = ep.major_id)
           WHEN 8  THEN (SELECT SCHEMA_NAME(tt.schema_id) + '.' + tt.name + '.' + c.name FROM sys.table_types tt
                         JOIN sys.columns c ON c.object_id = tt.type_table_object_id AND c.column_id = ep.minor_id
                         WHERE tt.user_type_id = ep.major_id)
           WHEN 7  THEN OBJECT_SCHEMA_NAME(ep.major_id) + '.' + OBJECT_NAME(ep.major_id) + '.' +
                        (SELECT i.name FROM sys.indexes i WHERE i.object_id = ep.major_id AND i.index_id = ep.minor_id)
           WHEN 10 THEN (SELECT SCHEMA_NAME(x.schema_id) + '.' + x.name FROM sys.xml_schema_collections x WHERE x.xml_collection_id = ep.major_id)
           WHEN 20 THEN (SELECT ds.name FROM sys.data_spaces ds WHERE ds.data_space_id = ep.major_id)
           WHEN 21 THEN (SELECT pf.name FROM sys.partition_functions pf WHERE pf.function_id = ep.major_id)
       END AS target,
       ep.name,
       ISNULL(CONVERT(sysname, SQL_VARIANT_PROPERTY(ep.value, 'BaseType')), '') AS base,
       ISNULL(CONVERT(varchar(max), CONVERT(varbinary(max), ep.value), 1), 'NULL') AS bytes
FROM   sys.extended_properties ep
WHERE  ep.class IN (1, 4, 6, 7, 8, 10, 20, 21)`

// Each single-object script outside tables and modules restates its own
// properties — and only its own: the check constraint and the index named
// alike on one table each carry the other's name, so a script that filters
// by name alone picks up the wrong one.
func TestLiveScriptOtherSecurablesExtendedProperties(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_extprop_other_src")
	defer dropSrc()
	dst, dropDst := liveScratchDB(t, db, ctx, "gosmo_extprop_other_dst")
	defer dropDst()

	add := func(name, value string, levels ...string) string {
		s := "DECLARE @v sql_variant = " + value + "; EXEC sys.sp_addextendedproperty @name = N'" + name + "', @value = @v"
		for i := 0; i < len(levels); i += 2 {
			s += ", @level" + string(rune('0'+i/2)) + "type = N'" + levels[i] + "', @level" + string(rune('0'+i/2)) +
				"name = N'" + strings.ReplaceAll(levels[i+1], "'", "''") + "'"
		}
		return s
	}
	tables := []string{
		`CREATE TABLE dbo.P (ID int NOT NULL, Code int NOT NULL)`,
		`CREATE TABLE dbo.C (ID int NOT NULL, PID int NOT NULL, Qty int NOT NULL)`,
	}
	liveExecIn(t, src, ctx, tables...)
	liveExecIn(t, dst, ctx, tables...)
	liveExecIn(t, src, ctx,
		`ALTER TABLE dbo.P ADD CONSTRAINT PK_P PRIMARY KEY (ID), CONSTRAINT UQ_P UNIQUE (Code)`,
		`ALTER TABLE dbo.C ADD CONSTRAINT [CK_C'Qty] CHECK (Qty > 0), CONSTRAINT FK_C_P FOREIGN KEY (PID) REFERENCES dbo.P (ID)`,
		`CREATE INDEX [CK_C'Qty] ON dbo.C (Qty)`,
		`CREATE USER [u'1] WITHOUT LOGIN`,
		`CREATE ROLE r1`,
		`CREATE TYPE dbo.Alias FROM int NOT NULL`,
		`CREATE TYPE dbo.TT AS TABLE (A int, B nvarchar(10))`,
		`CREATE SEQUENCE dbo.Seq AS int START WITH 1`,
		`CREATE SYNONYM dbo.Syn FOR dbo.P`,
		`CREATE XML SCHEMA COLLECTION dbo.XC AS N'<xsd:schema xmlns:xsd="http://www.w3.org/2001/XMLSchema"><xsd:element name="a" type="xsd:string"/></xsd:schema>'`,
		`CREATE PARTITION FUNCTION PF (int) AS RANGE LEFT FOR VALUES (1, 10)`,
		`CREATE PARTITION SCHEME PS AS PARTITION PF ALL TO ([PRIMARY])`,
		`CREATE TRIGGER ddl_tr ON DATABASE FOR CREATE_TABLE AS SET NOCOUNT ON`,
		add("MS_Description", "N'user, it''s'", "USER", "u'1"),
		add("Version", "CAST(2 AS int)", "USER", "u'1"),
		add("MS_Description", "N'role'", "USER", "r1"),
		add("MS_Description", "N'alias'", "SCHEMA", "dbo", "TYPE", "Alias"),
		add("MS_Description", "N'table type'", "SCHEMA", "dbo", "TYPE", "TT"),
		add("MS_Description", "N'tt column b'", "SCHEMA", "dbo", "TYPE", "TT", "COLUMN", "B"),
		add("Empty", "NULL", "SCHEMA", "dbo", "TYPE", "TT", "COLUMN", "A"),
		add("MS_Description", "N'sequence'", "SCHEMA", "dbo", "SEQUENCE", "Seq"),
		add("MS_Description", "N'synonym'", "SCHEMA", "dbo", "SYNONYM", "Syn"),
		add("MS_Description", "N'xml'", "SCHEMA", "dbo", "XML SCHEMA COLLECTION", "XC"),
		add("MS_Description", "N'pf'", "PARTITION FUNCTION", "PF"),
		add("MS_Description", "N'ps'", "PARTITION SCHEME", "PS"),
		add("MS_Description", "N'ddl'", "TRIGGER", "ddl_tr"),
		add("MS_Description", "N'pk constraint'", "SCHEMA", "dbo", "TABLE", "P", "CONSTRAINT", "PK_P"),
		add("MS_Description", "N'pk index'", "SCHEMA", "dbo", "TABLE", "P", "INDEX", "PK_P"),
		add("MS_Description", "N'uq'", "SCHEMA", "dbo", "TABLE", "P", "CONSTRAINT", "UQ_P"),
		add("MS_Description", "N'check'", "SCHEMA", "dbo", "TABLE", "C", "CONSTRAINT", "CK_C'Qty"),
		add("MS_Description", "N'index named as the check'", "SCHEMA", "dbo", "TABLE", "C", "INDEX", "CK_C'Qty"),
		add("MS_Description", "N'fk'", "SCHEMA", "dbo", "TABLE", "C", "CONSTRAINT", "FK_C_P"),
	)

	// In dependency order: the partition function before its scheme, the
	// referenced key before the foreign key.
	scriptAll := func(sc *Scripter) []string {
		t.Helper()
		var out []string
		for _, f := range []func() (string, error){
			func() (string, error) { return sc.ScriptUser(ctx, "u'1") },
			func() (string, error) { return sc.ScriptDatabaseRole(ctx, "r1") },
			func() (string, error) { return sc.ScriptUserDefinedDataType(ctx, "dbo", "Alias") },
			func() (string, error) { return sc.ScriptUserDefinedTableType(ctx, "dbo", "TT") },
			func() (string, error) { return sc.ScriptSequence(ctx, "dbo", "Seq") },
			func() (string, error) { return sc.ScriptSynonym(ctx, "dbo", "Syn") },
			func() (string, error) { return sc.ScriptXMLSchemaCollection(ctx, "dbo", "XC") },
			func() (string, error) { return sc.ScriptPartitionFunction(ctx, "PF") },
			func() (string, error) { return sc.ScriptPartitionScheme(ctx, "PS") },
			func() (string, error) { return sc.ScriptDatabaseTrigger(ctx, "ddl_tr") },
			func() (string, error) { return sc.ScriptIndex(ctx, "dbo", "P", "PK_P") },
			func() (string, error) { return sc.ScriptIndex(ctx, "dbo", "P", "UQ_P") },
			func() (string, error) { return sc.ScriptCheckConstraint(ctx, "dbo", "C", "CK_C'Qty") },
			func() (string, error) { return sc.ScriptIndex(ctx, "dbo", "C", "CK_C'Qty") },
			func() (string, error) { return sc.ScriptForeignKey(ctx, "dbo", "C", "FK_C_P") },
		} {
			s, err := f()
			if err != nil {
				t.Fatalf("script: %v", err)
			}
			out = append(out, s)
		}
		return out
	}
	compare := func(what string) {
		t.Helper()
		want := liveRowsAsStrings(t, src, ctx, liveOtherExtPropsOracle)
		got := liveRowsAsStrings(t, dst, ctx, liveOtherExtPropsOracle)
		if len(want) != 19 {
			t.Fatalf("%s: the source has %d properties, want 19:\n  %s", what, len(want), strings.Join(want, "\n  "))
		}
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(want, got) {
			t.Errorf("%s: properties differ.\nsource:\n  %s\nreplay:\n  %s", what, strings.Join(want, "\n  "), strings.Join(got, "\n  "))
		}
	}

	opts := DefaultScriptOptions()
	opts.IncludeHeaders = false
	creates := scriptAll(NewScripter(src, opts))
	for _, s := range creates {
		livePinnedRun(t, db, ctx, dst.Name, s)
	}
	compare("CREATE")

	// DROP AND CREATE of everything nothing else depends on takes the
	// properties along and puts them back. Not the alias and table types,
	// the partition function and scheme, or the referenced primary key.
	dc := opts
	dc.Verb = ScriptDropAndCreate
	for i, s := range scriptAll(NewScripter(src, dc)) {
		switch i {
		case 2, 3, 7, 8, 10:
			continue
		}
		livePinnedRun(t, db, ctx, dst.Name, s)
	}
	compare("DROP AND CREATE")

	off := opts
	off.ExtendedProperties = false
	for _, s := range scriptAll(NewScripter(src, off)) {
		if strings.Contains(s, "sp_addextendedproperty") {
			t.Errorf("ExtendedProperties off, but:\n%s", s)
		}
	}
}

// liveRestExtPropsOracle is liveExtPropsOracle for the securables
// TestLiveScriptRestOfSecurablesExtendedProperties scripts, each named: rules,
// defaults and queues (class 1), assemblies (5), CLR types (6), the five
// schemaless Service Broker families (15–19) and plan guides (27).
const liveRestExtPropsOracle = `
SELECT ep.class,
       CASE ep.class
           WHEN 1  THEN OBJECT_SCHEMA_NAME(ep.major_id) + '.' + OBJECT_NAME(ep.major_id)
           WHEN 5  THEN (SELECT a.name FROM sys.assemblies a WHERE a.assembly_id = ep.major_id)
           WHEN 6  THEN (SELECT SCHEMA_NAME(ty.schema_id) + '.' + ty.name FROM sys.types ty WHERE ty.user_type_id = ep.major_id)
           WHEN 15 THEN (SELECT x.name COLLATE DATABASE_DEFAULT FROM sys.service_message_types x WHERE x.message_type_id = ep.major_id)
           WHEN 16 THEN (SELECT x.name COLLATE DATABASE_DEFAULT FROM sys.service_contracts x WHERE x.service_contract_id = ep.major_id)
           WHEN 17 THEN (SELECT x.name COLLATE DATABASE_DEFAULT FROM sys.services x WHERE x.service_id = ep.major_id)
           WHEN 18 THEN (SELECT x.name COLLATE DATABASE_DEFAULT FROM sys.remote_service_bindings x WHERE x.remote_service_binding_id = ep.major_id)
           WHEN 19 THEN (SELECT x.name COLLATE DATABASE_DEFAULT FROM sys.routes x WHERE x.route_id = ep.major_id)
           WHEN 27 THEN (SELECT x.name FROM sys.plan_guides x WHERE x.plan_guide_id = ep.major_id)
       END COLLATE DATABASE_DEFAULT AS target,
       ep.name,
       ISNULL(CONVERT(sysname, SQL_VARIANT_PROPERTY(ep.value, 'BaseType')), '') AS base,
       ISNULL(CONVERT(varchar(max), CONVERT(varbinary(max), ep.value), 1), 'NULL') AS bytes
FROM   sys.extended_properties ep
WHERE  ep.class IN (1, 5, 6, 15, 16, 17, 18, 19, 27)`

// The securables liveOtherExtPropsOracle's test leaves out restate their own
// properties too: a CLR type, a rule, a default, an assembly (its script's
// placeholder filled in with the bytes), a plan guide and the six Service
// Broker families that can carry one. A broker priority has none to carry.
func TestLiveScriptRestOfSecurablesExtendedProperties(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_extprop_rest_src")
	t.Cleanup(dropSrc)
	dst, dropDst := liveScratchDB(t, db, ctx, "gosmo_extprop_rest_dst")
	t.Cleanup(dropDst)
	liveCLRAssembly(t, db, ctx, src, "w7clr")
	dll, err := os.ReadFile("testdata/clr/w7clr.dll")
	if err != nil {
		t.Fatal(err)
	}

	add := func(name, value string, levels ...string) string {
		s := "DECLARE @v sql_variant = " + value + "; EXEC sys.sp_addextendedproperty @name = N'" + name + "', @value = @v"
		for i := 0; i < len(levels); i += 2 {
			s += ", @level" + string(rune('0'+i/2)) + "type = N'" + levels[i] + "', @level" + string(rune('0'+i/2)) +
				"name = N'" + strings.ReplaceAll(levels[i+1], "'", "''") + "'"
		}
		return s
	}
	common := []string{`CREATE TABLE dbo.t (a int)`, `CREATE USER u1 WITHOUT LOGIN`}
	liveExecIn(t, src, ctx, common...)
	liveExecIn(t, dst, ctx, common...)
	liveExecIn(t, src, ctx,
		`CREATE TYPE dbo.[Pt'] EXTERNAL NAME w7clr.W7Point`,
		`CREATE RULE dbo.[r'1] AS @v > 0`,
		`CREATE DEFAULT dbo.d1 AS 0`,
		`CREATE MESSAGE TYPE [mt'1] VALIDATION = NONE`,
		`CREATE CONTRACT c1 ([mt'1] SENT BY ANY)`,
		`CREATE QUEUE dbo.q1`,
		`CREATE SERVICE s1 ON QUEUE dbo.q1 (c1)`,
		`CREATE ROUTE rt1 WITH ADDRESS = 'LOCAL'`,
		`CREATE REMOTE SERVICE BINDING rsb1 TO SERVICE 'x' WITH USER = u1`,
		`CREATE BROKER PRIORITY bp1 FOR CONVERSATION SET (PRIORITY_LEVEL = 3)`,
		`EXEC sp_create_plan_guide @name = N'pg''1', @stmt = N'SELECT a FROM dbo.t', @type = N'SQL',
		     @module_or_batch = NULL, @params = NULL, @hints = N'OPTION (MAXDOP 1)'`,
		add("MS_Description", "N'assembly'", "ASSEMBLY", "w7clr"),
		add("MS_Description", "N'clr type, it''s'", "SCHEMA", "dbo", "TYPE", "Pt'"),
		add("Version", "CAST(2 AS int)", "SCHEMA", "dbo", "TYPE", "Pt'"),
		add("MS_Description", "N'rule'", "SCHEMA", "dbo", "RULE", "r'1"),
		add("MS_Description", "N'default'", "SCHEMA", "dbo", "DEFAULT", "d1"),
		add("MS_Description", "N'message type'", "MESSAGE TYPE", "mt'1"),
		add("MS_Description", "N'contract'", "CONTRACT", "c1"),
		add("MS_Description", "N'queue'", "SCHEMA", "dbo", "QUEUE", "q1"),
		add("MS_Description", "N'service'", "SERVICE", "s1"),
		add("MS_Description", "N'route'", "ROUTE", "rt1"),
		add("MS_Description", "N'binding'", "REMOTE SERVICE BINDING", "rsb1"),
		add("MS_Description", "N'plan guide'", "PLAN GUIDE", "pg'1"),
	)

	// In dependency order: the assembly before its type, a message type
	// before its contract, the queue and contract before the service.
	scriptAll := func(sc *Scripter) []string {
		t.Helper()
		var out []string
		for _, f := range []func() (string, error){
			func() (string, error) { return sc.ScriptAssembly(ctx, "w7clr") },
			func() (string, error) { return sc.ScriptClrType(ctx, "dbo", "Pt'") },
			func() (string, error) { return sc.ScriptRule(ctx, "dbo", "r'1") },
			func() (string, error) { return sc.ScriptDefault(ctx, "dbo", "d1") },
			func() (string, error) { return sc.ScriptMessageType(ctx, "mt'1") },
			func() (string, error) { return sc.ScriptContract(ctx, "c1") },
			func() (string, error) { return sc.ScriptBrokerQueue(ctx, "dbo", "q1") },
			func() (string, error) { return sc.ScriptBrokerService(ctx, "s1") },
			func() (string, error) { return sc.ScriptRoute(ctx, "rt1") },
			func() (string, error) { return sc.ScriptRemoteServiceBinding(ctx, "rsb1") },
			func() (string, error) { return sc.ScriptPlanGuide(ctx, "pg'1") },
			func() (string, error) { return sc.ScriptBrokerPriority(ctx, "bp1") },
		} {
			s, err := f()
			if err != nil {
				t.Fatalf("script: %v", err)
			}
			out = append(out, strings.Replace(s, assemblyBinaryPlaceholder, "0x"+hex.EncodeToString(dll), 1))
		}
		return out
	}
	compare := func(what string) {
		t.Helper()
		want := liveRowsAsStrings(t, src, ctx, liveRestExtPropsOracle)
		got := liveRowsAsStrings(t, dst, ctx, liveRestExtPropsOracle)
		if len(want) != 12 {
			t.Fatalf("%s: the source has %d properties, want 12:\n  %s", what, len(want), strings.Join(want, "\n  "))
		}
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(want, got) {
			t.Errorf("%s: properties differ.\nsource:\n  %s\nreplay:\n  %s", what, strings.Join(want, "\n  "), strings.Join(got, "\n  "))
		}
	}

	opts := DefaultScriptOptions()
	opts.IncludeHeaders = false
	for _, s := range scriptAll(NewScripter(src, opts)) {
		livePinnedRun(t, db, ctx, dst.Name, s)
	}
	compare("CREATE")

	// DROP AND CREATE of everything nothing else here depends on takes the
	// properties along and puts them back: not the assembly, the message
	// type, the contract or the queue.
	dc := opts
	dc.Verb = ScriptDropAndCreate
	for i, s := range scriptAll(NewScripter(src, dc)) {
		switch i {
		case 0, 4, 5, 6:
			continue
		}
		livePinnedRun(t, db, ctx, dst.Name, s)
	}
	compare("DROP AND CREATE")

	off := opts
	off.ExtendedProperties = false
	for _, s := range scriptAll(NewScripter(src, off)) {
		if strings.Contains(s, "sp_addextendedproperty") {
			t.Errorf("ExtendedProperties off, but:\n%s", s)
		}
	}
}
