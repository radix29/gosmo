package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/golang-sql/sqlexp"
	mssql "github.com/microsoft/go-mssqldb"
)

// ============================================================
// Backup
// ============================================================

// BackupOptions configures a BACKUP DATABASE or BACKUP LOG operation.
type BackupOptions struct {
	// Database to back up (required).
	Database string
	// Action: DATABASE (default), LOG, or FILES.
	Action BackupAction
	// Files and FileGroups name the logical files / filegroups a
	// BackupActionFiles backup covers. At least one of the two is required
	// for that action and both are ignored for every other one — there is no
	// "BACKUP FILES" verb in T-SQL; a file/filegroup backup is a BACKUP
	// DATABASE carrying FILE = / FILEGROUP = clauses.
	Files      []string
	FileGroups []string
	// Devices is where the backup is written: one or more DiskTarget paths
	// (`C:\Backups\MyDB.bak`), URLTarget blobs, or DeviceTarget logical
	// backup devices. A striped backup names several.
	Devices []BackupTarget
	// BackupSetName is the NAME clause.
	BackupSetName string
	// Description is the DESCRIPTION clause.
	Description string
	// MediaDescription is the MEDIADESCRIPTION clause.
	MediaDescription string
	// Compression: nil = server default, new(true) = force on, new(false) = force off.
	Compression *bool
	// CopyOnly marks this as a copy-only backup (does not break the log chain).
	CopyOnly bool
	// Checksum adds WITH CHECKSUM.
	Checksum bool
	// Format reinitialises the media.
	Format bool
	// Init overwrites existing backup sets on the media.
	Init bool
	// Stats controls progress reporting frequency (e.g. 10 = every 10%).
	// If Progress is set and Stats is left at 0, it defaults to 10 so
	// percent-complete messages actually get emitted.
	Stats int
	// Credential names the SQL Server credential a BACKUP ... TO URL
	// authenticates to Azure Storage with (WITH CREDENTIAL = N'name'), the
	// storage-account-key form. Leave it empty for the shared access
	// signature form, which is what Managed Instance uses: there the
	// credential's *name* is the container URL and SQL Server finds it
	// itself, so naming it here is not merely unnecessary but wrong.
	// Ignored for a DISK device, which authenticates as the service account.
	Credential string
	// Progress, if set, is called for every message SQL Server emits while
	// the backup runs, including the "N percent processed" notices STATS
	// produces — pct is -1 for a message that doesn't carry a percentage.
	Progress func(pct int, message string)
}

// Backup performs a BACKUP DATABASE (or LOG) operation.
func (s *Server) Backup(ctx context.Context, opts BackupOptions) error {
	if opts.Progress != nil && opts.Stats == 0 {
		opts.Stats = 10
	}
	sqlText, err := s.BuildBackupStatement(opts)
	if err != nil {
		return err
	}

	if opts.Progress == nil || Scripting(ctx) {
		if err := s.exec(ctx, sqlText); err != nil {
			return fmt.Errorf("gosmo: backup %q: %w", opts.Database, err)
		}
		return nil
	}
	if err := refuseInTx(ctx, s, "backup with progress"); err != nil {
		return err
	}
	if err := s.execWithProgress(ctx, sqlText, opts.Progress); err != nil {
		return fmt.Errorf("gosmo: backup %q: %w", opts.Database, err)
	}
	observe(ctx, s, ScriptEntry{Server: scriptServerName(ctx, s), SQL: sqlText})
	return nil
}

// BuildBackupStatement returns the T-SQL BACKUP statement opts describes,
// without executing it — for callers that want to show or hand off the
// script (e.g. an editor pane) rather than run it immediately.
// Backup validates and builds the statement the same way, then runs
// what this returns.
//
// It is a Server method, like BuildRestoreStatement, although nothing in the
// statement depends on the instance yet: a BACKUP form that does (a Managed
// Instance's COPY_ONLY rule, an edition's compression support) then has
// somewhere to go without another signature change.
func (s *Server) BuildBackupStatement(opts BackupOptions) (string, error) {
	return buildBackupStatement(opts)
}

// buildBackupStatement is Server.BuildBackupStatement.
func buildBackupStatement(opts BackupOptions) (string, error) {
	if opts.Database == "" {
		return "", fmt.Errorf("gosmo: backup: database name is required")
	}
	if err := checkTargets("backup", opts.Devices); err != nil {
		return "", err
	}
	if opts.Action == "" {
		opts.Action = BackupActionDatabase
	}
	if !validBackupAction(opts.Action) {
		return "", fmt.Errorf("gosmo: backup: unrecognized action %q", opts.Action)
	}

	// Neither DIFFERENTIAL nor FILES is its own BACKUP verb: a differential is
	// a BACKUP DATABASE with a DIFFERENTIAL clause, and a file/filegroup
	// backup is a BACKUP DATABASE carrying FILE = / FILEGROUP = clauses.
	// Emitting the action name literally produced "BACKUP FILES [db]", which
	// SQL Server rejects outright.
	action := opts.Action
	if action == BackupActionDifferential || action == BackupActionFiles {
		action = BackupActionDatabase
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "BACKUP %s %s", action, quoteIdent(opts.Database))
	if opts.Action == BackupActionFiles {
		spec, err := backupFileSpec("backup", opts.Files, opts.FileGroups)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&sb, " %s", spec)
	}
	sb.WriteString(" TO ")
	sb.WriteString(targetList(opts.Devices))

	var withs []string
	if opts.Action == BackupActionDifferential {
		withs = append(withs, "DIFFERENTIAL")
	}
	if opts.BackupSetName != "" {
		withs = append(withs, fmt.Sprintf("NAME = N'%s'", escapeSingle(opts.BackupSetName)))
	}
	if opts.Description != "" {
		withs = append(withs, fmt.Sprintf("DESCRIPTION = N'%s'", escapeSingle(opts.Description)))
	}
	if opts.MediaDescription != "" {
		withs = append(withs, fmt.Sprintf("MEDIADESCRIPTION = N'%s'", escapeSingle(opts.MediaDescription)))
	}
	if opts.Credential != "" {
		withs = append(withs, fmt.Sprintf("CREDENTIAL = N'%s'", escapeSingle(opts.Credential)))
	}
	if opts.CopyOnly {
		withs = append(withs, "COPY_ONLY")
	}
	if opts.Compression != nil {
		if *opts.Compression {
			withs = append(withs, "COMPRESSION")
		} else {
			withs = append(withs, "NO_COMPRESSION")
		}
	}
	if opts.Checksum {
		withs = append(withs, "CHECKSUM")
	}
	if opts.Format {
		withs = append(withs, "FORMAT")
	}
	if opts.Init {
		withs = append(withs, "INIT")
	}
	if opts.Stats > 0 {
		withs = append(withs, fmt.Sprintf("STATS = %d", opts.Stats))
	}
	if len(withs) > 0 {
		fmt.Fprintf(&sb, " WITH %s", strings.Join(withs, ", "))
	}

	return sb.String(), nil
}

// backupFileSpec renders the FILE = / FILEGROUP = clause list that a
// file-or-filegroup BACKUP/RESTORE carries between the database name and the
// TO/FROM keyword. verb names the operation for the error message.
//
// At least one file or filegroup is required: BackupActionFiles with neither
// would otherwise render as a plain full BACKUP DATABASE, quietly doing far
// more work than the caller asked for rather than failing.
func backupFileSpec(verb string, files, fileGroups []string) (string, error) {
	if len(files) == 0 && len(fileGroups) == 0 {
		return "", fmt.Errorf("gosmo: %s: action %s needs at least one file or filegroup", verb, BackupActionFiles)
	}
	parts := make([]string, 0, len(files)+len(fileGroups))
	for _, f := range files {
		parts = append(parts, fmt.Sprintf("FILE = N'%s'", escapeSingle(f)))
	}
	for _, g := range fileGroups {
		parts = append(parts, fmt.Sprintf("FILEGROUP = N'%s'", escapeSingle(g)))
	}
	return strings.Join(parts, ", "), nil
}

// execWithProgress runs sqlText on a dedicated connection, draining the
// driver's message stream and forwarding each notice to progress — this is
// how BACKUP/RESTORE's WITH STATS = N percentage messages reach the caller.
//
// The message stream delivers each error message on its own, so the stream
// is drained to the end and every one is kept: a failed BACKUP sends the
// cause first ("Cannot open backup device ...") and "BACKUP DATABASE is
// terminating abnormally" after it, and returning at the first — or keeping
// only the last — told the caller half of it. The collected messages are
// combined the way exec's are, by withAllMessages. Never run under
// WithScript: Backup and Restore take exec's path there. Its callers report
// a success to the statement observer themselves, as exec does.
func (s *Server) execWithProgress(ctx context.Context, sqlText string, progress func(pct int, message string)) error {
	ctx, release := s.bound(ctx)
	defer release()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	retmsg := &sqlexp.ReturnMessage{}
	rows, err := conn.QueryContext(ctx, sqlText, retmsg)
	if err != nil {
		return err
	}
	defer rows.Close()

	var msgs []mssql.Error
	var other error
	for active := true; active; {
		switch m := retmsg.Message(ctx).(type) {
		case sqlexp.MsgNotice:
			text := m.Message.String()
			progress(noticePercent(m.Message), text)
		case sqlexp.MsgError:
			if me, ok := errors.AsType[mssql.Error](m.Error); ok {
				msgs = append(msgs, me)
			} else if other == nil {
				other = m.Error
			}
		case sqlexp.MsgNext:
			for rows.Next() {
			}
		case sqlexp.MsgNextResultSet:
			active = rows.NextResultSet()
		}
	}
	return progressError(msgs, other, rows.Err())
}

// progressError is execWithProgress's result from what the stream carried:
// the server's error messages as one mssql.Error — the last as its headline,
// as database/sql reports a failed exec, with every one in All — then any
// other error the stream or the rows reported.
func progressError(msgs []mssql.Error, other, rowsErr error) error {
	if len(msgs) > 0 {
		last := msgs[len(msgs)-1]
		last.All = msgs
		return withAllMessages(last)
	}
	if other != nil {
		return other
	}
	return rowsErr
}

// msgPercentProcessed is the number of STATS = N's progress notice, "%d
// percent processed.".
const msgPercentProcessed = 3211

// noticePercent returns the percentage a WITH STATS progress notice carries,
// or -1 for any other notice. The server localises message 3211 to the
// session language — "50 Prozent verarbeitet.", "Bylo zpracováno 50
// procent.", "Yüzde 50 işlendi." — so matching on the English text left the
// progress at -1 for every login whose default language is not English. The
// driver hands each notice over as an mssql.Error carrying its number; the
// English text is only the fallback for a notice that isn't one.
func noticePercent(msg fmt.Stringer) int {
	if me, ok := msg.(mssql.Error); ok {
		if me.Number != msgPercentProcessed {
			return -1
		}
		return firstInt(me.Message)
	}
	return parsePercent(msg.String())
}

// parsePercent extracts the integer from an English "N percent processed."
// message; it returns -1 for any message that isn't shaped like one.
func parsePercent(text string) int {
	if !strings.Contains(text, "percent processed") {
		return -1
	}
	return firstInt(text)
}

// firstInt returns the first run of ASCII digits in text as an integer, or
// -1 when there is none. The number is not always the leading token: Czech
// and Turkish put it mid-sentence, and Chinese right before the full stop.
func firstInt(text string) int {
	i := strings.IndexFunc(text, func(r rune) bool { return r >= '0' && r <= '9' })
	if i < 0 {
		return -1
	}
	j := i
	for j < len(text) && text[j] >= '0' && text[j] <= '9' {
		j++
	}
	n, err := strconv.Atoi(text[i:j])
	if err != nil {
		return -1
	}
	return n
}

// ============================================================
// Log backup chain
// ============================================================

// DatabaseRecoveryStatus reports a database's place in its log backup chain,
// from sys.database_recovery_status.
//
// The distinction it carries is not "has a backup" — it is whether the log
// backup chain has been started at all, which is what SQL Server tests before
// it will let a database join an availability group or a mirroring session.
// A database in the FULL recovery model that has never had a full backup is
// running in the so-called pseudo-simple model: no log chain exists, and
// ALTER AVAILABILITY GROUP ... ADD DATABASE fails with Msg 1475 ("might
// contain bulk logged changes that have not been backed up"). Switching a
// database to SIMPLE and back to FULL breaks the chain again.
type DatabaseRecoveryStatus struct {
	// DatabaseName is the database this row describes.
	DatabaseName string

	// LastLogBackupLSN is the log sequence number of the last log backup, in
	// the decimal form SQL Server stores it (numeric(25,0)), or "" when the
	// column is NULL — which is the pseudo-simple state above.
	LastLogBackupLSN string

	// LogBackupChainStarted is LastLogBackupLSN != "", named for the question
	// callers actually ask.
	LogBackupChainStarted bool
}

// DatabaseRecoveryStatuses returns the log backup chain state of every
// database on the server.
//
// One read for the whole server: the state is wanted per database, but a
// caller deciding which databases qualify for something needs them all, and
// sys.database_recovery_status is a server-scoped view.
func (s *Server) DatabaseRecoveryStatuses(ctx context.Context) ([]*DatabaseRecoveryStatus, error) {
	const q = `
SELECT d.name, ISNULL(CONVERT(varchar(40), rs.last_log_backup_lsn), '')
FROM   sys.database_recovery_status rs
JOIN   sys.databases d ON d.database_id = rs.database_id
ORDER  BY d.name`

	rows, err := s.query(ctx, q)
	return scanRows(rows, err, "database recovery statuses", func(scan func(...any) error) (*DatabaseRecoveryStatus, error) {
		st := &DatabaseRecoveryStatus{}
		if err := scan(&st.DatabaseName, &st.LastLogBackupLSN); err != nil {
			return nil, err
		}
		st.LogBackupChainStarted = st.LastLogBackupLSN != ""
		return st, nil
	})
}

// RecoveryStatus returns this database's place in its log backup chain.
func (d *Database) RecoveryStatus(ctx context.Context) (*DatabaseRecoveryStatus, error) {
	const q = `
SELECT d.name, ISNULL(CONVERT(varchar(40), rs.last_log_backup_lsn), '')
FROM   sys.database_recovery_status rs
JOIN   sys.databases d ON d.database_id = rs.database_id
WHERE  d.name = @p1`

	st := &DatabaseRecoveryStatus{}
	err := d.server.queryRow(ctx, func(r *sql.Row) error {
		return r.Scan(&st.DatabaseName, &st.LastLogBackupLSN)
	}, q, d.Name)
	if err != nil {
		return nil, fmt.Errorf("gosmo: recovery status for %q: %w", d.Name, err)
	}
	st.LogBackupChainStarted = st.LastLogBackupLSN != ""
	return st, nil
}
