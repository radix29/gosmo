package gosmo

import (
	"context"
	"database/sql"
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
func (d *Database) DatabaseTriggers(ctx context.Context) ([]*DatabaseTrigger, error) {
	rows, err := d.query(ctx, databaseTriggerSelect+`
ORDER  BY tr.name`)
	return scanRows(rows, err, fmt.Sprintf("list database triggers in %q", d.Name), func(scan func(...any) error) (*DatabaseTrigger, error) {
		return scanDatabaseTrigger(d, scan)
	})
}

// DatabaseTriggerByName returns one database-scope DDL trigger with every
// field populated, or a not-found error (errors.Is ErrNotFound) when the
// database has none by that name.
func (d *Database) DatabaseTriggerByName(ctx context.Context, name string) (*DatabaseTrigger, error) {
	var t *DatabaseTrigger
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		t, err = scanDatabaseTrigger(d, row.Scan)
		return err
	}, databaseTriggerSelect+`
   AND tr.name = @p1`, name)
	return foundRow(t, err, notFoundf("gosmo: database trigger %q not found in %q", name, d.Name), fmt.Sprintf("read database trigger %q in %q", name, d.Name))
}

// DatabaseTriggerRef returns a lightweight handle for a database-scope DDL
// trigger by name, without querying sys.triggers — the counterpart of
// Server.DatabaseRef and Server.ServerTriggerRef.
//
// Every other field stays at its zero value; DatabaseTriggerByName is what
// populates them. Enable, Disable and Drop address the
// trigger by name, so this handle is enough to act on one the caller already
// knows exists, and is the form to use when there is nothing to read yet —
// under a WithScript-derived context, DatabaseTriggerByName's lookup is
// a real read and therefore finds nothing for a trigger whose CREATE was
// merely collected.
func (d *Database) DatabaseTriggerRef(name string) *DatabaseTrigger {
	return &DatabaseTrigger{db: d, Name: name}
}

// Database returns the database the trigger is defined on.
func (t *DatabaseTrigger) Database() *Database { return t.db }

// scanDatabaseTrigger decodes one sys.triggers row into a DatabaseTrigger.
//
// Twin of scanServerTrigger in server_trigger.go. That file and this one are
// deliberately near-identical: this function, and the Enable/Disable/setEnabled
// block below it, differ only in the receiver type and in the scope the
// statement targets (ON DATABASE here, ON ALL SERVER there). Unifying them
// needs either generics over two receivers with different db/server fields or
// a shared struct both embed, and the second changes two exported types'
// shapes for no caller's benefit — so the duplication stays. Change one, look
// at the other.
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
func (t *DatabaseTrigger) Enable(ctx context.Context) error {
	return t.setEnabled(ctx, true)
}

// Disable disables the trigger, leaving its definition in place.
func (t *DatabaseTrigger) Disable(ctx context.Context) error {
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
func (t *DatabaseTrigger) Drop(ctx context.Context) error {
	stmt := fmt.Sprintf("DROP TRIGGER %s ON DATABASE", quoteIdent(t.Name))
	if _, err := t.db.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: drop database trigger %q: %w", t.Name, err)
	}
	return nil
}
