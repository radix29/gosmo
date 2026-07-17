package gosmo

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// ============================================================
// Catalog: a single bulk snapshot of every table/view and its columns,
// for callers (like a SQL editor's autocomplete) that need to inventory a
// whole database up front instead of querying one object at a time via
// Table.Columns/Database.Tables/Database.Views.
// ============================================================

// CatalogObjectType distinguishes a Catalog entry's underlying object kind.
type CatalogObjectType int

const (
	CatalogTable CatalogObjectType = iota
	CatalogView
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

// CatalogObject is one table or view and its columns, in ordinal order.
type CatalogObject struct {
	ObjectID int
	Schema   string
	Name     string
	Type     CatalogObjectType
	Columns  []CatalogColumn
}

// Catalog is a bulk snapshot of every user table and view in a database,
// each with its columns already loaded — see Database.Catalog.
type Catalog struct {
	Schemas []string
	Objects []CatalogObject
}

// Catalog returns a bulk snapshot of every user table and view in the
// database, each with its columns, sorted by schema then name.
func (d *Database) Catalog() (*Catalog, error) {
	return d.CatalogContext(context.Background())
}

// CatalogContext is the context-aware variant of Catalog.
func (d *Database) CatalogContext(ctx context.Context) (*Catalog, error) {
	return d.catalogContext(ctx, "sys.objects", "sys.columns", "o.type IN ('U','V') AND o.is_ms_shipped = 0")
}

// SystemCatalog returns a bulk snapshot of every catalog view in the "sys"
// schema (sys.tables, sys.columns, sys.objects, ...) — see
// SystemCatalogContext.
func (d *Database) SystemCatalog() (*Catalog, error) {
	return d.SystemCatalogContext(context.Background())
}

// SystemCatalogContext is the context-aware variant of SystemCatalog. The
// "sys" schema's catalog views are defined identically in every database on
// a server, so a caller only needs to load this once per connection — any
// database works equally well as the query target, not just master.
//
// Unlike CatalogContext, this queries sys.all_objects/sys.all_columns
// rather than sys.objects/sys.columns: the latter two, despite the generic
// names, only ever surface user-created objects (is_ms_shipped=1 rows are
// invisible through them) — sys.tables, sys.columns, sys.objects itself,
// and every other built-in catalog view only show up through the "all_"
// variants.
func (d *Database) SystemCatalogContext(ctx context.Context) (*Catalog, error) {
	return d.catalogContext(ctx, "sys.all_objects", "sys.all_columns", "o.type = 'V' AND SCHEMA_NAME(o.schema_id) = 'sys'")
}

// catalogContext is the shared implementation behind CatalogContext and
// SystemCatalogContext — they differ only in which objects/columns views
// and where clause (fixed, package-internal constants — never
// caller-supplied) select the rows.
func (d *Database) catalogContext(ctx context.Context, objectsView, columnsView, where string) (*Catalog, error) {
	objects, err := d.catalogObjectsContext(ctx, objectsView, where)
	if err != nil {
		return nil, err
	}
	if err := d.catalogColumnsContext(ctx, objects, objectsView, columnsView, where); err != nil {
		return nil, err
	}

	var schemas []string
	for _, o := range objects {
		if !slices.Contains(schemas, o.Schema) {
			schemas = append(schemas, o.Schema)
		}
	}
	slices.Sort(schemas)

	return &Catalog{Schemas: schemas, Objects: objects}, nil
}

// catalogObjectType maps a sys.objects.type code to a CatalogObjectType —
// "V" is a view, anything else (the query only ever selects "U" or "V") is
// a table.
func catalogObjectType(typeCode string) CatalogObjectType {
	if typeCode == "V" {
		return CatalogView
	}
	return CatalogTable
}

// catalogObjectsContext loads every object matching where (no columns yet),
// sorted by schema then name.
func (d *Database) catalogObjectsContext(ctx context.Context, objectsView, where string) ([]CatalogObject, error) {
	q := fmt.Sprintf(`
SELECT o.object_id, SCHEMA_NAME(o.schema_id), o.name, o.type
FROM   %s o
WHERE  %s
ORDER  BY SCHEMA_NAME(o.schema_id), o.name`, objectsView, where)

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: load catalog for %q: %w", d.name, err)
	}
	defer rows.Close()

	var objects []CatalogObject
	for rows.Next() {
		var o CatalogObject
		var typeCode string
		if err := rows.Scan(&o.ObjectID, &o.Schema, &o.Name, &typeCode); err != nil {
			return nil, err
		}
		// sys.objects.type is CHAR(2): 'U'/'V' come back space-padded
		// ("U ", "V "), so this must trim before comparing.
		o.Type = catalogObjectType(strings.TrimSpace(typeCode))
		objects = append(objects, o)
	}
	return objects, rows.Err()
}

// catalogColumnsContext loads every column of every object matching where in
// one query and distributes them into the matching CatalogObject by
// object_id.
func (d *Database) catalogColumnsContext(ctx context.Context, objects []CatalogObject, objectsView, columnsView, where string) error {
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
		return fmt.Errorf("gosmo: load catalog columns for %q: %w", d.name, err)
	}
	defer rows.Close()

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
