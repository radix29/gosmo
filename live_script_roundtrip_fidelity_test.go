//go:build livedb

// Live round trips of what a script once lost without a word: a renamed
// module's name, an index's data space and incremental statistics, a table's
// and an index's XML compression, a masking function's quotes, a DDL
// trigger's SET options, a column's bound rule and default, an alias type's
// rule schema, and a graph table's SELECT. Each is asserted on the catalog
// after a replay (or on the replay running at all), never on the text.
//
//	go test -tags livedb . -run TestLiveRoundTrip -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops gz_* databases, and one gz_* server trigger and login-less
// users inside them; touches nothing else.
package gosmo

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestLiveRoundTripRenamedModules(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	d, drop := liveScratchDB(t, db, ctx, "gz_rt_rename")
	defer drop()

	liveExecIn(t, d, ctx,
		`CREATE SCHEMA hist`,
		`CREATE TABLE dbo.t (id int)`,
		`CREATE PROCEDURE dbo.oldp AS SELECT 1 AS one`,
		`EXEC sp_rename 'dbo.oldp', 'newp'`,
		`CREATE VIEW dbo.v AS SELECT 1 AS one`,
		`ALTER SCHEMA hist TRANSFER dbo.v`,
		`CREATE FUNCTION /* c */ [dbo].[oldf]() RETURNS int AS BEGIN RETURN 1 END`,
		`EXEC sp_rename 'dbo.oldf', 'newf'`,
		`CREATE TRIGGER dbo.oldtr ON dbo.t AFTER INSERT AS SET NOCOUNT ON`,
		`EXEC sp_rename 'dbo.oldtr', 'newtr'`,
		`CREATE PROCEDURE oldone AS SELECT 2 AS two`,
		`EXEC sp_rename 'dbo.oldone', 'newone'`,
	)
	type mod struct {
		schema, name, old string
		script            func(sc *Scripter) (string, error)
	}
	mods := []mod{
		{"dbo", "newp", "dbo.oldp", func(sc *Scripter) (string, error) { return sc.ScriptStoredProcedure(ctx, "dbo", "newp") }},
		{"hist", "v", "dbo.v", func(sc *Scripter) (string, error) { return sc.ScriptView(ctx, "hist", "v") }},
		{"dbo", "newf", "dbo.oldf", func(sc *Scripter) (string, error) { return sc.ScriptFunction(ctx, "dbo", "newf") }},
		{"dbo", "newtr", "dbo.oldtr", func(sc *Scripter) (string, error) { return sc.ScriptTrigger(ctx, "dbo", "newtr") }},
		{"dbo", "newone", "dbo.oldone", func(sc *Scripter) (string, error) { return sc.ScriptStoredProcedure(ctx, "dbo", "newone") }},
	}
	for _, verb := range []ScriptVerb{ScriptDropAndCreate, ScriptAlter} {
		opts := DefaultScriptOptions()
		opts.Verb = verb
		sc := NewScripter(d, opts)
		for _, m := range mods {
			script, err := m.script(sc)
			if err != nil {
				t.Fatalf("script %s.%s: %v", m.schema, m.name, err)
			}
			livePinnedRun(t, db, ctx, d.Name, script)
			if got := liveScalar(t, d, ctx, `SELECT ISNULL(CAST(OBJECT_ID(@p1) AS nvarchar(20)), '')`, qualifiedName(m.schema, m.name)); got == "" {
				t.Errorf("verb %v: %s.%s is gone after its own script:\n%s", verb, m.schema, m.name, script)
			}
			if got := liveScalar(t, d, ctx, `SELECT ISNULL(CAST(OBJECT_ID(@p1) AS nvarchar(20)), '')`, m.old); got != "" {
				t.Errorf("verb %v: the script recreated the old name %s:\n%s", verb, m.old, script)
			}
		}
	}
}

// liveFilegroupSetup gives d a second filegroup [fg 2] with one file, and a
// partition scheme ps_s on [PRIMARY].
func liveFilegroupSetup(t *testing.T, d *Database, ctx context.Context) {
	t.Helper()
	liveExecIn(t, d, ctx,
		`ALTER DATABASE CURRENT ADD FILEGROUP [fg 2]`,
		`DECLARE @p nvarchar(4000) = CONVERT(nvarchar(4000), SERVERPROPERTY('InstanceDefaultDataPath')) + DB_NAME() + N'_fg2.ndf';
		 DECLARE @s nvarchar(max) = N'ALTER DATABASE CURRENT ADD FILE (NAME = N''fg2_file'', FILENAME = N''' + REPLACE(@p, '''', '''''') + N''') TO FILEGROUP [fg 2]';
		 EXEC (@s)`,
		`CREATE PARTITION FUNCTION pf_s (int) AS RANGE RIGHT FOR VALUES (100)`,
		`CREATE PARTITION SCHEME ps_s AS PARTITION pf_s ALL TO ([PRIMARY])`,
	)
}

const liveIndexDataSpaces = `
SELECT OBJECT_NAME(i.object_id) + '.' + ISNULL(i.name, '(heap)') + '=' + ds.name
FROM   sys.indexes i JOIN sys.data_spaces ds ON ds.data_space_id = i.data_space_id
WHERE  OBJECTPROPERTY(i.object_id, 'IsUserTable') = 1
ORDER  BY 1`

func TestLiveRoundTripIndexDataSpace(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	src, dropSrc := liveScratchDB(t, db, ctx, "gz_rt_ds_src")
	defer dropSrc()
	dst, dropDst := liveScratchDB(t, db, ctx, "gz_rt_ds_dst")
	defer dropDst()
	liveFilegroupSetup(t, src, ctx)
	liveFilegroupSetup(t, dst, ctx)

	liveExecIn(t, src, ctx,
		`CREATE TABLE dbo.tp (id int NOT NULL, a int NULL) ON ps_s(id)`,
		`CREATE INDEX nx ON dbo.tp (a) ON [PRIMARY]`,
		`CREATE INDEX ax ON dbo.tp (a)`,
		`CREATE TABLE dbo.tf (id int NOT NULL CONSTRAINT pk_tf PRIMARY KEY NONCLUSTERED ON [PRIMARY], a int NULL,
		     CONSTRAINT uq_tf UNIQUE (a) ON [PRIMARY]) ON [fg 2]`,
		`CREATE INDEX nx2 ON dbo.tf (a) ON [PRIMARY]`,
		`CREATE INDEX fx2 ON dbo.tf (id)`,
	)
	want := liveRowsAsStrings(t, src, ctx, liveIndexDataSpaces)

	sc := NewScripter(src, DefaultScriptOptions())
	for _, tbl := range []string{"tp", "tf"} {
		script, err := sc.ScriptTable(ctx, "dbo", tbl)
		if err != nil {
			t.Fatalf("ScriptTable %s: %v", tbl, err)
		}
		livePinnedRun(t, db, ctx, dst.Name, script)
	}
	if got := liveRowsAsStrings(t, dst, ctx, liveIndexDataSpaces); !slices.Equal(got, want) {
		t.Errorf("table scripts: data spaces differ\n got %v\nwant %v", got, want)
	}

	// ScriptIndex alone, DROP AND CREATE in the source itself.
	opts := DefaultScriptOptions()
	opts.Verb = ScriptDropAndCreate
	sc = NewScripter(src, opts)
	for _, ix := range [][2]string{{"tp", "nx"}, {"tf", "nx2"}, {"tf", "uq_tf"}, {"tf", "fx2"}} {
		script, err := sc.ScriptIndex(ctx, "dbo", ix[0], ix[1])
		if err != nil {
			t.Fatalf("ScriptIndex %v: %v", ix, err)
		}
		livePinnedRun(t, db, ctx, src.Name, script)
	}
	if got := liveRowsAsStrings(t, src, ctx, liveIndexDataSpaces); !slices.Equal(got, want) {
		t.Errorf("index scripts: data spaces differ\n got %v\nwant %v", got, want)
	}
}

const liveIndexIncremental = `
SELECT OBJECT_NAME(s.object_id) + '.' + s.name + '=' + CAST(s.is_incremental AS varchar(1))
FROM   sys.stats s JOIN sys.indexes i ON i.object_id = s.object_id AND i.index_id = s.stats_id
WHERE  OBJECTPROPERTY(s.object_id, 'IsUserTable') = 1
ORDER  BY 1`

// TestLiveRoundTripIndexIncrementalStatistics: an index's
// STATISTICS_INCREMENTAL lives in sys.stats, not sys.indexes, and a script
// once recreated it with full-table statistics — on a PRIMARY KEY inside
// CREATE TABLE and on a CREATE INDEX alike.
func TestLiveRoundTripIndexIncrementalStatistics(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	src, dropSrc := liveScratchDB(t, db, ctx, "gz_rt_inc_src")
	defer dropSrc()
	dst, dropDst := liveScratchDB(t, db, ctx, "gz_rt_inc_dst")
	defer dropDst()
	liveFilegroupSetup(t, src, ctx)
	liveFilegroupSetup(t, dst, ctx)

	liveExecIn(t, src, ctx,
		`CREATE TABLE dbo.tp (id int NOT NULL, a int NULL, b int NULL,
		     CONSTRAINT pk_tp PRIMARY KEY CLUSTERED (id) WITH (STATISTICS_INCREMENTAL = ON)) ON ps_s(id)`,
		`CREATE INDEX ix_inc ON dbo.tp (a) WITH (STATISTICS_INCREMENTAL = ON)`,
		`CREATE INDEX ix_full ON dbo.tp (b)`,
	)
	want := liveRowsAsStrings(t, src, ctx, liveIndexIncremental)
	if strings.Join(want, " ") != "=tp.ix_full=0 =tp.ix_inc=1 =tp.pk_tp=1" {
		t.Fatalf("setup did not produce the mix it needs: %v", want)
	}

	sc := NewScripter(src, DefaultScriptOptions())
	script, err := sc.ScriptTable(ctx, "dbo", "tp")
	if err != nil {
		t.Fatalf("ScriptTable: %v", err)
	}
	livePinnedRun(t, db, ctx, dst.Name, script)
	if got := liveRowsAsStrings(t, dst, ctx, liveIndexIncremental); !slices.Equal(got, want) {
		t.Errorf("table script: incremental statistics differ\n got %v\nwant %v\n%s", got, want, script)
	}

	opts := DefaultScriptOptions()
	opts.Verb = ScriptDropAndCreate
	sc = NewScripter(src, opts)
	for _, ix := range []string{"ix_inc", "ix_full"} {
		script, err := sc.ScriptIndex(ctx, "dbo", "tp", ix)
		if err != nil {
			t.Fatalf("ScriptIndex %s: %v", ix, err)
		}
		livePinnedRun(t, db, ctx, src.Name, script)
	}
	if got := liveRowsAsStrings(t, src, ctx, liveIndexIncremental); !slices.Equal(got, want) {
		t.Errorf("index scripts: incremental statistics differ\n got %v\nwant %v", got, want)
	}
}

const liveXMLCompression = `
SELECT OBJECT_NAME(p.object_id) + '.' + ISNULL(i.name, '(heap)') + '#' + CAST(p.partition_number AS varchar(4))
       + '=' + CAST(p.xml_compression AS varchar(1)) + '/' + p.data_compression_desc COLLATE DATABASE_DEFAULT
FROM   sys.partitions p JOIN sys.indexes i ON i.object_id = p.object_id AND i.index_id = p.index_id
WHERE  OBJECTPROPERTY(p.object_id, 'IsUserTable') = 1
ORDER  BY 1`

// TestLiveRoundTripXMLCompression: XML_COMPRESSION (SQL Server 2022) is
// per partition in sys.partitions, and a script once recreated every heap,
// PRIMARY KEY and index with it off.
func TestLiveRoundTripXMLCompression(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	src, dropSrc := liveScratchDB(t, db, ctx, "gz_rt_xmlc_src")
	defer dropSrc()
	if major := src.serverMajorVersion(); major != 0 && major < int(SQLServer2022) {
		t.Skipf("major %d has no XML_COMPRESSION", major)
	}
	dst, dropDst := liveScratchDB(t, db, ctx, "gz_rt_xmlc_dst")
	defer dropDst()
	for _, d := range []*Database{src, dst} {
		liveExecIn(t, d, ctx,
			`CREATE PARTITION FUNCTION pf_x (int) AS RANGE RIGHT FOR VALUES (100, 200)`,
			`CREATE PARTITION SCHEME ps_x AS PARTITION pf_x ALL TO ([PRIMARY])`)
	}
	liveExecIn(t, src, ctx,
		`CREATE TABLE dbo.h (id int NOT NULL, x xml NULL) ON ps_x(id) WITH (XML_COMPRESSION = ON ON PARTITIONS (1, 3))`,
		`CREATE TABLE dbo.c (id int NOT NULL, x xml NULL,
		     CONSTRAINT pk_c PRIMARY KEY CLUSTERED (id) WITH (XML_COMPRESSION = ON)) ON ps_x(id)`,
		`CREATE INDEX ix_c ON dbo.c (id) INCLUDE (x)
		     WITH (XML_COMPRESSION = ON ON PARTITIONS (2), DATA_COMPRESSION = PAGE ON PARTITIONS (2 TO 3))`,
		`CREATE TABLE dbo.u (id int NOT NULL, x xml NULL) WITH (XML_COMPRESSION = ON)`,
	)
	want := liveRowsAsStrings(t, src, ctx, liveXMLCompression)

	sc := NewScripter(src, DefaultScriptOptions())
	for _, tbl := range []string{"h", "c", "u"} {
		script, err := sc.ScriptTable(ctx, "dbo", tbl)
		if err != nil {
			t.Fatalf("ScriptTable %s: %v", tbl, err)
		}
		livePinnedRun(t, db, ctx, dst.Name, script)
	}
	if got := liveRowsAsStrings(t, dst, ctx, liveXMLCompression); !slices.Equal(got, want) {
		t.Errorf("table scripts: XML compression differs\n got %v\nwant %v", got, want)
	}

	opts := DefaultScriptOptions()
	opts.Verb = ScriptDropAndCreate
	script, err := NewScripter(src, opts).ScriptIndex(ctx, "dbo", "c", "ix_c")
	if err != nil {
		t.Fatalf("ScriptIndex: %v", err)
	}
	livePinnedRun(t, db, ctx, src.Name, script)
	if got := liveRowsAsStrings(t, src, ctx, liveXMLCompression); !slices.Equal(got, want) {
		t.Errorf("index script: XML compression differs\n got %v\nwant %v\n%s", got, want, script)
	}

	// A rebuild naming DATA_COMPRESSION keeps XML_COMPRESSION (17.0, checked
	// 2026-10-21), so Index.Rebuild need not restate it; this pins that.
	tbl, err := src.TableByName(ctx, "dbo", "c")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pk_c", "ix_c"} {
		idx, err := tbl.IndexByName(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := idx.Rebuild(ctx, IndexRebuildOptions{DataCompression: DataCompressionPage}); err != nil {
			t.Fatalf("Rebuild %s: %v", name, err)
		}
	}
	got := liveRowsAsStrings(t, src, ctx, liveXMLCompression)
	for i, w := range want {
		if strings.HasPrefix(w, "=c.") {
			want[i] = w[:strings.LastIndex(w, "/")] + "/PAGE"
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("Rebuild with DATA_COMPRESSION: XML compression differs\n got %v\nwant %v", got, want)
	}
}

func TestLiveRoundTripMaskingFunctionQuotes(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	src, dropSrc := liveScratchDB(t, db, ctx, "gz_rt_mask_src")
	defer dropSrc()
	dst, dropDst := liveScratchDB(t, db, ctx, "gz_rt_mask_dst")
	defer dropDst()

	liveExecIn(t, src, ctx,
		`CREATE TABLE dbo.m (s varchar(20) MASKED WITH (FUNCTION = 'partial(1, "X''X", 2)') NULL,
		                     q varchar(20) MASKED WITH (FUNCTION = 'partial(0, "''''", 0)') NULL)`)
	masked := func(d *Database) string {
		liveExecIn(t, d, ctx,
			`CREATE USER gz_nounmask WITHOUT LOGIN`,
			`GRANT SELECT ON dbo.m TO gz_nounmask`,
			`INSERT dbo.m VALUES ('abcdefgh', 'abc')`)
		// One column per SELECT: an expression over masked columns is masked
		// as a whole ("xxxx").
		var out []string
		for _, col := range []string{"s", "q"} {
			out = append(out, liveScalar(t, d, ctx, `EXECUTE AS USER = 'gz_nounmask';
SELECT `+col+` FROM dbo.m;
REVERT;`))
		}
		return strings.Join(out, "|")
	}
	sc := NewScripter(src, DefaultScriptOptions())
	for range 2 { // twice: each round trip doubled the quotes again
		script, err := sc.ScriptTable(ctx, "dbo", "m")
		if err != nil {
			t.Fatalf("ScriptTable: %v", err)
		}
		liveExecIn(t, dst, ctx, `DROP TABLE IF EXISTS dbo.m`, `DROP USER IF EXISTS gz_nounmask`)
		livePinnedRun(t, db, ctx, dst.Name, script)
		sc = NewScripter(dst, DefaultScriptOptions())
	}
	if got, want := masked(dst), masked(src); got != want || want != "aX'Xgh|''" {
		t.Errorf("masked output after two round trips = %q, source %q (want aX'Xgh|'')", got, want)
	}
}

func TestLiveRoundTripDDLTriggerSetOptions(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	d, drop := liveScratchDB(t, db, ctx, "gz_rt_ddltrg")
	defer drop()
	srv := liveServer(t, db, ctx)
	master, err := srv.DatabaseByName(ctx, "master")
	if err != nil {
		t.Fatal(err)
	}

	livePinnedRun(t, db, ctx, d.Name, "SET QUOTED_IDENTIFIER OFF;\nGO\n"+
		"CREATE TRIGGER gz_qi ON DATABASE FOR CREATE_VIEW AS PRINT \"view created\"\nGO\n")
	opts := DefaultScriptOptions()
	opts.Verb = ScriptDropAndCreate
	script, err := NewScripter(d, opts).ScriptDatabaseTrigger(ctx, "gz_qi")
	if err != nil {
		t.Fatal(err)
	}
	livePinnedRun(t, db, ctx, d.Name, script)
	if got := liveScalar(t, d, ctx, `SELECT CAST(m.uses_quoted_identifier AS nvarchar(1)) FROM sys.triggers tr
JOIN sys.sql_modules m ON m.object_id = tr.object_id WHERE tr.name = N'gz_qi' AND tr.parent_class = 0`); got != "0" {
		t.Errorf("database trigger replayed with QUOTED_IDENTIFIER %q, want 0", got)
	}

	t.Cleanup(func() { db.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS gz_rt_qi ON ALL SERVER`) })
	livePinnedRun(t, db, ctx, "master", "SET QUOTED_IDENTIFIER OFF;\nGO\n"+
		"CREATE TRIGGER gz_rt_qi ON ALL SERVER FOR DROP_CREDENTIAL AS PRINT \"credential dropped\"\nGO\n")
	script, err = NewServerScripter(srv, opts).ScriptServerTrigger(ctx, "gz_rt_qi")
	if err != nil {
		t.Fatal(err)
	}
	livePinnedRun(t, db, ctx, "master", script)
	if got := liveScalar(t, master, ctx, `SELECT CAST(m.uses_quoted_identifier AS nvarchar(1)) FROM sys.server_triggers tr
JOIN sys.server_sql_modules m ON m.object_id = tr.object_id WHERE tr.name = N'gz_rt_qi'`); got != "0" {
		t.Errorf("server trigger replayed with QUOTED_IDENTIFIER %q, want 0", got)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER gz_rt_qi ON ALL SERVER`); err != nil {
		t.Errorf("drop server trigger: %v", err)
	}
}

const liveBindings = `
SELECT 'col ' + c.name + ' rule=' + ISNULL(OBJECT_SCHEMA_NAME(c.rule_object_id) + '.' + OBJECT_NAME(c.rule_object_id), '-')
       + ' default=' + ISNULL(OBJECT_SCHEMA_NAME(c.default_object_id) + '.' + OBJECT_NAME(c.default_object_id), '-')
FROM   sys.columns c WHERE c.object_id = OBJECT_ID(N'dbo.t')
UNION ALL
SELECT 'type ' + t.name + ' rule=' + ISNULL(OBJECT_SCHEMA_NAME(t.rule_object_id) + '.' + OBJECT_NAME(t.rule_object_id), '-')
       + ' default=' + ISNULL(OBJECT_SCHEMA_NAME(t.default_object_id) + '.' + OBJECT_NAME(t.default_object_id), '-')
FROM   sys.types t WHERE t.is_user_defined = 1`

func TestLiveRoundTripBoundRulesAndDefaults(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	src, dropSrc := liveScratchDB(t, db, ctx, "gz_rt_bind_src")
	defer dropSrc()
	dst, dropDst := liveScratchDB(t, db, ctx, "gz_rt_bind_dst")
	defer dropDst()

	objects := []string{
		`CREATE SCHEMA hist`,
		`CREATE RULE hist.r_hist AS @v > 0`,
		// A same-named rule in dbo: an unqualified bind would take this one.
		`CREATE RULE dbo.r_hist AS @v < 0`,
		`CREATE DEFAULT dbo.d_zero AS 0`,
		`CREATE DEFAULT hist.d_seven AS 7`,
	}
	liveExecIn(t, src, ctx, objects...)
	liveExecIn(t, dst, ctx, objects...)
	liveExecIn(t, src, ctx,
		`CREATE TYPE dbo.small FROM int NULL`,
		`EXEC sp_bindrule N'hist.r_hist', N'dbo.small'`,
		`EXEC sp_bindefault N'hist.d_seven', N'dbo.small'`,
		`CREATE TABLE dbo.t (a int NULL, b dbo.small NULL, c dbo.small NULL, d int NULL CONSTRAINT df_d DEFAULT 5)`,
		`EXEC sp_bindrule N'hist.r_hist', N'dbo.t.a'`,
		`EXEC sp_bindefault N'dbo.d_zero', N'dbo.t.a'`,
		// c overrides its type's default with its own.
		`EXEC sp_bindefault N'dbo.d_zero', N'dbo.t.c'`,
	)
	want := liveRowsAsStrings(t, src, ctx, liveBindings)

	sc := NewScripter(src, DefaultScriptOptions())
	typ, err := sc.ScriptUserDefinedDataType(ctx, "dbo", "small")
	if err != nil {
		t.Fatal(err)
	}
	tbl, err := sc.ScriptTable(ctx, "dbo", "t")
	if err != nil {
		t.Fatal(err)
	}
	livePinnedRun(t, db, ctx, dst.Name, typ)
	livePinnedRun(t, db, ctx, dst.Name, tbl)
	if got := liveRowsAsStrings(t, dst, ctx, liveBindings); !slices.Equal(got, want) {
		t.Errorf("bindings differ after replay\n got %v\nwant %v\ntype script:\n%s\ntable script:\n%s", got, want, typ, tbl)
	}
	if n := strings.Count(tbl, "sp_bind"); n != 3 {
		t.Errorf("table script binds %d times, want 3 (a's rule and default, c's default):\n%s", n, tbl)
	}
}

func TestLiveRoundTripGraphSelect(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv := liveServer(t, db, ctx)
	if m := srv.serverMajorVersion(); m != 0 && m < int(SQLServer2017) {
		t.Skip("graph tables are SQL Server 2017's")
	}
	d, drop := liveScratchDB(t, db, ctx, "gz_rt_graph")
	defer drop()
	liveExecIn(t, d, ctx,
		`CREATE TABLE dbo.n (id int) AS NODE`,
		`CREATE TABLE dbo.e (w int) AS EDGE`,
		`INSERT dbo.n VALUES (1), (2)`,
		`INSERT dbo.e ($from_id, $to_id, w) SELECT a.$node_id, b.$node_id, 9 FROM dbo.n a, dbo.n b WHERE a.id = 1 AND b.id = 2`,
	)
	sc := NewScripter(d, DefaultScriptOptions())
	for _, tbl := range []string{"n", "e"} {
		script, err := sc.ScriptSelect(ctx, "dbo", tbl)
		if err != nil {
			t.Fatalf("ScriptSelect %s: %v", tbl, err)
		}
		livePinnedRun(t, db, ctx, d.Name, script)
	}
}

// liveUserStatistics is every user-created statistic with its options and
// columns, the shape a table or view script must restate.
const liveUserStatistics = `
SELECT OBJECT_NAME(s.object_id) + '.' + s.name + ' nr=' + CAST(s.no_recompute AS varchar(1))
       + ' f=' + ISNULL(s.filter_definition, '-') + ' cols=' +
       STUFF((SELECT ',' + c.name FROM sys.stats_columns sc
              JOIN sys.columns c ON c.object_id = sc.object_id AND c.column_id = sc.column_id
              WHERE sc.object_id = s.object_id AND sc.stats_id = s.stats_id
              ORDER BY sc.stats_column_id FOR XML PATH('')), 1, 1, '') AS st
FROM   sys.stats s
WHERE  s.user_created = 1 AND OBJECTPROPERTY(s.object_id, 'IsMSShipped') = 0
ORDER  BY 1`

// TestLiveRoundTripUserStatistics: a table's and an indexed view's
// user-created statistics (ScriptOptions.Statistics) come back from
// ScriptTable/ScriptView and from ScriptDatabaseObjectList, while the
// auto-created and index-owned ones are left to the server. Before
// 2026-11-14 neither script mentioned them.
func TestLiveRoundTripUserStatistics(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	src, dropSrc := liveScratchDB(t, db, ctx, "gz_rt_stats_src")
	defer dropSrc()
	dst, dropDst := liveScratchDB(t, db, ctx, "gz_rt_stats_dst")
	defer dropDst()
	dst2, dropDst2 := liveScratchDB(t, db, ctx, "gz_rt_stats_dst2")
	defer dropDst2()

	liveExecIn(t, src, ctx,
		`CREATE TABLE dbo.t (id int NOT NULL CONSTRAINT pk_t PRIMARY KEY, pid int NULL, qty int NULL, note varchar(20) NULL)`,
		`CREATE INDEX ix_pid ON dbo.t (pid)`,
		`CREATE STATISTICS st1 ON dbo.t (pid, qty) WITH NORECOMPUTE`,
		`CREATE STATISTICS [st 2] ON dbo.t (qty) WHERE qty > 10`,
		`INSERT dbo.t VALUES (1, 1, 5, 'a'), (2, 2, 50, 'b')`,
		// Auto-creates a _WA_Sys_ statistic on note.
		`SELECT COUNT(*) FROM dbo.t WHERE note = 'a'`,
		`CREATE VIEW dbo.v WITH SCHEMABINDING AS SELECT id, pid, qty FROM dbo.t`,
		`CREATE UNIQUE CLUSTERED INDEX cix_v ON dbo.v (id)`,
		`CREATE STATISTICS st_v ON dbo.v (pid, qty)`,
	)
	want := liveRowsAsStrings(t, src, ctx, liveUserStatistics)
	if len(want) != 3 {
		t.Fatalf("setup: want three user statistics, got %v", want)
	}
	if auto := liveRowsAsStrings(t, src, ctx, `SELECT name FROM sys.stats WHERE auto_created = 1 AND object_id = OBJECT_ID('dbo.t')`); len(auto) == 0 {
		t.Log("no auto-created statistic appeared; the exclusion is not exercised")
	}

	sc := NewScripter(src, DefaultScriptOptions())
	tbl, err := sc.ScriptTable(ctx, "dbo", "t")
	if err != nil {
		t.Fatalf("ScriptTable: %v", err)
	}
	view, err := sc.ScriptView(ctx, "dbo", "v")
	if err != nil {
		t.Fatalf("ScriptView: %v", err)
	}
	if strings.Contains(tbl, "_WA_Sys_") || strings.Contains(tbl, "STATISTICS [ix_pid]") {
		t.Errorf("table script restates a statistic that is not user-created:\n%s", tbl)
	}
	livePinnedRun(t, db, ctx, dst.Name, tbl)
	livePinnedRun(t, db, ctx, dst.Name, view)
	if got := liveRowsAsStrings(t, dst, ctx, liveUserStatistics); !slices.Equal(got, want) {
		t.Errorf("object scripts: statistics differ\n got %v\nwant %v\n%s\n%s", got, want, tbl, view)
	}

	res, err := sc.ScriptDatabaseObjectList(ctx, ObjectScriptOptions{})
	if err != nil {
		t.Fatalf("ScriptDatabaseObjectList: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings: %v", res.Warnings)
	}
	livePinnedRun(t, db, ctx, dst2.Name, res.String())
	if got := liveRowsAsStrings(t, dst2, ctx, liveUserStatistics); !slices.Equal(got, want) {
		t.Errorf("database script: statistics differ\n got %v\nwant %v\n%s", got, want, res.String())
	}

	opts := DefaultScriptOptions()
	opts.Statistics = false
	if off, err := NewScripter(src, opts).ScriptTable(ctx, "dbo", "t"); err != nil || strings.Contains(off, "CREATE STATISTICS") {
		t.Errorf("Statistics off: %v\n%s", err, off)
	}
}
