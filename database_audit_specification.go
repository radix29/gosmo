package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// database_audit_specification.go covers database audit specifications —
// sys.database_audit_specifications, SSMS's <database> > Security > Database
// Audit Specifications folder. It is the database-scope counterpart of
// audit_specification.go, and everything that file says about the disable
// window applies here unchanged: SQL Server refuses every ALTER but the state
// toggle, and the DROP, while the specification is enabled.
//
// # It is not a copy of the server half
//
// A server specification records action *groups* only. A database one also
// records individual actions on securables —
// sys.database_audit_specification_details carries class_desc, major_id,
// minor_id and audited_principal_id per row — so a clause is either
//
//	ADD (SCHEMA_OBJECT_ACCESS_GROUP)
//
// or
//
//	ADD (SELECT ON OBJECT::[dbo].[T] BY [public])
//
// The two halves of that second form quote in opposite ways, and getting them
// the wrong way round is the injection hole here: the action name and the
// class are keywords, checked with validateAuditActionGroup and interpolated
// literally, while the securable and the principal are identifiers and go
// through qualifiedName/quoteIdent.

// DatabaseAuditSpecification mirrors a row of
// sys.database_audit_specifications.
type DatabaseAuditSpecification struct {
	db *Database

	SpecificationID int
	Name            string
	AuditGUID       string

	// AuditName is the server audit this specification writes to, resolved
	// through sys.server_audits. It is empty for an orphaned specification:
	// dropping an audit a specification still references succeeds and leaves
	// the audit_guid pointing at nothing, which is why the read below joins
	// with a LEFT JOIN.
	AuditName string

	IsEnabled  bool
	CreateDate time.Time
	ModifyDate time.Time

	// ActionGroups are the audit action groups the specification records, in
	// name order — the detail rows with is_group = 1.
	ActionGroups []string

	// Actions are the individual actions on securables the specification
	// records — the detail rows with is_group = 0.
	Actions []DatabaseAuditAction
}

// DatabaseAuditAction is one audited action on one securable: the
// `SELECT ON OBJECT::dbo.T BY public` form of a detail row.
type DatabaseAuditAction struct {
	// ActionName is the action keyword — SELECT, INSERT, EXECUTE, …
	ActionName string

	// ClassDesc is the securable class: OBJECT, SCHEMA or DATABASE. An empty
	// value writes OBJECT, which is what SSMS defaults to.
	ClassDesc string

	// SchemaName is the securable's schema, for an OBJECT. Empty for the
	// SCHEMA and DATABASE classes, whose securable is a single name.
	SchemaName string

	// ObjectName is the securable's name — the object, schema or database.
	ObjectName string

	// Principal is the database principal whose access is audited. An empty
	// value writes public.
	Principal string

	// AuditedResult is what the server records for the action (SUCCESS AND
	// FAILURE, SUCCESS, FAILURE). It is a property of the row, not part of
	// the ADD clause, and is read-only.
	AuditedResult string
}

// FullName returns the securable as it appears in the ADD clause —
// [dbo].[T] for an object, [dbo] for a schema.
func (a DatabaseAuditAction) FullName() string {
	return qualifiedName(a.SchemaName, a.ObjectName)
}

var databaseAuditSpecificationSelect = `
SELECT s.database_specification_id, s.name, CONVERT(varchar(36), s.audit_guid),
       a.name, s.is_state_enabled, s.create_date, s.modify_date
FROM   sys.database_audit_specifications s
LEFT   JOIN sys.server_audits a ON a.audit_guid = s.audit_guid`

// DatabaseAuditSpecifications returns every audit specification in the
// database, with its action groups and actions.
func (d *Database) DatabaseAuditSpecifications() ([]*DatabaseAuditSpecification, error) {
	return d.DatabaseAuditSpecificationsContext(context.Background())
}

// DatabaseAuditSpecificationsContext is the context-aware variant of
// DatabaseAuditSpecifications.
func (d *Database) DatabaseAuditSpecificationsContext(ctx context.Context) ([]*DatabaseAuditSpecification, error) {
	rows, err := d.query(ctx, databaseAuditSpecificationSelect+`
ORDER  BY s.name`)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list database audit specifications in %q: %w", d.name, err)
	}
	defer rows.Close()

	byID := map[int]*DatabaseAuditSpecification{}
	var out []*DatabaseAuditSpecification
	for rows.Next() {
		spec, err := scanDatabaseAuditSpecification(d, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list database audit specifications in %q: %w", d.name, err)
		}
		byID[spec.SpecificationID] = spec
		out = append(out, spec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list database audit specifications in %q: %w", d.name, err)
	}
	if len(out) == 0 {
		return out, nil
	}
	// One details query for every specification, grouped in Go — never one
	// per row inside the loop above.
	if err := d.loadAuditSpecificationDetails(ctx, byID, 0); err != nil {
		return nil, fmt.Errorf("gosmo: list database audit specifications in %q: %w", d.name, err)
	}
	return out, nil
}

// DatabaseAuditSpecificationByName returns one specification with every field
// populated, or a not-found error (errors.Is ErrNotFound).
func (d *Database) DatabaseAuditSpecificationByName(name string) (*DatabaseAuditSpecification, error) {
	return d.DatabaseAuditSpecificationByNameContext(context.Background(), name)
}

// DatabaseAuditSpecificationByNameContext is the context-aware variant of
// DatabaseAuditSpecificationByName.
func (d *Database) DatabaseAuditSpecificationByNameContext(ctx context.Context, name string) (*DatabaseAuditSpecification, error) {
	var spec *DatabaseAuditSpecification
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		spec, err = scanDatabaseAuditSpecification(d, row.Scan)
		return err
	}, databaseAuditSpecificationSelect+`
WHERE  s.name = @p1`, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: database audit specification %q not found in %q", name, d.name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read database audit specification %q in %q: %w", name, d.name, err)
	}
	byID := map[int]*DatabaseAuditSpecification{spec.SpecificationID: spec}
	if err := d.loadAuditSpecificationDetails(ctx, byID, spec.SpecificationID); err != nil {
		return nil, fmt.Errorf("gosmo: read database audit specification %q in %q: %w", name, d.name, err)
	}
	return spec, nil
}

// DatabaseAuditSpecification returns a lightweight handle by name, without
// querying the catalog — the counterpart of Server.Database. Every cached
// field stays at its zero value; DatabaseAuditSpecificationByName populates
// them. This is the only form usable under a WithScript-derived context.
func (d *Database) DatabaseAuditSpecification(name string) *DatabaseAuditSpecification {
	return &DatabaseAuditSpecification{db: d, Name: name}
}

// Database returns the database the specification lives in.
func (spec *DatabaseAuditSpecification) Database() *Database { return spec.db }

func scanDatabaseAuditSpecification(d *Database, scan func(...any) error) (*DatabaseAuditSpecification, error) {
	spec := &DatabaseAuditSpecification{db: d}
	var guid, auditName sql.NullString
	var enabled sql.NullBool
	if err := scan(&spec.SpecificationID, &spec.Name, &guid, &auditName, &enabled,
		&spec.CreateDate, &spec.ModifyDate); err != nil {
		return nil, err
	}
	spec.AuditGUID, spec.AuditName = guid.String, auditName.String
	spec.IsEnabled = enabled.Bool
	return spec, nil
}

// databaseAuditDetailSelect resolves major_id in SQL rather than handing the
// caller a raw id: the ids mean a different thing per class (object_id,
// schema_id, database_id), and a second round trip per row to translate them
// is the shape that exhausts a connection pool.
//
// class_desc is translated on the way out, and that is not cosmetic: the
// catalog records an object row as OBJECT_OR_COLUMN, which is not the keyword
// the ADD clause takes — verified live on major 17, a specification read back
// and re-scripted with the catalog's own value fails as an invalid class, and
// the OBJECT_SCHEMA_NAME/OBJECT_NAME arms never fire, so the securable comes
// back empty.
const databaseAuditDetailSelect = `
SELECT d.database_specification_id, d.audit_action_name,
       CASE d.class_desc WHEN 'OBJECT_OR_COLUMN' THEN 'OBJECT' ELSE d.class_desc END,
       d.is_group, d.audited_result,
       CASE d.class_desc WHEN 'OBJECT_OR_COLUMN' THEN OBJECT_SCHEMA_NAME(d.major_id) END,
       CASE d.class_desc
            WHEN 'OBJECT_OR_COLUMN' THEN OBJECT_NAME(d.major_id)
            WHEN 'SCHEMA'           THEN SCHEMA_NAME(d.major_id)
            WHEN 'DATABASE'         THEN DB_NAME(d.major_id)
       END,
       p.name
FROM   sys.database_audit_specification_details d
LEFT   JOIN sys.database_principals p ON p.principal_id = d.audited_principal_id`

// loadAuditSpecificationDetails fills in ActionGroups and Actions for every
// specification in byID with one query. specID narrows the read to a single
// specification; zero reads the details of them all.
func (d *Database) loadAuditSpecificationDetails(ctx context.Context, byID map[int]*DatabaseAuditSpecification, specID int) error {
	where, args := "", []any(nil)
	if specID != 0 {
		where, args = "\nWHERE  d.database_specification_id = @p1", []any{specID}
	}
	rows, err := d.query(ctx, databaseAuditDetailSelect+where+`
ORDER  BY d.database_specification_id, d.audit_action_name`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var id int
		var action, class string
		var isGroup bool
		var result, schema, object, principal sql.NullString
		if err := rows.Scan(&id, &action, &class, &isGroup, &result, &schema, &object, &principal); err != nil {
			return err
		}
		spec, ok := byID[id]
		if !ok {
			continue
		}
		if isGroup {
			spec.ActionGroups = append(spec.ActionGroups, action)
			continue
		}
		spec.Actions = append(spec.Actions, DatabaseAuditAction{
			ActionName:    action,
			ClassDesc:     class,
			SchemaName:    schema.String,
			ObjectName:    object.String,
			Principal:     principal.String,
			AuditedResult: result.String,
		})
	}
	return rows.Err()
}

// -- The action pick lists -------------------------------------------------------

// DatabaseAuditActionGroups returns every database-scope audit action group
// the instance knows about.
func (s *Server) DatabaseAuditActionGroups() ([]string, error) {
	return s.DatabaseAuditActionGroupsContext(context.Background())
}

// DatabaseAuditActionGroupsContext is the context-aware variant of
// DatabaseAuditActionGroups.
//
// Read from sys.dm_audit_actions for the same reason the server-scope list is
// (AuditActionGroupsContext): every release adds groups, and a hard-coded
// table would quietly hide the new ones from a pick list.
func (s *Server) DatabaseAuditActionGroupsContext(ctx context.Context) ([]string, error) {
	return s.auditActionNames(ctx, "class_desc = 'DATABASE' AND configuration_level = 'Group'",
		"list database audit action groups")
}

// DatabaseAuditActions returns every individual database-scope audit action —
// SELECT, INSERT, EXECUTE and the rest — for the per-securable clause form.
//
// All three securable classes are read, not just DATABASE: the same action
// name is a separate row per class in sys.dm_audit_actions, and a pick list
// restricted to the DATABASE class would leave out what can be audited on an
// object or a schema.
func (s *Server) DatabaseAuditActions() ([]string, error) {
	return s.DatabaseAuditActionsContext(context.Background())
}

// DatabaseAuditActionsContext is the context-aware variant of
// DatabaseAuditActions.
func (s *Server) DatabaseAuditActionsContext(ctx context.Context) ([]string, error) {
	return s.auditActionNames(ctx,
		"class_desc IN ('DATABASE', 'SCHEMA', 'OBJECT') AND configuration_level = 'Action'",
		"list database audit actions")
}

// auditActionNames reads distinct names out of sys.dm_audit_actions under
// where. The predicate is a constant assembled by the two callers above, never
// caller input — sys.dm_audit_actions takes no parameter here because the
// column is compared to a literal keyword set.
func (s *Server) auditActionNames(ctx context.Context, where, what string) ([]string, error) {
	q := `
SELECT DISTINCT name
FROM   sys.dm_audit_actions
WHERE  ` + where + `
ORDER  BY name`

	rows, err := s.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: %s: %w", what, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("gosmo: %s: %w", what, err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: %s: %w", what, err)
	}
	return out, nil
}

// -- Writes ----------------------------------------------------------------------

// DatabaseAuditSpecificationSpec describes a specification to create.
type DatabaseAuditSpecificationSpec struct {
	Name string

	// AuditName is the server audit the specification writes to. Required.
	AuditName string

	// ActionGroups are the database-scope action groups to record.
	ActionGroups []string

	// Actions are the individual actions on securables to record. A
	// specification with neither groups nor actions is legal and records
	// nothing.
	Actions []DatabaseAuditAction

	// Enabled creates the specification with STATE = ON.
	Enabled bool
}

// auditSecurableClasses are the securable classes a database audit
// specification's ADD clause accepts. The class is a keyword, so it is
// checked against this set rather than quoted.
// OBJECT_OR_COLUMN is accepted as a spelling of OBJECT: it is what
// sys.database_audit_specification_details records, so a caller building an
// action out of a raw catalog read reaches here with it.
var auditSecurableClasses = map[string]string{
	"OBJECT":           "OBJECT",
	"OBJECT_OR_COLUMN": "OBJECT",
	"SCHEMA":           "SCHEMA",
	"DATABASE":         "DATABASE",
}

// clause renders the action as the inside of an ADD/DROP (...) — the
// `SELECT ON OBJECT::[dbo].[T] BY [public]` form.
func (a DatabaseAuditAction) clause() (string, error) {
	// The action name is a keyword, exactly like a group name: it cannot be
	// bracket-quoted, so it is charset-checked and interpolated literally.
	if err := validateAuditActionGroup(a.ActionName); err != nil {
		return "", fmt.Errorf("invalid audit action: %w", err)
	}
	class := strings.ToUpper(strings.TrimSpace(a.ClassDesc))
	if class == "" {
		class = "OBJECT"
	}
	keyword, ok := auditSecurableClasses[class]
	if !ok {
		return "", fmt.Errorf("invalid audit securable class %q", a.ClassDesc)
	}
	class = keyword
	if strings.TrimSpace(a.ObjectName) == "" {
		return "", fmt.Errorf("audit action %s names no securable", a.ActionName)
	}
	principal := a.Principal
	if strings.TrimSpace(principal) == "" {
		principal = "public"
	}
	// The securable and the principal are identifiers, and quote the
	// opposite way to the two keywords above.
	return fmt.Sprintf("%s ON %s::%s BY %s",
		a.ActionName, class, a.FullName(), quoteIdent(principal)), nil
}

// databaseAuditClauses renders the whole ADD/DROP clause list — groups first,
// then per-securable actions, in the order given.
func databaseAuditClauses(verb string, groups []string, actions []DatabaseAuditAction) (string, error) {
	parts := make([]string, 0, len(groups)+len(actions))
	for _, g := range groups {
		if err := validateAuditActionGroup(g); err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", verb, g))
	}
	for _, a := range actions {
		clause, err := a.clause()
		if err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", verb, clause))
	}
	return strings.Join(parts, ",\n    "), nil
}

func (spec DatabaseAuditSpecificationSpec) createStatement() (string, error) {
	if strings.TrimSpace(spec.Name) == "" {
		return "", fmt.Errorf("database audit specification has no name")
	}
	if strings.TrimSpace(spec.AuditName) == "" {
		return "", fmt.Errorf("database audit specification %q names no audit", spec.Name)
	}
	stmt := fmt.Sprintf("CREATE DATABASE AUDIT SPECIFICATION %s\nFOR SERVER AUDIT %s",
		quoteIdent(spec.Name), quoteIdent(spec.AuditName))
	if len(spec.ActionGroups) > 0 || len(spec.Actions) > 0 {
		clauses, err := databaseAuditClauses("ADD", spec.ActionGroups, spec.Actions)
		if err != nil {
			return "", err
		}
		stmt += "\n    " + clauses
	}
	state := "OFF"
	if spec.Enabled {
		state = "ON"
	}
	return stmt + "\nWITH ( STATE = " + state + " )", nil
}

// CreateDatabaseAuditSpecification creates a database audit specification.
func (d *Database) CreateDatabaseAuditSpecification(spec DatabaseAuditSpecificationSpec) (*DatabaseAuditSpecification, error) {
	return d.CreateDatabaseAuditSpecificationContext(context.Background(), spec)
}

// CreateDatabaseAuditSpecificationContext is the context-aware variant of
// CreateDatabaseAuditSpecification.
func (d *Database) CreateDatabaseAuditSpecificationContext(ctx context.Context, spec DatabaseAuditSpecificationSpec) (*DatabaseAuditSpecification, error) {
	stmt, err := spec.createStatement()
	if err != nil {
		return nil, fmt.Errorf("gosmo: create database audit specification: %w", err)
	}
	if _, err := d.exec(ctx, stmt); err != nil {
		return nil, fmt.Errorf("gosmo: create database audit specification %q in %q: %w", spec.Name, d.name, err)
	}
	if Scripting(ctx) {
		// The CREATE was only collected, so there is nothing to read back.
		return d.DatabaseAuditSpecification(spec.Name), nil
	}
	return d.DatabaseAuditSpecificationByNameContext(ctx, spec.Name)
}

// SetState enables or disables the specification.
func (spec *DatabaseAuditSpecification) SetState(on bool) error {
	return spec.SetStateContext(context.Background(), on)
}

// SetStateContext is the context-aware variant of SetState. This is the one
// ALTER form the server accepts on an enabled specification.
func (spec *DatabaseAuditSpecification) SetStateContext(ctx context.Context, on bool) error {
	state := "OFF"
	if on {
		state = "ON"
	}
	stmt := fmt.Sprintf("ALTER DATABASE AUDIT SPECIFICATION %s WITH ( STATE = %s )",
		quoteIdent(spec.Name), state)
	if _, err := spec.db.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: set database audit specification %q state: %w", spec.Name, err)
	}
	setIfApplied(ctx, &spec.IsEnabled, on)
	return nil
}

// isEnabledContext reads the specification's current state from the catalog
// rather than trusting the receiver, which may be a name-only handle.
func (spec *DatabaseAuditSpecification) isEnabledContext(ctx context.Context) (bool, error) {
	var enabled sql.NullBool
	err := spec.db.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&enabled)
	}, "SELECT is_state_enabled FROM sys.database_audit_specifications WHERE name = @p1", spec.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, notFoundf("gosmo: database audit specification %q not found in %q", spec.Name, spec.db.name)
	}
	if err != nil {
		return false, err
	}
	return enabled.Bool, nil
}

// windowKey qualifies the specification with its database, so that the
// disable-window marker shared with the server half (specificationDisabledKey)
// cannot confuse a database specification with a server one, or with a
// same-named specification in another database.
func (spec *DatabaseAuditSpecification) windowKey() string {
	return spec.db.name + "." + spec.Name
}

// inSpecificationWindow reports whether an enclosing WithDisabled window
// already has this specification switched off, so a nested write must not open
// a second one.
func (spec *DatabaseAuditSpecification) inSpecificationWindow(ctx context.Context) bool {
	name, _ := ctx.Value(specificationDisabledKey{}).(string)
	return name == spec.windowKey()
}

// WithDisabled runs fn with the specification disabled, restoring the state
// afterwards. Every write method already does this for itself, so a caller
// needs WithDisabled only to make several of them share one window: recording
// then stops once for the whole batch instead of once per statement, and a
// failure part-way through cannot leave the specification off.
//
// fn must use the context it is handed — that is what the nested writes read
// to know the window is already open.
func (spec *DatabaseAuditSpecification) WithDisabled(ctx context.Context, fn func(context.Context) error) error {
	return spec.withSpecificationDisabled(ctx, fn)
}

// withSpecificationDisabled is the database-scope twin of the server half's
// method of the same name (audit_specification.go), and must stay identical in
// behaviour: restore only if this call was the one that disabled the
// specification, and restore on the failure path too, so a failed apply cannot
// silently leave auditing switched off.
func (spec *DatabaseAuditSpecification) withSpecificationDisabled(ctx context.Context, fn func(context.Context) error) error {
	if spec.inSpecificationWindow(ctx) {
		return fn(ctx)
	}
	enabled, err := spec.isEnabledContext(ctx)
	if err != nil {
		return err
	}
	inner := context.WithValue(ctx, specificationDisabledKey{}, spec.windowKey())
	if !enabled {
		return fn(inner)
	}
	if err := spec.SetStateContext(ctx, false); err != nil {
		return err
	}
	enable := func(ctx context.Context) error { return spec.SetStateContext(ctx, true) }
	if err := fn(inner); err != nil {
		// Best effort: report the original failure, not the restore's.
		_ = restoreWindow(ctx, enable)
		return err
	}
	return restoreWindow(ctx, enable)
}

// AddActions adds audit action groups and per-securable actions to the
// specification. Either list may be empty; both being empty writes nothing.
func (spec *DatabaseAuditSpecification) AddActions(groups []string, actions []DatabaseAuditAction) error {
	return spec.AddActionsContext(context.Background(), groups, actions)
}

// AddActionsContext is the context-aware variant of AddActions. The
// specification is disabled for the duration and restored afterwards.
func (spec *DatabaseAuditSpecification) AddActionsContext(ctx context.Context, groups []string, actions []DatabaseAuditAction) error {
	return spec.alterActionsContext(ctx, "ADD", groups, actions)
}

// DropActions removes audit action groups and per-securable actions from the
// specification.
func (spec *DatabaseAuditSpecification) DropActions(groups []string, actions []DatabaseAuditAction) error {
	return spec.DropActionsContext(context.Background(), groups, actions)
}

// DropActionsContext is the context-aware variant of DropActions.
func (spec *DatabaseAuditSpecification) DropActionsContext(ctx context.Context, groups []string, actions []DatabaseAuditAction) error {
	return spec.alterActionsContext(ctx, "DROP", groups, actions)
}

func (spec *DatabaseAuditSpecification) alterActionsContext(ctx context.Context, verb string, groups []string, actions []DatabaseAuditAction) error {
	if len(groups) == 0 && len(actions) == 0 {
		return nil
	}
	clauses, err := databaseAuditClauses(verb, groups, actions)
	if err != nil {
		return fmt.Errorf("gosmo: alter database audit specification %q: %w", spec.Name, err)
	}
	stmt := fmt.Sprintf("ALTER DATABASE AUDIT SPECIFICATION %s\n    %s", quoteIdent(spec.Name), clauses)
	return spec.withSpecificationDisabled(ctx, func(ctx context.Context) error {
		if _, err := spec.db.exec(ctx, stmt); err != nil {
			return fmt.Errorf("gosmo: alter database audit specification %q: %w", spec.Name, err)
		}
		return nil
	})
}

// SetAudit rebinds the specification to a different server audit.
func (spec *DatabaseAuditSpecification) SetAudit(auditName string) error {
	return spec.SetAuditContext(context.Background(), auditName)
}

// SetAuditContext is the context-aware variant of SetAudit. The specification
// is disabled for the duration and restored afterwards.
func (spec *DatabaseAuditSpecification) SetAuditContext(ctx context.Context, auditName string) error {
	if strings.TrimSpace(auditName) == "" {
		return fmt.Errorf("gosmo: alter database audit specification %q: audit name is empty", spec.Name)
	}
	stmt := fmt.Sprintf("ALTER DATABASE AUDIT SPECIFICATION %s\nFOR SERVER AUDIT %s",
		quoteIdent(spec.Name), quoteIdent(auditName))
	err := spec.withSpecificationDisabled(ctx, func(ctx context.Context) error {
		if _, err := spec.db.exec(ctx, stmt); err != nil {
			return fmt.Errorf("gosmo: alter database audit specification %q: %w", spec.Name, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	setIfApplied(ctx, &spec.AuditName, auditName)
	return nil
}

// Drop deletes the specification.
func (spec *DatabaseAuditSpecification) Drop() error {
	return spec.DropContext(context.Background())
}

// DropContext is the context-aware variant of Drop. An enabled specification
// is disabled first; there is nothing to restore afterwards.
func (spec *DatabaseAuditSpecification) DropContext(ctx context.Context) error {
	enabled, err := spec.isEnabledContext(ctx)
	if err != nil {
		return err
	}
	if enabled {
		if err := spec.SetStateContext(ctx, false); err != nil {
			return err
		}
	}
	stmt := "DROP DATABASE AUDIT SPECIFICATION " + quoteIdent(spec.Name)
	if _, err := spec.db.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: drop database audit specification %q: %w", spec.Name, err)
	}
	return nil
}
