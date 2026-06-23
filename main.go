// Package main demonstrates the gosmo library – a Go SMO analogue for SQL Server.
// Run: go run ./examples/main.go
//
// Set environment variables before running:
//
//	MSSQL_SERVER   = "localhost:1433"
//	MSSQL_USER     = "sa"
//	MSSQL_PASSWORD = "YourStr0ngPa$$word"
package main

import (
	"fmt"
	"log"
	"os"
	"strings"
)

func main() {
	server := envOr("MSSQL_SERVER", "localhost:1433")
	user := envOr("MSSQL_USER", "sa")
	password := envOr("MSSQL_PASSWORD", "")

	// ── 1. Connect ────────────────────────────────────────────────────────────
	fmt.Println("=== Connecting to SQL Server ===")
	srv, err := smo.Connect(smo.ConnectionOptions{
		Server:                 server,
		User:                   user,
		Password:               password,
		TrustServerCertificate: true,
	})
	must(err)
	defer srv.Close()

	info := srv.Info()
	fmt.Printf("Connected to: %s\n", info.Name)
	fmt.Printf("Edition:      %s\n", info.Edition)
	fmt.Printf("Version:      %s (%d.%d.%d)\n",
		info.ProductLevel, info.VersionMajor, info.VersionMinor, info.VersionBuild)
	fmt.Printf("Memory:       %d MB  CPUs: %d\n", info.PhysicalMemoryMB, info.LogicalCPUCount)

	// ── 2. List databases ─────────────────────────────────────────────────────
	fmt.Println("\n=== Databases ===")
	dbs, err := srv.Databases()
	must(err)
	for _, d := range dbs {
		fmt.Printf("  %-30s  state=%-10s recovery=%-12s compat=%d\n",
			d.Name(), d.State(), d.RecoveryModel(), d.CompatibilityLevel())
	}

	// ── 3. Create a demo database ─────────────────────────────────────────────
	const dbName = "GoSMODemo"
	fmt.Printf("\n=== Creating database [%s] ===\n", dbName)
	_ = srv.DropDatabase(dbName, true) // clean slate
	must(srv.CreateDatabase(dbName, &smo.CreateDatabaseOptions{
		RecoveryModel: smo.RecoveryModelSimple,
		CompatLevel:   smo.CompatLevel2019,
	}))
	fmt.Printf("Created %s\n", dbName)

	db, err := srv.DatabaseByName(dbName)
	must(err)

	// ── 4. Extended property on the database ──────────────────────────────────
	must(db.AddExtendedProperty("MS_Description", "GoSMO demonstration database",
		smo.ExtendedPropertyLevel{}))
	fmt.Println("Added extended property MS_Description")

	// ── 5. Schemas ────────────────────────────────────────────────────────────
	fmt.Println("\n=== Creating schemas ===")
	must(db.CreateSchema("Sales", "dbo"))
	must(db.CreateSchema("HR", "dbo"))
	schemas, err := db.Schemas()
	must(err)
	for _, s := range schemas {
		fmt.Printf("  [%s] owned by %s\n", s.Name, s.Owner)
	}

	// ── 6. Create tables ──────────────────────────────────────────────────────
	fmt.Println("\n=== Creating tables ===")

	must(db.CreateTable(smo.CreateTableRequest{
		Schema: "dbo",
		Name:   "Customers",
		Columns: []smo.ColumnDefinition{
			{Name: "CustomerID", DataType: smo.DataTypeInt, IsNullable: false, IsIdentity: true, IdentitySeed: 1, IdentityIncr: 1, IsPrimaryKey: true},
			{Name: "FirstName", DataType: smo.DataTypeNVarChar, MaxLength: 100, IsNullable: false},
			{Name: "LastName", DataType: smo.DataTypeNVarChar, MaxLength: 100, IsNullable: false},
			{Name: "Email", DataType: smo.DataTypeNVarChar, MaxLength: 255, IsNullable: true},
			{Name: "CreatedAt", DataType: smo.DataTypeDatetime2, Scale: 3, IsNullable: false, DefaultValue: "sysdatetime()"},
			{Name: "IsActive", DataType: smo.DataTypeBit, IsNullable: false, DefaultValue: "1"},
		},
	}))

	must(db.CreateTable(smo.CreateTableRequest{
		Schema: "Sales",
		Name:   "Orders",
		Columns: []smo.ColumnDefinition{
			{Name: "OrderID", DataType: smo.DataTypeBigInt, IsNullable: false, IsIdentity: true, IdentitySeed: 1000, IdentityIncr: 1, IsPrimaryKey: true},
			{Name: "CustomerID", DataType: smo.DataTypeInt, IsNullable: false},
			{Name: "OrderDate", DataType: smo.DataTypeDate, IsNullable: false, DefaultValue: "CAST(SYSDATETIME() AS DATE)"},
			{Name: "TotalAmount", DataType: smo.DataTypeDecimal, Precision: 18, Scale: 2, IsNullable: false, DefaultValue: "0"},
			{Name: "Status", DataType: smo.DataTypeVarChar, MaxLength: 20, IsNullable: false, DefaultValue: "'PENDING'"},
		},
	}))

	must(db.CreateTable(smo.CreateTableRequest{
		Schema: "Sales",
		Name:   "OrderItems",
		Columns: []smo.ColumnDefinition{
			{Name: "ItemID", DataType: smo.DataTypeBigInt, IsNullable: false, IsIdentity: true, IdentitySeed: 1, IdentityIncr: 1, IsPrimaryKey: true},
			{Name: "OrderID", DataType: smo.DataTypeBigInt, IsNullable: false},
			{Name: "ProductName", DataType: smo.DataTypeNVarChar, MaxLength: 200, IsNullable: false},
			{Name: "Quantity", DataType: smo.DataTypeInt, IsNullable: false},
			{Name: "UnitPrice", DataType: smo.DataTypeDecimal, Precision: 10, Scale: 2, IsNullable: false},
		},
	}))

	tables, err := db.Tables()
	must(err)
	fmt.Printf("Created %d tables:\n", len(tables))
	for _, t := range tables {
		rc, _ := t.RowCount()
		fmt.Printf("  %s  (rows: %d)\n", t.FullName(), rc)
	}

	// ── 7. Indexes ────────────────────────────────────────────────────────────
	fmt.Println("\n=== Creating indexes ===")
	custTable, err := db.TableByName("dbo", "Customers")
	must(err)

	must(custTable.CreateIndex(smo.CreateIndexRequest{
		Name: "IX_Customers_LastName_FirstName",
		Type: smo.IndexTypeNonClustered,
		KeyColumns: []smo.IndexColumnDef{
			{Name: "LastName"},
			{Name: "FirstName"},
		},
		FillFactor: 90,
	}))

	must(custTable.CreateIndex(smo.CreateIndexRequest{
		Name:             "UIX_Customers_Email",
		Type:             smo.IndexTypeNonClustered,
		IsUnique:         true,
		KeyColumns:       []smo.IndexColumnDef{{Name: "Email"}},
		FilterDefinition: "Email IS NOT NULL",
	}))

	orderTable, err := db.TableByName("Sales", "Orders")
	must(err)
	must(orderTable.CreateIndex(smo.CreateIndexRequest{
		Name: "IX_Orders_CustomerID_OrderDate",
		Type: smo.IndexTypeNonClustered,
		KeyColumns: []smo.IndexColumnDef{
			{Name: "CustomerID"},
			{Name: "OrderDate", Descending: true},
		},
		IncludedColumns: []string{"TotalAmount", "Status"},
	}))

	fmt.Println("Indexes created successfully")

	// ── 8. Columns ────────────────────────────────────────────────────────────
	fmt.Println("\n=== Columns on dbo.Customers ===")
	cols, err := custTable.Columns()
	must(err)
	for _, c := range cols {
		nullable := "NOT NULL"
		if c.IsNullable {
			nullable = "NULL"
		}
		identity := ""
		if c.IsIdentity {
			identity = fmt.Sprintf(" IDENTITY(%d,%d)", c.IdentitySeed, c.IdentityIncrement)
		}
		fmt.Printf("  %-20s %-20s %s%s\n", c.Name, scriptColType(c), nullable, identity)
	}

	// ── 9. Stored procedure ───────────────────────────────────────────────────
	fmt.Println("\n=== Creating stored procedure ===")
	must(db.CreateStoredProcedure("dbo", "GetCustomerOrders", `
    @CustomerID INT
AS
BEGIN
    SET NOCOUNT ON;
    SELECT o.OrderID, o.OrderDate, o.TotalAmount, o.Status
    FROM   Sales.Orders o
    WHERE  o.CustomerID = @CustomerID
    ORDER  BY o.OrderDate DESC;
END`))
	fmt.Println("Created [dbo].[GetCustomerOrders]")

	// ── 10. View ──────────────────────────────────────────────────────────────
	fmt.Println("\n=== Creating view ===")
	must(db.CreateStoredProcedure("", "", "")) // just to show the API; create view directly:
	_, _ = db.DB().Exec(fmt.Sprintf("USE [%s]; CREATE VIEW Sales.vwActiveCustomers AS SELECT CustomerID, FirstName, LastName, Email FROM dbo.Customers WHERE IsActive = 1", dbName))
	fmt.Println("Created [Sales].[vwActiveCustomers]")

	// ── 11. Sequence ──────────────────────────────────────────────────────────
	fmt.Println("\n=== Sequence ===")
	noCache := 0
	must(db.CreateSequence(smo.CreateSequenceRequest{
		Schema:     "dbo",
		Name:       "InvoiceNumberSeq",
		DataType:   smo.DataTypeBigInt,
		StartValue: 100000,
		Increment:  1,
		Cache:      &noCache,
	}))
	seqs, err := db.Sequences()
	must(err)
	for _, s := range seqs {
		fmt.Printf("  [%s].[%s]  start=%d  increment=%d\n",
			s.Schema, s.Name, s.StartValue, s.Increment)
	}

	// ── 12. Users & Roles ─────────────────────────────────────────────────────
	fmt.Println("\n=== Logins and Users ===")
	_ = srv.DropLogin("gosmo_app")
	must(srv.CreateLogin("gosmo_app", "Gosmo@pp!2024", &smo.CreateLoginOptions{
		DefaultDatabase: dbName,
	}))
	must(db.CreateUser("gosmo_app", "gosmo_app", "dbo"))
	must(db.AddRoleMember("db_datareader", "gosmo_app"))
	must(db.AddRoleMember("db_datawriter", "gosmo_app"))
	fmt.Println("Login gosmo_app created, mapped to db_datareader + db_datawriter")

	users, err := db.Users()
	must(err)
	for _, u := range users {
		fmt.Printf("  %-20s type=%-20s schema=%s\n", u.Name, u.UserType, u.DefaultSchema)
	}

	// ── 13. Scripter ─────────────────────────────────────────────────────────
	fmt.Println("\n=== Scripting dbo.Customers ===")
	scripter := smo.NewScripter(db, smo.DefaultScriptOptions())
	ddl, err := scripter.ScriptTable("dbo", "Customers")
	must(err)
	// Print first 20 lines only
	lines := strings.Split(ddl, "\n")
	for i, l := range lines {
		if i >= 20 {
			fmt.Println("  ... (truncated)")
			break
		}
		fmt.Println(" ", l)
	}

	// ── 14. Fragmentation stats ───────────────────────────────────────────────
	fmt.Println("\n=== Index fragmentation (dbo.Customers) ===")
	frags, err := custTable.FragmentationStats("LIMITED")
	must(err)
	if len(frags) == 0 {
		fmt.Println("  (no data yet – table is empty)")
	}
	for _, f := range frags {
		fmt.Printf("  %-40s  frag=%.1f%%  pages=%d\n",
			f.IndexName, f.AvgFragmentationPct, f.PageCount)
	}

	// ── 15. Server configuration ──────────────────────────────────────────────
	fmt.Println("\n=== Selected server configuration ===")
	cfgs, err := srv.Configurations()
	must(err)
	want := map[string]bool{
		"max degree of parallelism":      true,
		"max server memory (MB)":         true,
		"optimize for ad hoc workloads":  true,
		"cost threshold for parallelism": true,
	}
	for _, c := range cfgs {
		if want[c.Name] {
			fmt.Printf("  %-40s  value=%d  in_use=%d\n", c.Name, c.Value, c.ValueInUse)
		}
	}

	// ── 16. Active sessions ───────────────────────────────────────────────────
	fmt.Println("\n=== Active sessions ===")
	sessions, err := srv.ActiveSessions(false)
	must(err)
	fmt.Printf("  %d user session(s) active\n", len(sessions))

	// ── 17. Agent jobs ────────────────────────────────────────────────────────
	fmt.Println("\n=== SQL Server Agent Jobs ===")
	jobs, err := srv.Jobs()
	must(err)
	if len(jobs) == 0 {
		fmt.Println("  (no jobs defined)")
	}
	for _, j := range jobs {
		status := "enabled"
		if !j.IsEnabled {
			status = "disabled"
		}
		fmt.Printf("  %-40s  %s\n", j.Name, status)
	}

	// ── 18. Partition function ────────────────────────────────────────────────
	fmt.Println("\n=== Partition function ===")
	must(db.CreatePartitionFunction(smo.CreatePartitionFunctionRequest{
		Name:       "pf_OrderDate",
		InputType:  smo.DataTypeDate,
		IsRight:    true,
		Boundaries: []string{"'2023-01-01'", "'2024-01-01'", "'2025-01-01'"},
	}))
	pfs, err := db.PartitionFunctions()
	must(err)
	for _, pf := range pfs {
		fmt.Printf("  [%s]  input=%s  boundaries=%d\n",
			pf.Name, pf.InputType, pf.BoundaryCount)
	}

	// ── 19. Database mail profiles ────────────────────────────────────────────
	fmt.Println("\n=== Database Mail profiles ===")
	profs, err := srv.MailProfiles()
	must(err)
	if len(profs) == 0 {
		fmt.Println("  (no mail profiles configured)")
	}
	for _, p := range profs {
		def := ""
		if p.IsDefault {
			def = " [default]"
		}
		fmt.Printf("  %s%s\n", p.Name, def)
	}

	// ── 20. Cleanup ───────────────────────────────────────────────────────────
	fmt.Printf("\n=== Cleanup ===\n")
	must(srv.DropDatabase(dbName, true))
	_ = srv.DropLogin("gosmo_app")
	fmt.Printf("Dropped database [%s] and login gosmo_app\n", dbName)
	fmt.Println("\nDone.")
}

// ── helpers ───────────────────────────────────────────────────────────────────

func must(err error) {
	if err != nil {
		log.Fatalf("ERROR: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// scriptColType is a small helper so example doesn't import unexported function.
func scriptColType(col *smo.Column) string {
	switch col.DataType {
	case smo.DataTypeNVarChar, smo.DataTypeNChar:
		if col.MaxLength == -1 {
			return fmt.Sprintf("%s(MAX)", col.DataType)
		}
		return fmt.Sprintf("%s(%d)", col.DataType, col.MaxLength/2)
	case smo.DataTypeVarChar, smo.DataTypeChar:
		if col.MaxLength == -1 {
			return fmt.Sprintf("%s(MAX)", col.DataType)
		}
		return fmt.Sprintf("%s(%d)", col.DataType, col.MaxLength)
	case smo.DataTypeDecimal, smo.DataTypeNumeric:
		return fmt.Sprintf("%s(%d,%d)", col.DataType, col.Precision, col.Scale)
	case smo.DataTypeDatetime2, smo.DataTypeTime:
		if col.Scale > 0 {
			return fmt.Sprintf("%s(%d)", col.DataType, col.Scale)
		}
	}
	return string(col.DataType)
}
