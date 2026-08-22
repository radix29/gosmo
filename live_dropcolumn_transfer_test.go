//go:build livedb

// Live verification of Table.DropColumn and Database.TransferObject, the two
// writes added for gossms's Object Explorer column Delete and Move to Schema.
//
// Both are one statement whose text a unit test already pins, so what only a
// server can answer is what happens around them: DROP COLUMN is refused while
// a default constraint depends on the column (the case a UI has to report
// rather than work around), and ALTER SCHEMA TRANSFER moves the object while
// keeping its object_id, which is what makes it a move and not a re-create.
//
//	go test -tags livedb . -run TestLiveDropColumn -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
package gosmo

import (
	"strings"
	"testing"
)

func TestLiveDropColumnAndTransferObject(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_dropcol_live")
	defer drop()

	exec := func(stmt string) {
		t.Helper()
		if _, err := d.exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	exec("CREATE SCHEMA [arch]")
	exec("CREATE TABLE [dbo].[Orders] (id INT NOT NULL, spare NVARCHAR(50) NULL, flagged BIT NOT NULL CONSTRAINT [DF_Orders_flagged] DEFAULT (0))")

	tbl := d.Table("dbo", "Orders")

	// The plain case: a column nothing depends on goes.
	if err := tbl.DropColumnContext(ctx, "spare"); err != nil {
		t.Fatalf("DropColumnContext(spare): %v", err)
	}
	cols, err := d.ObjectColumnsContext(ctx, "dbo", "Orders")
	if err != nil {
		t.Fatalf("ObjectColumnsContext: %v", err)
	}
	for _, c := range cols {
		if c.Name == "spare" {
			t.Errorf("column spare survived the drop")
		}
	}

	// The refusal, which is the documented behaviour rather than a bug: a
	// column with a default constraint on it cannot be dropped until that
	// constraint is.
	//
	// The server's message does *not* name the constraint — verified here on
	// SQL Server 17: "ALTER TABLE DROP COLUMN flagged failed because one or
	// more objects access this column." A UI cannot pass the error through and
	// expect the user to know what to drop first, which is why gossms's own
	// warning says what the classes of blocker are rather than promising the
	// server will name one.
	err = tbl.DropColumnContext(ctx, "flagged")
	if err == nil {
		t.Fatal("dropping a column with a default constraint succeeded; expected the server to refuse")
	}
	if !strings.Contains(err.Error(), "one or more objects access this column") {
		t.Errorf("refusal = %v, want the server's dependency refusal", err)
	}
	if strings.Contains(err.Error(), "DF_Orders_flagged") {
		t.Errorf("the server now names the blocking constraint (%v) — gossms's warning can say so", err)
	}
	if err := tbl.DropConstraintContext(ctx, "DF_Orders_flagged"); err != nil {
		t.Fatalf("DropConstraintContext: %v", err)
	}
	if err := tbl.DropColumnContext(ctx, "flagged"); err != nil {
		t.Errorf("DropColumnContext after dropping the constraint: %v", err)
	}

	// The transfer: same object, different schema.
	before, err := d.TableByNameContext(ctx, "dbo", "Orders")
	if err != nil {
		t.Fatalf("TableByNameContext before the transfer: %v", err)
	}
	if err := d.TransferObjectContext(ctx, "arch", "dbo", "Orders"); err != nil {
		t.Fatalf("TransferObjectContext: %v", err)
	}
	after, err := d.TableByNameContext(ctx, "arch", "Orders")
	if err != nil {
		t.Fatalf("TableByNameContext after the transfer: %v", err)
	}
	if after.ObjectID != before.ObjectID {
		t.Errorf("object_id = %d after the transfer, want %d — the object was replaced, not moved",
			after.ObjectID, before.ObjectID)
	}
	if _, err := d.TableByNameContext(ctx, "dbo", "Orders"); err == nil {
		t.Errorf("the table is still in dbo as well")
	}

	// Transferring back into the schema it is already in is refused before
	// anything reaches the server.
	if err := d.TransferObjectContext(ctx, "arch", "arch", "Orders"); err == nil {
		t.Errorf("a same-schema transfer was accepted")
	}
}
