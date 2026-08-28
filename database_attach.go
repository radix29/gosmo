package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// database_attach.go is detaching a database from an instance and attaching
// one back — SSMS's Tasks > Detach and the Databases folder's Attach — plus
// the read that makes an Attach dialog possible at all: the file list held
// inside a detached primary data file.

// DetachOptions are sp_detach_db's three choices, named for what they do
// rather than for the procedure's parameters, whose senses are inverted.
type DetachOptions struct {
	// DropConnections rolls back and disconnects everything using the
	// database first (SET SINGLE_USER WITH ROLLBACK IMMEDIATE). Without it a
	// database with any other connection open refuses to detach. A detach
	// that then fails is put back to MULTI_USER, so a refusal never leaves
	// the database single-user — same contract as RenameDatabaseContext.
	DropConnections bool

	// UpdateStatistics runs UPDATE STATISTICS across the database before
	// detaching, so the statistics survive into whatever attaches it next.
	//
	// The zero value skips it — sp_detach_db's @skipchecks = 'true' — which
	// is both what SSMS's Detach dialog offers unchecked and what a large
	// database wants: the update scans every statistics object in it. Note
	// this is the opposite of what the procedure does when left to its own
	// defaults.
	UpdateStatistics bool

	// DropFullTextIndexFile deletes the full-text index files rather than
	// leaving them beside the data files. Named for what it *does*, not for
	// what it keeps, so that the zero value is sp_detach_db's own default
	// (@keepfulltextindexfile = 'true') and no caller loses a catalog by
	// leaving a field unset.
	DropFullTextIndexFile bool
}

// DetachDatabase detaches the named database from the instance, leaving its
// files on disk.
func (s *Server) DetachDatabase(name string, opts DetachOptions) error {
	return s.DetachDatabaseContext(context.Background(), name, opts)
}

// DetachDatabaseContext is the context-aware variant of DetachDatabase.
//
// The database's files are left where they are — this is not a delete, and
// AttachDatabaseContext brings the same files back, under this name or
// another one.
func (s *Server) DetachDatabaseContext(ctx context.Context, name string, opts DetachOptions) error {
	if name == "" {
		return fmt.Errorf("gosmo: detach database: name is required")
	}
	if opts.DropConnections {
		if err := s.execContext(ctx,
			fmt.Sprintf("ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE", quoteIdent(name)),
		); err != nil {
			return fmt.Errorf("gosmo: set single user on %q: %w", name, err)
		}
	}
	err := s.execContext(ctx, fmt.Sprintf(
		"EXEC master.dbo.sp_detach_db @dbname = %s, @skipchecks = %s, @keepfulltextindexfile = %s",
		nStringLiteral(name),
		sqlTextBool(!opts.UpdateStatistics),
		sqlTextBool(!opts.DropFullTextIndexFile),
	))
	if err != nil {
		// Only on failure: a detach that succeeded has no database left to
		// set anything on, and the ALTER would fail with "not found".
		if opts.DropConnections {
			_ = s.restoreMultiUser(ctx, name)
		}
		return fmt.Errorf("gosmo: detach database %q: %w", name, err)
	}
	return nil
}

// sqlTextBool renders a bool as the 'true'/'false' *text* the sp_detach_db
// family takes — its flags are nvarchar, not bit, and a 0/1 is rejected.
func sqlTextBool(v bool) string {
	if v {
		return "'true'"
	}
	return "'false'"
}

// AttachSpec describes one database to attach.
type AttachSpec struct {
	// Name is what the database is attached as. It need not be the name it
	// was detached under — DetachedDatabase.Name reports that one.
	Name string

	// Files are the physical paths of the database's files on the server's
	// own filesystem, primary data file first. Every file must be listed:
	// SQL Server only finds the others by itself when they sit at the paths
	// recorded inside the primary file, which is exactly what stops being
	// true the moment a database is moved — the case Attach exists for.
	Files []string

	// Owner, if set, is the principal the attached database's ownership is
	// transferred to. Left empty, the database is owned by the login that
	// attached it, which is what CREATE DATABASE ... FOR ATTACH does on its
	// own.
	Owner string

	// RebuildLog attaches without a log file and builds a new one
	// (FOR ATTACH_REBUILD_LOG). It is for a database whose log was lost or
	// deliberately not copied, and it is not free: an unclean database
	// cannot be recovered without its log, so the attach fails rather than
	// silently losing the transactions in it.
	RebuildLog bool
}

// AttachDatabase attaches a set of database files to the instance.
func (s *Server) AttachDatabase(spec AttachSpec) error {
	return s.AttachDatabaseContext(context.Background(), spec)
}

// AttachDatabaseContext is the context-aware variant of AttachDatabase.
func (s *Server) AttachDatabaseContext(ctx context.Context, spec AttachSpec) error {
	if spec.Name == "" {
		return fmt.Errorf("gosmo: attach database: name is required")
	}
	if len(spec.Files) == 0 {
		return fmt.Errorf("gosmo: attach database %q: at least one file is required", spec.Name)
	}
	if err := s.execContext(ctx, buildAttachStatement(spec)); err != nil {
		return fmt.Errorf("gosmo: attach database %q: %w", spec.Name, err)
	}
	if spec.Owner != "" {
		if err := s.execContext(ctx, fmt.Sprintf("ALTER AUTHORIZATION ON DATABASE::%s TO %s",
			quoteIdent(spec.Name), quoteIdent(spec.Owner))); err != nil {
			return fmt.Errorf("gosmo: set owner of attached database %q: %w", spec.Name, err)
		}
	}
	return nil
}

// buildAttachStatement builds the CREATE DATABASE ... FOR ATTACH statement.
// Unexported and side-effect-free so it is unit-testable without a server,
// mirroring buildCreateDatabaseStatement.
func buildAttachStatement(spec AttachSpec) string {
	clauses := make([]string, 0, len(spec.Files))
	for _, f := range spec.Files {
		clauses = append(clauses, fmt.Sprintf("  (FILENAME = %s)", nStringLiteral(f)))
	}
	forClause := "FOR ATTACH"
	if spec.RebuildLog {
		forClause = "FOR ATTACH_REBUILD_LOG"
	}
	return fmt.Sprintf("CREATE DATABASE %s ON\n%s\n%s",
		quoteIdent(spec.Name), strings.Join(clauses, ",\n"), forClause)
}

// DetachedFile is one file of a detached database, as its primary data file
// records it.
type DetachedFile struct {
	FileID int
	// Name is the file's logical name, PhysicalName the path it was detached
	// from — which is where the file is only until someone moves it.
	Name         string
	PhysicalName string
	IsLog        bool
}

// DetachedDatabase is what a detached primary data file says about the
// database it belongs to.
type DetachedDatabase struct {
	// Name is the name the database was detached under. An attach may use a
	// different one; nothing in the files ties them together.
	Name      string
	Version   string
	Collation string
	Files     []*DetachedFile
}

// LogFiles returns the log files of the detached database, and DataFiles the
// rest. Attach's two interesting subsets, so a caller building a file list
// does not re-derive the status bit.
func (d *DetachedDatabase) LogFiles() []*DetachedFile  { return d.filesWhere(true) }
func (d *DetachedDatabase) DataFiles() []*DetachedFile { return d.filesWhere(false) }

// PrimaryFile returns the database's primary data file — the one whose path
// DetachedDatabaseInfo was given, and the only one an attach can relocate
// without the caller naming it.
//
// file_id 1 is the primary data file, always; DBCC CHECKPRIMARYFILE's row
// order is not documented to match it, and the primary is not documented to
// come back first. The fallback to the first data file is for a fileid column
// that came back NULL — better the wrong data file than nil, which reads as
// "this database has no primary".
func (d *DetachedDatabase) PrimaryFile() *DetachedFile {
	for _, f := range d.Files {
		if f.FileID == 1 && !f.IsLog {
			return f
		}
	}
	for _, f := range d.Files {
		if !f.IsLog {
			return f
		}
	}
	return nil
}

func (d *DetachedDatabase) filesWhere(isLog bool) []*DetachedFile {
	var out []*DetachedFile
	for _, f := range d.Files {
		if f.IsLog == isLog {
			out = append(out, f)
		}
	}
	return out
}

// DetachedDatabaseInfo reads a detached database's name and file list out of
// its primary data file.
func (s *Server) DetachedDatabaseInfo(primaryFilePath string) (*DetachedDatabase, error) {
	return s.DetachedDatabaseInfoContext(context.Background(), primaryFilePath)
}

// DetachedDatabaseInfoContext is the context-aware variant of
// DetachedDatabaseInfo. primaryFilePath is a path on the *server's* host, not
// the caller's.
//
// This is what makes an Attach dialog more than a list of paths typed by
// hand: a database's secondary and log files are named only inside its
// primary file, and SQL Server has no documented way to read them back. The
// undocumented DBCC CHECKPRIMARYFILE is what SMO — and therefore SSMS —
// uses, and it needs the rights DBCC needs. A caller that cannot run it can
// still attach: AttachSpec takes the paths directly.
func (s *Server) DetachedDatabaseInfoContext(ctx context.Context, primaryFilePath string) (*DetachedDatabase, error) {
	if strings.TrimSpace(primaryFilePath) == "" {
		return nil, fmt.Errorf("gosmo: detached database info: a primary file path is required")
	}
	d := &DetachedDatabase{}
	if err := s.readDetachedProperties(ctx, primaryFilePath, d); err != nil {
		return nil, err
	}
	if err := s.readDetachedFiles(ctx, primaryFilePath, d); err != nil {
		return nil, err
	}
	return d, nil
}

// detachedFileIsLog is the bit DBCC CHECKPRIMARYFILE's status column marks a
// log file with — 0x40, alongside 0x02 for "in use". Measured against a
// detached three-file database: the two data files come back 2, the log 66.
// The column is undocumented, so this is what it is known to be, not what it
// is specified to be; the fallback below keeps a changed encoding from
// producing a database with no log file at all.
const detachedFileIsLog = 0x40

// readDetachedProperties fills Name/Version/Collation from option 2, whose
// result is one property-name/value row per property rather than one row of
// columns.
func (s *Server) readDetachedProperties(ctx context.Context, path string, d *DetachedDatabase) error {
	rows, err := s.query(ctx, fmt.Sprintf("DBCC CHECKPRIMARYFILE (%s, 2) WITH NO_INFOMSGS", nStringLiteral(path)))
	if err != nil {
		return fmt.Errorf("gosmo: detached database info for %q: %w", path, err)
	}
	defer rows.Close()
	for rows.Next() {
		var property, value sql.NullString
		if err := rows.Scan(&property, &value); err != nil {
			return fmt.Errorf("gosmo: detached database info for %q: %w", path, err)
		}
		switch strings.ToLower(strings.TrimSpace(property.String)) {
		case "database name":
			d.Name = strings.TrimSpace(value.String)
		case "database version":
			d.Version = strings.TrimSpace(value.String)
		case "collation":
			d.Collation = strings.TrimSpace(value.String)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("gosmo: detached database info for %q: %w", path, err)
	}
	return nil
}

// readDetachedFiles fills Files from option 3: one row per file, as
// status/fileid/name/filename.
func (s *Server) readDetachedFiles(ctx context.Context, path string, d *DetachedDatabase) error {
	rows, err := s.query(ctx, fmt.Sprintf("DBCC CHECKPRIMARYFILE (%s, 3) WITH NO_INFOMSGS", nStringLiteral(path)))
	if err != nil {
		return fmt.Errorf("gosmo: detached database files for %q: %w", path, err)
	}
	defer rows.Close()
	for rows.Next() {
		var status, fileID sql.NullInt64
		var name, filename sql.NullString
		if err := rows.Scan(&status, &fileID, &name, &filename); err != nil {
			return fmt.Errorf("gosmo: detached database files for %q: %w", path, err)
		}
		d.Files = append(d.Files, &DetachedFile{
			FileID:       int(fileID.Int64),
			Name:         strings.TrimSpace(name.String),
			PhysicalName: strings.TrimSpace(filename.String),
			IsLog:        status.Int64&detachedFileIsLog != 0,
		})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("gosmo: detached database files for %q: %w", path, err)
	}
	markLogByExtension(d.Files)
	return nil
}

// markLogByExtension is the fallback for the undocumented status bit: if no
// file came back flagged as the log, take the .ldf. A file list with no log
// in it drives a caller straight to FOR ATTACH_REBUILD_LOG — throwing away a
// perfectly good log file — so a status encoding that changed under us must
// not read as "there is no log".
func markLogByExtension(files []*DetachedFile) {
	for _, f := range files {
		if f.IsLog {
			return
		}
	}
	for _, f := range files {
		if strings.EqualFold(pathExt(f.PhysicalName), ".ldf") {
			f.IsLog = true
		}
	}
}

// pathExt is filepath.Ext for a path in the *server's* rules, not the
// caller's: the separator differs, but an extension is the tail after the
// last dot in the last segment either way.
func pathExt(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		p = p[i+1:]
	}
	if i := strings.LastIndex(p, "."); i >= 0 {
		return p[i:]
	}
	return ""
}
