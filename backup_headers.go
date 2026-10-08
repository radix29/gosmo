package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

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

// BackupTarget names where a backup is written or read: a physical path, a
// blob URL, or a logical backup device from sys.backup_devices. The three are
// addressed differently and are not interchangeable — a logical device is
// named bare, as TO [devicename] / FROM [devicename], and passing its name as
// a path produces DISK = N'devicename', which SQL Server reads as a file of
// that name in the server's default backup directory.
//
// It is the one way to name a backup location, for BACKUP, RESTORE and the
// RESTORE-side reads alike. Until 2026-09-23 BackupOptions.Devices and
// RestoreOptions.Devices were plain strings, sniffed for a URL, and so could
// not name a logical device at all.
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

// clause renders the target as a TO operand of BACKUP or a FROM operand of
// RESTORE — the same form in both.
func (t BackupTarget) clause() string {
	if t.logical {
		return quoteIdent(t.name)
	}
	return deviceClause(t.name, t.url)
}

// String returns the target's name, for error messages and display.
func (t BackupTarget) String() string { return t.name }

// IsURL reports whether the target is an Azure Storage blob.
func (t BackupTarget) IsURL() bool { return t.url }

// IsDevice reports whether the target is a logical backup device.
func (t BackupTarget) IsDevice() bool { return t.logical }

// checkTargets refuses an empty device list, and a target with no name — the
// zero BackupTarget, which would render as DISK = N”.
func checkTargets(verb string, targets []BackupTarget) error {
	if len(targets) == 0 {
		return invalidf("gosmo: %s: at least one device is required", verb)
	}
	for i, t := range targets {
		if t.name == "" {
			return invalidf("gosmo: %s: device %d has no name", verb, i+1)
		}
	}
	return nil
}

// targetList renders targets as the comma-separated TO/FROM operand list.
func targetList(targets []BackupTarget) string {
	parts := make([]string, len(targets))
	for i, t := range targets {
		parts[i] = t.clause()
	}
	return strings.Join(parts, ", ")
}

// VerifyBackup checks that the backup set on targets is complete and
// readable (RESTORE VERIFYONLY), without restoring it.
//
// Every read on the RESTORE side takes the media set's full device list, as
// RestoreOptions.Devices does: a backup striped over several files is one
// media set, and VERIFYONLY on one stripe of it is refused ("The media set
// has 2 media families but only 1 are provided").
func (s *Server) VerifyBackup(ctx context.Context, targets ...BackupTarget) error {
	if err := checkTargets("verify backup", targets); err != nil {
		return err
	}
	stmt := "RESTORE VERIFYONLY FROM " + targetList(targets)
	if err := s.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: verify backup %q: %w", targetNames(targets), err)
	}
	return nil
}

// targetNames joins targets' names for an error message.
func targetNames(targets []BackupTarget) string {
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.name
	}
	return strings.Join(names, ", ")
}

// BackupHeaders reads the backup sets on targets (RESTORE HEADERONLY) — one
// BackupHeader per set, in position order. targets is the media set's full
// device list — see VerifyBackup.
func (s *Server) BackupHeaders(ctx context.Context, targets ...BackupTarget) ([]*BackupHeader, error) {
	if err := checkTargets("read backup header", targets); err != nil {
		return nil, err
	}
	device := targetNames(targets)
	q := "RESTORE HEADERONLY FROM " + targetList(targets)
	rows, err := s.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: read backup header %q: %w", device, err)
	}
	defer rows.Close()

	nr, err := newNamedRow(rows.Rows)
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
// file list of one backup set on targets. fileNumber 0 leaves the WITH clause
// off, which SQL Server reads as the first set.
func backupFileListQuery(fileNumber int, targets []BackupTarget) string {
	q := "RESTORE FILELISTONLY FROM " + targetList(targets)
	if fileNumber > 0 {
		q += fmt.Sprintf(" WITH FILE = %d", fileNumber)
	}
	return q
}

// BackupFileList reads the database files contained in one particular
// backup set on targets (RESTORE FILELISTONLY WITH FILE = n). targets is the
// media set's full device list — see VerifyBackup.
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
func (s *Server) BackupFileList(ctx context.Context, fileNumber int, targets ...BackupTarget) ([]*BackupFile, error) {
	if err := checkTargets("read backup file list", targets); err != nil {
		return nil, err
	}
	device := targetNames(targets)
	rows, err := s.query(ctx, backupFileListQuery(fileNumber, targets))
	if err != nil {
		return nil, fmt.Errorf("gosmo: read backup file list %q: %w", device, err)
	}
	defer rows.Close()

	nr, err := newNamedRow(rows.Rows)
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
