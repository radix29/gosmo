package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// database_trigger.go covers database-scope DDL triggers — sys.triggers with
// parent_class = 0, SSMS's <database> > Programmability > Database Triggers
// folder.
//
// This is the third trigger family, and the one neither of the other two can
// be widened to cover. Database.Triggers reads parent_class = 1 (a DML trigger
// on a table or a view) and its Trigger carries TableName and Schema, which a
// DDL trigger has nothing to put in: it has no parent object, so no schema
// either. ServerTriggers reads sys.server_triggers, a different view
// altogether. Everything here addresses the trigger by bare name, ON DATABASE.
type DatabaseTrigger struct {
	db *Database

	Name string

	// IsEnabled is the inverse of the catalog's is_disabled.
	IsEnabled bool

	CreateDate time.Time
	ModifyDate time.Time

	// Events are the type_desc values from sys.trigger_events —
	// "CREATE_TABLE", "ALTER_PROCEDURE", and so on. A trigger declared FOR a
	// whole event group lists the group's individual events, which is what
	// the catalog records.
	Events []string

	// Definition is the trigger body from sys.sql_modules. It is empty for an
	// encrypted trigger (the catalog reports NULL) and for a CLR trigger,
	// which has no row there at all.
	Definition string
}

// databaseTriggerSelect reads the parent_class = 0 rows of sys.triggers.
//
// The join onto sys.sql_modules is a LEFT JOIN because a CLR trigger (type
// 'TA') has no row there — an inner join silently drops it from the folder,
// which is the same reason serverTriggerSelect uses one. There is no join onto
// sys.objects: parent_id is 0 here, so the schema Database.triggersWhere
// resolves through it does not exist.
var databaseTriggerSelect = `
SELECT tr.name, tr.is_disabled, tr.create_date, tr.modify_date,
       ` + commaList("te.type_desc", `
        FROM   sys.trigger_events te
        WHERE  te.object_id = tr.object_id`, "") + ` AS events,
       m.definition
FROM   sys.triggers tr
LEFT   JOIN sys.sql_modules m ON m.object_id = tr.object_id
WHERE  tr.is_ms_shipped = 0 AND tr.parent_class = 0`

// DatabaseTriggers returns every database-scope DDL trigger.
func (d *Database) DatabaseTriggers() ([]*DatabaseTrigger, error) {
	return d.DatabaseTriggersContext(context.Background())
}

// DatabaseTriggersContext is the context-aware variant of DatabaseTriggers.
func (d *Database) DatabaseTriggersContext(ctx context.Context) ([]*DatabaseTrigger, error) {
	rows, err := d.query(ctx, databaseTriggerSelect+`
ORDER  BY tr.name`)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list database triggers in %q: %w", d.name, err)
	}
	defer rows.Close()

	var triggers []*DatabaseTrigger
	for rows.Next() {
		t, err := scanDatabaseTrigger(d, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list database triggers in %q: %w", d.name, err)
		}
		triggers = append(triggers, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list database triggers in %q: %w", d.name, err)
	}
	return triggers, nil
}

// DatabaseTriggerByName returns one database-scope DDL trigger with every
// field populated, or a not-found error (errors.Is ErrNotFound) when the
// database has none by that name.
func (d *Database) DatabaseTriggerByName(name string) (*DatabaseTrigger, error) {
	return d.DatabaseTriggerByNameContext(context.Background(), name)
}

// DatabaseTriggerByNameContext is the context-aware variant of
// DatabaseTriggerByName.
func (d *Database) DatabaseTriggerByNameContext(ctx context.Context, name string) (*DatabaseTrigger, error) {
	var t *DatabaseTrigger
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		t, err = scanDatabaseTrigger(d, row.Scan)
		return err
	}, databaseTriggerSelect+`
   AND tr.name = @p1`, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: database trigger %q not found in %q", name, d.name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read database trigger %q in %q: %w", name, d.name, err)
	}
	return t, nil
}

// DatabaseTrigger returns a lightweight handle for a database-scope DDL
// trigger by name, without querying sys.triggers — the counterpart of
// Server.Database and Server.ServerTrigger.
//
// Every other field stays at its zero value; DatabaseTriggerByName is what
// populates them. EnableContext, DisableContext and DropContext address the
// trigger by name, so this handle is enough to act on one the caller already
// knows exists, and is the only usable form under a WithScript context, where
// DatabaseTriggerByNameContext's lookup is a real read.
func (d *Database) DatabaseTrigger(name string) *DatabaseTrigger {
	return &DatabaseTrigger{db: d, Name: name}
}

// Database returns the database the trigger is defined on.
func (t *DatabaseTrigger) Database() *Database { return t.db }

func scanDatabaseTrigger(d *Database, scan func(...any) error) (*DatabaseTrigger, error) {
	t := &DatabaseTrigger{db: d}
	var isDisabled bool
	var events, definition sql.NullString
	if err := scan(&t.Name, &isDisabled, &t.CreateDate, &t.ModifyDate, &events, &definition); err != nil {
		return nil, err
	}
	t.IsEnabled = !isDisabled
	if events.Valid && events.String != "" {
		t.Events = strings.Split(events.String, ",")
	}
	t.Definition = definition.String
	return t, nil
}

// -- Writes ----------------------------------------------------------------------

// Enable enables the trigger.
func (t *DatabaseTrigger) Enable() error { return t.EnableContext(context.Background()) }

// EnableContext is the context-aware variant of Enable.
func (t *DatabaseTrigger) EnableContext(ctx context.Context) error {
	return t.setEnabled(ctx, true)
}

// Disable disables the trigger, leaving its definition in place.
func (t *DatabaseTrigger) Disable() error { return t.DisableContext(context.Background()) }

// DisableContext is the context-aware variant of Disable.
func (t *DatabaseTrigger) DisableContext(ctx context.Context) error {
	return t.setEnabled(ctx, false)
}

func (t *DatabaseTrigger) setEnabled(ctx context.Context, enabled bool) error {
	verb := "DISABLE"
	if enabled {
		verb = "ENABLE"
	}
	stmt := fmt.Sprintf("%s TRIGGER %s ON DATABASE", verb, quoteIdent(t.Name))
	if _, err := t.db.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: %s database trigger %q: %w", strings.ToLower(verb), t.Name, err)
	}
	setIfApplied(ctx, &t.IsEnabled, enabled)
	return nil
}

// Drop removes the trigger. A trigger that isn't there is the server's error,
// not a silent success — see the note on Database.DropTable.
//
// This is not Database.DropTrigger: that one schema-qualifies the name, which
// a DDL trigger has no schema for, and omits the ON DATABASE clause the
// server requires here.
func (t *DatabaseTrigger) Drop() error { return t.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (t *DatabaseTrigger) DropContext(ctx context.Context) error {
	stmt := fmt.Sprintf("DROP TRIGGER %s ON DATABASE", quoteIdent(t.Name))
	if _, err := t.db.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: drop database trigger %q: %w", t.Name, err)
	}
	return nil
}
