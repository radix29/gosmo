package gosmo

import (
	"context"
	"database/sql/driver"
	"errors"
	"slices"
	"strings"
	"testing"
)

// dependencyReads are the two directions of the dependency graph, which share
// their argument binding and scan through dependencyEdges.
var dependencyReads = []struct {
	name string
	read func(d *Database, schema, name string) ([]*Dependency, error)
}{
	{"Dependencies", func(d *Database, schema, name string) ([]*Dependency, error) {
		return d.Dependencies(context.Background(), schema, name)
	}},
	{"Dependents", func(d *Database, schema, name string) ([]*Dependency, error) {
		return d.Dependents(context.Background(), schema, name)
	}},
	{"FabricDependents", func(d *Database, schema, name string) ([]*Dependency, error) {
		d.server.IsFabricDW = true
		return d.Dependents(context.Background(), schema, name)
	}},
}

// TestDependencyReadsRefuseAnEmptySchema pins that neither read guesses a
// schema: OBJECT_ID of a one-part name resolves against the connection's
// default schema, which is not necessarily the caller's.
func TestDependencyReadsRefuseAnEmptySchema(t *testing.T) {
	for _, r := range dependencyReads {
		t.Run(r.name, func(t *testing.T) {
			d := qsRecDB(t, 17, nil, nil)
			if _, err := r.read(d, "", "orders"); !errors.Is(err, ErrSchemaRequired) {
				t.Fatalf("err = %v, want ErrSchemaRequired", err)
			}
			if calls := qsRec.recorded(); len(calls) != 0 {
				t.Errorf("a refused read reached the driver: %q", calls[0].sql)
			}
		})
	}
}

// TestDependencyReadsBindTheQuotedNameAndScanInOrder pins the one argument
// both queries take — the bracket-quoted two-part name OBJECT_ID needs, with
// a ] in it doubled — and the order of the two bit columns, which a swap
// would turn into schema-bound edges reported as caller-dependent.
func TestDependencyReadsBindTheQuotedNameAndScanInOrder(t *testing.T) {
	cols := []string{"schema", "name", "type_desc", "is_schema_bound", "is_caller_dependent"}
	rows := [][]driver.Value{
		{"dbo", "orders", "USER_TABLE", true, false},
		{"app", "get_order", "SQL_STORED_PROCEDURE", false, true},
	}
	want := []Dependency{
		{Schema: "dbo", Name: "orders", TypeDesc: "USER_TABLE", IsSchemaBound: true},
		{Schema: "app", Name: "get_order", TypeDesc: "SQL_STORED_PROCEDURE", IsCallerDependent: true},
	}
	for _, r := range dependencyReads {
		t.Run(r.name, func(t *testing.T) {
			d := qsRecDB(t, 17, cols, rows)
			got, err := r.read(d, "sa]les", "v")
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if args := qsRec.last(t).args; !slices.Equal(args, []any{"[sa]]les].[v]"}) {
				t.Errorf("bound %v, want [[sa]]les].[v]]", args)
			}
			if len(got) != len(want) {
				t.Fatalf("got %d edges, want %d", len(got), len(want))
			}
			for i := range want {
				if *got[i] != want[i] {
					t.Errorf("edge %d = %+v, want %+v", i, *got[i], want[i])
				}
			}
		})
	}
}

func TestDependentsUsesFabricCatalogOnlyOnFabricDW(t *testing.T) {
	for _, fabric := range []bool{false, true} {
		name := "SQLServer"
		if fabric {
			name = "FabricDW"
		}
		t.Run(name, func(t *testing.T) {
			d := qsRecDB(t, 17, nil, nil)
			d.server.IsFabricDW = fabric
			if _, err := d.Dependents(t.Context(), "dbo", "orders"); err != nil {
				t.Fatal(err)
			}
			q := strings.Join(strings.Fields(qsRec.last(t).sql), " ")
			if strings.Contains(q, "sys.dm_sql_referencing_entities") == fabric {
				t.Errorf("fabric=%v: incorrect dependency source:\n%s", fabric, q)
			}
			if fabric {
				for _, clause := range []string{
					"JOIN sys.objects o ON o.object_id = sed.referencing_id",
					"COALESCE(sed.referenced_id,",
					"sed.referenced_schema_name IS NULL",
					"sed.referenced_database_name IS NULL",
					"sed.referenced_server_name IS NULL",
					"OBJECT_SCHEMA_NAME(sed.referencing_id)",
					"OBJECT_ID(N'[dbo].' + QUOTENAME(sed.referenced_entity_name))",
					"= OBJECT_ID(@p1)",
					"sed.referencing_class = 1",
					"sed.referenced_class = 1",
				} {
					if !strings.Contains(q, clause) {
						t.Errorf("Fabric dependency lookup lost %q:\n%s", clause, q)
					}
				}
			}
		})
	}
}
