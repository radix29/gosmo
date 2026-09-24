//go:build livedb

// Live verification of ScriptTable for external tables: each is replayed
// into a second database, which has neither the data source nor the file
// formats, and the two catalogs are compared.
//
//	go test -tags livedb . -run TestLiveScriptExternalTable -v \
//	  -livedb 'sqlserver://user:PASS@host,port?TrustServerCertificate=true'
//
// Needs PolyBase (or, on Azure SQL Managed Instance, data virtualization):
// skipped where SERVERPROPERTY('IsPolyBaseInstalled') is not 1, which is
// every on-premises test instance so far. Nothing reads through the tables,
// so the storage account they name need not exist. Creates and drops two
// throwaway databases; touches nothing else.
package gosmo

import (
	"context"
	"slices"
	"testing"
)

func TestLiveScriptExternalTable(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	// Creating a database on Managed Instance outlasts liveDB's minute.
	ctx, cancel := context.WithTimeout(context.Background(), 10*60e9)
	defer cancel()

	var polyBase int
	if err := db.QueryRowContext(ctx, "SELECT CONVERT(int, ISNULL(SERVERPROPERTY('IsPolyBaseInstalled'), 0))").Scan(&polyBase); err != nil {
		t.Fatalf("IsPolyBaseInstalled: %v", err)
	}
	if polyBase != 1 {
		t.Skip("PolyBase is not installed on this instance")
	}

	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_ext_src")
	defer dropSrc()
	dst, dropDst := liveScratchDB(t, db, ctx, "gosmo_ext_dst")
	defer dropDst()

	liveExecIn(t, src, ctx,
		"CREATE EXTERNAL DATA SOURCE [ds probe] WITH (LOCATION = 'abs://probe@gosmoprobe.blob.core.windows.net/')",
		"CREATE EXTERNAL FILE FORMAT ff_parquet WITH (FORMAT_TYPE = PARQUET)",
		"CREATE EXTERNAL FILE FORMAT ff_csv WITH (FORMAT_TYPE = DELIMITEDTEXT, FORMAT_OPTIONS (FIELD_TERMINATOR = ',', FIRST_ROW = 2))",
		`CREATE EXTERNAL TABLE dbo.ET1 (id int NOT NULL, name nvarchar(50) COLLATE Latin1_General_BIN2 NULL, amount decimal(12,3) NULL)
			WITH (LOCATION = 'x/*.parquet', DATA_SOURCE = [ds probe], FILE_FORMAT = ff_parquet)`,
		`CREATE EXTERNAL TABLE dbo.ET2 (id int, v varchar(10))
			WITH (LOCATION = 'y/''q''/', DATA_SOURCE = [ds probe], FILE_FORMAT = ff_csv,
				REJECT_TYPE = PERCENTAGE, REJECT_VALUE = 12.5, REJECT_SAMPLE_VALUE = 1000)`,
	)

	// CREATE twice: the second run finds the data source and file formats
	// already there, which their guards must allow. Then DROP AND CREATE,
	// and DROP on its own.
	for _, verb := range []ScriptVerb{ScriptCreate, ScriptDropAndCreate, ScriptDrop, ScriptCreate} {
		opts := DefaultScriptOptions()
		opts.Verb = verb
		for _, name := range []string{"ET1", "ET2"} {
			s, err := NewScripter(src, opts).ScriptTable(ctx, "dbo", name)
			if err != nil {
				t.Fatalf("ScriptTable %s (verb %d): %v", name, verb, err)
			}
			liveRunScript(t, dst, ctx, s)
		}
	}

	for what, q := range map[string]string{
		"external tables": `
SELECT OBJECT_NAME(et.object_id), et.location, ds.name, ff.name, et.reject_type, et.reject_value, ISNULL(et.reject_sample_value, -1)
FROM   sys.external_tables et
JOIN   sys.external_data_sources ds ON ds.data_source_id = et.data_source_id
LEFT   JOIN sys.external_file_formats ff ON ff.file_format_id = et.file_format_id
ORDER  BY 1`,
		"columns": `
SELECT OBJECT_NAME(c.object_id), c.name, TYPE_NAME(c.user_type_id), c.max_length, c.precision, c.scale,
       ISNULL(c.collation_name, '-'), c.is_nullable
FROM   sys.columns c JOIN sys.external_tables et ON et.object_id = c.object_id
ORDER  BY 1, c.column_id`,
		"data sources": "SELECT name, location, type_desc FROM sys.external_data_sources ORDER BY name",
		"file formats": `
SELECT name, format_type, ISNULL(field_terminator, '-'), ISNULL(first_row, 0)
FROM   sys.external_file_formats ORDER BY name`,
	} {
		if a, b := liveRowsAsStrings(t, src, ctx, q), liveRowsAsStrings(t, dst, ctx, q); !slices.Equal(a, b) {
			t.Errorf("%s differ after replay:\nsource: %v\nreplay: %v", what, a, b)
		}
	}
}
