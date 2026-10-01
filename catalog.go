package gosmo

import (
	"context"
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

// Catalog is a bulk snapshot of every user table and view in a database,
// each with its columns already loaded — see Database.Catalog.
//
// Functions is kept apart from Objects because a table-valued function is
// not interchangeable with a table: it is only usable called, with its
// argument list. Schemas names the schemas Objects spans, not Functions'.
type Catalog struct {
	Schemas   []string
	Objects   []CatalogObject
	Functions []CatalogObject
}

// Catalog returns a bulk snapshot of every user table and view in the
// database, and every user table-valued function, each with its columns,
// sorted by schema then name.
func (d *Database) Catalog(ctx context.Context) (*Catalog, error) {
	return d.catalog(ctx, "sys.objects", "sys.columns",
		"o.type IN ('U','V') AND o.is_ms_shipped = 0",
		"o.type IN ('IF','TF','FT') AND o.is_ms_shipped = 0")
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
func (d *Database) SystemCatalog(ctx context.Context) (*Catalog, error) {
	return d.catalog(ctx, "sys.all_objects", "sys.all_columns",
		"o.type = 'V' AND SCHEMA_NAME(o.schema_id) = 'sys'",
		"o.type IN ('IF','TF','FT') AND SCHEMA_NAME(o.schema_id) = 'sys'")
}

// catalog is the shared implementation behind Catalog and
// SystemCatalog — they differ only in which objects/columns views
// and where clauses (fixed, package-internal constants — never
// caller-supplied) select the rows: where for Objects, fnWhere for
// Functions.
func (d *Database) catalog(ctx context.Context, objectsView, columnsView, where, fnWhere string) (*Catalog, error) {
	objects, err := d.catalogObjects(ctx, objectsView, where)
	if err != nil {
		return nil, err
	}
	if err := d.catalogColumns(ctx, objects, objectsView, columnsView, where); err != nil {
		return nil, err
	}
	functions, err := d.catalogObjects(ctx, objectsView, fnWhere)
	if err != nil {
		return nil, err
	}
	if err := d.catalogColumns(ctx, functions, objectsView, columnsView, fnWhere); err != nil {
		return nil, err
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

	return &Catalog{Schemas: schemas, Objects: objects, Functions: functions}, nil
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

// catalogObjects loads every object matching where (no columns yet),
// sorted by schema then name.
func (d *Database) catalogObjects(ctx context.Context, objectsView, where string) ([]CatalogObject, error) {
	q := fmt.Sprintf(`
SELECT o.object_id, SCHEMA_NAME(o.schema_id), o.name, o.type
FROM   %s o
WHERE  %s
ORDER  BY SCHEMA_NAME(o.schema_id), o.name`, objectsView, where)

	rows, err := d.query(ctx, q)
	return scanRows(rows, err, fmt.Sprintf("load catalog for %q", d.Name), func(scan func(...any) error) (CatalogObject, error) {
		var o CatalogObject
		var typeCode string
		if err := scan(&o.ObjectID, &o.Schema, &o.Name, &typeCode); err != nil {
			return CatalogObject{}, err
		}
		// sys.objects.type is CHAR(2): 'U'/'V' come back space-padded
		// ("U ", "V "), so this must trim before comparing.
		o.Type = catalogObjectType(strings.TrimSpace(typeCode))
		return o, nil
	})
}

// catalogColumns loads every column of every object matching where in
// one query and distributes them into the matching CatalogObject by
// object_id.
func (d *Database) catalogColumns(ctx context.Context, objects []CatalogObject, objectsView, columnsView, where string) error {
	byID := make(map[int]*CatalogObject, len(objects))
	for i := range objects {
		byID[objects[i].ObjectID] = &objects[i]
	}

	q := fmt.Sprintf(`
SELECT c.object_id, c.name, tp.name,
       c.max_length, c.precision, c.scale, c.is_nullable
FROM   %s c
JOIN   sys.types tp ON tp.user_type_id = c.user_type_id
JOIN   %s o ON o.object_id = c.object_id
WHERE  %s
ORDER  BY c.object_id, c.column_id`, columnsView, objectsView, where)

	rows, err := d.query(ctx, q)
	if err != nil {
		return fmt.Errorf("gosmo: load catalog columns for %q: %w", d.Name, err)
	}
	defer rows.Close()

	for rows.Next() {
		var objectID int
		var col CatalogColumn
		if err := rows.Scan(&objectID, &col.Name, &col.DataType,
			&col.MaxLength, &col.Precision, &col.Scale, &col.IsNullable); err != nil {
			return fmt.Errorf("gosmo: load catalog columns for %q: %w", d.Name, err)
		}
		if o, ok := byID[objectID]; ok {
			o.Columns = append(o.Columns, col)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("gosmo: load catalog columns for %q: %w", d.Name, err)
	}
	return nil
}
