package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
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
	Stats int
}

// Backup performs a BACKUP DATABASE (or LOG) operation.
func (s *Server) Backup(opts BackupOptions) error {
	return s.BackupContext(context.Background(), opts)
}

// BackupContext is the context-aware variant of Backup.
func (s *Server) BackupContext(ctx context.Context, opts BackupOptions) error {
	if opts.Database == "" {
		return fmt.Errorf("gosmo: backup: database name is required")
	}
	if len(opts.Devices) == 0 {
		return fmt.Errorf("gosmo: backup: at least one device is required")
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

	if _, err := s.db.ExecContext(ctx, sb.String()); err != nil {
		return fmt.Errorf("gosmo: backup %q: %w", opts.Database, err)
	}
	return nil
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

	if _, err := s.db.ExecContext(ctx, sb.String()); err != nil {
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
SELECT bs.database_name, bs.name, ISNULL(bs.description,''), bs.type,
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
