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
	// Progress, if set, is called for every message SQL Server emits while
	// the backup runs, including the "N percent processed" notices STATS
	// produces — pct is -1 for a message that doesn't carry a percentage.
	Progress func(pct int, message string)
}

// Backup performs a BACKUP DATABASE (or LOG) operation.
func (s *Server) Backup(opts BackupOptions) error {
	return s.BackupContext(context.Background(), opts)
}

// BackupContext is the context-aware variant of Backup.
func (s *Server) BackupContext(ctx context.Context, opts BackupOptions) error {
	if opts.Progress != nil && opts.Stats == 0 {
		opts.Stats = 10
	}
	sqlText, err := BuildBackupStatement(opts)
	if err != nil {
		return err
	}

	if opts.Progress == nil {
		if err := s.execContext(ctx, sqlText); err != nil {
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
// BackupContext validates and builds the statement the same way, then runs
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

	var sb strings.Builder
	fmt.Fprintf(&sb, "BACKUP %s %s TO ", opts.Action, quoteIdent(opts.Database))

	deviceList := make([]string, len(opts.Devices))
	for i, d := range opts.Devices {
		deviceList[i] = fmt.Sprintf("DISK = N'%s'", escapeSingle(d))
	}
	sb.WriteString(strings.Join(deviceList, ", "))

	var withs []string
	if opts.BackupSetName != "" {
		withs = append(withs, fmt.Sprintf("NAME = N'%s'", escapeSingle(opts.BackupSetName)))
	}
	if opts.Description != "" {
		withs = append(withs, fmt.Sprintf("DESCRIPTION = N'%s'", escapeSingle(opts.Description)))
	}
	if opts.MediaDescription != "" {
		withs = append(withs, fmt.Sprintf("MEDIADESCRIPTION = N'%s'", escapeSingle(opts.MediaDescription)))
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
	// Action: DATABASE (default) or LOG.
	Action BackupAction
	// Devices is one or more backup file paths (required).
	Devices []string
	// RelocateFiles maps logical file names to new physical paths.
	RelocateFiles []RelocateFile
	// NoRecovery keeps the database in RESTORING state (for log shipping / tail-log).
	NoRecovery bool
	// Recovery transitions the database to ONLINE (default when neither flag is set).
	Recovery bool
	// StandBy sets standby mode; provide the undo-file path.
	StandBy string
	// Replace forces restoration over an existing database.
	Replace bool
	// Checksum verifies backup checksums.
	Checksum bool
	// Stats controls progress reporting frequency.
	Stats int
	// StopAt performs a point-in-time restore.
	StopAt *time.Time
}

// RelocateFile maps a logical file name to a new physical path.
type RelocateFile struct {
	LogicalName  string
	PhysicalName string
}

// Restore performs a RESTORE DATABASE (or LOG) operation.
func (s *Server) Restore(opts RestoreOptions) error {
	return s.RestoreContext(context.Background(), opts)
}

// RestoreContext is the context-aware variant of Restore.
func (s *Server) RestoreContext(ctx context.Context, opts RestoreOptions) error {
	if opts.Database == "" {
		return fmt.Errorf("gosmo: restore: database name is required")
	}
	if len(opts.Devices) == 0 {
		return fmt.Errorf("gosmo: restore: at least one device is required")
	}
	if opts.Action == "" {
		opts.Action = BackupActionDatabase
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "RESTORE %s %s FROM ", opts.Action, quoteIdent(opts.Database))

	deviceList := make([]string, len(opts.Devices))
	for i, d := range opts.Devices {
		deviceList[i] = fmt.Sprintf("DISK = N'%s'", escapeSingle(d))
	}
	sb.WriteString(strings.Join(deviceList, ", "))

	var withs []string
	for _, rf := range opts.RelocateFiles {
		withs = append(withs, fmt.Sprintf("MOVE N'%s' TO N'%s'",
			escapeSingle(rf.LogicalName), escapeSingle(rf.PhysicalName)))
	}
	if opts.NoRecovery {
		withs = append(withs, "NORECOVERY")
	} else if opts.Recovery {
		withs = append(withs, "RECOVERY")
	}
	if opts.StandBy != "" {
		withs = append(withs, fmt.Sprintf("STANDBY = N'%s'", escapeSingle(opts.StandBy)))
	}
	if opts.Replace {
		withs = append(withs, "REPLACE")
	}
	if opts.Checksum {
		withs = append(withs, "CHECKSUM")
	}
	if opts.Stats > 0 {
		withs = append(withs, fmt.Sprintf("STATS = %d", opts.Stats))
	}
	if opts.StopAt != nil {
		withs = append(withs, fmt.Sprintf("STOPAT = '%s'", opts.StopAt.Format("2006-01-02T15:04:05")))
	}
	if len(withs) > 0 {
		fmt.Fprintf(&sb, " WITH %s", strings.Join(withs, ", "))
	}

	if err := s.execContext(ctx, sb.String()); err != nil {
		return fmt.Errorf("gosmo: restore %q: %w", opts.Database, err)
	}
	return nil
}

// BackupHistory returns the backup history for a database from msdb.
func (s *Server) BackupHistory(databaseName string) ([]*BackupInfo, error) {
	return s.BackupHistoryContext(context.Background(), databaseName)
}

// BackupHistoryContext is the context-aware variant of BackupHistory.
func (s *Server) BackupHistoryContext(ctx context.Context, databaseName string) ([]*BackupInfo, error) {
	const q = `
SELECT bs.database_name, ISNULL(bs.name,''), ISNULL(bs.description,''), bs.type,
       bs.backup_start_date, bs.backup_finish_date, bs.backup_size,
       bmf.physical_device_name, bs.user_name, bs.server_name,
       bs.database_version, bs.compatibility_level
FROM   msdb.dbo.backupset bs
JOIN   msdb.dbo.backupmediafamily bmf ON bmf.media_set_id = bs.media_set_id
WHERE  bs.database_name = @p1
ORDER  BY bs.backup_finish_date DESC`

	rows, err := s.db.QueryContext(ctx, q, databaseName)
	if err != nil {
		return nil, fmt.Errorf("gosmo: backup history for %q: %w", databaseName, err)
	}
	defer rows.Close()

	var history []*BackupInfo
	for rows.Next() {
		b := &BackupInfo{}
		var bType string
		var desc sql.NullString
		if err := rows.Scan(
			&b.DatabaseName, &b.BackupSetName, &desc, &bType,
			&b.BackupStart, &b.BackupFinish, &b.BackupSize,
			&b.DeviceName, &b.UserName, &b.ServerName,
			&b.DatabaseVersion, &b.CompatibilityLevel,
		); err != nil {
			return nil, err
		}
		b.Description = desc.String
		switch bType {
		case "D":
			b.BackupType = BackupActionDatabase
		case "L":
			b.BackupType = BackupActionLog
		case "F":
			b.BackupType = BackupActionFiles
		}
		history = append(history, b)
	}
	return history, rows.Err()
}
