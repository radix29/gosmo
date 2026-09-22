package gosmo

import (
	"context"
	"fmt"
)

// ============================================================
// Object dependencies
// ============================================================

// Dependency is one edge in an object dependency graph, as reported by
// sys.sql_expression_dependencies — e.g. a view referencing a table, or a
// stored procedure referencing a function.
type Dependency struct {
	Schema        string
	Name          string
	TypeDesc      string // e.g. "USER_TABLE", "VIEW", "SQL_STORED_PROCEDURE"
	IsSchemaBound bool
}

// Dependencies returns the objects that schema.name's own definition
// references — SSMS's "Object Dependencies > Objects on which ... depends".
func (d *Database) Dependencies(ctx context.Context, schema, name string) ([]*Dependency, error) {
	const q = `
SELECT DISTINCT SCHEMA_NAME(o.schema_id), o.name, o.type_desc, sed.is_schema_bound_reference
FROM   sys.sql_expression_dependencies sed
JOIN   sys.objects o ON o.object_id = sed.referenced_id
WHERE  sed.referencing_id = OBJECT_ID(@p1)
ORDER  BY o.name`
	return d.dependencyEdges(ctx, q, schema, name)
}

// Dependents returns the objects whose own definition references
// schema.name — SSMS's "Object Dependencies > Objects that depend on ...".
func (d *Database) Dependents(ctx context.Context, schema, name string) ([]*Dependency, error) {
	const q = `
SELECT DISTINCT SCHEMA_NAME(o.schema_id), o.name, o.type_desc, sed.is_schema_bound_reference
FROM   sys.sql_expression_dependencies sed
JOIN   sys.objects o ON o.object_id = sed.referencing_id
WHERE  sed.referenced_id = OBJECT_ID(@p1)
ORDER  BY o.name`
	return d.dependencyEdges(ctx, q, schema, name)
}

func (d *Database) dependencyEdges(ctx context.Context, q, schema, name string) ([]*Dependency, error) {
	ref := qualifiedName(schema, name)
	rows, err := d.query(ctx, q, ref)
	return scanRows(rows, err, fmt.Sprintf("dependencies for %s", ref), func(scan func(...any) error) (*Dependency, error) {
		dep := &Dependency{}
		if err := scan(&dep.Schema, &dep.Name, &dep.TypeDesc, &dep.IsSchemaBound); err != nil {
			return nil, err
		}
		return dep, nil
	})
}
