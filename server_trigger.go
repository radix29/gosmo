package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// server_trigger.go covers server-scope DDL and logon triggers —
// sys.server_triggers, SSMS's Server Objects > Triggers folder.
//
// These are a different family from Database.Triggers, which reads
// sys.triggers with parent_class = 1 (DML triggers on a table). Database-scope
// DDL triggers (sys.triggers, parent_class = 0) are a third family and are not
// read here.

// ServerTrigger mirrors a row of sys.server_triggers — a DDL or LOGON trigger
// defined ON ALL SERVER.
type ServerTrigger struct {
	server *Server

	Name string

	// IsEnabled is the inverse of the catalog's is_disabled.
	IsEnabled bool

	CreateDate time.Time
	ModifyDate time.Time

	// Events are the type_desc values from sys.server_trigger_events —
	// "CREATE_DATABASE", "LOGON", and so on. A trigger declared FOR a whole
	// event group lists the group's individual events, which is what the
	// catalog records.
	Events []string

	// Definition is the trigger body from sys.server_sql_modules. It is empty
	// for an encrypted trigger (the catalog reports NULL) and for a CLR
	// trigger, which has no row there at all.
	Definition string
}

// Server returns the server the trigger belongs to.
func (t *ServerTrigger) Server() *Server { return t.server }

// serverTriggerSelect reads sys.server_triggers.
//
// The join onto sys.server_sql_modules is a LEFT JOIN because a CLR trigger
// (type 'TA') has no row there — an inner join silently drops it from the
// folder. is_ms_shipped = 0 matches what SSMS lists and what
// Database.triggersWhere does.
var serverTriggerSelect = `
SELECT tr.name, tr.is_disabled, tr.create_date, tr.modify_date,
       ` + jsonList("te.type_desc", `
        FROM   sys.server_trigger_events te
        WHERE  te.object_id = tr.object_id`, "") + ` AS events,
       m.definition
FROM   sys.server_triggers tr
LEFT   JOIN sys.server_sql_modules m ON m.object_id = tr.object_id
WHERE  tr.is_ms_shipped = 0`

// ServerTriggers returns every server-scope DDL or logon trigger.
func (s *Server) ServerTriggers(ctx context.Context) ([]*ServerTrigger, error) {
	rows, err := s.query(ctx, serverTriggerSelect+`
ORDER  BY tr.name`)
	return scanRows(rows, err, "list server triggers", func(scan func(...any) error) (*ServerTrigger, error) {
		return scanServerTrigger(s, scan)
	})
}

// ServerTriggerByName returns one server trigger with every field populated,
// or a not-found error (errors.Is ErrNotFound) when the server has none by
// that name.
func (s *Server) ServerTriggerByName(ctx context.Context, name string) (*ServerTrigger, error) {
	var t *ServerTrigger
	err := s.queryRow(ctx, func(row *sql.Row) error {
		var err error
		t, err = scanServerTrigger(s, row.Scan)
		return err
	}, serverTriggerSelect+`
   AND tr.name = @p1`, name)
	return foundRow(t, err, notFoundf("gosmo: server trigger %q not found", name), fmt.Sprintf("read server trigger %q", name))
}

// ServerTriggerRef returns a lightweight handle for a server trigger by name,
// without querying sys.server_triggers — the counterpart of Server.DatabaseRef.
// Every other field stays at its zero value; ServerTriggerByName is what
// populates them.
//
// Enable, Disable and Drop address the trigger by name,
// so this handle is enough to act on one the caller already knows exists, and
// is the form to use when there is nothing to read yet — under a
// WithScript-derived context, ServerTriggerByName's lookup is a real
// read and therefore finds nothing for a trigger whose CREATE was merely
// collected.
func (s *Server) ServerTriggerRef(name string) *ServerTrigger {
	return &ServerTrigger{server: s, Name: name}
}

// scanServerTrigger decodes one sys.server_triggers row into a ServerTrigger.
//
// Twin of scanDatabaseTrigger in database_trigger.go. That file and this one
// are deliberately near-identical: this function, and the
// Enable/Disable/setEnabled block below it, differ only in the receiver type
// and in the scope the statement targets (ON ALL SERVER here, ON DATABASE
// there). Unifying them needs either generics over two receivers with
// different db/server fields or a shared struct both embed, and the second
// changes two exported types' shapes for no caller's benefit — so the
// duplication stays. Change one, look at the other.
func scanServerTrigger(s *Server, scan func(...any) error) (*ServerTrigger, error) {
	t := &ServerTrigger{server: s}
	var isDisabled bool
	var events, definition sql.NullString
	if err := scan(&t.Name, &isDisabled, &t.CreateDate, &t.ModifyDate, &events, &definition); err != nil {
		return nil, err
	}
	t.IsEnabled = !isDisabled
	var err error
	if t.Events, err = decodeJSONList(events); err != nil {
		return nil, err
	}
	t.Definition = definition.String
	return t, nil
}

// -- Writes ----------------------------------------------------------------------

// Enable enables the trigger.
func (t *ServerTrigger) Enable(ctx context.Context) error {
	return t.setEnabled(ctx, true)
}

// Disable disables the trigger, leaving its definition in place.
func (t *ServerTrigger) Disable(ctx context.Context) error {
	return t.setEnabled(ctx, false)
}

func (t *ServerTrigger) setEnabled(ctx context.Context, enabled bool) error {
	verb := "DISABLE"
	if enabled {
		verb = "ENABLE"
	}
	stmt := fmt.Sprintf("%s TRIGGER %s ON ALL SERVER", verb, quoteIdent(t.Name))
	if err := t.server.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: %s server trigger %q: %w", strings.ToLower(verb), t.Name, err)
	}
	setIfApplied(ctx, &t.IsEnabled, enabled)
	return nil
}

// Drop removes the trigger. A trigger that isn't there is the server's error,
// not a silent success — see the note on Database.DropTable.
func (t *ServerTrigger) Drop(ctx context.Context) error {
	stmt := fmt.Sprintf("DROP TRIGGER %s ON ALL SERVER", quoteIdent(t.Name))
	if err := t.server.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: drop server trigger %q: %w", t.Name, err)
	}
	return nil
}
