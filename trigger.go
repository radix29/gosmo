package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// -- Triggers ------------------------------------------------------------------

// Trigger represents a DML trigger attached to a table or view.
type Trigger struct {
	db *Database

	Name      string
	TableName string
	Schema    string
	IsEnabled bool
	Events    []string
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
        WHERE  te.object_id = tr.object_id`, "") + ` AS events
FROM   sys.triggers tr
JOIN   sys.objects o   ON o.object_id  = tr.parent_id
WHERE  tr.is_ms_shipped = 0 AND tr.parent_class = 1 ` + where + `
ORDER  BY tr.name`

	rows, err := d.query(ctx, q, args...)
	return scanRows(rows, err, fmt.Sprintf("list triggers in %q", d.Name), func(scan func(...any) error) (*Trigger, error) {
		t := &Trigger{db: d}
		var events sql.NullString
		var isDisabled bool
		if err := scan(&t.Name, &t.TableName, &t.Schema, &isDisabled,
			&events); err != nil {
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

// TriggerByName returns one DML trigger by schema and name. schema is the
// trigger's own schema, which is the schema of the table or view it is
// defined on.
//
// It returns an error satisfying errors.Is(err, ErrNotFound) when the
// database has no such DML trigger.
func (d *Database) TriggerByName(ctx context.Context, schema, name string) (*Trigger, error) {
	if err := requireSchema("trigger by name", schema, name); err != nil {
		return nil, err
	}
	trs, err := d.triggersWhere(ctx,
		"AND SCHEMA_NAME(o.schema_id) COLLATE DATABASE_DEFAULT = @p1 AND tr.name = @p2", []any{schema, name})
	if err != nil {
		return nil, fmt.Errorf("gosmo: find trigger %s in %q: %w", qualifiedName(schema, name), d.Name, err)
	}
	if len(trs) == 0 {
		return nil, notFoundf("gosmo: trigger %s not found in %q", qualifiedName(schema, name), d.Name)
	}
	return trs[0], nil
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

// Definition returns the trigger's T-SQL text: "" for an encrypted or CLR
// trigger, ErrNotFound when there is no such DML trigger. Listings do not
// carry it; this reads it by name, so a TriggerRef is enough.
func (t *Trigger) Definition(ctx context.Context) (string, error) {
	return t.db.moduleDefinition(ctx, "trigger", "o.type IN ('TR','TA')", t.Schema, t.Name)
}

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

// Enable enables the trigger.
func (t *Trigger) Enable(ctx context.Context) error {
	return t.SetEnabled(ctx, true)
}

// Disable disables the trigger, leaving its definition in place.
func (t *Trigger) Disable(ctx context.Context) error {
	return t.SetEnabled(ctx, false)
}

// SetEnabled is Enable when on is true and Disable otherwise. The statement
// names the table or view the trigger is on; a TriggerRef, which has no
// TableName, looks it up by name first (ErrNotFound when there is no such
// DML trigger) without caching it on the handle.
func (t *Trigger) SetEnabled(ctx context.Context, on bool) error {
	verb := "DISABLE"
	if on {
		verb = "ENABLE"
	}
	what := fmt.Sprintf("%s trigger %s in %q", strings.ToLower(verb), qualifiedName(t.Schema, t.Name), t.db.Name)
	if err := requireSchema(strings.ToLower(verb)+" trigger", t.Schema, t.Name); err != nil {
		return err
	}
	parent, err := t.parentName(ctx, what)
	if err != nil {
		return err
	}
	stmt := fmt.Sprintf("%s TRIGGER %s ON %s", verb, qualifiedName(t.Schema, t.Name), qualifiedName(t.Schema, parent))
	if _, err := t.db.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: %s: %w", what, err)
	}
	setIfApplied(ctx, &t.IsEnabled, on)
	return nil
}

// parentName is the table or view the trigger is on: TableName when the
// handle was read from the catalog, else one lookup by name. A DML trigger's
// schema is its parent's, so the schema predicate is on the parent.
func (t *Trigger) parentName(ctx context.Context, what string) (string, error) {
	if t.TableName != "" {
		return t.TableName, nil
	}
	const q = `
SELECT p.name
FROM   sys.triggers tr
JOIN   sys.objects p ON p.object_id = tr.parent_id
WHERE  tr.parent_class = 1 AND SCHEMA_NAME(p.schema_id) COLLATE DATABASE_DEFAULT = @p1 AND tr.name = @p2`
	var parent string
	err := t.db.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&parent)
	}, q, t.Schema, t.Name)
	return foundRow(parent, err,
		notFoundf("gosmo: trigger %s not found in %q", qualifiedName(t.Schema, t.Name), t.db.Name), what)
}
