package gosmo

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The handle fold of 2026-09-25 moved every Drop/Rename/Transfer that lived
// on the parent as Database.XxxObject(ctx, schema, name, …) onto the object's
// own handle. These pin the statement each handle method builds, so the move
// changed where the call lives and nothing about what it sends.
func TestSchemaObjectHandleStatements(t *testing.T) {
	d := (&Server{}).DatabaseRef("AppDB")
	cases := []struct {
		name string
		act  func(context.Context) error
		want []string
	}{
		{"View.Drop", func(c context.Context) error { return d.ViewRef("s]1", "v").Drop(c) }, []string{"DROP VIEW [s]]1].[v]"}},
		{"StoredProcedure.Drop", func(c context.Context) error { return d.StoredProcedureRef("dbo", "p").Drop(c) }, []string{"DROP PROCEDURE [dbo].[p]"}},
		{"UserDefinedFunction.Drop", func(c context.Context) error { return d.UserDefinedFunctionRef("dbo", "f").Drop(c) }, []string{"DROP FUNCTION [dbo].[f]"}},
		{"Trigger.Drop", func(c context.Context) error { return d.TriggerRef("dbo", "tr").Drop(c) }, []string{"DROP TRIGGER [dbo].[tr]"}},
		{"Table.Drop", func(c context.Context) error { return d.TableRef("dbo", "t").Drop(c, false) }, []string{"DROP TABLE [dbo].[t]"}},
		{"Table.Truncate", func(c context.Context) error { return d.TableRef("dbo", "t").Truncate(c) }, []string{"TRUNCATE TABLE [dbo].[t]"}},

		{"Table.Rename", func(c context.Context) error { return d.TableRef("dbo", "t").Rename(c, "t2") }, []string{"sp_rename", "N'[dbo].[t]'", "N't2'", "N'OBJECT'"}},
		{"View.Rename", func(c context.Context) error { return d.ViewRef("dbo", "v").Rename(c, "v2") }, []string{"sp_rename", "N'[dbo].[v]'", "N'v2'", "N'OBJECT'"}},
		{"StoredProcedure.Rename", func(c context.Context) error { return d.StoredProcedureRef("dbo", "p").Rename(c, "p2") }, []string{"sp_rename", "N'[dbo].[p]'", "N'OBJECT'"}},
		{"UserDefinedFunction.Rename", func(c context.Context) error { return d.UserDefinedFunctionRef("dbo", "f").Rename(c, "f2") }, []string{"sp_rename", "N'[dbo].[f]'", "N'OBJECT'"}},
		{"Trigger.Rename", func(c context.Context) error { return d.TriggerRef("dbo", "tr").Rename(c, "tr2") }, []string{"sp_rename", "N'[dbo].[tr]'", "N'OBJECT'"}},
		{"Sequence.Rename", func(c context.Context) error { return d.SequenceRef("dbo", "s").Rename(c, "s2") }, []string{"sp_rename", "N'[dbo].[s]'", "N'OBJECT'"}},
		{"Synonym.Rename", func(c context.Context) error { return d.SynonymRef("dbo", "sy").Rename(c, "sy2") }, []string{"sp_rename", "N'[dbo].[sy]'", "N'OBJECT'"}},
		{"Rule.Rename", func(c context.Context) error { return d.RuleRef("dbo", "r").Rename(c, "r2") }, []string{"sp_rename", "N'[dbo].[r]'", "N'OBJECT'"}},
		{"Default.Rename", func(c context.Context) error { return d.DefaultRef("dbo", "df").Rename(c, "df2") }, []string{"sp_rename", "N'[dbo].[df]'", "N'OBJECT'"}},
		{"UserDefinedDataType.Rename", func(c context.Context) error { return d.UserDefinedDataTypeRef("dbo", "Phone").Rename(c, "Tel") }, []string{"sp_rename", "N'[dbo].[Phone]'", "N'Tel'", "N'USERDATATYPE'"}},
		// A constraint is addressed in its table's schema, not by the table.
		{"Table.RenameConstraint", func(c context.Context) error { return d.TableRef("s", "t").RenameConstraint(c, "FK_a", "FK_b") }, []string{"sp_rename", "N'[s].[FK_a]'", "N'FK_b'", "N'OBJECT'"}},

		{"Table.Transfer", func(c context.Context) error { return d.TableRef("dbo", "t").Transfer(c, "arch") }, []string{"ALTER SCHEMA [arch] TRANSFER [dbo].[t]"}},
		{"View.Transfer", func(c context.Context) error { return d.ViewRef("dbo", "v").Transfer(c, "arch") }, []string{"ALTER SCHEMA [arch] TRANSFER [dbo].[v]"}},
		{"StoredProcedure.Transfer", func(c context.Context) error { return d.StoredProcedureRef("dbo", "p").Transfer(c, "arch") }, []string{"ALTER SCHEMA [arch] TRANSFER [dbo].[p]"}},
		{"UserDefinedFunction.Transfer", func(c context.Context) error { return d.UserDefinedFunctionRef("dbo", "f").Transfer(c, "arch") }, []string{"ALTER SCHEMA [arch] TRANSFER [dbo].[f]"}},
		{"Sequence.Transfer", func(c context.Context) error { return d.SequenceRef("dbo", "s").Transfer(c, "arch") }, []string{"ALTER SCHEMA [arch] TRANSFER [dbo].[s]"}},
		{"Synonym.Transfer", func(c context.Context) error { return d.SynonymRef("dbo", "sy").Transfer(c, "arch") }, []string{"ALTER SCHEMA [arch] TRANSFER [dbo].[sy]"}},
		{"Rule.Transfer", func(c context.Context) error { return d.RuleRef("dbo", "r").Transfer(c, "arch") }, []string{"ALTER SCHEMA [arch] TRANSFER [dbo].[r]"}},
		{"Default.Transfer", func(c context.Context) error { return d.DefaultRef("dbo", "df").Transfer(c, "arch") }, []string{"ALTER SCHEMA [arch] TRANSFER [dbo].[df]"}},
		{"BrokerQueue.Transfer", func(c context.Context) error { return d.BrokerQueueRef("dbo", "q").Transfer(c, "arch") }, []string{"ALTER SCHEMA [arch] TRANSFER [dbo].[q]"}},
		{"UserDefinedDataType.Transfer", func(c context.Context) error { return d.UserDefinedDataTypeRef("dbo", "t").Transfer(c, "arch") }, []string{"ALTER SCHEMA [arch] TRANSFER TYPE::[dbo].[t]"}},
		{"UserDefinedTableType.Transfer", func(c context.Context) error { return d.UserDefinedTableTypeRef("dbo", "t").Transfer(c, "arch") }, []string{"ALTER SCHEMA [arch] TRANSFER TYPE::[dbo].[t]"}},
		{"ClrType.Transfer", func(c context.Context) error { return d.ClrTypeRef("dbo", "t").Transfer(c, "arch") }, []string{"ALTER SCHEMA [arch] TRANSFER TYPE::[dbo].[t]"}},
		{"XMLSchemaCollection.Transfer", func(c context.Context) error { return d.XMLSchemaCollectionRef("dbo", "x").Transfer(c, "arch") }, []string{"ALTER SCHEMA [arch] TRANSFER XML SCHEMA COLLECTION::[dbo].[x]"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, col := WithScript(context.Background())
			if err := tc.act(ctx); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if len(col.Statements()) != 1 {
				t.Fatalf("got %d statements, want 1: %v", len(col.Statements()), col.Statements())
			}
			got := col.Statements()[0]
			if !strings.HasPrefix(got, useAppDB) {
				t.Errorf("statement is not batched behind the database's USE:\n%s", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("statement lacks %q:\n%s", w, got)
				}
			}
		})
	}
}

// Rename and Transfer mirror the change onto the handle through
// setIfApplied: under WithScript nothing ran, so the handle must still name
// the object the server has.
func TestSchemaObjectHandleIsNotMutatedWhileScripting(t *testing.T) {
	ctx, _ := WithScript(context.Background())
	d := (&Server{}).DatabaseRef("AppDB")

	v := d.ViewRef("dbo", "v")
	if err := v.Rename(ctx, "v2"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := v.Transfer(ctx, "arch"); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if v.Schema != "dbo" || v.Name != "v" {
		t.Errorf("view handle is [%s].[%s] after scripted writes, want [dbo].[v]", v.Schema, v.Name)
	}

	if err := d.Rename(ctx, "AppDB2", false); err != nil {
		t.Fatalf("Database.Rename: %v", err)
	}
	if d.Name != "AppDB" {
		t.Errorf("Database.Name = %q after a scripted rename, want AppDB", d.Name)
	}
}

// A new name and a target schema are both required, and refused before a
// statement is built.
func TestSchemaObjectHandleRefusesEmptyArguments(t *testing.T) {
	ctx, col := WithScript(context.Background())
	d := (&Server{}).DatabaseRef("AppDB")
	if err := d.ViewRef("dbo", "v").Rename(ctx, ""); err == nil {
		t.Error("Rename to an empty name succeeded")
	}
	if err := d.TableRef("dbo", "t").RenameConstraint(ctx, "", "x"); err == nil {
		t.Error("RenameConstraint of an empty name succeeded")
	}
	if err := d.SequenceRef("dbo", "s").Transfer(ctx, ""); !errors.Is(err, ErrSchemaRequired) {
		t.Errorf("Transfer to an empty schema: err = %v, want ErrSchemaRequired", err)
	}
	if err := d.SequenceRef("dbo", "s").Transfer(ctx, "dbo"); err == nil {
		t.Error("Transfer into the schema the object is already in succeeded")
	}
	if n := len(col.Statements()); n != 0 {
		t.Errorf("refusals captured %d statements: %v", n, col.Statements())
	}
}

// The database's own Drop, Rename and Detach address it by the handle's
// name; they were Server.DropDatabase/RenameDatabase/DetachDatabase.
func TestDatabaseHandleWrites(t *testing.T) {
	cases := []struct {
		name string
		act  func(context.Context, *Database) error
		want string
	}{
		{"Drop", func(c context.Context, d *Database) error { return d.Drop(c, false) }, "DROP DATABASE [odd]]db]"},
		{"Rename", func(c context.Context, d *Database) error { return d.Rename(c, "new", false) }, "ALTER DATABASE [odd]]db] MODIFY NAME = [new]"},
		{"Detach", func(c context.Context, d *Database) error { return d.Detach(c, DetachOptions{}) }, "sp_detach_db @dbname = N'odd]db'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, col := WithScript(context.Background())
			if err := tc.act(ctx, (&Server{}).DatabaseRef("odd]db")); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if len(col.Statements()) != 1 || !strings.Contains(col.Statements()[0], tc.want) {
				t.Errorf("got %v, want one statement containing %q", col.Statements(), tc.want)
			}
		})
	}
}
