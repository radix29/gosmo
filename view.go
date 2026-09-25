package gosmo

import (
	"context"
	"fmt"
	"time"
)

// -- Views ---------------------------------------------------------------------

// View represents a database view.
type View struct {
	db *Database

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
		v := &View{db: d}
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
		v := &View{db: d}
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

// ObjectTriggers returns the DML triggers defined on one table or view —
// AFTER and INSTEAD OF triggers, the parent_class = 1 family, for a single
// parent named rather than handed over as a *Table.
//
// It is Table.Triggers' by-name counterpart, and serves a view as well as a
// table: a view's INSTEAD OF triggers are read here, or through
// View.Triggers. The parent is resolved by OBJECT_ID, which does not care
// which of the two it is.
func (d *Database) ObjectTriggers(ctx context.Context, schema, name string) ([]*Trigger, error) {
	if err := requireSchema("object triggers", schema, name); err != nil {
		return nil, err
	}
	return d.triggersWhere(ctx, "AND tr.parent_id = OBJECT_ID(@p1)",
		[]any{qualifiedName(schema, name)})
}

// Triggers returns the view's INSTEAD OF triggers — ObjectTriggers for this
// view.
func (v *View) Triggers(ctx context.Context) ([]*Trigger, error) {
	return v.db.ObjectTriggers(ctx, v.Schema, v.Name)
}

// ViewRef returns a lightweight handle for the view [schema].[name] — no
// query; every field but Schema and Name is zero. See Server.DatabaseRef for
// when a handle is the right form.
func (d *Database) ViewRef(schema, name string) *View {
	return &View{db: d, Schema: schema, Name: name}
}

// Database returns the database the view belongs to.
func (v *View) Database() *Database { return v.db }

// Drop drops the view. A view that isn't there is the server's
// error, not a silent success — see the note on Table.Drop.
func (v *View) Drop(ctx context.Context) error {
	return v.db.dropSchemaObject(ctx, "view", "VIEW", v.Schema, v.Name)
}

// Rename renames the view (sp_rename's 'OBJECT' class). newName is a bare
// name; a rename never moves the view between schemas — see Transfer.
func (v *View) Rename(ctx context.Context, newName string) error {
	if err := v.db.renameSchemaObject(ctx, "view", renameObjectClass, v.Schema, v.Name, newName); err != nil {
		return err
	}
	setIfApplied(ctx, &v.Name, newName)
	return nil
}

// Transfer moves the view into another schema (ALTER SCHEMA ... TRANSFER).
// It keeps its name and object_id; permissions granted on it directly are
// dropped by the server.
func (v *View) Transfer(ctx context.Context, targetSchema string) error {
	if err := v.db.transferSchemaObject(ctx, "view", transferObjectClass, targetSchema, v.Schema, v.Name); err != nil {
		return err
	}
	setIfApplied(ctx, &v.Schema, targetSchema)
	return nil
}
