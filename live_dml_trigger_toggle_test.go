//go:build livedb

// Live round trip of the DML trigger toggle (Trigger.Enable/Disable): the
// statement names the parent, which a catalog-read Trigger carries and a
// TriggerRef looks up. It runs both on a table and on a view, in a schema
// other than dbo, and reads is_disabled back each time.
//
//	go test -tags livedb . -run TestLiveDMLTriggerToggle -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
package gosmo

import (
	"errors"
	"testing"
)

const liveDMLTriggerToggleDB = "gossms_dml_trig_toggle_db"

func TestLiveDMLTriggerToggle(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	d, dropDB := liveScratchDB(t, db, ctx, liveDMLTriggerToggleDB)
	defer dropDB()

	liveExecIn(t, d, ctx,
		"CREATE SCHEMA [Sales.Archive]",
		"CREATE TABLE [Sales.Archive].[o'brien] (id int NOT NULL)",
		"CREATE VIEW [Sales.Archive].V AS SELECT id FROM [Sales.Archive].[o'brien]")
	liveExecIn(t, d, ctx,
		"CREATE TRIGGER [Sales.Archive].[tr]]T] ON [Sales.Archive].[o'brien] AFTER INSERT AS PRINT 'dml';")
	liveExecIn(t, d, ctx,
		"CREATE TRIGGER [Sales.Archive].tr_V ON [Sales.Archive].V INSTEAD OF INSERT AS PRINT 'view dml';")

	enabled := func(name string) bool {
		t.Helper()
		tr, err := d.TriggerByName(ctx, "Sales.Archive", name)
		if err != nil {
			t.Fatalf("TriggerByName %s: %v", name, err)
		}
		return tr.IsEnabled
	}

	// A catalog-read trigger on a table.
	tr, err := d.TriggerByName(ctx, "Sales.Archive", "tr]T")
	if err != nil {
		t.Fatalf("TriggerByName: %v", err)
	}
	if err := tr.Disable(ctx); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if tr.IsEnabled || enabled("tr]T") {
		t.Error("table trigger still enabled after Disable")
	}
	if err := tr.Enable(ctx); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if !tr.IsEnabled || !enabled("tr]T") {
		t.Error("table trigger still disabled after Enable")
	}

	// A handle on a view's trigger: the parent is looked up.
	ref := d.TriggerRef("Sales.Archive", "tr_V")
	if err := ref.SetEnabled(ctx, false); err != nil {
		t.Fatalf("TriggerRef SetEnabled(false): %v", err)
	}
	if enabled("tr_V") {
		t.Error("view trigger still enabled after SetEnabled(false)")
	}
	if err := ref.SetEnabled(ctx, true); err != nil {
		t.Fatalf("TriggerRef SetEnabled(true): %v", err)
	}
	if !enabled("tr_V") {
		t.Error("view trigger still disabled after SetEnabled(true)")
	}

	if err := d.TriggerRef("Sales.Archive", "gone").Disable(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing trigger: err = %v, want ErrNotFound", err)
	}
}
