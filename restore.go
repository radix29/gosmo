package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

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
	// Devices is where the backup is read from (required) — DiskTarget,
	// URLTarget or DeviceTarget, several for a striped backup.
	Devices []BackupTarget
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
	// An online database is set SINGLE_USER WITH ROLLBACK IMMEDIATE and,
	// when that succeeded, put back to MULTI_USER after the RESTORE whenever
	// it is still online and read-write — which also repairs it when the
	// RESTORE fails, since a refused RESTORE does not end the batch; Restore
	// repairs it again, off the caller's cancellation, only if the batch
	// itself was cut short. A database that does not exist yet or is RESTORING has
	// nobody to close and is left alone. One in STANDBY refuses the ALTER
	// but can have readers, so its sessions are killed instead and no access
	// mode is changed. A Managed Instance refuses SET SINGLE_USER, so
	// Server.BuildRestoreStatement and Restore kill the database's
	// sessions there too, whatever its state, and change no access mode.
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

	if opts.Progress == nil || Scripting(ctx) {
		err = s.exec(ctx, sqlText)
	} else if err = refuseInTx(ctx, s, "restore with progress"); err != nil {
		return err
	} else {
		err = s.execWithProgress(ctx, sqlText, opts.Progress)
		if err == nil {
			observe(ctx, s, ScriptEntry{Server: scriptServerName(ctx, s), SQL: sqlText})
		}
	}
	if err != nil {
		// The batch's own MULTI_USER does not run when the batch is cut
		// short — a cancel or an expired deadline, the likeliest ways for a
		// long restore to fail. Only then: a failed RESTORE is
		// statement-level (Msg 3201/3013 at severity 16, probed on 17 and
		// 13), so the batch has already released what it took, and a
		// repair after any error would flip a database deliberately left
		// RESTRICTED_USER to MULTI_USER (see batchCutShort). Best effort: a
		// database left RESTORING, or never created, refuses it, and the
		// restore's error is what the caller is told about.
		if opts.CloseExistingConnections && batchCutShort(err) && !s.refusesSingleUser() {
			_ = s.restoreMultiUser(ctx, opts.Database)
		}
		return fmt.Errorf("gosmo: restore %q: %w", opts.Database, err)
	}
	return nil
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
// the form the instance accepts: SET SINGLE_USER, or killing the database's
// sessions on a Managed Instance, which refuses SET SINGLE_USER. That is why
// this is a Server method: until 2026-09-23 a package-level
// BuildRestoreStatement sat beside this one and always wrote the SINGLE_USER
// form, which a Managed Instance fails.
func (s *Server) BuildRestoreStatement(opts RestoreOptions) (string, error) {
	return buildRestoreStatement(opts, s.refusesSingleUser())
}

// buildRestoreStatement is Server.BuildRestoreStatement, closing existing
// connections by killing sessions rather than by SET SINGLE_USER when
// killSessions is set.
func buildRestoreStatement(opts RestoreOptions, killSessions bool) (string, error) {
	if opts.Database == "" {
		return "", invalidf("gosmo: restore: database name is required")
	}
	if err := checkTargets("restore", opts.Devices); err != nil {
		return "", err
	}
	if opts.Action == "" {
		opts.Action = BackupActionDatabase
	}
	if !validBackupAction(opts.Action) {
		return "", invalidf("gosmo: restore: unrecognized action %q", opts.Action)
	}
	if !restoreRecoveryNames[opts.Recovery] {
		return "", invalidf("gosmo: restore: unrecognized recovery %q", opts.Recovery)
	}
	if (opts.Recovery == RestoreWithStandBy) != (opts.StandByFile != "") {
		return "", invalidf("gosmo: restore: a standby file goes with, and only with, RestoreWithStandBy")
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
	sb.WriteString(targetList(opts.Devices))

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
	// has no connections to close. One in STANDBY refuses the ALTER but is
	// readable, so it has readers — the log-shipping secondary — and they
	// would fail the RESTORE with "Exclusive access could not be obtained";
	// they are killed instead, as on a Managed Instance, and no access mode
	// is set, so none is released.
	lit := QuoteLiteral(opts.Database)
	online := fmt.Sprintf("EXISTS (SELECT 1 FROM sys.databases WHERE name = %s AND state = 0 AND is_in_standby = 0)", lit)
	standby := fmt.Sprintf("EXISTS (SELECT 1 FROM sys.databases WHERE name = %s AND state = 0 AND is_in_standby = 1)", lit)
	db := quoteIdent(opts.Database)
	return fmt.Sprintf(`DECLARE @closed bit = 0;
IF %[1]s
BEGIN
    ALTER DATABASE %[2]s SET SINGLE_USER WITH ROLLBACK IMMEDIATE;
    IF @@ERROR = 0 SET @closed = 1;
END
ELSE IF %[4]s
BEGIN
%[5]s;
END;
%[3]s;
IF @closed = 1 AND %[1]s
    ALTER DATABASE %[2]s SET MULTI_USER;`, online, db, sb.String(), standby, killDatabaseSessionsBatch(opts.Database)), nil
}

// backupHistorySelect is the msdb read behind BackupHistory, at package
// scope so TestBackupHistoryQueryWrapsEveryNullableColumn can check that every
// nullable column is still wrapped.
//
// backupmediafamily has one row per media family *and* per mirror, so the
// join returns a striped set once per stripe. Only the first mirror is read
// (any mirror is a complete copy), and BackupHistory folds the families back
// into one BackupInfo per set — which is why the ORDER BY keeps a set's rows
// together, in family order.
const backupHistorySelect = `
SELECT ISNULL(bs.database_name,''), ISNULL(bs.name,''), ISNULL(bs.description,''),
       ISNULL(bs.type,''),
       bs.backup_start_date, bs.backup_finish_date, ISNULL(bs.backup_size,0),
       ISNULL(bmf.physical_device_name,''), ISNULL(bs.user_name,''),
       ISNULL(bs.server_name,''),
       ISNULL(bs.database_version,0), ISNULL(bs.compatibility_level,0),
       ISNULL(bs.position,0), ISNULL(bs.backup_set_id,0), ISNULL(bs.media_set_id,0),
       ISNULL(bms.mirror_count,1)
FROM   msdb.dbo.backupset bs
JOIN   msdb.dbo.backupmediafamily bmf ON bmf.media_set_id = bs.media_set_id AND bmf.mirror = 0
LEFT JOIN msdb.dbo.backupmediaset bms ON bms.media_set_id = bs.media_set_id
WHERE  bs.database_name = @p1
ORDER  BY bs.backup_finish_date DESC, bs.backup_set_id DESC, bmf.family_sequence_number`

// BackupHistory returns the backup history for a database from msdb, newest
// first, one BackupInfo per backup set whatever number of files it was striped
// over — see BackupInfo.Devices.
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
	perFamily, err := scanRows(rows, err, fmt.Sprintf("backup history for %q", databaseName), func(scan func(...any) error) (*BackupInfo, error) {
		b := &BackupInfo{}
		var dbName, setName, desc, bType, device, user, server sql.NullString
		var start, finish sql.NullTime
		var size, dbVersion, compat, position, setID, mediaSetID, mirrors sql.NullInt64
		if err := scan(
			&dbName, &setName, &desc, &bType,
			&start, &finish, &size,
			&device, &user, &server,
			&dbVersion, &compat,
			&position, &setID, &mediaSetID, &mirrors,
		); err != nil {
			return nil, err
		}
		b.DatabaseName, b.BackupSetName, b.Description = dbName.String, setName.String, desc.String
		b.BackupStart, b.BackupFinish = start.Time, finish.Time
		b.BackupSize = size.Int64
		b.DeviceName, b.UserName, b.ServerName = device.String, user.String, server.String
		b.Devices = []string{device.String}
		b.DatabaseVersion = int(dbVersion.Int64)
		b.CompatibilityLevel = CompatibilityLevel(compat.Int64)
		b.Position, b.BackupSetID, b.MediaSetID = int(position.Int64), setID.Int64, int(mediaSetID.Int64)
		b.MirrorCount = max(int(mirrors.Int64), 1)
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
	if err != nil {
		return nil, err
	}
	return groupBackupFamilies(perFamily), nil
}

// groupBackupFamilies folds BackupHistory's one-row-per-media-family read into
// one BackupInfo per backup set, keeping the sets' order and each set's
// families in the order read. Rows of one set arrive together (the query
// orders by set id before family), but the fold keys on the id rather than on
// adjacency, so an ordering change cannot split a set in two. A row with no
// set id — only a fake or a damaged msdb produces one — stays its own entry.
func groupBackupFamilies(rows []*BackupInfo) []*BackupInfo {
	out := make([]*BackupInfo, 0, len(rows))
	byID := make(map[int64]*BackupInfo, len(rows))
	for _, b := range rows {
		if b.BackupSetID != 0 {
			if first, ok := byID[b.BackupSetID]; ok {
				first.Devices = append(first.Devices, b.Devices...)
				continue
			}
			byID[b.BackupSetID] = b
		}
		out = append(out, b)
	}
	return out
}
