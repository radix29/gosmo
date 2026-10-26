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
	CreateDate time.Time
	ModifyDate time.Time
	// IsSchemaBound is whether the view was created WITH SCHEMABINDING
	// (OBJECTPROPERTY IsSchemaBound) — what an indexed view requires before
	// its first index, a unique clustered one, can be created.
	IsSchemaBound bool
}

// Views returns the views an ObjectFilter matches, narrowed by the server
// rather than by the caller. The zero ObjectFilter returns every view.
func (d *Database) Views(ctx context.Context, filter ObjectFilter) ([]*View, error) {
	where, args := filter.clause(moduleFilterColumns)
	return moduleListWhere(ctx, d, "views", userViews, viewExtra, where, args, scanView)
}

var (
	userViews   = moduleSource{from: "sys.views o", kind: "o.is_ms_shipped = 0"}
	systemViews = moduleSource{from: "sys.all_objects o", kind: "o.type = 'V' AND o.is_ms_shipped = 1 AND o.schema_id = 4", system: true}
)

// viewSelect is the column list Views and ViewByName share, so a view
// carries the same fields however it was fetched.
const viewSelect = moduleColumns + viewExtra + `
FROM   sys.views o`

// viewExtra is the column a view carries beyond moduleColumns.
const viewExtra = `,
       CAST(ISNULL(OBJECTPROPERTY(o.object_id, 'IsSchemaBound'), 0) AS bit)`

func scanView(d *Database, scan func(...any) error) (*View, error) {
	v := &View{db: d}
	if err := scan(&v.ObjectID, &v.Schema, &v.Name,
		&v.CreateDate, &v.ModifyDate, &v.IsSchemaBound); err != nil {
		return nil, err
	}
	return v, nil
}

// ViewByName returns one view by schema and name from sys.views — the
// populated counterpart of ViewRef, carrying the ObjectID its index and
// statistics reads are keyed by. The sys schema's catalog views are not in
// sys.views and are not found here (see SystemViews).
//
// It returns an error satisfying errors.Is(err, ErrNotFound) when there is
// no such view.
func (d *Database) ViewByName(ctx context.Context, schema, name string) (*View, error) {
	if err := requireSchema("view by name", schema, name); err != nil {
		return nil, err
	}
	q := viewSelect + `
WHERE  SCHEMA_NAME(o.schema_id) COLLATE DATABASE_DEFAULT = @p1
  AND  o.name                                           = @p2`

	return readByName(ctx, d, scanView, q, []any{schema, name},
		notFoundf("gosmo: view %s not found in %q", qualifiedName(schema, name), d.Name),
		fmt.Sprintf("find view %s in %q", qualifiedName(schema, name), d.Name))
}

// SystemViews returns every catalog view SQL Server ships in the "sys"
// schema (sys.tables, sys.columns, sys.objects, ...) — see SystemViews.
//
// Unlike Views, this reads sys.all_objects/sys.all_sql_modules rather than
// sys.views/sys.sql_modules: the "sys." schema's own views are shipped objects
// (is_ms_shipped=1), invisible through the non-"all_" catalog views — same
// reasoning as SystemCatalog. The "sys" schema's catalog views are
// defined identically in every database on a server, so a caller only needs to
// load this once per connection. The zero ObjectFilter returns every one.
func (d *Database) SystemViews(ctx context.Context, filter ObjectFilter) ([]*View, error) {
	where, args := filter.clause(moduleFilterColumns)
	return moduleListWhere(ctx, d, "system views", systemViews, viewExtra, where, args, scanView)
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

// FullName returns [Schema].[Name].
func (v *View) FullName() string { return qualifiedName(v.Schema, v.Name) }

// Definition returns the view's T-SQL text: "" for an encrypted view,
// ErrNotFound when there is no such view. Listings do not carry it; this
// reads it by name, so a ViewRef is enough.
func (v *View) Definition(ctx context.Context) (string, error) {
	return v.db.moduleDefinition(ctx, "view", "o.type = 'V'", v.Schema, v.Name)
}

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
