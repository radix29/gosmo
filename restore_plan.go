package gosmo

import (
	"fmt"
	"strings"
)

// ============================================================
// Restore planning: which set, and where its files go
// ============================================================
//
// A RESTORE names a backup set twice: once in WITH FILE = n, and again in
// every MOVE clause, whose logical names come from that set's RESTORE
// FILELISTONLY. The two must agree — MOVE clauses read from a different set
// name logical files the restored set does not contain, and the whole
// RESTORE fails. The rules below are the one place both are decided, so a
// caller's preview of the file paths, its file-list read and the statement
// it runs cannot drift apart.

// SetNumber is the WITH FILE = n (RestoreOptions.FileNumber) that restores
// this set: its Position, or 0 — no clause — for the first set. SQL Server
// reads a RESTORE without the clause as set 1, so the common single-backup
// device restores without a redundant WITH FILE = 1. A header with no
// Position (none from RESTORE HEADERONLY) is read as set 1 too.
func (h *BackupHeader) SetNumber() int { return setNumber(h.Position) }

// SetNumber is BackupHeader.SetNumber for a backup-history entry: the
// WITH FILE = n that restores it from its devices.
//
// A file written with NOINIT holds one set per backup appended to it, and a
// RESTORE that leaves WITH FILE off reads set 1 — the oldest — so restoring
// the newest history entry without this restores the oldest backup instead.
func (b *BackupInfo) SetNumber() int { return setNumber(b.Position) }

func setNumber(position int) int {
	if position > 1 {
		return position
	}
	return 0
}

// Restorable reports whether a RESTORE statement can name this backup: it was
// written to a device. A Managed Instance's automated backups are recorded in
// msdb with no physical device (NULL, read as ""), and are restored through
// the control plane's point-in-time restore instead. BackupHistory keeps them —
// as history they are true — so a caller offering restore sources filters on
// this.
func (b *BackupInfo) Restorable() bool { return strings.TrimSpace(b.DeviceName) != "" }

// BackupSetAt returns the header RESTORE reads for WITH FILE = setNumber —
// the one whose SetNumber matches — or nil when the device holds no such set.
// A device can hold sets from more than one database, so the set being
// restored, not the device's first, says what the source database was.
//
// There is deliberately no fallback to the first set: a history entry whose
// file has since been overwritten WITH INIT names a position the device no
// longer has, and restoring set 1 in its place restores a different backup.
func BackupSetAt(headers []*BackupHeader, setNumber int) *BackupHeader {
	for _, h := range headers {
		if h.SetNumber() == setNumber {
			return h
		}
	}
	return nil
}

// RelocationMode chooses where a restore puts the backup set's files.
type RelocationMode int

const (
	// RelocateIfRenamed leaves the files at the paths recorded in the backup
	// when restoring under the source database's own name, and otherwise
	// moves every file to the default data and log directories — a copy
	// restored beside its original would collide with the original's files.
	RelocateIfRenamed RelocationMode = iota
	// RelocateNone restores every file to the path recorded in the backup.
	RelocateNone
	// RelocateToFolders moves every file into DataDir and LogDir, renamed
	// or not.
	RelocateToFolders
)

// RestoreRelocation is where a restore puts the backup set's files — the
// MOVE clauses Moves builds.
type RestoreRelocation struct {
	Mode RelocationMode
	// DataDir and LogDir are RelocateToFolders' directories. An empty one
	// falls back to the matching default.
	DataDir, LogDir string
	// DefaultDataDir and DefaultLogDir are the server's default directories
	// (Server.DefaultPaths): RelocateIfRenamed's destination, and the
	// fallback for an empty DataDir or LogDir.
	DefaultDataDir, DefaultLogDir string
	// Collation is the server's, which decides whether restoring Sales as
	// sales is a rename: on a case-sensitive instance it is, and without
	// MOVE clauses the copy collides with the source database's files.
	// Empty is read as case-insensitive, the default.
	Collation string
}

// NeedsFileList reports whether restoring the set of database source as
// target moves any file, and so whether the RESTORE FILELISTONLY that Moves
// is built from is worth running at all.
func (r RestoreRelocation) NeedsFileList(source, target string) bool {
	switch r.Mode {
	case RelocateToFolders:
		return true
	case RelocateNone:
		return false
	default:
		return !SameName(r.Collation, source, target)
	}
}

// Moves returns the MOVE clauses that put files — the restored set's RESTORE
// FILELISTONLY — where r asks, or nil to leave every file at its recorded
// path.
//
// A rename decides the file names in every mode that moves anything:
// restoring under a different database name mints "<target>_<logical><ext>",
// so the copy cannot collide with the original database's files, while a
// same-name restore keeps the backup's file names and changes only the
// directory. A file with no extension gets .ldf for the log and .ndf
// otherwise — except a FILESTREAM container (type S) or a legacy full-text
// catalog (F), which is a directory: a renamed one is "<target>_<logical>",
// with no extension, so it does not pass for a data file.
//
// Two adjustments keep every path one the server can create. A character no
// Windows file name may hold (`\ / : * ? " < > |`, a control character) is
// replaced with "_": a logical or database name is a sysname and may hold
// any of them, and in a minted name a separator makes a subdirectory and a
// colon an NTFS stream (Msg 3634, access denied). And two files landing on
// one path — same-named files from two source directories relocated into
// one folder, or two names alike once replaced — are told apart as
// "name_2.ndf", "name_3.ndf", …: SQL Server refuses the restore otherwise
// (Msg 3176, "is claimed by"). Both rules are the stricter Windows ones on
// every platform, and paths are compared case-insensitively, so a name
// minted for one host still fits another. Only the physical name changes;
// the MOVE clause still addresses the file by its logical name.
func (r RestoreRelocation) Moves(files []*BackupFile, source, target string) []RelocateFile {
	if !r.NeedsFileList(source, target) {
		return nil
	}
	dataDir, logDir := r.DefaultDataDir, r.DefaultLogDir
	if r.Mode == RelocateToFolders {
		if r.DataDir != "" {
			dataDir = r.DataDir
		}
		if r.LogDir != "" {
			logDir = r.LogDir
		}
	}
	renamed := !SameName(r.Collation, source, target)

	var moves []RelocateFile
	taken := map[string]bool{}
	for _, f := range files {
		dir, ext := dataDir, ServerPathExt(f.PhysicalName)
		switch {
		case f.Type == "S" || f.Type == "F":
			ext = ""
		case f.Type == "L":
			dir = logDir
			if ext == "" {
				ext = ".ldf"
			}
		case ext == "":
			ext = ".ndf"
		}
		name := ServerPathBase(f.PhysicalName)
		if renamed {
			name = target + "_" + f.LogicalName + ext
		} else if ext != "" {
			ext = ServerPathExt(name) // the kept name's own, if any
		}
		stem := fileNameSafe(strings.TrimSuffix(name, ext))
		path := JoinServerPath(dir, stem+ext)
		// NameKey under the empty (case-insensitive) collation folds case.
		for n := 2; taken[NameKey("", path)]; n++ {
			path = JoinServerPath(dir, fmt.Sprintf("%s_%d%s", stem, n, ext))
		}
		taken[NameKey("", path)] = true
		moves = append(moves, RelocateFile{LogicalName: f.LogicalName, PhysicalName: path})
	}
	return moves
}

// fileNameSafe replaces each character a Windows file name cannot hold with
// "_".
func fileNameSafe(name string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`\/:*?"<>|`, r) {
			return '_'
		}
		return r
	}, name)
}

// FromHeader points o at backup set h: the verb its type restores with
// (h.SetType.RestoreVerb(), replacing any o.Action), WITH FILE =
// h.SetNumber(), and — for a set that places the database's files
// (h.SetType.PlacesFiles()) — the MOVE clauses r builds from files, which
// must be h's own RESTORE FILELISTONLY (Server.BackupFileList with
// h.SetNumber()). A log or differential set is restored onto files an
// earlier restore placed, so it gets no MOVE clauses and files may be nil.
// o.Database is the target the moves are planned for, so set it first.
//
// Until 2026-10-09 the verb was left to the caller, and gossms's Restore
// dialog, which never set it, sent RESTORE DATABASE for a log backup.
func (o *RestoreOptions) FromHeader(h *BackupHeader, files []*BackupFile, r RestoreRelocation) {
	o.Action = h.SetType.RestoreVerb()
	o.FileNumber = h.SetNumber()
	o.RelocateFiles = nil
	if h.SetType.PlacesFiles() {
		o.RelocateFiles = r.Moves(files, h.DatabaseName, o.Database)
	}
}
