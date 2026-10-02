//go:build livedb

// Live verification of the reads through a linked server.
//
//	go test -tags livedb . -run TestLiveLinkedServerCatalog -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own loopback linked server (MSOLEDBSQL, or SQLNCLI11
// before 2022 — so a Windows instance) and throwaway database; touches nothing else.
package gosmo

import (
	"context"
	"slices"
	"testing"
)

// TestLiveLinkedServerCatalog pins that LinkedServerCatalog reports the
// remote's declared types exactly — decimal not numeric, nchar not nvarchar,
// datetime2's scale, CLR-typed columns present — which the provider's schema
// rowsets (sp_columns_ex) do not; that a database name needing both quoting
// layers resolves; and that LinkedServerDatabases lists it.
func TestLiveLinkedServerCatalog(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const ls = "gosmo_live_ls"
	const dbName = "gosmo_ls'live]x"
	qdb, ldb := QuoteName(dbName), QuoteLiteral(dbName)

	dropLS := func(c context.Context) {
		db.ExecContext(c, "IF EXISTS (SELECT 1 FROM sys.servers WHERE name = N'"+ls+"') EXEC sp_dropserver N'"+ls+"', 'droplogins'")
	}
	dropDB := func(c context.Context) {
		db.ExecContext(c, "IF DB_ID("+ldb+") IS NOT NULL ALTER DATABASE "+qdb+" SET SINGLE_USER WITH ROLLBACK IMMEDIATE")
		db.ExecContext(c, "IF DB_ID("+ldb+") IS NOT NULL DROP DATABASE "+qdb)
	}
	dropLS(ctx)
	dropDB(ctx)
	defer dropDB(context.Background())
	defer dropLS(context.Background())

	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	// MSOLEDBSQL is refused in-process before 2022 (Msg 7430) unless its
	// AllowInProcess option is set; SQLNCLI11 ships with those versions.
	provider := "MSOLEDBSQL"
	if srv.Info().VersionMajor < 16 {
		provider = "SQLNCLI11"
	}
	for _, s := range []string{
		"EXEC sp_addlinkedserver @server = N'" + ls + "', @srvproduct = N'', @provider = N'" + provider + "', @datasrc = @@SERVERNAME, @provstr = N'TrustServerCertificate=yes'",
		"CREATE DATABASE " + qdb,
	} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("setup %.60q: %v", s, err)
		}
	}
	d := srv.DatabaseRef(dbName)
	liveExecIn(t, d, ctx,
		`CREATE SCHEMA sales`,
		`CREATE TABLE sales.Orders (id int NOT NULL, amt decimal(10,2), code nchar(3),
			at datetime2(3), geo geography, big nvarchar(max))`,
		`CREATE VIEW dbo.V AS SELECT id FROM sales.Orders`)

	all, err := srv.LinkedServers(ctx)
	if err != nil {
		t.Fatalf("LinkedServers: %v", err)
	}
	i := slices.IndexFunc(all, func(l *LinkedServer) bool { return l.Name == ls })
	if i < 0 || !all[i].DataAccess {
		t.Fatalf("LinkedServers: %s missing or DataAccess false", ls)
	}

	dbs, err := srv.LinkedServerDatabases(ctx, ls)
	if err != nil {
		t.Fatalf("LinkedServerDatabases: %v", err)
	}
	if !slices.Contains(dbs, dbName) || !slices.Contains(dbs, "master") {
		t.Errorf("LinkedServerDatabases = %q, want %q and master", dbs, dbName)
	}

	cat, err := srv.LinkedServerCatalog(ctx, ls, dbName)
	if err != nil {
		t.Fatalf("LinkedServerCatalog: %v", err)
	}
	if !slices.Equal(cat.Schemas, []string{"dbo", "sales"}) {
		t.Errorf("Schemas = %q", cat.Schemas)
	}
	if len(cat.Objects) != 2 || cat.Objects[0].Name != "V" || cat.Objects[0].Type != CatalogView ||
		cat.Objects[1].Name != "Orders" || cat.Objects[1].Type != CatalogTable {
		t.Fatalf("Objects = %+v", cat.Objects)
	}
	got := map[string]string{}
	for _, c := range cat.Objects[1].Columns {
		got[c.Name] = TypeString(c.DataType, c.MaxLength, c.Precision, c.Scale)
	}
	want := map[string]string{
		"id": "int", "amt": "decimal(10,2)", "code": "nchar(3)",
		"at": "datetime2(3)", "geo": "geography", "big": "nvarchar(MAX)",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("column %s = %q, want %q", k, got[k], v)
		}
	}
	if len(cat.Objects[1].Columns) != len(want) || cat.Objects[1].Columns[0].IsNullable {
		t.Errorf("columns = %+v", cat.Objects[1].Columns)
	}

	if _, err := srv.LinkedServerCatalog(ctx, ls, "gosmo_no_such_db"); err == nil {
		t.Error("LinkedServerCatalog on a missing database: no error")
	}
}
