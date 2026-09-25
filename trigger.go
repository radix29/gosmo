package gosmo

import (
	"context"
	"database/sql"
	"fmt"
)

// -- Triggers ------------------------------------------------------------------

// Trigger represents a DML trigger attached to a table or view.
type Trigger struct {
	db *Database

	Name       string
	TableName  string
	Schema     string
	IsEnabled  bool
	Events     []string
	Definition string
}

// Triggers returns all DML triggers in the database.
func (d *Database) Triggers(ctx context.Context) ([]*Trigger, error) {
	return d.triggersWhere(ctx, "", nil)
}

func (d *Database) triggersWhere(ctx context.Context, where string, args []any) ([]*Trigger, error) {
	q := `
SELECT tr.name, OBJECT_NAME(tr.parent_id), SCHEMA_NAME(o.schema_id),
       tr.is_disabled,
       ` + jsonList("te.type_desc", `
        FROM   sys.trigger_events te
        WHERE  te.object_id = tr.object_id`, "") + ` AS events,
       ISNULL(m.definition, '')
FROM   sys.triggers tr
JOIN   sys.objects o   ON o.object_id  = tr.parent_id
JOIN   sys.sql_modules m ON m.object_id = tr.object_id
WHERE  tr.is_ms_shipped = 0 AND tr.parent_class = 1 ` + where + `
ORDER  BY tr.name`

	rows, err := d.query(ctx, q, args...)
	return scanRows(rows, err, fmt.Sprintf("list triggers in %q", d.Name), func(scan func(...any) error) (*Trigger, error) {
		t := &Trigger{db: d}
		var events sql.NullString
		var isDisabled bool
		if err := scan(&t.Name, &t.TableName, &t.Schema, &isDisabled,
			&events, &t.Definition); err != nil {
			return nil, err
		}
		t.IsEnabled = !isDisabled
		var err error
		if t.Events, err = decodeJSONList(events); err != nil {
			return nil, err
		}
		return t, nil
	})
}

// TriggerRef returns a lightweight handle for the DML trigger [schema].[name]
// — no query; every field but Schema and Name is zero. schema is the
// trigger's own schema, which is the schema of the table or view it is
// defined on. See Server.DatabaseRef for when a handle is the right form.
func (d *Database) TriggerRef(schema, name string) *Trigger {
	return &Trigger{db: d, Schema: schema, Name: name}
}

// Database returns the database the trigger belongs to.
func (t *Trigger) Database() *Database { return t.db }

// Drop drops the trigger. A trigger that isn't there is the server's error,
// not a silent success — see the note on Table.Drop.
func (t *Trigger) Drop(ctx context.Context) error {
	return t.db.dropSchemaObject(ctx, "trigger", "TRIGGER", t.Schema, t.Name)
}

// Rename renames the trigger (sp_rename's 'OBJECT' class). newName is a bare
// name.
//
// There is no Transfer: a DML trigger belongs to its table and moves with
// it, and ALTER SCHEMA ... TRANSFER refuses one.
func (t *Trigger) Rename(ctx context.Context, newName string) error {
	if err := t.db.renameSchemaObject(ctx, "trigger", renameObjectClass, t.Schema, t.Name, newName); err != nil {
		return err
	}
	setIfApplied(ctx, &t.Name, newName)
	return nil
}
