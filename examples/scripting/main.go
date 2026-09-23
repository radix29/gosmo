// Command scripting demonstrates the two ways gosmo produces T-SQL text
// instead of changing the server:
//
//   - Scripter reverse-engineers DDL for objects that already exist —
//     SSMS's "Script Table as > CREATE To".
//
//   - WithScript intercepts writes you are about to make and collects the
//     statements instead of executing them — SSMS's "Script Action to New
//     Query Window" button on a properties dialog.
//
// Run it with:
//
//	MSSQL_SERVER=localhost:1433 MSSQL_USER=sa MSSQL_PASSWORD=YourPw go run ./examples/scripting
package main

import (
	"context"
	"fmt"
	"strings"

	gosmo "github.com/radix29/gosmo"
	"github.com/radix29/gosmo/examples/internal/demo"
)

const dbName = "GoSMOScriptDemo"

func main() {
	// First, so it runs after the cleanup deferred below it.
	defer demo.Exit()

	srv := demo.Connect()
	defer srv.Close()

	db, drop := demo.TempDatabase(srv, dbName)
	defer drop()

	ctx := context.Background()
	demo.Must(db.CreateSchema(ctx, "Sales", "dbo"))
	demo.Must(db.CreateTable(ctx, gosmo.CreateTableRequest{
		Schema: "Sales",
		Name:   "Invoice",
		Columns: []gosmo.ColumnDefinition{
			{Name: "InvoiceID", DataType: gosmo.DataTypeInt, IsIdentity: true, IdentitySeed: 1, IdentityIncr: 1, IsPrimaryKey: true},
			{Name: "CustomerName", DataType: gosmo.DataTypeNVarChar, MaxLength: 200, IsNullable: false},
			{Name: "Notes", DataType: gosmo.DataTypeNVarChar, MaxLength: -1, IsNullable: true}, // -1 = MAX
			{Name: "Amount", DataType: gosmo.DataTypeDecimal, Precision: new(19), Scale: new(4), IsNullable: false, DefaultValue: "0"},
			{Name: "Issued", DataType: gosmo.DataTypeDatetime2, Scale: new(7), IsNullable: false, DefaultValue: "sysutcdatetime()"},
		},
	}))
	inv := demo.Value(db.TableByName(ctx, "Sales", "Invoice"))
	demo.Must(inv.CreateIndex(ctx, gosmo.CreateIndexRequest{
		Name:            "IX_Invoice_Customer",
		Type:            gosmo.IndexTypeNonClustered,
		KeyColumns:      []gosmo.IndexColumnDef{{Name: "CustomerName"}, {Name: "Issued", Descending: true}},
		IncludedColumns: []string{"Amount"},
	}))
	// CreateStoredProcedure's body is the T-SQL after AS; gosmo writes the
	// CREATE OR ALTER PROCEDURE header itself.
	demo.Must(db.CreateStoredProcedure(ctx, "Sales", "InvoiceTotals", `
BEGIN
    SET NOCOUNT ON;
    SELECT CustomerName, SUM(Amount) AS Total
    FROM   Sales.Invoice
    GROUP  BY CustomerName;
END`))

	// -- Default options ---------------------------------------------------
	demo.Section("ScriptTable, default options")
	sc := gosmo.NewScripter(db, gosmo.DefaultScriptOptions())
	fmt.Println(indent(demo.Value(sc.ScriptTable(ctx, "Sales", "Invoice"))))

	// -- Every option turned up -------------------------------------------
	//
	// IncludeIfNotExists guards each statement separately, never as one block
	// spanning several: a BEGIN...END wrapping GO separators would be split
	// across batches and could not parse.
	demo.Section("ScriptTable, headers + IF NOT EXISTS guards")
	verbose := gosmo.NewScripter(db, gosmo.ScriptOptions{
		IncludeHeaders:     true,
		IncludeIfNotExists: true,
		SchemaQualify:      true,
		AnsiPadding:        true,
	})
	fmt.Println(indent(demo.Value(verbose.ScriptTable(ctx, "Sales", "Invoice"))))

	// -- DROP instead of CREATE -------------------------------------------
	demo.Section("ScriptTable, ScriptDrops")
	dropper := gosmo.NewScripter(db, gosmo.ScriptOptions{ScriptDrops: true, SchemaQualify: true})
	fmt.Println(indent(demo.Value(dropper.ScriptTable(ctx, "Sales", "Invoice"))))

	// -- Modules come back verbatim ---------------------------------------
	//
	// ScriptView/StoredProcedure/Function return the definition stored in
	// sys.sql_modules as-is, so IncludeHeaders and IncludeIfNotExists have
	// nothing to add to them — those two options apply to synthesized DDL.
	demo.Section("ScriptStoredProcedure")
	fmt.Println(indent(demo.Value(sc.ScriptStoredProcedure(ctx, "Sales", "InvoiceTotals"))))

	// -- The whole database ------------------------------------------------
	demo.Section("ScriptDatabase (line count only)")
	whole := demo.Value(sc.ScriptDatabase(ctx))
	fmt.Printf("  %d lines, %d bytes\n", strings.Count(whole, "\n")+1, len(whole))

	// -- Collect writes instead of running them ---------------------------
	//
	// Under a WithScript context every write method records its statement
	// and returns success without touching the server. Note two things:
	//
	//   - Use srv.DatabaseRef(name), not srv.DatabaseByName(name). The latter
	//     queries sys.databases for metadata; the lightweight handle is the
	//     one that works when nothing is meant to reach the server.
	//   - A write "succeeds" without happening, so any state you derive from
	//     it is wrong — a rename followed by a re-read by the new name finds
	//     nothing. gosmo.Scripting(ctx) is how you detect that case.
	demo.Section("WithScript: collect, don't execute")
	ctx, script := gosmo.WithScript(ctx)
	fmt.Printf("  gosmo.Scripting(ctx) = %t\n\n", gosmo.Scripting(ctx))

	pending := srv.DatabaseRef(dbName)
	demo.Must(pending.CreateSchema(ctx, "Archive", "dbo"))
	demo.Must(pending.CreateTable(ctx, gosmo.CreateTableRequest{
		Schema: "Archive",
		Name:   "InvoiceHistory",
		Columns: []gosmo.ColumnDefinition{
			{Name: "InvoiceID", DataType: gosmo.DataTypeInt, IsNullable: false, IsPrimaryKey: true},
			{Name: "ArchivedAt", DataType: gosmo.DataTypeDatetime2, Scale: new(0), IsNullable: false},
		},
	}))
	demo.Must(pending.GrantDatabasePermission(ctx, "SELECT", "public", gosmo.PermissionOptions{}))
	demo.Must(pending.SetDatabaseOption(ctx, gosmo.DBOptAutoShrink, "OFF", gosmo.TerminationNone))
	demo.Must(pending.SetRecoveryModel(ctx, gosmo.RecoveryModelBulkLogged))

	// String renders the capture as one runnable script: each statement in a
	// batch of its own, with a USE wherever the database changes.
	fmt.Printf("%s\n", indent(script.String()))

	// Nothing above reached the server: the Archive schema does not exist.
	names := make([]string, 0)
	for _, s := range demo.Value(db.Schemas(ctx)) {
		names = append(names, s.Name)
	}
	fmt.Printf("  schemas actually in [%s]: %s\n", dbName, strings.Join(names, ", "))
}

// indent prefixes every line with two spaces so scripted output reads as a
// block against the surrounding log.
func indent(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n  ")
}
