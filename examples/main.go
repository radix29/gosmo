// Package main demonstrates every authentication method and major feature of
// the gosmo library. Set the environment variables documented below before
// running.
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
	"log"
	"os"
	"strings"

	gosmo "github.com/radix29/gosmo"
)

func main() {
	srv := mustConnect()
	defer srv.Close()

	info := srv.Info()
	fmt.Printf("Connected to  : %s\n", info.Name)
	fmt.Printf("Edition       : %s\n", info.Edition)
	fmt.Printf("Version       : %s (%d.%d.%d)\n",
		info.ProductLevel, info.VersionMajor, info.VersionMinor, info.VersionBuild)
	fmt.Printf("Memory / CPUs : %d MB / %d\n", info.PhysicalMemoryMB, info.LogicalCPUCount)

	// -- List databases ---------------------------------------------------
	fmt.Println("\n=== Databases ===")
	dbs, err := srv.Databases()
	must(err)
	for _, d := range dbs {
		fmt.Printf("  %-30s state=%-10s recovery=%-12s compat=%d\n",
			d.Name(), d.State(), d.RecoveryModel(), d.CompatibilityLevel())
	}

	// -- Create demo database ---------------------------------------------
	const dbName = "GoSMODemo"
	fmt.Printf("\n=== Creating database [%s] ===\n", dbName)
	_ = srv.DropDatabase(dbName, true)
	must(srv.CreateDatabase(dbName, &gosmo.CreateDatabaseOptions{
		RecoveryModel: gosmo.RecoveryModelSimple,
		CompatLevel:   gosmo.CompatLevel2019,
	}))

	db, err := srv.DatabaseByName(dbName)
	must(err)

	// -- Extended property ------------------------------------------------
	must(db.AddExtendedProperty("MS_Description", "GoSMO demo database",
		gosmo.ExtendedPropertyLevel{}))

	// -- Schemas ----------------------------------------------------------
	fmt.Println("\n=== Schemas ===")
	must(db.CreateSchema("Sales", "dbo"))
	must(db.CreateSchema("HR", "dbo"))
	schemas, err := db.Schemas()
	must(err)
	for _, s := range schemas {
		fmt.Printf("  [%s] owner=%s\n", s.Name, s.Owner)
	}

	// -- Tables -----------------------------------------------------------
	fmt.Println("\n=== Tables ===")
	must(db.CreateTable(gosmo.CreateTableRequest{
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
	must(db.CreateTable(gosmo.CreateTableRequest{
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
	tables, err := db.Tables()
	must(err)
	for _, t := range tables {
		rc, _ := t.RowCount()
		fmt.Printf("  %s  rows=%d\n", t.FullName(), rc)
	}

	// -- Index -----------------------------------------------------------
	fmt.Println("\n=== Indexes ===")
	cust, err := db.TableByName("dbo", "Customers")
	must(err)
	must(cust.CreateIndex(gosmo.CreateIndexRequest{
		Name: "IX_Customers_LastName",
		Type: gosmo.IndexTypeNonClustered,
		KeyColumns: []gosmo.IndexColumnDef{
			{Name: "LastName"},
			{Name: "FirstName"},
		},
		FillFactor: 90,
	}))
	must(cust.CreateIndex(gosmo.CreateIndexRequest{
		Name:             "UIX_Customers_Email",
		Type:             gosmo.IndexTypeNonClustered,
		IsUnique:         true,
		KeyColumns:       []gosmo.IndexColumnDef{{Name: "Email"}},
		FilterDefinition: "Email IS NOT NULL",
	}))
	fmt.Println("  Indexes created")

	// -- Sequence --------------------------------------------------------
	fmt.Println("\n=== Sequence ===")
	noCache := 0
	must(db.CreateSequence(gosmo.CreateSequenceRequest{
		Schema:     "dbo",
		Name:       "InvoiceSeq",
		DataType:   gosmo.DataTypeBigInt,
		StartValue: 100000,
		Increment:  1,
		Cache:      &noCache,
	}))
	seqs, err := db.Sequences()
	must(err)
	for _, s := range seqs {
		fmt.Printf("  [%s].[%s] start=%d incr=%d\n", s.Schema, s.Name, s.StartValue, s.Increment)
	}

	// -- Stored procedure ------------------------------------------------
	fmt.Println("\n=== Stored Procedure ===")
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
	fmt.Println("  [dbo].[GetCustomerOrders] created")

	// -- Scripter --------------------------------------------------------
	fmt.Println("\n=== DDL Script (first 15 lines of dbo.Customers) ===")
	sc := gosmo.NewScripter(db, gosmo.DefaultScriptOptions())
	ddl, err := sc.ScriptTable("dbo", "Customers")
	must(err)
	for i, line := range strings.Split(ddl, "\n") {
		if i >= 15 {
			fmt.Println("  ...")
			break
		}
		fmt.Println(" ", line)
	}

	// -- Partition function ----------------------------------------------
	fmt.Println("\n=== Partition Function ===")
	must(db.CreatePartitionFunction(gosmo.CreatePartitionFunctionRequest{
		Name:       "pf_OrderDate",
		InputType:  gosmo.DataTypeDate,
		IsRight:    true,
		Boundaries: []string{"'2023-01-01'", "'2024-01-01'", "'2025-01-01'"},
	}))
	pfs, err := db.PartitionFunctions()
	must(err)
	for _, pf := range pfs {
		fmt.Printf("  [%s] input=%s boundaries=%d\n", pf.Name, pf.InputType, pf.BoundaryCount)
	}

	// -- Server config ---------------------------------------------------
	fmt.Println("\n=== Server configuration (selected) ===")
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
			fmt.Printf("  %-40s value=%-8d in_use=%d\n", c.Name, c.Value, c.ValueInUse)
		}
	}

	// -- Active sessions -------------------------------------------------
	fmt.Println("\n=== Active sessions ===")
	sessions, err := srv.ActiveSessions(false)
	must(err)
	fmt.Printf("  %d user session(s) active\n", len(sessions))

	// -- Agent jobs (read-only) ------------------------------------------
	fmt.Println("\n=== SQL Server Agent Jobs ===")
	jobs, err := srv.Jobs()
	must(err)
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

	// -- Cleanup ---------------------------------------------------------
	fmt.Printf("\n=== Cleanup ===\n")
	must(srv.DropDatabase(dbName, true))
	fmt.Printf("Dropped [%s]\n", dbName)
	fmt.Println("\nDone.")
}

// -- Connection factory -------------------------------------------------------

// mustConnect reads environment variables and calls gosmo.Connect.
// Supported MSSQL_AUTH values:
//
//	""  / "sql"   - SQL Server auth (default)
//	"windows"     - Windows/Kerberos auth (on-premises)
//	"msi"         - Managed Identity (system-assigned)
//	"msi-user"    - Managed Identity (user-assigned, needs AZURE_CLIENT_ID)
//	"sp"          - Service Principal (needs AZURE_TENANT_ID, AZURE_CLIENT_ID,
//	                  AZURE_CLIENT_SECRET)
//	"sp-cert"     - Service Principal + certificate (needs AZURE_TENANT_ID,
//	                  AZURE_CLIENT_ID, AZURE_CLIENT_CERT_PATH)
//	"default"     - DefaultAzureCredential chain
//	"azcli"       - Azure CLI credential
//	"azd"         - Azure Developer CLI credential
//	"password"    - Entra user + password
//	"interactive" - Browser interactive sign-in
//	"devicecode"  - Device-code flow
func mustConnect() *gosmo.Server {
	server := envOr("MSSQL_SERVER", "localhost:1433")
	authStr := strings.ToLower(envOr("MSSQL_AUTH", "sql"))
	database := envOr("MSSQL_DATABASE", "master")

	opts := gosmo.ConnectionOptions{
		Server:                 server,
		Database:               database,
		TrustServerCertificate: os.Getenv("MSSQL_TRUST_CERT") == "true",
	}

	switch authStr {
	case "", "sql":
		opts.Auth = gosmo.AuthSQLServer
		opts.User = envOr("MSSQL_USER", "sa")
		opts.Password = os.Getenv("MSSQL_PASSWORD")
		fmt.Printf("Auth: SQL Server (%s)\n", opts.User)

	case "windows":
		opts.Auth = gosmo.AuthWindows
		fmt.Println("Auth: Windows / Kerberos")

	case "msi":
		opts.Auth = gosmo.AuthEntraMSI
		fmt.Println("Auth: Managed Identity (system-assigned)")

	case "msi-user":
		opts.Auth = gosmo.AuthEntraMSI
		opts.ClientID = mustEnv("AZURE_CLIENT_ID")
		fmt.Printf("Auth: Managed Identity (user-assigned: %s)\n", opts.ClientID)

	case "sp":
		opts.Auth = gosmo.AuthEntraServicePrincipal
		opts.TenantID = mustEnv("AZURE_TENANT_ID")
		opts.User = mustEnv("AZURE_CLIENT_ID")
		opts.Password = mustEnv("AZURE_CLIENT_SECRET")
		fmt.Printf("Auth: Service Principal (client %s)\n", opts.User)

	case "sp-cert":
		opts.Auth = gosmo.AuthEntraServicePrincipal
		opts.TenantID = mustEnv("AZURE_TENANT_ID")
		opts.User = mustEnv("AZURE_CLIENT_ID")
		opts.ClientCertPath = mustEnv("AZURE_CLIENT_CERT_PATH")
		opts.ClientCertPassword = os.Getenv("AZURE_CLIENT_CERT_PASSWORD")
		fmt.Printf("Auth: Service Principal + certificate (client %s)\n", opts.User)

	case "default":
		opts.Auth = gosmo.AuthEntraDefault
		fmt.Println("Auth: DefaultAzureCredential chain")

	case "azcli":
		opts.Auth = gosmo.AuthEntraAzCLI
		fmt.Println("Auth: Azure CLI (az login)")

	case "azd":
		opts.Auth = gosmo.AuthEntraAzureDeveloperCLI
		fmt.Println("Auth: Azure Developer CLI (azd auth login)")

	case "password":
		opts.Auth = gosmo.AuthEntraPassword
		opts.User = mustEnv("AZURE_USER")
		opts.Password = mustEnv("AZURE_PASSWORD")
		fmt.Printf("Auth: Entra password (%s)\n", opts.User)

	case "interactive":
		opts.Auth = gosmo.AuthEntraInteractive
		opts.ApplicationClientID = os.Getenv("AZURE_APPLICATION_CLIENT_ID")
		fmt.Println("Auth: Entra interactive (browser)")

	case "devicecode":
		opts.Auth = gosmo.AuthEntraDeviceCode
		opts.ApplicationClientID = os.Getenv("AZURE_APPLICATION_CLIENT_ID")
		fmt.Println("Auth: Entra device code")

	default:
		log.Fatalf("unknown MSSQL_AUTH value: %q", authStr)
	}

	srv, err := gosmo.Connect(opts)
	must(err)
	return srv
}

// -- Helpers ------------------------------------------------------------------

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

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required environment variable %s is not set", key)
	}
	return v
}
