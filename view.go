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
func (d *Database) Views() ([]*View, error) {
	return d.ViewsContext(context.Background())
}

// ViewsContext is the context-aware variant of Views.
func (d *Database) ViewsContext(ctx context.Context) ([]*View, error) {
	const q = `
SELECT v.object_id, SCHEMA_NAME(v.schema_id), v.name,
       ISNULL(m.definition,''), v.create_date, v.modify_date
FROM   sys.views v
JOIN   sys.sql_modules m ON m.object_id = v.object_id
WHERE  v.is_ms_shipped = 0
ORDER  BY SCHEMA_NAME(v.schema_id), v.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list views in %q: %w", d.name, err)
	}
	defer rows.Close()

	var views []*View
	for rows.Next() {
		v := &View{}
		if err := rows.Scan(&v.ObjectID, &v.Schema, &v.Name,
			&v.Definition, &v.CreateDate, &v.ModifyDate); err != nil {
			return nil, fmt.Errorf("gosmo: list views in %q: %w", d.name, err)
		}
		views = append(views, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list views in %q: %w", d.name, err)
	}
	return views, nil
}

// SystemViews returns every catalog view SQL Server ships in the "sys"
// schema (sys.tables, sys.columns, sys.objects, ...) — see SystemViewsContext.
func (d *Database) SystemViews() ([]*View, error) {
	return d.SystemViewsContext(context.Background())
}

// SystemViewsContext is the context-aware variant of SystemViews. Unlike
// Views, this reads sys.all_objects/sys.all_sql_modules rather than
// sys.views/sys.sql_modules: the "sys." schema's own views are shipped
// objects (is_ms_shipped=1), invisible through the non-"all_" catalog
// views — same reasoning as SystemCatalogContext. The "sys" schema's
// catalog views are defined identically in every database on a server, so
// a caller only needs to load this once per connection.
func (d *Database) SystemViewsContext(ctx context.Context) ([]*View, error) {
	const q = `
SELECT o.object_id, SCHEMA_NAME(o.schema_id), o.name,
       ISNULL(m.definition,''), o.create_date, o.modify_date
FROM   sys.all_objects o
LEFT JOIN sys.all_sql_modules m ON m.object_id = o.object_id
WHERE  o.type = 'V' AND o.is_ms_shipped = 1 AND SCHEMA_NAME(o.schema_id) = 'sys'
ORDER  BY o.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list system views in %q: %w", d.name, err)
	}
	defer rows.Close()

	var views []*View
	for rows.Next() {
		v := &View{}
		if err := rows.Scan(&v.ObjectID, &v.Schema, &v.Name,
			&v.Definition, &v.CreateDate, &v.ModifyDate); err != nil {
			return nil, fmt.Errorf("gosmo: list system views in %q: %w", d.name, err)
		}
		views = append(views, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list system views in %q: %w", d.name, err)
	}
	return views, nil
}

// DropView drops a view. A view that isn't there is the server's error, not
// a silent success — see the note on Database.DropTable.
func (d *Database) DropView(schema, name string) error {
	return d.DropViewContext(context.Background(), schema, name)
}

// DropViewContext is the context-aware variant of DropView.
func (d *Database) DropViewContext(ctx context.Context, schema, name string) error {
	if schema == "" {
		schema = "dbo"
	}
	if _, err := d.exec(ctx, "DROP VIEW "+qualifiedName(schema, name)); err != nil {
		return fmt.Errorf("gosmo: drop view [%s].[%s]: %w", schema, name, err)
	}
	return nil
}
