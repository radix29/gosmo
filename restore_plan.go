package gosmo

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"
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
		case f.Type == BackupFileStream || f.Type == BackupFileFullText:
			ext = ""
		case f.Type == BackupFileLog:
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

// ============================================================
// Restore sequence: which sets, from backup history
// ============================================================

// RestorePlanOptions shapes PlanRestore.
type RestorePlanOptions struct {
	// StopAt is the point in time to restore to, the server's wall clock as
	// RestoreOptions.StopAt reads it, or nil for the end of the log chain.
	// It is compared with BackupInfo.BackupFinish, which msdb records on the
	// same clock; only the date and time fields count, never the Location.
	StopAt *time.Time
	// TailLog says the caller backs up the tail of the log (BackupOptions.
	// NoRecovery) and restores it after the plan's sets. The chain must then
	// reach the newest log backup in history — a tail log follows nothing
	// else — and a StopAt past that log is the tail's to reach, not an error.
	TailLog bool
}

// RestorePlan is the backup sets a restore applies, in order.
type RestorePlan struct {
	// Sets is one full database backup, then at most one differential
	// taken against it, then the log backups that follow on, unbroken.
	// Every set but the last is restored WITH NORECOVERY.
	Sets []*BackupInfo
	// TailLog reports that the caller's tail-log backup is restored after
	// Sets: RestorePlanOptions.TailLog was set and StopAt, if any, is not
	// inside Sets. When StopAt falls inside a log in Sets, the tail log is
	// not restored — the database stops before it — and this is false.
	TailLog bool
}

// PlanRestore picks, from a database's backup history (Server.BackupHistory),
// the sets that restore it to opts.StopAt, or as far as the log chain runs —
// SSMS's Restore Database plan:
//
//   - the newest full database backup finished by StopAt;
//   - the newest differential taken against that full (its
//     DifferentialBaseGUID) and finished by StopAt — none for a COPY_ONLY
//     full, which is no differential's base;
//   - then every log backup that continues the chain from there — the first
//     holds the base's LastLSN, each next starts at the last one's LastLSN —
//     up to and including the first that finished after StopAt, the log
//     holding the point. COPY_ONLY logs are not in the chain.
//
// A gap in the chain ends it. A StopAt the chain does not reach — past a gap,
// or past the newest log — is refused (ErrInvalidRequest), unless the gap is
// only the tail of the log and opts.TailLog says the caller backs it up. With
// no full backup to start from the result is ErrNotFound. Sets no RESTORE
// can name (BackupInfo.Restorable) and file, filegroup and partial backups
// are ignored.
//
// A log backup is chosen by when it finished, which is not quite when its
// last record was logged: a STOPAT a moment before a log backup finished can
// fall after its LastLSN, and SQL Server then leaves the database RESTORING
// ("too early to apply") rather than recovering it. Restoring the next log,
// or the tail, recovers it.
func PlanRestore(history []*BackupInfo, opts RestorePlanOptions) (*RestorePlan, error) {
	var stop time.Time
	if opts.StopAt != nil {
		stop = wallClock(*opts.StopAt)
	}
	by := func(t time.Time) bool { return opts.StopAt == nil || !wallClock(t).After(stop) }
	newest := func(a, b *BackupInfo) int {
		return cmp.Or(a.BackupFinish.Compare(b.BackupFinish), cmp.Compare(a.BackupSetID, b.BackupSetID))
	}
	var fulls, diffs, logs []*BackupInfo
	for _, b := range history {
		if !b.Restorable() || b.BackupFinish.IsZero() {
			continue
		}
		switch b.SetType {
		case BackupSetDatabase:
			if by(b.BackupFinish) {
				fulls = append(fulls, b)
			}
		case BackupSetDifferential:
			if by(b.BackupFinish) {
				diffs = append(diffs, b)
			}
		case BackupSetLog:
			if !b.IsCopyOnly && b.FirstLSN != "" && b.LastLSN != "" {
				logs = append(logs, b)
			}
		}
	}
	if len(fulls) == 0 {
		if opts.StopAt != nil {
			return nil, notFoundf("gosmo: plan restore: no full database backup finished by %s", stop.Format(time.DateTime))
		}
		return nil, notFoundf("gosmo: plan restore: no full database backup in the history")
	}
	full := slices.MaxFunc(fulls, newest)
	plan := &RestorePlan{Sets: []*BackupInfo{full}}

	diffs = slices.DeleteFunc(diffs, func(d *BackupInfo) bool {
		return full.IsCopyOnly || full.BackupSetGUID == "" || d.DifferentialBaseGUID != full.BackupSetGUID
	})
	if len(diffs) > 0 {
		plan.Sets = append(plan.Sets, slices.MaxFunc(diffs, newest))
	}

	// The chain: the first log holds the base's last LSN, each next one
	// starts where the last ended.
	base := plan.Sets[len(plan.Sets)-1]
	slices.SortStableFunc(logs, func(a, b *BackupInfo) int {
		return cmp.Or(CompareLSN(a.FirstLSN, b.FirstLSN), newest(a, b))
	})
	end, reached := base.LastLSN, false
	for end != "" && !reached {
		i := slices.IndexFunc(logs, func(l *BackupInfo) bool {
			return CompareLSN(l.FirstLSN, end) <= 0 && CompareLSN(end, l.LastLSN) < 0
		})
		if i < 0 {
			break
		}
		plan.Sets = append(plan.Sets, logs[i])
		end = logs[i].LastLSN
		reached = opts.StopAt != nil && wallClock(logs[i].BackupFinish).After(stop)
	}
	last := plan.Sets[len(plan.Sets)-1]
	atEnd := !slices.ContainsFunc(logs, func(l *BackupInfo) bool { return CompareLSN(l.LastLSN, end) > 0 })
	if reached {
		return plan, nil
	}
	if opts.TailLog {
		if !atEnd {
			return nil, invalidf("gosmo: plan restore: the log chain breaks after the %s backup finished %s, so a tail-log backup cannot follow it",
				strings.ToLower(string(last.SetType)), last.BackupFinish.Format(time.DateTime))
		}
		plan.TailLog = true
		return plan, nil
	}
	if opts.StopAt != nil {
		why := "take a tail-log backup to reach it"
		if !atEnd {
			why = "the log chain breaks after it"
		}
		return nil, invalidf("gosmo: plan restore: %s is after the last backup in the chain, the %s backup finished %s; %s",
			stop.Format(time.DateTime), strings.ToLower(string(last.SetType)), last.BackupFinish.Format(time.DateTime), why)
	}
	return plan, nil
}

// wallClock is t's date and time fields read in UTC, so two times compare by
// what their clocks say whatever Location each carries: msdb's datetimes
// arrive zoneless (UTC) and a caller's StopAt may be in any zone.
func wallClock(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

// CompareLSN compares two log sequence numbers in msdb's decimal form
// (numeric(25,0), as BackupInfo carries them): -1, 0 or +1 as a is less than,
// equal to or greater than b. An LSN has up to 25 digits — more than an
// int64 holds — so it is compared as text, by length and then digit by
// digit; leading zeros are ignored and "" is the smallest.
func CompareLSN(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	return cmp.Or(cmp.Compare(len(a), len(b)), strings.Compare(a, b))
}
