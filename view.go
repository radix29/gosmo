package gosmo

import (
	"context"
	"fmt"
	"time"
)

// -- Views ---------------------------------------------------------------------

// View represents a database view.
type View struct {
	ObjectID   int
	Schema     string
	Name       string
	Definition string
	CreateDate time.Time
	ModifyDate time.Time
}

// Views returns all views in the database.
func (d *Database) Views(ctx context.Context) ([]*View, error) {
	return d.viewsWhere(ctx, "", nil)
}

// ViewsFiltered returns the views an ObjectFilter matches, narrowed by the
// server rather than by the caller. An empty filter is Views.
func (d *Database) ViewsFiltered(ctx context.Context, filter ObjectFilter) ([]*View, error) {
	where, args := filter.clause(viewFilterColumns, 1)
	return d.viewsWhere(ctx, where, args)
}

// viewFilterColumns maps an ObjectFilter onto sys.views as viewsWhere aliases
// it. There is no memory-optimized column outside sys.tables.
var viewFilterColumns = filterColumns{
	name:    "v.name",
	schema:  "SCHEMA_NAME(v.schema_id)",
	created: "v.create_date",
}

func (d *Database) viewsWhere(ctx context.Context, where string, args []any) ([]*View, error) {
	q := `
SELECT v.object_id, SCHEMA_NAME(v.schema_id), v.name,
       ISNULL(m.definition,''), v.create_date, v.modify_date
FROM   sys.views v
JOIN   sys.sql_modules m ON m.object_id = v.object_id
WHERE  v.is_ms_shipped = 0 ` + where + `
ORDER  BY SCHEMA_NAME(v.schema_id), v.name`

	rows, err := d.query(ctx, q, args...)
	return scanRows(rows, err, fmt.Sprintf("list views in %q", d.Name), func(scan func(...any) error) (*View, error) {
		v := &View{}
		if err := scan(&v.ObjectID, &v.Schema, &v.Name,
			&v.Definition, &v.CreateDate, &v.ModifyDate); err != nil {
			return nil, err
		}
		return v, nil
	})
}

// SystemViews returns every catalog view SQL Server ships in the "sys"
// schema (sys.tables, sys.columns, sys.objects, ...) — see SystemViews.
//
// Unlike Views, this reads sys.all_objects/sys.all_sql_modules rather than
// sys.views/sys.sql_modules: the "sys." schema's own views are shipped objects
// (is_ms_shipped=1), invisible through the non-"all_" catalog views — same
// reasoning as SystemCatalog. The "sys" schema's catalog views are
// defined identically in every database on a server, so a caller only needs to
// load this once per connection.
func (d *Database) SystemViews(ctx context.Context) ([]*View, error) {
	return d.systemViewsWhere(ctx, "", nil)
}

// SystemViewsFiltered returns the system views an ObjectFilter matches,
// narrowed by the server. An empty filter is SystemViews.
func (d *Database) SystemViewsFiltered(ctx context.Context, filter ObjectFilter) ([]*View, error) {
	where, args := filter.clause(allObjectsFilterColumns, 1)
	return d.systemViewsWhere(ctx, where, args)
}

func (d *Database) systemViewsWhere(ctx context.Context, where string, args []any) ([]*View, error) {
	q := `
SELECT o.object_id, SCHEMA_NAME(o.schema_id), o.name,
       ISNULL(m.definition,''), o.create_date, o.modify_date
FROM   sys.all_objects o
LEFT JOIN sys.all_sql_modules m ON m.object_id = o.object_id
WHERE  o.type = 'V' AND o.is_ms_shipped = 1 AND SCHEMA_NAME(o.schema_id) = 'sys' ` + where + `
ORDER  BY o.name`

	rows, err := d.query(ctx, q, args...)
	return scanRows(rows, err, fmt.Sprintf("list system views in %q", d.Name), func(scan func(...any) error) (*View, error) {
		v := &View{}
		if err := scan(&v.ObjectID, &v.Schema, &v.Name,
			&v.Definition, &v.CreateDate, &v.ModifyDate); err != nil {
			return nil, err
		}
		return v, nil
	})
}

// allObjectsFilterColumns maps an ObjectFilter onto the sys.all_objects
// listings the three System* families share.
var allObjectsFilterColumns = filterColumns{
	name:    "o.name",
	schema:  "SCHEMA_NAME(o.schema_id)",
	created: "o.create_date",
}

// DropView drops a view. A view that isn't there is the server's error, not
// a silent success — see the note on Database.DropTable.
func (d *Database) DropView(ctx context.Context, schema, name string) error {
	if schema == "" {
		schema = "dbo"
	}
	if _, err := d.exec(ctx, "DROP VIEW "+qualifiedName(schema, name)); err != nil {
		return fmt.Errorf("gosmo: drop view [%s].[%s]: %w", schema, name, err)
	}
	return nil
}

// ObjectTriggers returns the DML triggers defined on one table or view —
// AFTER and INSTEAD OF triggers, the parent_class = 1 family, for a single
// parent named rather than handed over as a *Table.
//
// It is Table.Triggers' by-name counterpart, and it exists because View is a
// plain row struct with no back-pointer to its database, so a view's INSTEAD
// OF triggers had no reader at all. The parent is resolved by OBJECT_ID, which
// does not care which of the two it is.
func (d *Database) ObjectTriggers(ctx context.Context, schema, name string) ([]*Trigger, error) {
	if schema == "" {
		schema = "dbo"
	}
	return d.triggersWhere(ctx, "AND tr.parent_id = OBJECT_ID(@p1)",
		[]any{qualifiedName(schema, name)})
}
