// Command examples is a guided tour of gosmo: connect, inspect the instance,
// build a throwaway database with schemas, tables, indexes, a sequence and a
// procedure, script it, then drop it again.
//
// It is the "everything at a glance" example. The sibling directories go
// deeper on one subject each:
//
//	./backup     BACKUP/RESTORE, backup headers, progress reporting
//	./bulkcopy   loading rows with Database.BulkInsert
//	./diagnostic execution plans, ExecProc, SQLError, DMV reads
//	./iterators  the *Seq iterator API and context cancellation
//	./jobs       SQL Server Agent: jobs, steps, schedules, operators, alerts
//	./maintain   indexes, fragmentation, statistics, files, Query Store
//	./scripting  the Scripter, and WithScript's "collect, don't execute" mode
//	./security   logins, users, roles and permissions
//
// Every example reads the same environment variables — see package demo.
//
// Minimal (SQL auth):
//
//	MSSQL_SERVER=localhost:1433 MSSQL_USER=sa MSSQL_PASSWORD=YourPw go run ./examples
//
// Azure SQL with Managed Identity:
//
//	MSSQL_SERVER=myserver.database.windows.net MSSQL_AUTH=msi go run ./examples
//
// Azure SQL with Service Principal:
//
//	MSSQL_SERVER=myserver.database.windows.net \
//	MSSQL_AUTH=sp \
//	AZURE_TENANT_ID=<tid> \
//	AZURE_CLIENT_ID=<cid> \
//	AZURE_CLIENT_SECRET=<secret> \
//	go run ./examples
package main

import (
	"fmt"
	"strings"

	gosmo "github.com/radix29/gosmo"
	"github.com/radix29/gosmo/examples/internal/demo"
)

func main() {
	// First, so it runs after the cleanup deferred below it.
	defer demo.Exit()

	srv := demo.Connect()
	defer srv.Close()

	info := srv.Info()
	fmt.Printf("Connected to  : %s\n", info.Name)
	fmt.Printf("Edition       : %s\n", info.Edition)
	fmt.Printf("Version       : %s (%d.%d.%d)\n",
		info.ProductLevel, info.VersionMajor, info.VersionMinor, info.VersionBuild)
	fmt.Printf("Platform      : %s\n", info.Platform)
	fmt.Printf("Memory / CPUs : %d MB / %d\n", info.PhysicalMemoryMB, info.LogicalCPUCount)
	fmt.Printf("Default paths : data=%s log=%s backup=%s\n",
		info.DefaultDataPath, info.DefaultLogPath, info.DefaultBackupPath)

	// -- List databases ---------------------------------------------------
	demo.Section("Databases")
	for _, d := range demo.Value(srv.Databases()) {
		fmt.Printf("  %-30s state=%-10s recovery=%-12s compat=%d\n",
			d.Name(), d.State(), d.RecoveryModel(), d.CompatibilityLevel())
	}

	// -- Create demo database ---------------------------------------------
	db, drop := demo.TempDatabase(srv, "GoSMODemo")
	defer drop()

	// -- Extended property ------------------------------------------------
	demo.Must(db.AddExtendedProperty("MS_Description", "GoSMO demo database",
		gosmo.ExtendedPropertyLevel{}))

	// -- Schemas ----------------------------------------------------------
	demo.Section("Schemas")
	demo.Must(db.CreateSchema("Sales", "dbo"))
	demo.Must(db.CreateSchema("HR", "dbo"))
	for _, s := range demo.Value(db.Schemas()) {
		fmt.Printf("  [%s] owner=%s\n", s.Name, s.Owner)
	}

	// -- Tables -----------------------------------------------------------
	demo.Section("Tables")
	demo.Must(db.CreateTable(gosmo.CreateTableRequest{
		Schema: "dbo",
		Name:   "Customers",
		Columns: []gosmo.ColumnDefinition{
			{Name: "CustomerID", DataType: gosmo.DataTypeInt, IsNullable: false, IsIdentity: true, IdentitySeed: 1, IdentityIncr: 1, IsPrimaryKey: true},
			{Name: "FirstName", DataType: gosmo.DataTypeNVarChar, MaxLength: 100, IsNullable: false},
			{Name: "LastName", DataType: gosmo.DataTypeNVarChar, MaxLength: 100, IsNullable: false},
			{Name: "Email", DataType: gosmo.DataTypeNVarChar, MaxLength: 255, IsNullable: true},
			{Name: "CreatedAt", DataType: gosmo.DataTypeDatetime2, Scale: 3, IsNullable: false, DefaultValue: "sysdatetime()"},
			{Name: "IsActive", DataType: gosmo.DataTypeBit, IsNullable: false, DefaultValue: "1"},
		},
	}))
	demo.Must(db.CreateTable(gosmo.CreateTableRequest{
		Schema: "Sales",
		Name:   "Orders",
		Columns: []gosmo.ColumnDefinition{
			{Name: "OrderID", DataType: gosmo.DataTypeBigInt, IsNullable: false, IsIdentity: true, IdentitySeed: 1000, IdentityIncr: 1, IsPrimaryKey: true},
			{Name: "CustomerID", DataType: gosmo.DataTypeInt, IsNullable: false},
			{Name: "OrderDate", DataType: gosmo.DataTypeDate, IsNullable: false, DefaultValue: "CAST(SYSDATETIME() AS DATE)"},
			{Name: "TotalAmount", DataType: gosmo.DataTypeDecimal, Precision: 18, Scale: 2, IsNullable: false, DefaultValue: "0"},
			{Name: "Status", DataType: gosmo.DataTypeVarChar, MaxLength: 20, IsNullable: false, DefaultValue: "'PENDING'"},
		},
	}))
	for _, t := range demo.Value(db.Tables()) {
		rc, _ := t.RowCount()
		fmt.Printf("  %s  rows=%d\n", t.FullName(), rc)
	}

	// -- Columns ----------------------------------------------------------
	demo.Section("Columns of dbo.Customers")
	cust := demo.Value(db.TableByName("dbo", "Customers"))
	for _, c := range demo.Value(cust.Columns()) {
		null := "NOT NULL"
		if c.IsNullable {
			null = "NULL"
		}
		// ColumnTypeString renders the type the way SSMS does, resolving
		// MaxLength/Precision/Scale into "nvarchar(100)", "decimal(18,2)", ...
		fmt.Printf("  %-12s %-16s %s\n", c.Name, gosmo.ColumnTypeString(c), null)
	}

	// -- Index -----------------------------------------------------------
	demo.Section("Indexes")
	demo.Must(cust.CreateIndex(gosmo.CreateIndexRequest{
		Name: "IX_Customers_LastName",
		Type: gosmo.IndexTypeNonClustered,
		KeyColumns: []gosmo.IndexColumnDef{
			{Name: "LastName"},
			{Name: "FirstName"},
		},
		IncludedColumns: []string{"Email"},
		FillFactor:      90,
	}))
	demo.Must(cust.CreateIndex(gosmo.CreateIndexRequest{
		Name:             "UIX_Customers_Email",
		Type:             gosmo.IndexTypeNonClustered,
		IsUnique:         true,
		KeyColumns:       []gosmo.IndexColumnDef{{Name: "Email"}},
		FilterDefinition: "Email IS NOT NULL",
	}))
	for _, idx := range demo.Value(cust.Indexes()) {
		fmt.Printf("  %-24s %-16s unique=%-5t keys=%d\n",
			idx.Name, idx.Type, idx.IsUnique, len(idx.KeyColumns))
	}

	// -- Sequence --------------------------------------------------------
	demo.Section("Sequence")
	noCache := 0
	demo.Must(db.CreateSequence(gosmo.CreateSequenceRequest{
		Schema:     "dbo",
		Name:       "InvoiceSeq",
		DataType:   gosmo.DataTypeBigInt,
		StartValue: 100000,
		Increment:  1,
		Cache:      &noCache,
	}))
	for _, s := range demo.Value(db.Sequences()) {
		fmt.Printf("  [%s].[%s] start=%d incr=%d\n", s.Schema, s.Name, s.StartValue, s.Increment)
	}

	// -- Synonym ----------------------------------------------------------
	demo.Section("Synonym")
	demo.Must(db.CreateSynonym("dbo", "Cust", "dbo.Customers"))
	for _, syn := range demo.Value(db.Synonyms()) {
		fmt.Printf("  [%s].[%s] -> %s\n", syn.Schema, syn.Name, syn.BaseObject)
	}

	// -- Stored procedure ------------------------------------------------
	// The body is the T-SQL *after* AS — gosmo writes the CREATE OR ALTER
	// PROCEDURE header itself.
	demo.Section("Stored Procedure")
	demo.Must(db.CreateStoredProcedure("dbo", "RecentOrders", `
BEGIN
    SET NOCOUNT ON;
    SELECT TOP (100) o.OrderID, o.OrderDate, o.TotalAmount, o.Status
    FROM   Sales.Orders o
    ORDER  BY o.OrderDate DESC;
END`))
	fmt.Println("  [dbo].[RecentOrders] created")

	// -- Dependencies -----------------------------------------------------
	demo.Section("Dependencies of dbo.RecentOrders")
	for _, dep := range demo.Value(db.Dependencies("dbo", "RecentOrders")) {
		fmt.Printf("  references [%s].[%s] (%s)\n", dep.Schema, dep.Name, dep.TypeDesc)
	}

	// -- Scripter --------------------------------------------------------
	demo.Section("DDL Script (first 15 lines of dbo.Customers)")
	sc := gosmo.NewScripter(db, gosmo.DefaultScriptOptions())
	for i, line := range strings.Split(demo.Value(sc.ScriptTable("dbo", "Customers")), "\n") {
		if i >= 15 {
			fmt.Println("  ...")
			break
		}
		fmt.Println(" ", line)
	}

	// -- Partition function ----------------------------------------------
	demo.Section("Partition Function")
	demo.Must(db.CreatePartitionFunction(gosmo.CreatePartitionFunctionRequest{
		Name:       "pf_OrderDate",
		InputType:  gosmo.DataTypeDate,
		IsRight:    true,
		Boundaries: []string{"'2023-01-01'", "'2024-01-01'", "'2025-01-01'"},
	}))
	for _, pf := range demo.Value(db.PartitionFunctions()) {
		fmt.Printf("  [%s] input=%s boundaries=%d\n", pf.Name, pf.InputType, pf.BoundaryCount)
	}

	// -- Space used -------------------------------------------------------
	demo.Section("Space used")
	space := demo.Value(db.SpaceUsed())
	fmt.Printf("  total=%.2f MB  data=%.2f MB  log=%.2f MB  unallocated=%.2f MB\n",
		space.TotalMB, space.DataMB, space.LogMB, space.UnallocatedMB)

	// -- Server config ---------------------------------------------------
	demo.Section("Server configuration (selected)")
	want := map[string]bool{
		"max degree of parallelism":      true,
		"max server memory (MB)":         true,
		"optimize for ad hoc workloads":  true,
		"cost threshold for parallelism": true,
	}
	for _, c := range demo.Value(srv.Configurations()) {
		if want[c.Name] {
			fmt.Printf("  %-40s value=%-8d in_use=%d\n", c.Name, c.Value, c.ValueInUse)
		}
	}

	// -- Active sessions -------------------------------------------------
	demo.Section("Active sessions")
	fmt.Printf("  %d user session(s) active\n", len(demo.Value(srv.ActiveSessions(false))))

	// -- Agent jobs (read-only) ------------------------------------------
	demo.Section("SQL Server Agent Jobs")
	jobs := demo.Value(srv.Jobs())
	if len(jobs) == 0 {
		fmt.Println("  (no jobs defined)")
	}
	for _, j := range jobs {
		state := "enabled"
		if !j.IsEnabled {
			state = "disabled"
		}
		fmt.Printf("  %-40s %s\n", j.Name, state)
	}

	fmt.Println("\nDone.")
}
