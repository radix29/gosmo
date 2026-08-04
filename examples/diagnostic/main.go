// Command diagnostic demonstrates the parts of gosmo you reach for when
// something is wrong or you are exploring an unfamiliar instance: structured
// SQL Server errors, the transient-failure test, execution plans, calling
// procedures with output parameters, object search and dependency graphs,
// the bulk catalog snapshot, and the server-health DMV reads.
//
//	MSSQL_SERVER=localhost:1433 MSSQL_USER=sa MSSQL_PASSWORD=YourPw go run ./examples/diagnostic
package main

import (
	"fmt"
	"strings"

	gosmo "github.com/radix29/gosmo"
	"github.com/radix29/gosmo/examples/internal/demo"
)

const dbName = "GoSMODiagnosticDemo"

func main() {
	// First, so it runs after the cleanup deferred below it.
	defer demo.Exit()

	srv := demo.Connect()
	defer srv.Close()

	db, drop := demo.TempDatabase(srv, dbName)
	defer drop()

	demo.Must(db.CreateSchema("Sales", "dbo"))
	demo.Must(db.CreateTable(gosmo.CreateTableRequest{
		Schema: "Sales",
		Name:   "Order",
		Columns: []gosmo.ColumnDefinition{
			{Name: "OrderID", DataType: gosmo.DataTypeInt, IsIdentity: true, IdentitySeed: 1, IdentityIncr: 1, IsPrimaryKey: true},
			{Name: "CustomerID", DataType: gosmo.DataTypeInt, IsNullable: false},
			{Name: "Total", DataType: gosmo.DataTypeDecimal, Precision: 18, Scale: 2, IsNullable: false},
		},
	}))
	// CreateStoredProcedure's body is the T-SQL *after* AS — gosmo emits the
	// CREATE OR ALTER PROCEDURE header itself, so the procedure it makes
	// takes no parameters.
	demo.Must(db.CreateStoredProcedure("Sales", "CountOrders", `
BEGIN
    SET NOCOUNT ON;
    DECLARE @c INT;
    SELECT @c = COUNT(*) FROM Sales.[Order];
    RETURN @c;
END`))
	_ = demo.Value(db.BulkInsert(gosmo.BulkCopy{
		Schema:  "Sales",
		Table:   "Order",
		Columns: []string{"CustomerID", "Total"},
	}, gosmo.SliceRows([][]any{
		{7, 125.00}, {7, 80.50}, {9, 12.99},
	})))

	// -- Structured errors -------------------------------------------------
	//
	// AsSQLError pulls the "Msg 208, Level 16, State 1, Line 4" detail out
	// of any error the driver produced, so a caller can branch on the error
	// number without importing go-mssqldb. All carries every error the batch
	// raised, first to last, when the server reported more than one.
	demo.Section("SQL errors")
	_, err := db.EstimatedPlan("SELECT * FROM Sales.NoSuchTable")
	if sqlErr, ok := gosmo.AsSQLError(err); ok {
		fmt.Printf("  %s\n", sqlErr.Header())
		fmt.Printf("  number=%d class=%d state=%d line=%d\n",
			sqlErr.Number, sqlErr.Class, sqlErr.State, sqlErr.LineNo)
		fmt.Printf("  message: %s\n", sqlErr.Message)
		fmt.Printf("  is a real error (class > 10): %t\n", sqlErr.IsError())
		if len(sqlErr.All) > 1 {
			fmt.Printf("  the batch raised %d errors in total\n", len(sqlErr.All))
		}
	} else {
		fmt.Printf("  not a SQL Server error: %v\n", err)
	}

	// IsRetryable is the same transient-failure test gosmo's own read helpers
	// use — a dropped pooled connection, a torn TDS stream, a network blip.
	// Only idempotent work is safe to retry on it.
	fmt.Printf("  worth retrying? %t\n", gosmo.IsRetryable(err))

	// -- Calling a procedure -----------------------------------------------
	//
	// The procedure runs as an RPC, which is what makes its RETURN value
	// available at all — ProcResult.ReturnStatus.
	demo.Section("ExecProc and the return status")
	res := demo.Value(db.ExecProc("Sales", "CountOrders"))
	fmt.Printf("  Sales.CountOrders returned %d\n", res.ReturnStatus)

	// In/Out/InOut build the parameter list. Out and InOut carry a pointer
	// the value is written back into, like database/sql's sql.Out; InOut
	// also sends the pointee's current value in. Any result sets the
	// procedure emits are discarded — use the query methods for rows.
	demo.Section("ExecProc with output parameters")
	var orderCount int32
	var total float64
	demo.Value(db.ExecProc("sys", "sp_executesql",
		gosmo.In("stmt", `SELECT @count = COUNT(*), @total = ISNULL(SUM(Total), 0)
		                  FROM Sales.[Order] WHERE CustomerID = @customer`),
		gosmo.In("params", "@customer int, @count int OUTPUT, @total decimal(18,2) OUTPUT"),
		gosmo.In("customer", 7),
		gosmo.Out("count", &orderCount),
		gosmo.Out("total", &total),
	))
	fmt.Printf("  customer 7: %d orders totalling %.2f\n", orderCount, total)

	doubled := int32(21)
	demo.Value(db.ExecProc("sys", "sp_executesql",
		gosmo.In("stmt", "SET @n = @n * 2;"),
		gosmo.In("params", "@n int OUTPUT"),
		gosmo.InOut("n", &doubled),
	))
	fmt.Printf("  InOut sent 21 and got back %d\n", doubled)

	// -- Execution plans ---------------------------------------------------
	//
	// Estimated compiles the statement without running it; Actual runs it
	// and returns the plan with real row counts. Both come back as Showplan
	// XML — the same document SSMS parses to draw its graphical plan.
	demo.Section("Execution plans")
	estimated := demo.Value(db.EstimatedPlan(
		"SELECT CustomerID, SUM(Total) FROM Sales.[Order] GROUP BY CustomerID"))
	actual := demo.Value(db.ActualPlan(
		"SELECT CustomerID, SUM(Total) FROM Sales.[Order] GROUP BY CustomerID"))
	fmt.Printf("  estimated plan: %d bytes of Showplan XML\n", len(estimated.XML))
	fmt.Printf("  actual plan   : %d bytes\n", len(actual.XML))
	for _, op := range physicalOps(actual.XML) {
		fmt.Printf("    operator: %s\n", op)
	}

	// -- Finding objects ---------------------------------------------------
	//
	// Search is a substring match over every table, view, procedure,
	// function and trigger name — it wraps the term in its own wildcards and
	// escapes any % or _ you pass, so give it the bare text, not a pattern.
	demo.Section("Search")
	for _, r := range demo.Value(db.Search("Order")) {
		fmt.Printf("  [%s].[%s] %s\n", r.Schema, r.Name, r.TypeDesc)
	}

	// -- Dependency graph --------------------------------------------------
	//
	// Dependencies is "what does this object reference"; Dependents is the
	// reverse — "what would break if I dropped it".
	demo.Section("Dependencies")
	for _, d := range demo.Value(db.Dependencies("Sales", "CountOrders")) {
		fmt.Printf("  CountOrders references [%s].[%s] (%s, schemabound=%t)\n",
			d.Schema, d.Name, d.TypeDesc, d.IsSchemaBound)
	}
	for _, d := range demo.Value(db.Dependents("Sales", "Order")) {
		fmt.Printf("  [%s].[%s] (%s) depends on Sales.Order\n", d.Schema, d.Name, d.TypeDesc)
	}

	// -- Bulk catalog snapshot ---------------------------------------------
	//
	// Catalog loads every user table and view with its columns in one round
	// trip, rather than a query per object — the shape an autocomplete
	// provider or a schema diff wants. SystemCatalog does the same for the
	// system objects.
	demo.Section("Catalog snapshot")
	cat := demo.Value(db.Catalog())
	fmt.Printf("  %d schemas, %d objects\n", len(cat.Schemas), len(cat.Objects))
	for _, obj := range cat.Objects {
		kind := "table"
		if obj.Type == gosmo.CatalogView {
			kind = "view"
		}
		cols := make([]string, 0, len(obj.Columns))
		for _, c := range obj.Columns {
			cols = append(cols, c.Name)
		}
		fmt.Printf("  %-5s [%s].[%s]: %s\n", kind, obj.Schema, obj.Name, strings.Join(cols, ", "))
	}

	// -- Instance health ---------------------------------------------------
	demo.Section("Memory")
	mem := demo.Value(srv.MemoryStats())
	fmt.Printf("  physical=%d MB available=%d MB target=%d MB in use=%d MB\n",
		mem.PhysicalMemoryMB, mem.AvailableMemoryMB, mem.TargetServerMemoryMB, mem.TotalServerMemoryMB)

	demo.Section("Processors")
	cpu := demo.Value(srv.ProcessorInfo())
	fmt.Printf("  %d logical CPUs, %d NUMA node(s), hyperthread ratio %d\n",
		cpu.CPUCount, cpu.NUMANodeCount, cpu.HyperthreadRatio)

	demo.Section("Sessions")
	sessions := demo.Value(srv.ActiveSessions(false))
	fmt.Printf("  %d user session(s)\n", len(sessions))
	for _, s := range sessions {
		blocked := ""
		if s.BlockingSessionID != 0 {
			blocked = fmt.Sprintf(" blocked by %d (%s, %d ms)", s.BlockingSessionID, s.WaitType, s.WaitTimeMS)
		}
		fmt.Printf("  spid=%-5d %-20s %-16s db=%-16s %s%s\n",
			s.SessionID, s.LoginName, s.ProgramName, s.DatabaseName, s.Status, blocked)
	}
	// Server.KillSession(spid) ends one — not called here, for obvious reasons.

	demo.Section("Error log (last 5 entries)")
	entries := demo.Value(srv.ReadErrorLog(0))
	for _, e := range entries[max(0, len(entries)-5):] {
		fmt.Printf("  %s %-10s %s\n", e.LogDate, e.Process, e.Text)
	}
}

// physicalOps pulls the PhysicalOp attribute values out of a Showplan XML
// document. A real consumer would parse the XML properly; this is enough to
// show the plan is a plan.
func physicalOps(plan string) []string {
	var ops []string
	for rest := plan; ; {
		i := strings.Index(rest, `PhysicalOp="`)
		if i < 0 {
			return ops
		}
		rest = rest[i+len(`PhysicalOp="`):]
		j := strings.IndexByte(rest, '"')
		if j < 0 {
			return ops
		}
		ops = append(ops, rest[:j])
		rest = rest[j+1:]
	}
}
