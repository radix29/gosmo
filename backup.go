package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/golang-sql/sqlexp"
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
	// Devices is one or more backup device paths, e.g. `C:\Backups\MyDB.bak`.
	Devices []string
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
	sqlText, err := BuildBackupStatement(opts)
	if err != nil {
		return err
	}

	if opts.Progress == nil {
		if err := s.exec(ctx, sqlText); err != nil {
			return fmt.Errorf("gosmo: backup %q: %w", opts.Database, err)
		}
		return nil
	}
	if err := execWithProgress(ctx, s.db, sqlText, opts.Progress); err != nil {
		return fmt.Errorf("gosmo: backup %q: %w", opts.Database, err)
	}
	return nil
}

// BuildBackupStatement returns the T-SQL BACKUP statement opts describes,
// without executing it — for callers that want to show or hand off the
// script (e.g. an editor pane) rather than run it immediately.
// Backup validates and builds the statement the same way, then runs
// what this returns.
func BuildBackupStatement(opts BackupOptions) (string, error) {
	if opts.Database == "" {
		return "", fmt.Errorf("gosmo: backup: database name is required")
	}
	if len(opts.Devices) == 0 {
		return "", fmt.Errorf("gosmo: backup: at least one device is required")
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

	deviceList := make([]string, len(opts.Devices))
	for i, d := range opts.Devices {
		deviceList[i] = backupDeviceClause(d)
	}
	sb.WriteString(strings.Join(deviceList, ", "))

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
func execWithProgress(ctx context.Context, db *sql.DB, sqlText string, progress func(pct int, message string)) error {
	conn, err := db.Conn(ctx)
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

	for active := true; active; {
		switch m := retmsg.Message(ctx).(type) {
		case sqlexp.MsgNotice:
			text := m.Message.String()
			progress(parsePercent(text), text)
		case sqlexp.MsgError:
			return m.Error
		case sqlexp.MsgNext:
			for rows.Next() {
			}
		case sqlexp.MsgNextResultSet:
			active = rows.NextResultSet()
		}
	}
	return rows.Err()
}

// parsePercent extracts the leading integer from a "N percent processed."
// message; it returns -1 for any message that isn't shaped like one.
func parsePercent(text string) int {
	if !strings.Contains(text, "percent processed") {
		return -1
	}
	i := 0
	for i < len(text) && text[i] >= '0' && text[i] <= '9' {
		i++
	}
	if i == 0 {
		return -1
	}
	n, err := strconv.Atoi(text[:i])
	if err != nil {
		return -1
	}
	return n
}

// ============================================================
// Restore
// ============================================================

// RestoreOptions configures a RESTORE DATABASE or RESTORE LOG operation.
type RestoreOptions struct {
	// Database is the target database name (required).
	Database string
	// Action: DATABASE (default), LOG, or FILES.
	Action BackupAction
	// Files and FileGroups name the logical files / filegroups a
	// BackupActionFiles restore covers — see BackupOptions.Files for why
	// these are clauses on a RESTORE DATABASE rather than a verb of their own.
	Files      []string
	FileGroups []string
	// Devices is one or more backup file paths (required).
	Devices []string
	// FileNumber selects which backup set on the device to restore (RESTORE's
	// WITH FILE = n, 1-based, as reported by BackupHeader.Position). Zero
	// leaves the clause off, which SQL Server reads as the first set — so a
	// device holding an appended differential or log needs this set
	// explicitly, or the full backup at position 1 is restored instead.
	FileNumber int
	// RelocateFiles maps logical file names to new physical paths.
	RelocateFiles []RelocateFile
	// Recovery is the state the restore leaves the database in; the zero
	// value writes no clause, which SQL Server reads as RECOVERY.
	Recovery RestoreRecovery
	// StandByFile is the undo file RestoreWithStandBy writes to, required by
	// it and refused with any other Recovery.
	StandByFile string
	// CloseExistingConnections clears the database of other connections
	// before the RESTORE, in the same batch — SSMS's "Close existing
	// connections to destination database". Run as a separate statement
	// first, the single-user slot it frees is open to anyone until the
	// RESTORE arrives, and a reconnecting application takes it and fails the
	// restore with "Exclusive access could not be obtained".
	//
	// An online database is set SINGLE_USER WITH ROLLBACK IMMEDIATE and put
	// back to MULTI_USER after the RESTORE whenever it is still online and
	// read-write — which also repairs it when the RESTORE fails; Restore
	// repairs it again, off the caller's cancellation, if the batch itself
	// was cut short. A database that does not exist yet or is RESTORING has
	// nobody to close and is left alone, and so is one in STANDBY, which
	// refuses the ALTER. A Managed Instance refuses SET SINGLE_USER, so
	// Server.BuildRestoreStatement and Restore kill the database's
	// sessions there instead and change no access mode; the package-level
	// BuildRestoreStatement, having no server to ask, always writes the
	// SINGLE_USER form.
	CloseExistingConnections bool
	// Replace forces restoration over an existing database.
	Replace bool
	// Checksum verifies backup checksums.
	Checksum bool
	// Stats controls progress reporting frequency (e.g. 10 = every 10%).
	// If Progress is set and Stats is left at 0, it defaults to 10 so
	// percent-complete messages actually get emitted.
	Stats int
	// Credential names the SQL Server credential a RESTORE ... FROM URL
	// authenticates to Azure Storage with — see BackupOptions.Credential,
	// including why the shared access signature form leaves it empty.
	Credential string
	// StopAt performs a point-in-time restore, to this moment read as the
	// *server's* local wall-clock time. Its date and time-of-day fields are
	// sent as written, to the millisecond, and its Location is ignored: no
	// zone conversion is made, because RESTORE reads STOPAT in the server's
	// time zone and a caller already passing server-local times must keep
	// getting the point they asked for. A time taken in UTC or the client's
	// zone restores to the wrong point unless the zones agree — convert it
	// with In first. SQL Server rounds the milliseconds to datetime's 1/300 s.
	StopAt *time.Time
	// Progress, if set, is called for every message SQL Server emits while
	// the restore runs, including the "N percent processed" notices STATS
	// produces — pct is -1 for a message that doesn't carry a percentage.
	Progress func(pct int, message string)
}

// RelocateFile maps a logical file name to a new physical path.
type RelocateFile struct {
	LogicalName  string
	PhysicalName string
}

// Restore performs a RESTORE DATABASE (or LOG) operation.
func (s *Server) Restore(ctx context.Context, opts RestoreOptions) error {
	if opts.Progress != nil && opts.Stats == 0 {
		opts.Stats = 10
	}
	sqlText, err := s.BuildRestoreStatement(opts)
	if err != nil {
		return err
	}

	if opts.Progress == nil {
		err = s.exec(ctx, sqlText)
	} else {
		err = execWithProgress(ctx, s.db, sqlText, opts.Progress)
	}
	if err != nil {
		// The batch's own MULTI_USER does not run when the batch is cut
		// short — a cancel or an expired deadline, the likeliest ways for a
		// long restore to fail. Best effort: a database left RESTORING, or
		// never created, refuses it, and the restore's error is what the
		// caller is told about.
		if opts.CloseExistingConnections && !s.refusesSingleUser() {
			_ = s.restoreMultiUser(ctx, opts.Database)
		}
		return fmt.Errorf("gosmo: restore %q: %w", opts.Database, err)
	}
	return nil
}

// BuildRestoreStatement is the package-level BuildRestoreStatement for this
// instance: the same statement, but with CloseExistingConnections written in
// the form the instance accepts — killing the database's sessions on a Managed
// Instance, which refuses SET SINGLE_USER. Restore runs what this
// returns.
func (s *Server) BuildRestoreStatement(opts RestoreOptions) (string, error) {
	return buildRestoreStatement(opts, s.refusesSingleUser())
}

// BuildRestoreStatement returns the T-SQL RESTORE statement opts describes,
// without executing it — the RESTORE counterpart of BuildBackupStatement.
// Restore validates and builds the statement the same way, then runs
// what this returns.
//
// The statement is laid out over several lines — the target, the devices,
// then one WITH option per line — because a restore that relocates files
// carries a MOVE clause per database file, each holding two full paths. On
// one line that runs to several hundred columns, and a caller scripting it
// for review (goSSMS's Restore dialog does) sees the RESTORE with every MOVE
// off the right edge of the editor, which reads as the MOVE clauses being
// missing entirely. Whitespace is not significant to SQL Server here, so the
// executed statement is unchanged.
//
// With CloseExistingConnections the result is a batch — see that field — in
// the SINGLE_USER form; Server.BuildRestoreStatement picks the form the
// connected instance accepts.
func BuildRestoreStatement(opts RestoreOptions) (string, error) {
	return buildRestoreStatement(opts, false)
}

// buildRestoreStatement is BuildRestoreStatement, closing existing connections
// by killing sessions rather than by SET SINGLE_USER when killSessions is set.
func buildRestoreStatement(opts RestoreOptions, killSessions bool) (string, error) {
	if opts.Database == "" {
		return "", fmt.Errorf("gosmo: restore: database name is required")
	}
	if len(opts.Devices) == 0 {
		return "", fmt.Errorf("gosmo: restore: at least one device is required")
	}
	if opts.Action == "" {
		opts.Action = BackupActionDatabase
	}
	if !validBackupAction(opts.Action) {
		return "", fmt.Errorf("gosmo: restore: unrecognized action %q", opts.Action)
	}
	if !restoreRecoveryNames[opts.Recovery] {
		return "", fmt.Errorf("gosmo: restore: unrecognized recovery %q", opts.Recovery)
	}
	if (opts.Recovery == RestoreWithStandBy) != (opts.StandByFile != "") {
		return "", fmt.Errorf("gosmo: restore: a standby file goes with, and only with, RestoreWithStandBy")
	}

	// FILES is not a RESTORE verb any more than it is a BACKUP one — see
	// BuildBackupStatement.
	action := opts.Action
	if action == BackupActionDifferential || action == BackupActionFiles {
		action = BackupActionDatabase
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "RESTORE %s %s", action, quoteIdent(opts.Database))
	if opts.Action == BackupActionFiles {
		spec, err := backupFileSpec("restore", opts.Files, opts.FileGroups)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&sb, " %s", spec)
	}
	sb.WriteString("\nFROM ")

	deviceList := make([]string, len(opts.Devices))
	for i, d := range opts.Devices {
		deviceList[i] = backupDeviceClause(d)
	}
	sb.WriteString(strings.Join(deviceList, ", "))

	var withs []string
	if opts.FileNumber > 0 {
		withs = append(withs, fmt.Sprintf("FILE = %d", opts.FileNumber))
	}
	for _, rf := range opts.RelocateFiles {
		withs = append(withs, fmt.Sprintf("MOVE N'%s' TO N'%s'",
			escapeSingle(rf.LogicalName), escapeSingle(rf.PhysicalName)))
	}
	switch opts.Recovery {
	case RestoreWithRecovery, RestoreWithNoRecovery:
		withs = append(withs, string(opts.Recovery))
	case RestoreWithStandBy:
		withs = append(withs, fmt.Sprintf("STANDBY = N'%s'", escapeSingle(opts.StandByFile)))
	}
	if opts.Replace {
		withs = append(withs, "REPLACE")
	}
	if opts.Checksum {
		withs = append(withs, "CHECKSUM")
	}
	if opts.Credential != "" {
		withs = append(withs, fmt.Sprintf("CREDENTIAL = N'%s'", escapeSingle(opts.Credential)))
	}
	if opts.Stats > 0 {
		withs = append(withs, fmt.Sprintf("STATS = %d", opts.Stats))
	}
	if opts.StopAt != nil {
		withs = append(withs, fmt.Sprintf("STOPAT = '%s'", opts.StopAt.Format("2006-01-02T15:04:05.000")))
	}
	if len(withs) > 0 {
		fmt.Fprintf(&sb, "\nWITH %s", strings.Join(withs, ",\n     "))
	}
	if !opts.CloseExistingConnections {
		return sb.String(), nil
	}
	if killSessions {
		return killDatabaseSessionsBatch(opts.Database) + ";\n" + sb.String() + ";", nil
	}
	// Online and read-write: a database that does not exist yet or is RESTORING
	// has no connections to close, and one in STANDBY refuses the ALTER.
	online := fmt.Sprintf("EXISTS (SELECT 1 FROM sys.databases WHERE name = N%s AND state = 0 AND is_in_standby = 0)",
		QuoteLiteral(opts.Database))
	db := quoteIdent(opts.Database)
	return fmt.Sprintf(`DECLARE @closed bit = 0;
IF %[1]s
BEGIN
    ALTER DATABASE %[2]s SET SINGLE_USER WITH ROLLBACK IMMEDIATE;
    SET @closed = 1;
END;
%[3]s;
IF @closed = 1 AND %[1]s
    ALTER DATABASE %[2]s SET MULTI_USER;`, online, db, sb.String()), nil
}

// backupHistorySelect is the msdb read behind BackupHistory, at package
// scope so TestBackupHistoryQueryWrapsEveryNullableColumn can check that every
// nullable column is still wrapped.
const backupHistorySelect = `
SELECT ISNULL(bs.database_name,''), ISNULL(bs.name,''), ISNULL(bs.description,''),
       ISNULL(bs.type,''),
       bs.backup_start_date, bs.backup_finish_date, ISNULL(bs.backup_size,0),
       ISNULL(bmf.physical_device_name,''), ISNULL(bs.user_name,''),
       ISNULL(bs.server_name,''),
       ISNULL(bs.database_version,0), ISNULL(bs.compatibility_level,0)
FROM   msdb.dbo.backupset bs
JOIN   msdb.dbo.backupmediafamily bmf ON bmf.media_set_id = bs.media_set_id
WHERE  bs.database_name = @p1
ORDER  BY bs.backup_finish_date DESC`

// BackupHistory returns the backup history for a database from msdb.
//
// Every column read here is nullable in msdb, and a NULL in any of them used
// to kill the whole read — which took Database Properties' General page, the
// Backup History viewer and Restore's backup-history source down with it. On
// an on-prem instance the columns happen to be populated by whatever ran the
// backup, so this never showed; Azure SQL Managed Instance's automated backups
// leave physical_device_name, user_name and server_name NULL, and
//
//	sql: Scan error on column index 7, name "physical_device_name"
//
// was the entire General page. A NULL means "not recorded", so every column is
// ISNULL'd in the query *and* scanned through a sql.Null* destination: the
// ISNULL is what the server sends, the Null destination is what survives a
// column this list forgets to wrap. Do not narrow either half back because a
// particular server populates the columns.
func (s *Server) BackupHistory(ctx context.Context, databaseName string) ([]*BackupInfo, error) {
	rows, err := s.query(ctx, backupHistorySelect, databaseName)
	return scanRows(rows, err, fmt.Sprintf("backup history for %q", databaseName), func(scan func(...any) error) (*BackupInfo, error) {
		b := &BackupInfo{}
		var dbName, setName, desc, bType, device, user, server sql.NullString
		var start, finish sql.NullTime
		var size, dbVersion, compat sql.NullInt64
		if err := scan(
			&dbName, &setName, &desc, &bType,
			&start, &finish, &size,
			&device, &user, &server,
			&dbVersion, &compat,
		); err != nil {
			return nil, err
		}
		b.DatabaseName, b.BackupSetName, b.Description = dbName.String, setName.String, desc.String
		b.BackupStart, b.BackupFinish = start.Time, finish.Time
		b.BackupSize = size.Int64
		b.DeviceName, b.UserName, b.ServerName = device.String, user.String, server.String
		b.DatabaseVersion = int(dbVersion.Int64)
		b.CompatibilityLevel = CompatibilityLevel(compat.Int64)
		switch bType.String {
		case "D":
			b.BackupType = BackupActionDatabase
		case "I":
			b.BackupType = BackupActionDifferential
		case "L":
			b.BackupType = BackupActionLog
		case "F":
			b.BackupType = BackupActionFiles
		}
		return b, nil
	})
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

// ============================================================
// Backup device inspection
// ============================================================

// BackupHeader describes one backup set on a backup device, as reported by
// RESTORE HEADERONLY.
type BackupHeader struct {
	BackupName           string
	Description          string
	BackupType           BackupAction
	Position             int
	DatabaseName         string
	ServerName           string
	BackupStart          time.Time
	BackupFinish         time.Time
	BackupSize           int64 // bytes
	CompressedSize       int64 // bytes; equals BackupSize when not compressed
	Compressed           bool
	HasChecksums         bool
	IsCopyOnly           bool
	DatabaseVersion      int
	CompatibilityLevel   CompatibilityLevel
	SoftwareVersionMajor int
	RecoveryModel        string
}

// BackupFile describes one database file inside a backup set, as reported
// by RESTORE FILELISTONLY.
type BackupFile struct {
	LogicalName   string
	PhysicalName  string
	Type          string // "D" data, "L" log, "F" full-text, "S" FILESTREAM
	FileGroupName string
	Size          int64 // bytes
	MaxSize       int64 // bytes
}

// IsBackupURL reports whether device names a backup blob in Azure Storage
// rather than a path on the server's filesystem — an http:// or https:// URL.
//
// The distinction decides the device keyword: a blob is BACKUP ... TO URL /
// RESTORE ... FROM URL, and a filesystem path is TO DISK / FROM DISK. It is
// not cosmetic on Azure SQL Managed Instance, which answers any TO DISK or
// FROM DISK with
//
//	Msg 41902 ... SQL Database Managed Instance supports database restore
//	from URI backup device only.
//
// and whose SERVERPROPERTY('InstanceDefaultBackupPath') is itself a blob
// container URL, so a caller that builds a default destination out of it
// arrives here holding a URL without ever having decided to.
//
// The test is the scheme alone. A UNC path (\\host\share\db.bak) and a
// drive-letter path are both DISK devices, and neither has one.
func IsBackupURL(device string) bool {
	// ToLower rather than a length check plus EqualFold on a slice: the
	// obvious spelling of that reads d[:len("https://")] before it knows the
	// string is that long, and panics on the input that is exactly "http://".
	d := strings.ToLower(strings.TrimSpace(device))
	return strings.HasPrefix(d, "https://") || strings.HasPrefix(d, "http://")
}

// deviceClause renders one BACKUP/RESTORE device operand.
func deviceClause(name string, isURL bool) string {
	kw := "DISK"
	if isURL {
		kw = "URL"
	}
	return fmt.Sprintf("%s = N'%s'", kw, escapeSingle(name))
}

// backupDeviceClause renders one device from BackupOptions.Devices /
// RestoreOptions.Devices, choosing DISK or URL by the device's own shape —
// those two fields are plain strings, so the shape is all there is to go on,
// and IsBackupURL is what a caller should use to reach the same verdict.
func backupDeviceClause(device string) string {
	return deviceClause(device, IsBackupURL(device))
}

// BackupTarget names where a RESTORE-side read finds a backup: a physical
// path, a blob URL, or a logical backup device from sys.backup_devices. The
// three are addressed differently and are not interchangeable — a logical
// device is named bare, as FROM [devicename], and passing its name as a path
// produces FROM DISK = N'devicename', which SQL Server reads as a file of that
// name in the server's default backup directory.
//
// Build one with DiskTarget, URLTarget or DeviceTarget.
type BackupTarget struct {
	name    string
	logical bool
	url     bool
}

// DiskTarget names a backup by its location on the server: a filesystem path,
// or — when path is an http/https URL, per IsBackupURL — the blob holding it.
// The URL case is classified here rather than left to the caller because every
// RESTORE-side read (VerifyBackup, BackupHeaders, BackupFileList) funnels
// through this constructor, and a Managed Instance refuses all three as DISK.
// URLTarget states the same thing outright.
func DiskTarget(path string) BackupTarget {
	return BackupTarget{name: path, url: IsBackupURL(path)}
}

// URLTarget names a backup by its Azure Storage blob URL, whatever its
// spelling — DiskTarget already recognises the usual http/https one.
func URLTarget(url string) BackupTarget { return BackupTarget{name: url, url: true} }

// DeviceTarget names a backup by the logical backup device holding it —
// a row of sys.backup_devices, see Server.BackupDevices.
func DeviceTarget(name string) BackupTarget { return BackupTarget{name: name, logical: true} }

// clause renders the target as the FROM operand of a RESTORE statement.
func (t BackupTarget) clause() string {
	if t.logical {
		return quoteIdent(t.name)
	}
	return deviceClause(t.name, t.url)
}

// String returns the target's name, for error messages and display.
func (t BackupTarget) String() string { return t.name }

// VerifyBackup checks that the backup set on device is complete and
// readable (RESTORE VERIFYONLY), without restoring it. device is a path on
// the server's filesystem; VerifyBackupFrom takes a logical backup device.
func (s *Server) VerifyBackup(ctx context.Context, device string) error {
	return s.VerifyBackupFrom(ctx, DiskTarget(device))
}

// VerifyBackupFrom is VerifyBackup for any BackupTarget — a path or a
// logical backup device.
func (s *Server) VerifyBackupFrom(ctx context.Context, target BackupTarget) error {
	stmt := "RESTORE VERIFYONLY FROM " + target.clause()
	if err := s.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: verify backup %q: %w", target.name, err)
	}
	return nil
}

// BackupHeaders reads the backup sets on a backup device (RESTORE
// HEADERONLY) — one BackupHeader per set, in position order. device is a path
// on the server's filesystem; BackupHeadersFrom takes a logical backup device.
func (s *Server) BackupHeaders(ctx context.Context, device string) ([]*BackupHeader, error) {
	return s.BackupHeadersFrom(ctx, DiskTarget(device))
}

// BackupHeadersFrom is BackupHeaders for any BackupTarget — a path or a
// logical backup device.
func (s *Server) BackupHeadersFrom(ctx context.Context, target BackupTarget) ([]*BackupHeader, error) {
	device := target.name
	q := "RESTORE HEADERONLY FROM " + target.clause()
	rows, err := s.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: read backup header %q: %w", device, err)
	}
	defer rows.Close()

	nr, err := newNamedRow(rows)
	if err != nil {
		return nil, fmt.Errorf("gosmo: read backup header %q: %w", device, err)
	}
	var headers []*BackupHeader
	for rows.Next() {
		if err := nr.scan(); err != nil {
			return nil, fmt.Errorf("gosmo: read backup header %q: %w", device, err)
		}
		headers = append(headers, &BackupHeader{
			BackupName:           nr.str("BackupName"),
			Description:          nr.str("BackupDescription"),
			BackupType:           backupTypeFromHeader(nr.intv("BackupType")),
			Position:             nr.intv("Position"),
			DatabaseName:         nr.str("DatabaseName"),
			ServerName:           nr.str("ServerName"),
			BackupStart:          nr.timev("BackupStartDate"),
			BackupFinish:         nr.timev("BackupFinishDate"),
			BackupSize:           nr.int64v("BackupSize"),
			CompressedSize:       nr.int64v("CompressedBackupSize"),
			Compressed:           nr.boolv("Compressed"),
			HasChecksums:         nr.boolv("HasBackupChecksums"),
			IsCopyOnly:           nr.boolv("IsCopyOnly"),
			DatabaseVersion:      nr.intv("DatabaseVersion"),
			CompatibilityLevel:   CompatibilityLevel(nr.intv("CompatibilityLevel")),
			SoftwareVersionMajor: nr.intv("SoftwareVersionMajor"),
			RecoveryModel:        nr.str("RecoveryModel"),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: read backup header %q: %w", device, err)
	}
	return headers, nil
}

// backupTypeFromHeader maps RESTORE HEADERONLY's numeric BackupType column
// to a BackupAction.
func backupTypeFromHeader(n int) BackupAction {
	switch n {
	case 2:
		return BackupActionLog
	case 4, 6:
		return BackupActionFiles
	case 5:
		return BackupActionDifferential
	default: // 1 = full database; 7/8 (partial) have no closer mapping
		return BackupActionDatabase
	}
}

// backupFileListQuery builds the RESTORE FILELISTONLY statement reading the
// file list of one backup set on target. fileNumber 0 leaves the WITH clause
// off, which SQL Server reads as the first set.
func backupFileListQuery(target BackupTarget, fileNumber int) string {
	q := "RESTORE FILELISTONLY FROM " + target.clause()
	if fileNumber > 0 {
		q += fmt.Sprintf(" WITH FILE = %d", fileNumber)
	}
	return q
}

// BackupFileList reads the database files contained in the first backup set
// on a backup device (RESTORE FILELISTONLY). Use BackupFileListForSet for a
// device holding more than one set.
func (s *Server) BackupFileList(ctx context.Context, device string) ([]*BackupFile, error) {
	return s.BackupFileListForSet(ctx, device, 0)
}

// BackupFileListForSet reads the database files contained in one particular
// backup set on a device (RESTORE FILELISTONLY WITH FILE = n).
//
// fileNumber is 1-based, as reported by BackupHeader.Position, and matches
// RestoreOptions.FileNumber — pass the same value to both or the file list
// describes a different set from the one being restored. Zero leaves the
// clause off, which SQL Server reads as the first set.
//
// A device that backups were appended to holds one set per backup, and their
// file lists differ whenever the sets came from different databases or files
// were added between them. Building RESTORE's MOVE clauses from the wrong
// set names logical files the restored set does not contain, which SQL
// Server rejects outright.
func (s *Server) BackupFileListForSet(ctx context.Context, device string, fileNumber int) ([]*BackupFile, error) {
	return s.BackupFileListForSetFrom(ctx, DiskTarget(device), fileNumber)
}

// BackupFileListForSetFrom is BackupFileListForSet for any BackupTarget — a
// path or a logical backup device.
func (s *Server) BackupFileListForSetFrom(ctx context.Context, target BackupTarget, fileNumber int) ([]*BackupFile, error) {
	device := target.name
	rows, err := s.query(ctx, backupFileListQuery(target, fileNumber))
	if err != nil {
		return nil, fmt.Errorf("gosmo: read backup file list %q: %w", device, err)
	}
	defer rows.Close()

	nr, err := newNamedRow(rows)
	if err != nil {
		return nil, fmt.Errorf("gosmo: read backup file list %q: %w", device, err)
	}
	var files []*BackupFile
	for rows.Next() {
		if err := nr.scan(); err != nil {
			return nil, fmt.Errorf("gosmo: read backup file list %q: %w", device, err)
		}
		files = append(files, &BackupFile{
			LogicalName:   nr.str("LogicalName"),
			PhysicalName:  nr.str("PhysicalName"),
			Type:          nr.str("Type"),
			FileGroupName: nr.str("FileGroupName"),
			Size:          nr.int64v("Size"),
			MaxSize:       nr.int64v("MaxSize"),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: read backup file list %q: %w", device, err)
	}
	return files, nil
}

// namedRow scans result sets whose column layout varies between SQL Server
// versions (RESTORE HEADERONLY / FILELISTONLY) by column name instead of
// position; a column this server doesn't emit simply reads as a zero value.
type namedRow struct {
	rows *sql.Rows
	idx  map[string]int
	vals []any
}

func newNamedRow(rows *sql.Rows) (*namedRow, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	idx := make(map[string]int, len(cols))
	for i, c := range cols {
		idx[strings.ToLower(c)] = i
	}
	vals := make([]any, len(cols))
	for i := range vals {
		vals[i] = new(any)
	}
	return &namedRow{rows: rows, idx: idx, vals: vals}, nil
}

func (r *namedRow) scan() error { return r.rows.Scan(r.vals...) }

// val returns the raw driver value for the named column, or nil if the
// column is absent (or NULL).
func (r *namedRow) val(name string) any {
	i, ok := r.idx[strings.ToLower(name)]
	if !ok {
		return nil
	}
	return *(r.vals[i].(*any))
}

func (r *namedRow) str(name string) string {
	switch v := r.val(name).(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	return ""
}

// int64v coerces the named column to int64. DECIMAL/NUMERIC columns (backup
// and file sizes) arrive from the driver as their textual form, so string
// and []byte parse too.
func (r *namedRow) int64v(name string) int64 {
	switch v := r.val(name).(type) {
	case int64:
		return v
	case int32:
		return int64(v)
	case float64:
		return int64(v)
	case []byte:
		return parseInt64(string(v))
	case string:
		return parseInt64(v)
	}
	return 0
}

func (r *namedRow) intv(name string) int { return int(r.int64v(name)) }

func (r *namedRow) boolv(name string) bool {
	switch v := r.val(name).(type) {
	case bool:
		return v
	case int64:
		return v != 0
	case []byte:
		return string(v) == "1"
	case string:
		return v == "1"
	}
	return false
}

func (r *namedRow) timev(name string) time.Time {
	if v, ok := r.val(name).(time.Time); ok {
		return v
	}
	return time.Time{}
}

// parseInt64 parses the textual form of a DECIMAL/NUMERIC value, tolerating
// a fractional part.
func parseInt64(s string) int64 {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int64(f)
	}
	return 0
}
