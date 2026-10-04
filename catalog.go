package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// ============================================================
// Catalog: a single bulk snapshot of every table/view and its columns,
// and of every table-valued function and its result columns, for callers
// (like a SQL editor's autocomplete) that need to inventory a whole database
// up front instead of querying one object at a time via
// Table.Columns/Database.Tables/Database.Views.
// ============================================================

// CatalogObjectType distinguishes a Catalog entry's underlying object kind.
type CatalogObjectType int

const (
	CatalogTable CatalogObjectType = iota
	CatalogView
	// CatalogFunction is a table-valued function — inline (IF), multi-
	// statement (TF) or CLR (FT). Only Catalog.Functions holds these.
	CatalogFunction
)

// CatalogColumn is one column of a CatalogObject — the subset of Column's
// fields relevant to identifying and describing a column, without the
// per-table detail (identity, computed, default, rowguid) that a bulk
// snapshot has no need for.
type CatalogColumn struct {
	Name       string
	DataType   DataType
	MaxLength  int
	Precision  int
	Scale      int
	IsNullable bool
}

// CatalogObject is one table, view or table-valued function and its columns,
// in ordinal order. A function's columns are its result shape, which
// sys.columns records for all three table-valued kinds.
type CatalogObject struct {
	ObjectID int
	Schema   string
	Name     string
	Type     CatalogObjectType
	Columns  []CatalogColumn
}

// CatalogAggregate is a user-defined aggregate (a CLR one, sys.objects type
// AF — T-SQL has no other kind) and the type a call to it returns, as its
// CREATE AGGREGATE ... RETURNS declared it. Returns has no Name, and its
// IsNullable is unset: sys.parameters records no nullability for a return
// value, so a caller decides it (an aggregate over no rows may answer NULL).
type CatalogAggregate struct {
	ObjectID int
	Schema   string
	Name     string
	Returns  CatalogColumn
}

// Catalog is a bulk snapshot of every user table and view in a database,
// each with its columns already loaded — see Database.Catalog.
//
// Functions is kept apart from Objects because a table-valued function is
// not interchangeable with a table: it is only usable called, with its
// argument list. Schemas names the schemas Objects spans, not Functions'
// or Aggregates'.
type Catalog struct {
	Schemas    []string
	Objects    []CatalogObject
	Functions  []CatalogObject
	Aggregates []CatalogAggregate
}

// Catalog returns a bulk snapshot of every user table and view in the
// database, every user table-valued function, each with its columns, and
// every user-defined aggregate with its return type, sorted by schema then
// name.
func (d *Database) Catalog(ctx context.Context) (*Catalog, error) {
	return d.catalog(ctx, catalogUserViews,
		"o.type IN ('U','V') AND o.is_ms_shipped = 0",
		"o.type IN ('IF','TF','FT') AND o.is_ms_shipped = 0",
		"o.type = 'AF' AND o.is_ms_shipped = 0")
}

// CallerDefaultSchema returns the connected login's default schema in the
// database — the schema an unqualified or "db..name" reference resolves to
// there. It is read in the database (no-argument SCHEMA_NAME() answers for the
// caller), so it reflects the user the login maps to there: dbo for a
// sysadmin or the database owner, the user's DEFAULT_SCHEMA otherwise, and
// for a login reaching the database through guest, guest's. A user with no
// default schema (one mapped through a Windows group) answers "".
func (d *Database) CallerDefaultSchema(ctx context.Context) (string, error) {
	var schema sql.NullString
	err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&schema)
	}, `SELECT SCHEMA_NAME()`)
	if err != nil {
		return "", fmt.Errorf("gosmo: read the caller's default schema in %q: %w", d.Name, err)
	}
	return schema.String, nil
}

// SystemCatalog returns a bulk snapshot of every catalog view in the "sys"
// schema (sys.tables, sys.columns, sys.objects, ...), and of every
// table-valued function there that records its result columns
// (sys.dm_exec_sql_text, sys.dm_db_index_physical_stats, ...).
//
// The "sys" schema's catalog views are defined identically in every database
// on a server, so a caller only needs to load this once per connection — any
// database works equally well as the query target, not just master.
//
// Unlike Catalog, this queries sys.all_objects/sys.all_columns
// rather than sys.objects/sys.columns: the latter two, despite the generic
// names, only ever surface user-created objects (is_ms_shipped=1 rows are
// invisible through them) — sys.tables, sys.columns, sys.objects itself,
// and every other built-in catalog view only show up through the "all_"
// variants.
//
// Its Aggregates are the shipped CLR ones in "sys" (ORMask, and the spatial
// ones behind geometry::UnionAggregate and kin). The built-in aggregates —
// SUM, COUNT and the rest — are not objects and appear in neither catalog.
func (d *Database) SystemCatalog(ctx context.Context) (*Catalog, error) {
	return d.catalog(ctx, catalogAllViews,
		"o.type = 'V' AND SCHEMA_NAME(o.schema_id) = 'sys'",
		"o.type IN ('IF','TF','FT') AND SCHEMA_NAME(o.schema_id) = 'sys'",
		"o.type = 'AF' AND SCHEMA_NAME(o.schema_id) = 'sys'")
}

// catalogViews names the catalog views a catalog read selects from: the
// user-only sys.objects family, or the sys.all_ one that also surfaces
// shipped objects (see SystemCatalog).
type catalogViews struct {
	objects, columns, parameters string
}

var (
	catalogUserViews = catalogViews{"sys.objects", "sys.columns", "sys.parameters"}
	catalogAllViews  = catalogViews{"sys.all_objects", "sys.all_columns", "sys.all_parameters"}
)

// catalog is the shared implementation behind Catalog and
// SystemCatalog — they differ only in which views and where clauses (fixed,
// package-internal constants — never caller-supplied) select the rows: where
// for Objects, fnWhere for Functions, aggWhere for Aggregates.
//
// It is one batch of five result sets — objects, their columns, functions,
// their columns, aggregates — in one round trip on one connection.
func (d *Database) catalog(ctx context.Context, v catalogViews, where, fnWhere, aggWhere string) (*Catalog, error) {
	q := catalogObjectsSelect(v.objects, where) + ";\n" +
		catalogColumnsSelect(v.objects, v.columns, where) + ";\n" +
		catalogObjectsSelect(v.objects, fnWhere) + ";\n" +
		catalogColumnsSelect(v.objects, v.columns, fnWhere) + ";\n" +
		catalogAggregatesSelect(v.objects, v.parameters, aggWhere)

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: load catalog for %q: %w", d.Name, err)
	}
	defer rows.Close()

	var objects, functions []CatalogObject
	var aggregates []CatalogAggregate
	reads := []func() error{
		func() (err error) { objects, err = scanCatalogObjects(rows); return err },
		func() error { return scanCatalogColumns(rows, objects) },
		func() (err error) { functions, err = scanCatalogObjects(rows); return err },
		func() error { return scanCatalogColumns(rows, functions) },
		func() (err error) { aggregates, err = scanCatalogAggregates(rows); return err },
	}
	for i, read := range reads {
		if i > 0 && !rows.NextResultSet() {
			err := rows.Err()
			if err == nil {
				err = fmt.Errorf("result set %d of %d is missing", i+1, len(reads))
			}
			return nil, fmt.Errorf("gosmo: load catalog for %q: %w", d.Name, err)
		}
		if err := read(); err != nil {
			return nil, fmt.Errorf("gosmo: load catalog for %q: %w", d.Name, err)
		}
	}
	// Most system functions return a shape decided at run time and record
	// no columns; one that records none is no use to a caller binding a
	// result shape, so it is left out rather than listed empty.
	functions = slices.DeleteFunc(functions, func(o CatalogObject) bool { return len(o.Columns) == 0 })

	var schemas []string
	for _, o := range objects {
		if !slices.Contains(schemas, o.Schema) {
			schemas = append(schemas, o.Schema)
		}
	}
	slices.Sort(schemas)

	return &Catalog{Schemas: schemas, Objects: objects, Functions: functions, Aggregates: aggregates}, nil
}

// catalogObjectType maps a sys.objects.type code to a CatalogObjectType —
// "V" is a view, "IF"/"TF"/"FT" a table-valued function, anything else (the
// queries only ever select those or "U") a table.
func catalogObjectType(typeCode string) CatalogObjectType {
	switch typeCode {
	case "V":
		return CatalogView
	case "IF", "TF", "FT":
		return CatalogFunction
	}
	return CatalogTable
}

// catalogObjectsSelect selects every object matching where (no columns),
// sorted by schema then name.
func catalogObjectsSelect(objectsView, where string) string {
	return fmt.Sprintf(`
SELECT o.object_id, SCHEMA_NAME(o.schema_id), o.name, o.type
FROM   %s o
WHERE  %s
ORDER  BY SCHEMA_NAME(o.schema_id), o.name`, objectsView, where)
}

// catalogColumnsSelect selects every column of every object matching where,
// ordered by object.
func catalogColumnsSelect(objectsView, columnsView, where string) string {
	return fmt.Sprintf(`
SELECT c.object_id, c.name, tp.name,
       c.max_length, c.precision, c.scale, c.is_nullable
FROM   %s c
JOIN   sys.types tp ON tp.user_type_id = c.user_type_id
JOIN   %s o ON o.object_id = c.object_id
WHERE  %s
ORDER  BY c.object_id, c.column_id`, columnsView, objectsView, where)
}

// catalogAggregatesSelect selects every aggregate matching where with its
// return type — parameter_id 0, which every aggregate has — sorted by schema
// then name.
func catalogAggregatesSelect(objectsView, parametersView, where string) string {
	return fmt.Sprintf(`
SELECT o.object_id, SCHEMA_NAME(o.schema_id), o.name, tp.name,
       p.max_length, p.precision, p.scale
FROM   %s o
JOIN   %s p ON p.object_id = o.object_id AND p.parameter_id = 0
JOIN   sys.types tp ON tp.user_type_id = p.user_type_id
WHERE  %s
ORDER  BY SCHEMA_NAME(o.schema_id), o.name`, objectsView, parametersView, where)
}

// scanCatalogObjects drains the current result set of catalogObjectsSelect
// rows. It leaves rows open for the next result set, and returns bare errors:
// catalog wraps them.
func scanCatalogObjects(rows *dbRows) ([]CatalogObject, error) {
	var out []CatalogObject
	for rows.Next() {
		var o CatalogObject
		var typeCode string
		if err := rows.Scan(&o.ObjectID, &o.Schema, &o.Name, &typeCode); err != nil {
			return nil, err
		}
		// sys.objects.type is CHAR(2): 'U'/'V' come back space-padded
		// ("U ", "V "), so this must trim before comparing.
		o.Type = catalogObjectType(strings.TrimSpace(typeCode))
		out = append(out, o)
	}
	return out, rows.Err()
}

// scanCatalogColumns drains the current result set of catalogColumnsSelect
// rows into the matching CatalogObject by object_id. Bare errors, as
// scanCatalogObjects.
func scanCatalogColumns(rows *dbRows, objects []CatalogObject) error {
	byID := make(map[int]*CatalogObject, len(objects))
	for i := range objects {
		byID[objects[i].ObjectID] = &objects[i]
	}
	for rows.Next() {
		var objectID int
		var col CatalogColumn
		if err := rows.Scan(&objectID, &col.Name, &col.DataType,
			&col.MaxLength, &col.Precision, &col.Scale, &col.IsNullable); err != nil {
			return err
		}
		if o, ok := byID[objectID]; ok {
			o.Columns = append(o.Columns, col)
		}
	}
	return rows.Err()
}

// scanCatalogAggregates drains the current result set of
// catalogAggregatesSelect rows. Bare errors, as scanCatalogObjects.
func scanCatalogAggregates(rows *dbRows) ([]CatalogAggregate, error) {
	var out []CatalogAggregate
	for rows.Next() {
		var a CatalogAggregate
		if err := rows.Scan(&a.ObjectID, &a.Schema, &a.Name, &a.Returns.DataType,
			&a.Returns.MaxLength, &a.Returns.Precision, &a.Returns.Scale); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
