package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// audit_specification.go covers server audit specifications —
// sys.server_audit_specifications, SSMS's Security > Server Audit
// Specifications folder. A specification names the action groups an audit
// records and the audit it writes them to.
//
// # Every change needs the specification disabled
//
// As with a server audit, SQL Server refuses every ALTER but the state toggle,
// and the DROP, while the specification is enabled ("Changes to an audit
// specification must be done while the audit specification is disabled") —
// verified live. withSpecificationDisabled does the off/apply/on dance, and
// restores the state on the failure path.

// ServerAuditSpecification mirrors a row of sys.server_audit_specifications.
type ServerAuditSpecification struct {
	server *Server

	SpecificationID int
	Name            string
	AuditGUID       string

	// AuditName is the audit this specification writes to, resolved through
	// sys.server_audits. It is empty for an orphaned specification: dropping
	// an audit a specification still references succeeds and leaves the
	// audit_guid pointing at nothing, which is why the read below joins with
	// a LEFT JOIN.
	AuditName string

	IsEnabled  bool
	CreateDate time.Time
	ModifyDate time.Time

	// ActionGroups are the audit action groups the specification records, in
	// name order.
	ActionGroups []string
}

var serverAuditSpecificationSelect = `
SELECT s.server_specification_id, s.name, CONVERT(varchar(36), s.audit_guid),
       a.name, s.is_state_enabled, s.create_date, s.modify_date,
       ` + commaList("d.audit_action_name", `
        FROM   sys.server_audit_specification_details d
        WHERE  d.server_specification_id = s.server_specification_id`,
	"d.audit_action_name") + ` AS action_groups
FROM   sys.server_audit_specifications s
LEFT   JOIN sys.server_audits a ON a.audit_guid = s.audit_guid`

// ServerAuditSpecifications returns every server audit specification.
func (s *Server) ServerAuditSpecifications() ([]*ServerAuditSpecification, error) {
	return s.ServerAuditSpecificationsContext(context.Background())
}

// ServerAuditSpecificationsContext is the context-aware variant of
// ServerAuditSpecifications.
func (s *Server) ServerAuditSpecificationsContext(ctx context.Context) ([]*ServerAuditSpecification, error) {
	rows, err := s.query(ctx, serverAuditSpecificationSelect+`
ORDER  BY s.name`)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list server audit specifications: %w", err)
	}
	defer rows.Close()

	var out []*ServerAuditSpecification
	for rows.Next() {
		spec, err := scanServerAuditSpecification(s, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list server audit specifications: %w", err)
		}
		out = append(out, spec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list server audit specifications: %w", err)
	}
	return out, nil
}

// ServerAuditSpecificationByName returns one specification with every field
// populated, or a not-found error (errors.Is ErrNotFound).
func (s *Server) ServerAuditSpecificationByName(name string) (*ServerAuditSpecification, error) {
	return s.ServerAuditSpecificationByNameContext(context.Background(), name)
}

// ServerAuditSpecificationByNameContext is the context-aware variant of
// ServerAuditSpecificationByName.
func (s *Server) ServerAuditSpecificationByNameContext(ctx context.Context, name string) (*ServerAuditSpecification, error) {
	var spec *ServerAuditSpecification
	err := s.queryRow(ctx, func(row *sql.Row) error {
		var err error
		spec, err = scanServerAuditSpecification(s, row.Scan)
		return err
	}, serverAuditSpecificationSelect+`
WHERE  s.name = @p1`, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: server audit specification %q not found", name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read server audit specification %q: %w", name, err)
	}
	return spec, nil
}

// ServerAuditSpecification returns a lightweight handle by name, without
// querying the catalog — the counterpart of Server.Database. Every cached
// field stays at its zero value; ServerAuditSpecificationByName populates
// them.
func (s *Server) ServerAuditSpecification(name string) *ServerAuditSpecification {
	return &ServerAuditSpecification{server: s, Name: name}
}

func scanServerAuditSpecification(s *Server, scan func(...any) error) (*ServerAuditSpecification, error) {
	spec := &ServerAuditSpecification{server: s}
	var guid, auditName, groups sql.NullString
	var enabled sql.NullBool
	if err := scan(&spec.SpecificationID, &spec.Name, &guid, &auditName, &enabled,
		&spec.CreateDate, &spec.ModifyDate, &groups); err != nil {
		return nil, err
	}
	spec.AuditGUID, spec.AuditName = guid.String, auditName.String
	spec.IsEnabled = enabled.Bool
	if groups.String != "" {
		spec.ActionGroups = strings.Split(groups.String, ",")
	}
	return spec, nil
}

// -- The action-group pick list --------------------------------------------------

// AuditActionGroups returns every server-scope audit action group the instance
// knows about.
func (s *Server) AuditActionGroups() ([]string, error) {
	return s.AuditActionGroupsContext(context.Background())
}

// AuditActionGroupsContext is the context-aware variant of AuditActionGroups.
//
// The list is read from sys.dm_audit_actions rather than hard-coded so it stays
// right across versions: each release adds groups, and a fixed table would
// quietly hide the new ones from anything building a pick list.
func (s *Server) AuditActionGroupsContext(ctx context.Context) ([]string, error) {
	const q = `
SELECT name
FROM   sys.dm_audit_actions
WHERE  class_desc = 'SERVER' AND configuration_level = 'Group'
ORDER  BY name`

	rows, err := s.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list audit action groups: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("gosmo: list audit action groups: %w", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list audit action groups: %w", err)
	}
	return out, nil
}

// -- Writes ----------------------------------------------------------------------

// ServerAuditSpecificationSpec describes a specification to create.
type ServerAuditSpecificationSpec struct {
	Name string

	// AuditName is the server audit the specification writes to. Required.
	AuditName string

	// ActionGroups are the groups to record. A specification with none is
	// legal and records nothing.
	ActionGroups []string

	// Enabled creates the specification with STATE = ON.
	Enabled bool
}

// validateAuditActionGroup rejects anything that is not a bare group name.
//
// A group name is a keyword inside the ADD (...) clause, not an identifier:
// brackets are a syntax error there, so quoteIdent cannot be used and the name
// goes into the statement literally. Names come from sys.dm_audit_actions, but
// nothing stops a caller passing its own string, so the charset is checked
// rather than trusted.
func validateAuditActionGroup(group string) error {
	if group == "" {
		return fmt.Errorf("empty audit action group")
	}
	for _, r := range group {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
		default:
			return fmt.Errorf("invalid audit action group %q", group)
		}
	}
	return nil
}

func auditActionGroupClauses(verb string, groups []string) (string, error) {
	parts := make([]string, 0, len(groups))
	for _, g := range groups {
		if err := validateAuditActionGroup(g); err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", verb, g))
	}
	return strings.Join(parts, ",\n    "), nil
}

func (spec ServerAuditSpecificationSpec) createStatement() (string, error) {
	if strings.TrimSpace(spec.Name) == "" {
		return "", fmt.Errorf("server audit specification has no name")
	}
	if strings.TrimSpace(spec.AuditName) == "" {
		return "", fmt.Errorf("server audit specification %q names no audit", spec.Name)
	}
	stmt := fmt.Sprintf("CREATE SERVER AUDIT SPECIFICATION %s\nFOR SERVER AUDIT %s",
		quoteIdent(spec.Name), quoteIdent(spec.AuditName))
	if len(spec.ActionGroups) > 0 {
		clauses, err := auditActionGroupClauses("ADD", spec.ActionGroups)
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

// CreateServerAuditSpecification creates a server audit specification.
func (s *Server) CreateServerAuditSpecification(spec ServerAuditSpecificationSpec) (*ServerAuditSpecification, error) {
	return s.CreateServerAuditSpecificationContext(context.Background(), spec)
}

// CreateServerAuditSpecificationContext is the context-aware variant of
// CreateServerAuditSpecification.
func (s *Server) CreateServerAuditSpecificationContext(ctx context.Context, spec ServerAuditSpecificationSpec) (*ServerAuditSpecification, error) {
	stmt, err := spec.createStatement()
	if err != nil {
		return nil, fmt.Errorf("gosmo: create server audit specification: %w", err)
	}
	if err := s.execContext(ctx, stmt); err != nil {
		return nil, fmt.Errorf("gosmo: create server audit specification %q: %w", spec.Name, err)
	}
	if Scripting(ctx) {
		// The CREATE was only collected, so there is nothing to read back.
		return s.ServerAuditSpecification(spec.Name), nil
	}
	return s.ServerAuditSpecificationByNameContext(ctx, spec.Name)
}

// SetState enables or disables the specification.
func (spec *ServerAuditSpecification) SetState(on bool) error {
	return spec.SetStateContext(context.Background(), on)
}

// SetStateContext is the context-aware variant of SetState. This is the one
// ALTER form the server accepts on an enabled specification.
func (spec *ServerAuditSpecification) SetStateContext(ctx context.Context, on bool) error {
	state := "OFF"
	if on {
		state = "ON"
	}
	stmt := fmt.Sprintf("ALTER SERVER AUDIT SPECIFICATION %s WITH ( STATE = %s )",
		quoteIdent(spec.Name), state)
	if err := spec.server.execContext(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: set server audit specification %q state: %w", spec.Name, err)
	}
	setIfApplied(ctx, &spec.IsEnabled, on)
	return nil
}

// isEnabledContext reads the specification's current state from the catalog
// rather than trusting the receiver, which may be a name-only handle.
func (spec *ServerAuditSpecification) isEnabledContext(ctx context.Context) (bool, error) {
	var enabled sql.NullBool
	err := spec.server.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&enabled)
	}, "SELECT is_state_enabled FROM sys.server_audit_specifications WHERE name = @p1", spec.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, notFoundf("gosmo: server audit specification %q not found", spec.Name)
	}
	if err != nil {
		return false, err
	}
	return enabled.Bool, nil
}

// specificationDisabledKey marks a context that is already inside a disable
// window, carrying the name of the specification that window switched off.
type specificationDisabledKey struct{}

// inSpecificationWindow reports whether an enclosing WithDisabled window
// already has this specification switched off, so a nested write must not open
// a second one.
func (spec *ServerAuditSpecification) inSpecificationWindow(ctx context.Context) bool {
	name, _ := ctx.Value(specificationDisabledKey{}).(string)
	return name == spec.Name
}

// WithDisabled runs fn with the specification disabled, restoring the state
// afterwards. Every write method already does this for itself, so a caller
// needs WithDisabled only to make several of them share one window: recording
// then stops once for the whole batch instead of once per statement, and a
// failure part-way through cannot leave the specification off.
//
// fn must use the context it is handed — that is what the nested writes read
// to know the window is already open.
func (spec *ServerAuditSpecification) WithDisabled(ctx context.Context, fn func(context.Context) error) error {
	return spec.withSpecificationDisabled(ctx, fn)
}

// withSpecificationDisabled runs fn with the specification disabled, restoring
// the state afterwards only if it was this call that disabled it. Re-enabling
// unconditionally would turn on a specification the user had deliberately left
// off; restoring on the failure path is what keeps a failed apply from
// silently leaving auditing switched off.
func (spec *ServerAuditSpecification) withSpecificationDisabled(ctx context.Context, fn func(context.Context) error) error {
	if spec.inSpecificationWindow(ctx) {
		return fn(ctx)
	}
	enabled, err := spec.isEnabledContext(ctx)
	if err != nil {
		return err
	}
	inner := context.WithValue(ctx, specificationDisabledKey{}, spec.Name)
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

// AddActionGroups adds audit action groups to the specification.
func (spec *ServerAuditSpecification) AddActionGroups(groups ...string) error {
	return spec.AddActionGroupsContext(context.Background(), groups...)
}

// AddActionGroupsContext is the context-aware variant of AddActionGroups. The
// specification is disabled for the duration and restored afterwards.
func (spec *ServerAuditSpecification) AddActionGroupsContext(ctx context.Context, groups ...string) error {
	return spec.alterActionGroupsContext(ctx, "ADD", groups)
}

// DropActionGroups removes audit action groups from the specification.
func (spec *ServerAuditSpecification) DropActionGroups(groups ...string) error {
	return spec.DropActionGroupsContext(context.Background(), groups...)
}

// DropActionGroupsContext is the context-aware variant of DropActionGroups.
func (spec *ServerAuditSpecification) DropActionGroupsContext(ctx context.Context, groups ...string) error {
	return spec.alterActionGroupsContext(ctx, "DROP", groups)
}

func (spec *ServerAuditSpecification) alterActionGroupsContext(ctx context.Context, verb string, groups []string) error {
	if len(groups) == 0 {
		return nil
	}
	clauses, err := auditActionGroupClauses(verb, groups)
	if err != nil {
		return fmt.Errorf("gosmo: alter server audit specification %q: %w", spec.Name, err)
	}
	stmt := fmt.Sprintf("ALTER SERVER AUDIT SPECIFICATION %s\n    %s", quoteIdent(spec.Name), clauses)
	return spec.withSpecificationDisabled(ctx, func(ctx context.Context) error {
		if err := spec.server.execContext(ctx, stmt); err != nil {
			return fmt.Errorf("gosmo: alter server audit specification %q: %w", spec.Name, err)
		}
		return nil
	})
}

// SetAudit rebinds the specification to a different server audit.
func (spec *ServerAuditSpecification) SetAudit(auditName string) error {
	return spec.SetAuditContext(context.Background(), auditName)
}

// SetAuditContext is the context-aware variant of SetAudit. The specification
// is disabled for the duration and restored afterwards.
func (spec *ServerAuditSpecification) SetAuditContext(ctx context.Context, auditName string) error {
	if strings.TrimSpace(auditName) == "" {
		return fmt.Errorf("gosmo: alter server audit specification %q: audit name is empty", spec.Name)
	}
	stmt := fmt.Sprintf("ALTER SERVER AUDIT SPECIFICATION %s\nFOR SERVER AUDIT %s",
		quoteIdent(spec.Name), quoteIdent(auditName))
	err := spec.withSpecificationDisabled(ctx, func(ctx context.Context) error {
		if err := spec.server.execContext(ctx, stmt); err != nil {
			return fmt.Errorf("gosmo: alter server audit specification %q: %w", spec.Name, err)
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
func (spec *ServerAuditSpecification) Drop() error {
	return spec.DropContext(context.Background())
}

// DropContext is the context-aware variant of Drop. An enabled specification
// is disabled first; there is nothing to restore afterwards.
func (spec *ServerAuditSpecification) DropContext(ctx context.Context) error {
	enabled, err := spec.isEnabledContext(ctx)
	if err != nil {
		return err
	}
	if enabled {
		if err := spec.SetStateContext(ctx, false); err != nil {
			return err
		}
	}
	stmt := "DROP SERVER AUDIT SPECIFICATION " + quoteIdent(spec.Name)
	if err := spec.server.execContext(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: drop server audit specification %q: %w", spec.Name, err)
	}
	return nil
}
