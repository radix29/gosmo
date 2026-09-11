package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// audit.go covers server audits — sys.server_audits, SSMS's Security > Audits
// folder, and the target a server audit specification is bound to.
//
// # Every write but the state toggle needs the audit disabled
//
// SQL Server refuses ALTER SERVER AUDIT and DROP SERVER AUDIT on an enabled
// audit with "This command requires audit to be disabled" — verified live, not
// read from the documentation. AlterContext and DropContext therefore turn the
// audit off, do the work, and turn it back on only if they were the ones who
// turned it off. A caller doing it by hand would get it wrong on the failure
// path, which is why it lives here.

// Audit destinations, as sys.server_audits.type_desc records them. The T-SQL
// keyword differs for the two log destinations — APPLICATION LOG is written
// TO APPLICATION_LOG — so never build a statement out of these directly;
// auditDestinationKeyword does the translation.
const (
	AuditToFile           = "FILE"
	AuditToApplicationLog = "APPLICATION LOG"
	AuditToSecurityLog    = "SECURITY LOG"
)

// Audit on-failure actions, as sys.server_audits.on_failure_desc records them.
// As with the destinations, the description is not the keyword: SHUTDOWN
// SERVER INSTANCE is written ON_FAILURE = SHUTDOWN.
const (
	AuditFailureContinue = "CONTINUE"
	AuditFailureShutdown = "SHUTDOWN SERVER INSTANCE"
	AuditFailureFailOp   = "FAIL OPERATION"
)

// AuditUnlimited is what sys.server_file_audits stores for MAX_ROLLOVER_FILES
// = UNLIMITED, which is also the default. MaxFileSize uses 0 for UNLIMITED
// instead; the two columns do not agree on a sentinel, and this is the one
// place that difference is written down.
const AuditUnlimited = 2147483647

// ServerAudit mirrors a row of sys.server_audits, with the file-target block
// from sys.server_file_audits where there is one.
type ServerAudit struct {
	server *Server

	AuditID    int
	Name       string
	GUID       string
	Type       string // AuditToFile, AuditToApplicationLog, AuditToSecurityLog
	OnFailure  string // AuditFailureContinue, AuditFailureShutdown, AuditFailureFailOp
	QueueDelay int    // milliseconds; 0 means write synchronously
	Predicate  string // the WHERE filter, empty when the audit has none
	IsEnabled  bool
	CreateDate time.Time
	ModifyDate time.Time

	// The file-target block, all zero for a non-FILE audit — those have no
	// row in sys.server_file_audits at all, which is why the read below
	// joins to it rather than selecting from it.
	LogFilePath      string
	LogFileName      string
	MaxFileSize      int64 // MB; 0 is UNLIMITED
	MaxRolloverFiles int   // AuditUnlimited is UNLIMITED
	MaxFiles         int   // 0 unless MAX_FILES was used instead of rollover
	ReserveDiskSpace bool
}

// serverAuditSelect reads sys.server_audits.
//
// The join onto sys.server_file_audits is a LEFT JOIN because an audit
// targeting the Windows application or security log has no row there — an
// inner join silently drops it from the folder. sys.server_file_audits
// repeats every base column, so only the file-specific ones are taken from it.
const serverAuditSelect = `
SELECT a.audit_id, a.name, CONVERT(varchar(36), a.audit_guid), a.type_desc,
       a.on_failure_desc, a.queue_delay, a.predicate, a.is_state_enabled,
       a.create_date, a.modify_date,
       f.log_file_path, f.log_file_name, f.max_file_size,
       f.max_rollover_files, f.max_files, f.reserve_disk_space
FROM   sys.server_audits a
LEFT   JOIN sys.server_file_audits f ON f.audit_id = a.audit_id`

// ServerAudits returns every server audit.
func (s *Server) ServerAudits() ([]*ServerAudit, error) {
	return s.ServerAuditsContext(context.Background())
}

// ServerAuditsContext is the context-aware variant of ServerAudits.
func (s *Server) ServerAuditsContext(ctx context.Context) ([]*ServerAudit, error) {
	rows, err := s.query(ctx, serverAuditSelect+`
ORDER  BY a.name`)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list server audits: %w", err)
	}
	defer rows.Close()

	var out []*ServerAudit
	for rows.Next() {
		a, err := scanServerAudit(s, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list server audits: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list server audits: %w", err)
	}
	return out, nil
}

// ServerAuditByName returns one server audit with every field populated, or a
// not-found error (errors.Is ErrNotFound) when the server has none by that
// name.
func (s *Server) ServerAuditByName(name string) (*ServerAudit, error) {
	return s.ServerAuditByNameContext(context.Background(), name)
}

// ServerAuditByNameContext is the context-aware variant of ServerAuditByName.
func (s *Server) ServerAuditByNameContext(ctx context.Context, name string) (*ServerAudit, error) {
	var a *ServerAudit
	err := s.queryRow(ctx, func(row *sql.Row) error {
		var err error
		a, err = scanServerAudit(s, row.Scan)
		return err
	}, serverAuditSelect+`
WHERE  a.name = @p1`, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: server audit %q not found", name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read server audit %q: %w", name, err)
	}
	return a, nil
}

// ServerAudit returns a lightweight handle for a server audit by name, without
// querying sys.server_audits — the audit-side counterpart of Server.Database.
// Every cached field stays at its zero value; ServerAuditByName is what
// populates them.
//
// Every write method addresses the audit by name, so this handle is enough to
// go on operating on one the caller already knows exists.
func (s *Server) ServerAudit(name string) *ServerAudit {
	return &ServerAudit{server: s, Name: name}
}

func scanServerAudit(s *Server, scan func(...any) error) (*ServerAudit, error) {
	a := &ServerAudit{server: s}
	var guid, typeDesc, onFailure, predicate, logPath, logName sql.NullString
	var queueDelay, rollover, maxFiles sql.NullInt64
	var maxSize sql.NullInt64
	var enabled, reserve sql.NullBool
	if err := scan(&a.AuditID, &a.Name, &guid, &typeDesc, &onFailure, &queueDelay,
		&predicate, &enabled, &a.CreateDate, &a.ModifyDate,
		&logPath, &logName, &maxSize, &rollover, &maxFiles, &reserve); err != nil {
		return nil, err
	}
	a.GUID, a.Type, a.OnFailure, a.Predicate = guid.String, typeDesc.String, onFailure.String, predicate.String
	a.QueueDelay = int(queueDelay.Int64)
	a.IsEnabled = enabled.Bool
	a.LogFilePath, a.LogFileName = logPath.String, logName.String
	a.MaxFileSize = maxSize.Int64
	a.MaxRolloverFiles, a.MaxFiles = int(rollover.Int64), int(maxFiles.Int64)
	a.ReserveDiskSpace = reserve.Bool
	return a, nil
}

// -- Runtime status --------------------------------------------------------------

// ServerAuditStatus is an audit's runtime state, from sys.dm_server_audit_status.
type ServerAuditStatus struct {
	Status        string // STARTED / STOPPED
	StatusTime    time.Time
	AuditFilePath string // the file currently being written, empty for a log target
	AuditFileSize int64
}

// Status returns the audit's runtime state.
func (a *ServerAudit) Status() (*ServerAuditStatus, error) {
	return a.StatusContext(context.Background())
}

// StatusContext is the context-aware variant of Status.
//
// This is a separate read rather than more columns on ServerAudits because
// sys.dm_server_audit_status needs VIEW SERVER STATE: folded into the list
// query it would fail the whole folder for a login that can see the audits but
// not the DMV. An audit that has never been started has no row there and
// returns a not-found error.
func (a *ServerAudit) StatusContext(ctx context.Context) (*ServerAuditStatus, error) {
	st := &ServerAuditStatus{}
	var path sql.NullString
	var size sql.NullInt64
	err := a.server.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&st.Status, &st.StatusTime, &path, &size)
	}, `
SELECT status_desc, status_time, audit_file_path, audit_file_size
FROM   sys.dm_server_audit_status
WHERE  name = @p1`, a.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: server audit %q has no runtime status", a.Name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read server audit %q status: %w", a.Name, err)
	}
	st.AuditFilePath, st.AuditFileSize = path.String, size.Int64
	return st, nil
}

// -- Writes ----------------------------------------------------------------------

// ServerAuditSpec describes a server audit to create or alter.
type ServerAuditSpec struct {
	Name string

	// Type is the destination: AuditToFile, AuditToApplicationLog or
	// AuditToSecurityLog. Empty on an alter leaves the destination alone.
	Type string

	// QueueDelay is the write delay in milliseconds; 0 means synchronous.
	QueueDelay int

	// OnFailure is one of the AuditFailure* constants. Empty defaults to
	// CONTINUE on a create and leaves it alone on an alter.
	OnFailure string

	// Predicate is the WHERE filter without the keyword. On an alter, empty
	// means REMOVE WHERE — there is no form that leaves an existing
	// predicate alone while changing something else, so the caller must
	// carry the current one forward.
	Predicate string

	// The file-target block, used only when Type is AuditToFile.
	FilePath         string
	MaxFileSize      int64 // MB; 0 is UNLIMITED
	MaxRolloverFiles int   // AuditUnlimited, or 0 when MaxFiles is used
	MaxFiles         int   // mutually exclusive with MaxRolloverFiles
	ReserveDiskSpace bool
}

// auditDestinationKeyword translates a type_desc into the keyword that follows
// TO in the statement. The two differ for the log targets: type_desc records
// "APPLICATION LOG", the statement wants APPLICATION_LOG.
func auditDestinationKeyword(t string) string {
	return strings.ReplaceAll(strings.TrimSpace(t), " ", "_")
}

// auditFailureKeyword translates an on_failure_desc into the ON_FAILURE
// keyword. SHUTDOWN SERVER INSTANCE is written SHUTDOWN; the other two match.
func auditFailureKeyword(f string) string {
	if strings.EqualFold(strings.TrimSpace(f), AuditFailureShutdown) {
		return "SHUTDOWN"
	}
	return strings.ReplaceAll(strings.TrimSpace(f), " ", "_")
}

// auditTargetClause builds the "TO …" half, empty when the spec names no
// destination.
func (spec ServerAuditSpec) auditTargetClause() string {
	if spec.Type == "" {
		return ""
	}
	if !strings.EqualFold(spec.Type, AuditToFile) {
		return "\nTO " + auditDestinationKeyword(spec.Type)
	}

	var opts []string
	if spec.FilePath != "" {
		opts = append(opts, fmt.Sprintf("FILEPATH = N'%s'", escapeSingle(spec.FilePath)))
	}
	if spec.MaxFileSize > 0 {
		opts = append(opts, fmt.Sprintf("MAXSIZE = %d MB", spec.MaxFileSize))
	} else {
		opts = append(opts, "MAXSIZE = UNLIMITED")
	}
	// MAX_ROLLOVER_FILES and MAX_FILES are mutually exclusive; naming both in
	// one statement is a syntax error. MaxFiles wins when it is set, matching
	// the catalog, where a non-zero max_files is the discriminator.
	switch {
	case spec.MaxFiles > 0:
		opts = append(opts, fmt.Sprintf("MAX_FILES = %d", spec.MaxFiles))
	case spec.MaxRolloverFiles > 0 && spec.MaxRolloverFiles != AuditUnlimited:
		opts = append(opts, fmt.Sprintf("MAX_ROLLOVER_FILES = %d", spec.MaxRolloverFiles))
	default:
		opts = append(opts, "MAX_ROLLOVER_FILES = UNLIMITED")
	}
	if spec.ReserveDiskSpace {
		opts = append(opts, "RESERVE_DISK_SPACE = ON")
	} else {
		opts = append(opts, "RESERVE_DISK_SPACE = OFF")
	}
	return "\nTO FILE\n( " + strings.Join(opts, ",\n  ") + " )"
}

// auditWithClause builds the "WITH (…)" half.
func (spec ServerAuditSpec) auditWithClause() string {
	opts := []string{fmt.Sprintf("QUEUE_DELAY = %d", spec.QueueDelay)}
	if spec.OnFailure != "" {
		opts = append(opts, "ON_FAILURE = "+auditFailureKeyword(spec.OnFailure))
	}
	return "\nWITH ( " + strings.Join(opts, ", ") + " )"
}

// createServerAuditStatement builds CREATE SERVER AUDIT, validating the spec.
func (spec ServerAuditSpec) createServerAuditStatement() (string, error) {
	if strings.TrimSpace(spec.Name) == "" {
		return "", fmt.Errorf("server audit has no name")
	}
	if spec.Type == "" {
		return "", fmt.Errorf("server audit %q has no destination", spec.Name)
	}
	if strings.EqualFold(spec.Type, AuditToFile) && spec.FilePath == "" {
		return "", fmt.Errorf("server audit %q has no file path", spec.Name)
	}

	stmt := "CREATE SERVER AUDIT " + quoteIdent(spec.Name) +
		spec.auditTargetClause() + spec.auditWithClause()
	if spec.Predicate != "" {
		stmt += "\nWHERE " + spec.Predicate
	}
	return stmt, nil
}

// CreateServerAudit creates a server audit. It is created disabled, which is
// what CREATE SERVER AUDIT does; use SetState to turn it on.
func (s *Server) CreateServerAudit(spec ServerAuditSpec) (*ServerAudit, error) {
	return s.CreateServerAuditContext(context.Background(), spec)
}

// CreateServerAuditContext is the context-aware variant of CreateServerAudit.
func (s *Server) CreateServerAuditContext(ctx context.Context, spec ServerAuditSpec) (*ServerAudit, error) {
	stmt, err := spec.createServerAuditStatement()
	if err != nil {
		return nil, fmt.Errorf("gosmo: create server audit: %w", err)
	}
	if err := s.execContext(ctx, stmt); err != nil {
		return nil, fmt.Errorf("gosmo: create server audit %q: %w", spec.Name, err)
	}
	if Scripting(ctx) {
		// The CREATE was only collected, so there is nothing to read back.
		return s.ServerAudit(spec.Name), nil
	}
	return s.ServerAuditByNameContext(ctx, spec.Name)
}

// SetState enables or disables the audit.
func (a *ServerAudit) SetState(on bool) error {
	return a.SetStateContext(context.Background(), on)
}

// SetStateContext is the context-aware variant of SetState. This is the one
// ALTER SERVER AUDIT form the server accepts on an enabled audit.
func (a *ServerAudit) SetStateContext(ctx context.Context, on bool) error {
	return a.setStateNamedContext(ctx, a.Name, on)
}

// setStateNamedContext toggles the state of the audit addressed by name, which
// is not always a.Name: RenameContext restores the state after MODIFY NAME has
// committed, and under a WithScript context a.Name is never updated at all. An
// ON addressed to the old name fails after the rename has already committed,
// leaving auditing switched off — the exact failure the disable/restore dance
// exists to prevent.
func (a *ServerAudit) setStateNamedContext(ctx context.Context, name string, on bool) error {
	state := "OFF"
	if on {
		state = "ON"
	}
	stmt := fmt.Sprintf("ALTER SERVER AUDIT %s WITH ( STATE = %s )", quoteIdent(name), state)
	if err := a.server.execContext(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: set server audit %q state: %w", name, err)
	}
	setIfApplied(ctx, &a.IsEnabled, on)
	return nil
}

// isEnabledContext reads the audit's current state straight from the catalog
// rather than trusting the receiver, which may be a name-only handle.
func (a *ServerAudit) isEnabledContext(ctx context.Context) (bool, error) {
	var enabled sql.NullBool
	err := a.server.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&enabled)
	}, "SELECT is_state_enabled FROM sys.server_audits WHERE name = @p1", a.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, notFoundf("gosmo: server audit %q not found", a.Name)
	}
	if err != nil {
		return false, err
	}
	return enabled.Bool, nil
}

// auditDisabledKey marks a context that is already inside a disable window.
// The value is a *string holding the name the window will re-enable under; a
// rename inside the window rewrites it, because under a WithScript context
// a.Name is never updated and the restore would otherwise script the old name.
type auditDisabledKey struct{}

// auditWindow returns the enclosing WithDisabled window's restore name if that
// window switched off this audit, so a nested write does not open a second one.
func (a *ServerAudit) auditWindow(ctx context.Context) (*string, bool) {
	name, _ := ctx.Value(auditDisabledKey{}).(*string)
	if name == nil || *name != a.Name {
		return nil, false
	}
	return name, true
}

// WithDisabled runs fn with the audit disabled, restoring the state
// afterwards. Every write method already does this for itself, so a caller
// needs WithDisabled only to make several of them share one window: auditing
// then stops once for the whole batch instead of once per statement, and a
// failure part-way through cannot leave the audit off.
//
// fn must use the context it is handed — that is what the nested writes read
// to know the window is already open.
func (a *ServerAudit) WithDisabled(ctx context.Context, fn func(context.Context) error) error {
	return a.withAuditDisabled(ctx, fn)
}

// withAuditDisabled runs fn with the audit disabled, re-enabling it afterwards
// only if it was this call that disabled it.
//
// SQL Server refuses every ALTER but the state toggle, and the DROP, while the
// audit is enabled. Re-enabling unconditionally would turn on an audit the
// user had deliberately left off; re-enabling on the failure path is what
// keeps a failed apply from silently leaving auditing switched off.
func (a *ServerAudit) withAuditDisabled(ctx context.Context, fn func(context.Context) error) error {
	if _, ok := a.auditWindow(ctx); ok {
		return fn(ctx)
	}
	enabled, err := a.isEnabledContext(ctx)
	if err != nil {
		return err
	}
	name := a.Name
	inner := context.WithValue(ctx, auditDisabledKey{}, &name)
	if !enabled {
		return fn(inner)
	}
	if err := a.SetStateContext(ctx, false); err != nil {
		return err
	}
	enable := func(ctx context.Context) error { return a.setStateNamedContext(ctx, name, true) }
	if err := fn(inner); err != nil {
		// Best effort: report the original failure, not the restore's.
		_ = restoreWindow(ctx, enable)
		return err
	}
	return restoreWindow(ctx, enable)
}

// windowRestoreTimeout bounds a disable window's closing re-enable. Short for
// the same reason as multiUserRepairTimeout: the statement has nothing to wait
// for, and one that hangs is worse than one that gives up.
const windowRestoreTimeout = 10 * time.Second

// restoreWindow runs a disable window's closing re-enable — every
// WithDisabled, server and database scope alike — under ctx's values but not
// its cancellation.
//
// The window exists so that a write failing part-way cannot leave auditing off,
// and a cancelled context is the likeliest way for one to fail part-way: a user
// cancelling a slow Apply, or the caller's deadline running out inside fn.
// Re-enabling on that same context fails without reaching the server, so the
// audit stayed switched off exactly when the window promised it would not. Same
// shape as restoreMultiUser (server.go). The values are kept, so under WithScript
// the re-enable is still captured rather than run, and a rename inside the
// window still restores under its new name.
func restoreWindow(ctx context.Context, enable func(context.Context) error) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), windowRestoreTimeout)
	defer cancel()
	return enable(rctx)
}

// alterServerAuditStatements builds the statements ALTER needs for the spec.
//
// A predicate is set in the same statement as everything else, but clearing
// one is a statement of its own: ALTER SERVER AUDIT ... WITH (...) REMOVE WHERE
// is a syntax error ("Incorrect syntax near 'REMOVE'"), while the same REMOVE
// WHERE alone is accepted — verified live, and only after a Properties page
// Apply failed on it. There is no form that changes one setting while leaving
// an existing predicate alone, so a caller changing anything else must carry
// the current predicate forward in the spec.
func (spec ServerAuditSpec) alterServerAuditStatements(name string) []string {
	stmt := "ALTER SERVER AUDIT " + quoteIdent(name) +
		spec.auditTargetClause() + spec.auditWithClause()
	if spec.Predicate != "" {
		return []string{stmt + "\nWHERE " + spec.Predicate}
	}
	return []string{stmt, "ALTER SERVER AUDIT " + quoteIdent(name) + " REMOVE WHERE"}
}

// Alter changes the audit's settings.
func (a *ServerAudit) Alter(spec ServerAuditSpec) error {
	return a.AlterContext(context.Background(), spec)
}

// AlterContext is the context-aware variant of Alter. The audit is disabled
// for the duration and restored afterwards — see withAuditDisabled.
//
// Renaming is not part of this: ALTER SERVER AUDIT ... MODIFY NAME is a
// statement of its own and cannot be combined with any other clause, so it is
// Rename's job.
func (a *ServerAudit) AlterContext(ctx context.Context, spec ServerAuditSpec) error {
	return a.withAuditDisabled(ctx, func(ctx context.Context) error {
		for _, stmt := range spec.alterServerAuditStatements(a.Name) {
			if err := a.server.execContext(ctx, stmt); err != nil {
				return fmt.Errorf("gosmo: alter server audit %q: %w", a.Name, err)
			}
		}
		return nil
	})
}

// Rename changes the audit's name.
func (a *ServerAudit) Rename(newName string) error {
	return a.RenameContext(context.Background(), newName)
}

// RenameContext is the context-aware variant of Rename.
//
// This is the one write that cannot use withAuditDisabled: the wrapper restores
// the state through the receiver's name, and MODIFY NAME has changed what that
// name has to be. The restore is therefore spelled out here, addressed to
// newName on the path where the rename committed and to the old name on the
// path where it did not.
func (a *ServerAudit) RenameContext(ctx context.Context, newName string) error {
	if strings.TrimSpace(newName) == "" {
		return fmt.Errorf("gosmo: rename server audit %q: new name is empty", a.Name)
	}
	window, inWindow := a.auditWindow(ctx)
	restore := false
	if !inWindow {
		enabled, err := a.isEnabledContext(ctx)
		if err != nil {
			return err
		}
		if enabled {
			if err := a.SetStateContext(ctx, false); err != nil {
				return err
			}
			restore = true
		}
	}
	stmt := fmt.Sprintf("ALTER SERVER AUDIT %s MODIFY NAME = %s",
		quoteIdent(a.Name), quoteIdent(newName))
	if err := a.server.execContext(ctx, stmt); err != nil {
		if restore {
			// The rename did not commit, so the audit is still the old name.
			_ = a.setStateNamedContext(ctx, a.Name, true)
		}
		return fmt.Errorf("gosmo: rename server audit %q: %w", a.Name, err)
	}
	setIfApplied(ctx, &a.Name, newName)
	if inWindow {
		// The enclosing window has to re-enable under the name that now exists.
		*window = newName
	}
	if restore {
		return a.setStateNamedContext(ctx, newName, true)
	}
	return nil
}

// Drop deletes the audit.
func (a *ServerAudit) Drop() error { return a.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop. An enabled audit is
// disabled first; there is nothing to restore afterwards.
func (a *ServerAudit) DropContext(ctx context.Context) error {
	enabled, err := a.isEnabledContext(ctx)
	if err != nil {
		return err
	}
	if enabled {
		if err := a.SetStateContext(ctx, false); err != nil {
			return err
		}
	}
	if err := a.server.execContext(ctx, "DROP SERVER AUDIT "+quoteIdent(a.Name)); err != nil {
		return fmt.Errorf("gosmo: drop server audit %q: %w", a.Name, err)
	}
	return nil
}
