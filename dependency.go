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
	// IsCallerDependent marks a reference resolved by name rather than by the
	// catalog: a one-part name whose schema SQL Server binds at run time from
	// the caller's default schema (an unqualified EXEC target). The edge is the
	// object the module's owner would get, which another caller may not.
	IsCallerDependent bool
}

// Dependencies returns the objects that schema.name's own definition
// references — SSMS's "Object Dependencies > Objects on which ... depends".
//
// A caller-dependent reference — EXEC dep_b with no schema — is stored with
// referenced_id NULL, so a join on it alone dropped the edge (Dependents had
// the mirror-image bug). Such a reference is resolved here by name, the way
// the module's owner calling it would: the referencing object's own schema
// first, then dbo; it comes back with IsCallerDependent set. A reference to
// an object that does not exist resolves to nothing and is left out, as
// before. Several references to one object are one edge: schema-bound if any
// is, caller-dependent only if every one is.
func (d *Database) Dependencies(ctx context.Context, schema, name string) ([]*Dependency, error) {
	if err := requireSchema("dependencies", schema, name); err != nil {
		return nil, err
	}
	const q = `
SELECT SCHEMA_NAME(o.schema_id), o.name, o.type_desc,
       CAST(MAX(CAST(sed.is_schema_bound_reference AS int)) AS bit),
       CAST(MIN(CASE WHEN sed.referenced_id IS NULL THEN 1 ELSE 0 END) AS bit)
FROM   sys.sql_expression_dependencies sed
JOIN   sys.objects o ON o.object_id = COALESCE(sed.referenced_id,
           CASE WHEN sed.referenced_schema_name IS NULL
                 AND sed.referenced_database_name IS NULL
                 AND sed.referenced_server_name IS NULL
           THEN COALESCE(
               OBJECT_ID(QUOTENAME(OBJECT_SCHEMA_NAME(sed.referencing_id)) + N'.' + QUOTENAME(sed.referenced_entity_name)),
               OBJECT_ID(N'[dbo].' + QUOTENAME(sed.referenced_entity_name)))
           END)
WHERE  sed.referencing_id = OBJECT_ID(@p1)
  AND  sed.referenced_class = 1
GROUP  BY o.schema_id, o.name, o.type_desc
ORDER  BY o.name`
	return d.dependencyEdges(ctx, q, schema, name)
}

// Dependents returns the objects whose own definition references
// schema.name — SSMS's "Object Dependencies > Objects that depend on ...".
//
// On SQL Server, the list comes from sys.dm_sql_referencing_entities, not from joining
// sys.sql_expression_dependencies on referenced_id: a caller-dependent
// reference — an unqualified EXEC target, resolved against the caller's
// default schema at run time — is stored with referenced_id NULL, so that
// join missed every procedure calling schema.name without its schema. The
// expression-dependency row still says whether the reference is schema-bound
// (a caller-dependent one never is). IsCallerDependent follows Dependencies'
// rule — set only when no reference names schema.name exactly — where the
// server's own is_caller_dependent is set by any one unqualified reference.
// referenced_class = 1 keeps a type or XML schema collection whose id equals
// schema.name's object id out.
//
// Fabric Warehouse has no such DMV (Msg 15871), so there the list joins
// sys.sql_expression_dependencies instead, resolving a NULL referenced_id with
// Dependencies' schema-then-dbo rule. That finds a caller-dependent reference
// only when it resolves to schema.name for the referencing object's owner, as
// Dependencies would report it from the other side.
func (d *Database) Dependents(ctx context.Context, schema, name string) ([]*Dependency, error) {
	if err := requireSchema("dependents", schema, name); err != nil {
		return nil, err
	}
	if d.server.IsFabricDW() {
		const q = `
SELECT SCHEMA_NAME(o.schema_id), o.name, o.type_desc,
       CAST(MAX(CAST(sed.is_schema_bound_reference AS int)) AS bit),
       CAST(MIN(CASE WHEN sed.referenced_id IS NULL THEN 1 ELSE 0 END) AS bit)
FROM   sys.sql_expression_dependencies sed
JOIN   sys.objects o ON o.object_id = sed.referencing_id
WHERE  COALESCE(sed.referenced_id,
           CASE WHEN sed.referenced_schema_name IS NULL
                 AND sed.referenced_database_name IS NULL
                 AND sed.referenced_server_name IS NULL
           THEN COALESCE(
               OBJECT_ID(QUOTENAME(OBJECT_SCHEMA_NAME(sed.referencing_id)) + N'.' + QUOTENAME(sed.referenced_entity_name)),
               OBJECT_ID(N'[dbo].' + QUOTENAME(sed.referenced_entity_name)))
           END) = OBJECT_ID(@p1)
  AND  sed.referencing_class = 1
  AND  sed.referenced_class = 1
GROUP  BY o.schema_id, o.name, o.type_desc
ORDER  BY o.name`
		return d.dependencyEdges(ctx, q, schema, name)
	}
	const q = `
SELECT DISTINCT SCHEMA_NAME(o.schema_id), o.name, o.type_desc,
       CAST(CASE WHEN EXISTS (
           SELECT 1
           FROM   sys.sql_expression_dependencies sed
           WHERE  sed.referencing_id = r.referencing_id
             AND  sed.referenced_id = OBJECT_ID(@p1)
             AND  sed.referenced_class = 1
             AND  sed.is_schema_bound_reference = 1
       ) THEN 1 ELSE 0 END AS bit),
       CAST(CASE WHEN r.is_caller_dependent = 1 AND NOT EXISTS (
           SELECT 1
           FROM   sys.sql_expression_dependencies sed
           WHERE  sed.referencing_id = r.referencing_id
             AND  sed.referenced_id = OBJECT_ID(@p1)
             AND  sed.referenced_class = 1
       ) THEN 1 ELSE 0 END AS bit)
FROM   sys.dm_sql_referencing_entities(@p1, N'OBJECT') r
JOIN   sys.objects o ON o.object_id = r.referencing_id
WHERE  r.referencing_class = 1
ORDER  BY o.name`
	return d.dependencyEdges(ctx, q, schema, name)
}

func (d *Database) dependencyEdges(ctx context.Context, q, schema, name string) ([]*Dependency, error) {
	ref := qualifiedName(schema, name)
	rows, err := d.query(ctx, q, ref)
	return scanRows(rows, err, fmt.Sprintf("dependencies for %s", ref), func(scan func(...any) error) (*Dependency, error) {
		dep := &Dependency{}
		if err := scan(&dep.Schema, &dep.Name, &dep.TypeDesc, &dep.IsSchemaBound, &dep.IsCallerDependent); err != nil {
			return nil, err
		}
		return dep, nil
	})
}
