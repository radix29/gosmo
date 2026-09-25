//go:build livedb

// Live verification of the 2026-09-25 handle fold: every Rename, Transfer
// and Drop that moved from Database.XxxObject(ctx, schema, name, …) onto the
// object's own handle is run against a real server, and the catalog is read
// back to prove the object is where the handle says it is.
//
// The statements are the ones the parent forms sent, and schema_object_test
// pins their text; what only a server answers is that each class keyword and
// securable prefix is the right one for its family — sp_rename's OBJECT
// class for a constraint, TRANSFER TYPE:: for a table type — and that the
// handle mirrors the change onto itself.
//
//	go test -tags livedb . -run TestLiveHandleFold -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
package gosmo

import (
	"context"
	"database/sql"
	"testing"
)

func TestLiveHandleFold(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_handle_fold_live")
	defer drop()

	exec := func(stmt string) {
		t.Helper()
		if _, err := d.exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	for _, stmt := range []string{
		"CREATE SCHEMA [arch]",
		"CREATE TABLE [dbo].[T] (id INT NOT NULL CONSTRAINT [PK_T] PRIMARY KEY, n INT NULL CONSTRAINT [CK_T_n] CHECK (n > 0))",
		"CREATE TABLE [dbo].[C] (id INT NOT NULL CONSTRAINT [FK_C_T] REFERENCES [dbo].[T](id))",
		"CREATE VIEW [dbo].[V] AS SELECT id FROM [dbo].[T]",
		"CREATE PROCEDURE [dbo].[P] AS SELECT 1",
		"CREATE FUNCTION [dbo].[F]() RETURNS INT AS BEGIN RETURN 1 END",
		"CREATE TRIGGER [dbo].[TR] ON [dbo].[T] AFTER INSERT AS SET NOCOUNT ON",
		"CREATE SEQUENCE [dbo].[S] AS INT",
		"CREATE SYNONYM [dbo].[SY] FOR [dbo].[T]",
		"CREATE RULE [dbo].[R] AS @v > 0",
		"CREATE DEFAULT [dbo].[DF] AS 0",
		"CREATE TYPE [dbo].[Phone] FROM NVARCHAR(20)",
		"CREATE TYPE [dbo].[TT] AS TABLE (a INT)",
		"CREATE XML SCHEMA COLLECTION [dbo].[X] AS N'<xsd:schema xmlns:xsd=\"http://www.w3.org/2001/XMLSchema\"><xsd:element name=\"e\" type=\"xsd:string\"/></xsd:schema>'",
	} {
		exec(stmt)
	}

	// objectAt reports whether [schema].[name] is in sys.objects (or, for a
	// type or XML schema collection, where OBJECT_ID finds nothing, in
	// sys.types / sys.xml_schema_collections).
	objectAt := func(schema, name string) bool {
		t.Helper()
		var n int
		err := d.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&n) }, `
SELECT (SELECT COUNT(*) FROM sys.objects WHERE SCHEMA_NAME(schema_id) = @p1 AND name = @p2)
     + (SELECT COUNT(*) FROM sys.types WHERE is_user_defined = 1 AND SCHEMA_NAME(schema_id) = @p1 AND name = @p2)
     + (SELECT COUNT(*) FROM sys.xml_schema_collections WHERE SCHEMA_NAME(schema_id) = @p1 AND name = @p2)`, schema, name)
		if err != nil {
			t.Fatalf("look up [%s].[%s]: %v", schema, name, err)
		}
		return n > 0
	}
	check := func(what string, err error, schema, name string) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: %v", what, err)
			return
		}
		if !objectAt(schema, name) {
			t.Errorf("%s: [%s].[%s] is not in the catalog afterwards", what, schema, name)
		}
	}

	type renameTransfer interface {
		Rename(context.Context, string) error
		Transfer(context.Context, string) error
	}
	families := []struct {
		name string
		h    renameTransfer
	}{
		{"V", d.ViewRef("dbo", "V")},
		{"P", d.StoredProcedureRef("dbo", "P")},
		{"F", d.UserDefinedFunctionRef("dbo", "F")},
		{"S", d.SequenceRef("dbo", "S")},
		{"SY", d.SynonymRef("dbo", "SY")},
		{"R", d.RuleRef("dbo", "R")},
		{"DF", d.DefaultRef("dbo", "DF")},
		{"Phone", d.UserDefinedDataTypeRef("dbo", "Phone")},
	}
	for _, f := range families {
		newName := f.name + "2"
		check(f.name+" Rename", f.h.Rename(ctx, newName), "dbo", newName)
		check(f.name+" Transfer", f.h.Transfer(ctx, "arch"), "arch", newName)
	}

	check("TT Transfer", d.UserDefinedTableTypeRef("dbo", "TT").Transfer(ctx, "arch"), "arch", "TT")
	check("X Transfer", d.XMLSchemaCollectionRef("dbo", "X").Transfer(ctx, "arch"), "arch", "X")

	tr := d.TriggerRef("dbo", "TR")
	check("trigger Rename", tr.Rename(ctx, "TR2"), "dbo", "TR2")
	if tr.Name != "TR2" {
		t.Errorf("trigger handle Name = %q after Rename, want TR2", tr.Name)
	}
	// Dropped here, before its table moves: a DML trigger follows its table
	// into the new schema, so a handle made for [dbo] would miss it after the
	// Transfer below — which is also why Trigger has no Transfer of its own.
	if err := tr.Drop(ctx); err != nil {
		t.Errorf("trigger Drop: %v", err)
	} else if objectAt("dbo", "TR2") {
		t.Error("trigger Drop: [dbo].[TR2] is still in the catalog")
	}

	tbl := d.TableRef("dbo", "T")
	check("RenameConstraint CHECK", tbl.RenameConstraint(ctx, "CK_T_n", "CK_T_n2"), "dbo", "CK_T_n2")
	check("RenameConstraint FK", d.TableRef("dbo", "C").RenameConstraint(ctx, "FK_C_T", "FK_C_T2"), "dbo", "FK_C_T2")
	check("Table Rename", tbl.Rename(ctx, "T2"), "dbo", "T2")
	check("Table Transfer", tbl.Transfer(ctx, "arch"), "arch", "T2")
	if tbl.Schema != "arch" || tbl.Name != "T2" {
		t.Errorf("table handle is [%s].[%s] after Rename and Transfer, want [arch].[T2]", tbl.Schema, tbl.Name)
	}
	if err := tbl.Truncate(ctx); err == nil {
		t.Error("Truncate of a table a foreign key references succeeded; the server refuses it")
	}

	// The drops, through the handles the renames left behind — each has
	// mirrored its new name and schema, so a stale handle would miss.
	for _, dr := range []struct {
		what string
		err  error
		s, n string
	}{
		{"view Drop", families[0].h.(*View).Drop(ctx), "arch", "V2"},
		{"procedure Drop", families[1].h.(*StoredProcedure).Drop(ctx), "arch", "P2"},
		{"function Drop", families[2].h.(*UserDefinedFunction).Drop(ctx), "arch", "F2"},
		{"table Drop cascade", tbl.Drop(ctx, true), "arch", "T2"},
	} {
		if dr.err != nil {
			t.Errorf("%s: %v", dr.what, dr.err)
		} else if objectAt(dr.s, dr.n) {
			t.Errorf("%s: [%s].[%s] is still in the catalog", dr.what, dr.s, dr.n)
		}
	}
}
