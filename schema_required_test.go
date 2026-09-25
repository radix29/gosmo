package gosmo

import (
	"context"
	"errors"
	"testing"
)

// An empty schema used to mean two things: dbo to DropSequence and ~20 like
// it, the caller's own default schema to DropTable, which emitted the name
// unqualified. A login whose default schema was sales got sales.t from
// DropTable("", "t") and dbo.s from DropSequence("", "s"). It now means
// nothing — every call given one refuses it with ErrSchemaRequired before a
// statement is built — and these pin that across the three ways a schema
// reaches gosmo: a parameter, a request field, and a handle's Schema.
//
// Each runs under WithScript on a Database with no pool behind it, so a call
// that got past the refusal would either capture a statement or fail on the
// missing connection; both are reported.
func TestEmptySchemaIsRefused(t *testing.T) {
	cases := []struct {
		name string
		call func(context.Context, *Database) error
	}{
		// The plan's two examples, which disagreed.
		{"Table.Drop", func(c context.Context, d *Database) error { return d.TableRef("", "t").Drop(c, false) }},
		{"Sequence handle Drop", func(c context.Context, d *Database) error { return d.SequenceRef("", "s").Drop(c) }},

		{"View.Drop", func(c context.Context, d *Database) error { return d.ViewRef("", "v").Drop(c) }},
		{"UserDefinedFunction.Drop", func(c context.Context, d *Database) error { return d.UserDefinedFunctionRef("", "f").Drop(c) }},
		{"StoredProcedure.Drop", func(c context.Context, d *Database) error { return d.StoredProcedureRef("", "p").Drop(c) }},
		{"Trigger.Drop", func(c context.Context, d *Database) error { return d.TriggerRef("", "tr").Drop(c) }},
		{"Synonym handle Drop", func(c context.Context, d *Database) error { return d.SynonymRef("", "s").Drop(c) }},
		{"Rule handle Drop", func(c context.Context, d *Database) error { return d.RuleRef("", "r").Drop(c) }},
		{"Default handle Drop", func(c context.Context, d *Database) error { return d.DefaultRef("", "df").Drop(c) }},
		{"BrokerQueue handle Drop", func(c context.Context, d *Database) error { return d.BrokerQueueRef("", "q").Drop(c) }},
		{"alias type handle Drop", func(c context.Context, d *Database) error { return d.UserDefinedDataTypeRef("", "t").Drop(c) }},
		{"table type handle Drop", func(c context.Context, d *Database) error { return d.UserDefinedTableTypeRef("", "t").Drop(c) }},
		{"CLR type handle Drop", func(c context.Context, d *Database) error { return d.ClrTypeRef("", "t").Drop(c) }},
		{"XML schema collection handle Drop", func(c context.Context, d *Database) error {
			return d.XMLSchemaCollectionRef("", "x").Drop(c)
		}},
		{"Sequence handle Restart", func(c context.Context, d *Database) error { return d.SequenceRef("", "s").Restart(c, 1) }},

		// A TableRef's writes all go through Table.exec.
		{"Table handle DropColumn", func(c context.Context, d *Database) error { return d.TableRef("", "t").DropColumn(c, "c") }},
		{"Table handle Truncate", func(c context.Context, d *Database) error { return d.TableRef("", "t").Truncate(c) }},
		{"Index on a table handle", func(c context.Context, d *Database) error { return d.TableRef("", "t").IndexRef("ix").Drop(c) }},
		{"Statistic on a table handle", func(c context.Context, d *Database) error {
			return d.TableRef("", "t").StatisticRef("st").Drop(c)
		}},

		{"View.Rename", func(c context.Context, d *Database) error { return d.ViewRef("", "a").Rename(c, "b") }},
		{"Transfer source", func(c context.Context, d *Database) error { return d.TableRef("", "t").Transfer(c, "archive") }},
		{"Transfer target", func(c context.Context, d *Database) error { return d.TableRef("dbo", "t").Transfer(c, "") }},
		{"GrantPermission", func(c context.Context, d *Database) error {
			return d.GrantPermission(c, "", "t", PermSelect, "u", PermissionOptions{})
		}},
		{"BrokerQueue.Alter activation procedure", func(c context.Context, d *Database) error {
			return d.BrokerQueueRef("dbo", "q").Alter(c, QueueSettings{Activation: &QueueActivation{ProcedureName: "p"}})
		}},

		{"CreateTable", func(c context.Context, d *Database) error {
			return errOnly(d.CreateTable(c, CreateTableRequest{Name: "t", Columns: []ColumnDefinition{{Name: "a", DataType: DataTypeInt}}}))
		}},
		{"CreateSequence", func(c context.Context, d *Database) error {
			return errOnly(d.CreateSequence(c, CreateSequenceRequest{Name: "s"}))
		}},
		{"CreateSynonym", func(c context.Context, d *Database) error {
			return errOnly(d.CreateSynonym(c, CreateSynonymRequest{Name: "s", BaseObject: "[db].[dbo].[t]"}))
		}},
		{"CreateStoredProcedure", func(c context.Context, d *Database) error {
			return errOnly(d.CreateStoredProcedure(c, CreateStoredProcedureRequest{Name: "p", Body: "SELECT 1"}))
		}},

		// Reads refuse too, before a query: an empty schema would otherwise
		// find nothing, or the wrong object.
		{"TableByName", func(c context.Context, d *Database) error { return errOnly(d.TableByName(c, "", "t")) }},
		{"SequenceByName", func(c context.Context, d *Database) error { return errOnly(d.SequenceByName(c, "", "s")) }},
		{"StoredProcedureByName", func(c context.Context, d *Database) error {
			return errOnly(d.StoredProcedureByName(c, "", "p"))
		}},
		{"ScriptTable", func(c context.Context, d *Database) error {
			return errOnly(NewScripter(d, ScriptOptions{}).ScriptTable(c, "", "t"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Database{server: &Server{}, Name: "AppDB"}
			ctx, script := WithScript(context.Background())
			err := tc.call(ctx, d)
			if !errors.Is(err, ErrSchemaRequired) {
				t.Fatalf("err = %v, want ErrSchemaRequired", err)
			}
			if got := script.Statements(); len(got) != 0 {
				t.Errorf("captured %q, want nothing", got)
			}
		})
	}
}
