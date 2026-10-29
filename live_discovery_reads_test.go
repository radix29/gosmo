//go:build livedb

// Live verification of the discovery reads gossms's command line (phase 3)
// added: ObjectByName/ObjectsNamed, SearchDefinitions, SearchColumns,
// Table/View.IndexUsage and Server.DatabaseSizes.
//
//	go test -tags livedb . -run TestLiveDiscoveryReads -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops two throwaway databases (one case-sensitive); touches
// nothing else.
package gosmo

import (
	"database/sql"
	"errors"
	"net/url"
	"slices"
	"testing"
)

func TestLiveDiscoveryReads(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_discovery_live")
	defer drop()
	liveExecIn(t, d, ctx,
		"CREATE SCHEMA Sales",
		"CREATE TABLE Sales.Customer (id int CONSTRAINT PK_Customer PRIMARY KEY, CustomerName nvarchar(50), pct_1 decimal(9,2), pct11 int)",
		"CREATE INDEX IX_Customer_Name ON Sales.Customer (CustomerName)",
		"CREATE TABLE dbo.Customer (id int)",
		"CREATE TABLE dbo.Heap (x int)",
		"CREATE VIEW Sales.vCustomer WITH SCHEMABINDING AS SELECT id, CustomerName FROM Sales.Customer",
		"CREATE UNIQUE CLUSTERED INDEX CIX_vCustomer ON Sales.vCustomer (id)",
		"CREATE PROCEDURE Sales.p_needle AS SELECT 'needle_50%' AS x",
		"CREATE PROCEDURE Sales.p_other AS SELECT 'needle150x' AS x",
		"CREATE PROCEDURE Sales.p_secret WITH ENCRYPTION AS SELECT 'needle_50%' AS x",
	)

	t.Run("ObjectByName", func(t *testing.T) {
		o, err := d.ObjectByName(ctx, "Sales", "Customer")
		if err != nil {
			t.Fatalf("ObjectByName: %v", err)
		}
		if o.Type != "U" || o.TypeDesc != "USER_TABLE" || o.Schema != "Sales" || o.ObjectID == 0 || o.Created.IsZero() {
			t.Errorf("ObjectByName(Sales.Customer) = %+v", o)
		}
		pk, err := d.ObjectByName(ctx, "Sales", "PK_Customer")
		if err != nil {
			t.Fatalf("ObjectByName(PK): %v", err)
		}
		if pk.Type != "PK" || pk.ParentSchema != "Sales" || pk.ParentName != "Customer" {
			t.Errorf("ObjectByName(PK_Customer) = %+v, want PK under Sales.Customer", pk)
		}
		if _, err := d.ObjectByName(ctx, "Sales", "nope"); !errors.Is(err, ErrNotFound) {
			t.Errorf("ObjectByName(missing) err = %v, want ErrNotFound", err)
		}
	})

	t.Run("ObjectsNamed", func(t *testing.T) {
		got, err := d.ObjectsNamed(ctx, "Customer")
		if err != nil {
			t.Fatalf("ObjectsNamed: %v", err)
		}
		var schemas []string
		for _, o := range got {
			schemas = append(schemas, o.Schema)
		}
		if !slices.Equal(schemas, []string{"dbo", "Sales"}) {
			t.Errorf("ObjectsNamed(Customer) schemas = %q, want [dbo Sales]", schemas)
		}
		if got, err := d.ObjectsNamed(ctx, "nope"); err != nil || len(got) != 0 {
			t.Errorf("ObjectsNamed(nope) = %v, %v; want none", got, err)
		}
	})

	t.Run("SearchDefinitions", func(t *testing.T) {
		// % and _ match literally: p_other's needle150x is a wildcard match
		// only; the encrypted procedure has no readable text.
		got, err := d.SearchDefinitions(ctx, "NEEDLE_50%")
		if err != nil {
			t.Fatalf("SearchDefinitions: %v", err)
		}
		var names []string
		for _, r := range got {
			names = append(names, r.Schema+"."+r.Name)
		}
		if !slices.Equal(names, []string{"Sales.p_needle"}) {
			t.Errorf("SearchDefinitions(NEEDLE_50%%) = %q, want [Sales.p_needle]", names)
		}
	})

	t.Run("SearchColumns", func(t *testing.T) {
		got, err := d.SearchColumns(ctx, "PCT_1")
		if err != nil {
			t.Fatalf("SearchColumns: %v", err)
		}
		if len(got) != 1 || got[0].Column != "pct_1" || got[0].DataType != "decimal(9,2)" || got[0].TypeDesc != "USER_TABLE" {
			t.Errorf("SearchColumns(PCT_1) = %+v, want only Sales.Customer.pct_1 decimal(9,2)", got)
		}
		got, err = d.SearchColumns(ctx, "customername")
		if err != nil {
			t.Fatalf("SearchColumns: %v", err)
		}
		var objs []string
		for _, r := range got {
			objs = append(objs, r.Object+":"+r.DataType)
		}
		if !slices.Equal(objs, []string{"Customer:nvarchar(50)", "vCustomer:nvarchar(50)"}) {
			t.Errorf("SearchColumns(customername) = %q", objs)
		}
	})

	t.Run("IndexUsage", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "SELECT CustomerName FROM [gosmo_discovery_live].Sales.Customer WITH (INDEX(IX_Customer_Name)) WHERE CustomerName = N'x'"); err != nil {
			t.Fatalf("seek: %v", err)
		}
		tbl, err := d.TableByName(ctx, "Sales", "Customer")
		if err != nil {
			t.Fatalf("TableByName: %v", err)
		}
		u, err := tbl.IndexUsage(ctx)
		if err != nil {
			t.Fatalf("IndexUsage: %v", err)
		}
		if len(u) != 2 || u[0].IndexName != "PK_Customer" || u[1].IndexName != "IX_Customer_Name" {
			t.Fatalf("IndexUsage = %+v, want PK_Customer, IX_Customer_Name", u)
		}
		if !u[1].UsageVisible || u[1].UserSeeks+u[1].UserScans == 0 || (u[1].LastUserSeek.IsZero() && u[1].LastUserScan.IsZero()) {
			t.Errorf("IX_Customer_Name usage = %+v, want a read recorded (sa sees the DMV)", u[1])
		}
		heap, err := d.TableRef("dbo", "Heap").IndexUsage(ctx)
		if err != nil || len(heap) != 1 || heap[0].IndexID != 0 || heap[0].IndexName != "" {
			t.Errorf("IndexUsage(heap) = %+v, %v; want one row, index 0, no name", heap, err)
		}
		v, err := d.ViewByName(ctx, "Sales", "vCustomer")
		if err != nil {
			t.Fatalf("ViewByName: %v", err)
		}
		vu, err := v.IndexUsage(ctx)
		if err != nil || len(vu) != 1 || vu[0].IndexName != "CIX_vCustomer" {
			t.Errorf("View.IndexUsage = %+v, %v; want CIX_vCustomer", vu, err)
		}
		if got, err := d.TableRef("dbo", "nope").IndexUsage(ctx); err != nil || len(got) != 0 {
			t.Errorf("IndexUsage(missing) = %v, %v; want no rows", got, err)
		}
	})

	// Without VIEW SERVER STATE the DMV is refused (Msg 300); the read must
	// still list the indexes, marked unread.
	t.Run("IndexUsage without VIEW SERVER STATE", func(t *testing.T) {
		const login, pass = "gosmo_discovery_lowpriv", "Lp!7xQ#2dKz9"
		if _, err := db.ExecContext(ctx, "CREATE LOGIN "+login+" WITH PASSWORD = N'"+pass+"', CHECK_POLICY = OFF"); err != nil {
			t.Fatalf("create login: %v", err)
		}
		defer db.ExecContext(ctx, "DROP LOGIN "+login)
		liveExecIn(t, d, ctx, "CREATE USER "+login+" FOR LOGIN "+login, "GRANT VIEW DEFINITION TO "+login)
		defer liveExecIn(t, d, ctx, "DROP USER "+login)

		u, err := url.Parse(*liveDSN)
		if err != nil {
			t.Fatalf("parse DSN: %v", err)
		}
		u.User = url.UserPassword(login, pass)
		ldb, err := sql.Open("sqlserver", u.String())
		if err != nil {
			t.Fatalf("open as %s: %v", login, err)
		}
		defer ldb.Close()
		lsrv := liveServer(t, ldb, ctx)
		got, err := lsrv.DatabaseRef(d.Name).TableRef("Sales", "Customer").IndexUsage(ctx)
		if err != nil {
			t.Fatalf("IndexUsage as %s: %v", login, err)
		}
		if len(got) != 2 || got[0].UsageVisible || got[1].UsageVisible || got[1].UserSeeks != 0 {
			t.Errorf("IndexUsage as %s = %+v, want both indexes with UsageVisible false", login, got)
		}
	})

	t.Run("DatabaseSizes", func(t *testing.T) {
		sizes, err := d.Server().DatabaseSizes(ctx)
		if err != nil {
			t.Fatalf("DatabaseSizes: %v", err)
		}
		files, err := d.Server().DatabaseFiles(ctx, d.Name)
		if err != nil {
			t.Fatalf("DatabaseFiles: %v", err)
		}
		var want DatabaseSize
		for _, f := range files {
			if f.Type == "LOG" {
				want.LogKB += f.SizeKB
			} else {
				want.DataKB += f.SizeKB
			}
		}
		if got := sizes[d.ID]; got != want || got.DataKB == 0 || got.LogKB == 0 {
			t.Errorf("DatabaseSizes[%d] = %+v, want %+v from DatabaseFiles", d.ID, got, want)
		}
		if _, ok := sizes[1]; !ok {
			t.Error("DatabaseSizes has no master")
		}
	})
}

// In a case-sensitive database the lookup follows the catalog: the wrong case
// is not the object, as OBJECT_ID would answer.
func TestLiveObjectByNameCaseSensitive(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDBCollated(t, db, ctx, "gosmo_discovery_cs_live", "Latin1_General_CS_AS")
	defer drop()
	liveExecIn(t, d, ctx,
		"CREATE SCHEMA Sales",
		"CREATE TABLE Sales.Customer (id int)",
		"CREATE TABLE Sales.customer (id int)",
	)
	o, err := d.ObjectByName(ctx, "Sales", "customer")
	if err != nil || o.Name != "customer" {
		t.Fatalf("ObjectByName(Sales.customer) = %+v, %v; want the lower-case one", o, err)
	}
	if _, err := d.ObjectByName(ctx, "sales", "Customer"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ObjectByName(sales.Customer) err = %v, want ErrNotFound in a _CS_ database", err)
	}
	got, err := d.ObjectsNamed(ctx, "CUSTOMER")
	if err != nil || len(got) != 0 {
		t.Errorf("ObjectsNamed(CUSTOMER) = %v, %v; want none", got, err)
	}
	if got, err := d.SearchColumns(ctx, "ID"); err != nil || len(got) != 2 {
		t.Errorf("SearchColumns(ID) = %v, %v; want both tables (search ignores case)", got, err)
	}
}
