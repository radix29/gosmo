package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
)

// ============================================================
// Database files  (sys.database_files — SSMS's Database Properties >
// Files page)
// ============================================================

// DatabaseFileInfo describes a single database file, including log files —
// unlike FileGroups (database.go), which only sees files
// that belong to a filegroup and so omits the log. Sizes are normalized to
// KB (FileGroups's MaxSize/Growth fields are not, for backward
// compatibility with existing callers).
//
// It is also the handle file writes go through: Alter and Drop address the
// file by Name alone, so Database.FileRef is enough for either.
type DatabaseFileInfo struct {
	db              *Database
	FileID          int
	Name            string
	PhysicalName    string
	Type            string // "ROWS", "LOG", or "FILESTREAM" (sys.database_files.type_desc)
	FileGroup       string // "" for log files, which don't belong to one
	State           string // e.g. "ONLINE"
	SizeKB          int64
	MaxSizeKB       int64 // -1 = unlimited
	GrowthKB        int64 // 0 when IsPercentGrowth is true
	GrowthPercent   int   // 0 when IsPercentGrowth is false
	IsPercentGrowth bool
}

// Database returns the database the file belongs to.
func (f *DatabaseFileInfo) Database() *Database { return f.db }

// IsPrimaryFile reports whether this is the database's primary data file —
// file_id 1, the one holding the database's startup information.
func (f *DatabaseFileInfo) IsPrimaryFile() bool { return f.FileID == 1 }

// FileRef returns a lightweight handle for a database file by logical name,
// without querying the catalog — the counterpart of Server.DatabaseRef.
// Every field but the name stays at its zero value; Files is what populates
// them.
func (d *Database) FileRef(name string) *DatabaseFileInfo {
	return &DatabaseFileInfo{db: d, Name: name}
}

// Files returns every file in the database, data and log alike.
func (d *Database) Files(ctx context.Context) ([]*DatabaseFileInfo, error) {
	const q = `
SELECT df.file_id, df.name, df.physical_name, df.type_desc,
       ISNULL(fg.name, ''), df.state_desc,
       CAST(df.size AS bigint) * 8, df.max_size, df.growth, df.is_percent_growth
FROM   sys.database_files df
LEFT   JOIN sys.filegroups fg ON fg.data_space_id = df.data_space_id
ORDER  BY df.type_desc, df.file_id`

	rows, err := d.query(ctx, q)
	return scanRows(rows, err, fmt.Sprintf("list files in %q", d.Name), func(scan func(...any) error) (*DatabaseFileInfo, error) {
		f := &DatabaseFileInfo{db: d}
		var maxSizePages, growthRaw int64
		if err := scan(&f.FileID, &f.Name, &f.PhysicalName, &f.Type, &f.FileGroup, &f.State,
			&f.SizeKB, &maxSizePages, &growthRaw, &f.IsPercentGrowth); err != nil {
			return nil, err
		}
		f.MaxSizeKB, f.GrowthKB, f.GrowthPercent = normalizeFileGrowth(maxSizePages, growthRaw, f.IsPercentGrowth)
		return f, nil
	})
}

// DatabaseFiles returns one database's files read from the server-wide
// catalog, so it answers for a database in any state.
//
// It reads sys.master_files rather than sys.database_files, which is the whole
// point of it: Database.Files runs its read through a USE, and a
// database that is OFFLINE, RECOVERY_PENDING or SUSPECT refuses the USE — so
// the paths become unreadable in exactly the states someone needs them in,
// such as on the way to a detach. FileGroup is always "" here: sys.filegroups
// is database-scoped and cannot be joined from the server catalog.
//
// A database the login cannot see reads as no rows rather than an error, the
// way metadata visibility answers everywhere else.
func (s *Server) DatabaseFiles(ctx context.Context, database string) ([]*DatabaseFileInfo, error) {
	const q = `
SELECT mf.file_id, mf.name, mf.physical_name, mf.type_desc, mf.state_desc,
       CAST(mf.size AS bigint) * 8, mf.max_size, mf.growth, mf.is_percent_growth
FROM   sys.master_files mf
WHERE  mf.database_id = DB_ID(@p1)
ORDER  BY mf.type_desc, mf.file_id`

	dbRef := s.DatabaseRef(database)
	rows, err := s.query(ctx, q, database)
	return scanRows(rows, err, fmt.Sprintf("list files in %q", database), func(scan func(...any) error) (*DatabaseFileInfo, error) {
		f := &DatabaseFileInfo{db: dbRef}
		var maxSizePages, growthRaw int64
		if err := scan(&f.FileID, &f.Name, &f.PhysicalName, &f.Type, &f.State,
			&f.SizeKB, &maxSizePages, &growthRaw, &f.IsPercentGrowth); err != nil {
			return nil, err
		}
		f.MaxSizeKB, f.GrowthKB, f.GrowthPercent = normalizeFileGrowth(maxSizePages, growthRaw, f.IsPercentGrowth)
		return f, nil
	})
}

// DatabaseFileSpec describes a file to add via AddFile, or one of CREATE
// DATABASE's file definitions.
type DatabaseFileSpec struct {
	Name      string
	FileGroup string // ignored when Type is "LOG"
	Type      string // "LOG" adds a log file; anything else (including "") adds a data file
	Path      string
	SizeKB    int64
	// GrowthKB and GrowthPercent are mutually exclusive; GrowthPercent
	// wins if both are set. Leaving both zero omits FILEGROWTH (server
	// default).
	GrowthKB      int64
	GrowthPercent int
	// DisableGrowth creates the file with autogrowth off (FILEGROWTH = 0),
	// and takes precedence over GrowthKB/GrowthPercent. It exists because
	// zero cannot say it: leaving both growth fields at zero means "omit
	// FILEGROWTH, take the server default", which is the opposite of
	// switching growth off.
	DisableGrowth bool
	// MaxSizeKB: 0 omits MAXSIZE (server default), -1 means UNLIMITED,
	// >0 is the cap in KB.
	MaxSizeKB int64
}

// AddFile adds a new data or log file to the database.
func (d *Database) AddFile(ctx context.Context, spec DatabaseFileSpec) error {
	stmt, err := buildAddFileStatement(d.Name, spec)
	if err != nil {
		return err
	}
	if err := d.server.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: add file %q to %q: %w", spec.Name, d.Name, err)
	}
	return nil
}

// writeFileSizeClauses appends the SIZE/MAXSIZE/FILEGROWTH portions of spec
// to sb, in the order CREATE DATABASE and ALTER DATABASE ADD/MODIFY FILE
// expect them, omitting any spec leaves at its zero value (server default).
// Shared by buildAddFileStatement and buildFileDefClause so the two file-
// definition syntaxes (ALTER DATABASE ADD FILE's flat clause list and
// CREATE DATABASE's parenthesized ON PRIMARY/LOG ON file definitions)
// never drift apart.
func writeFileSizeClauses(sb *strings.Builder, spec DatabaseFileSpec) {
	if spec.SizeKB > 0 {
		fmt.Fprintf(sb, ", SIZE = %s", fileSizeLiteral(spec.SizeKB))
	}
	switch {
	case spec.MaxSizeKB < 0:
		sb.WriteString(", MAXSIZE = UNLIMITED")
	case spec.MaxSizeKB > 0:
		fmt.Fprintf(sb, ", MAXSIZE = %s", fileSizeLiteral(spec.MaxSizeKB))
	}
	switch {
	case spec.DisableGrowth:
		sb.WriteString(", FILEGROWTH = 0")
	case spec.GrowthPercent > 0:
		fmt.Fprintf(sb, ", FILEGROWTH = %d%%", spec.GrowthPercent)
	case spec.GrowthKB > 0:
		fmt.Fprintf(sb, ", FILEGROWTH = %s", fileSizeLiteral(spec.GrowthKB))
	}
}

// fileSizeLiteral writes a size in KB, or in MB from 2 TB up: the number is
// parsed as an int, so a log's default MAXSIZE of 2 TB as 2147483648KB is
// Msg 102. Above that a size not a whole number of MB is rounded up — the
// server rounds to its 8 KB pages anyway, and only past 2 TB does it arise.
func fileSizeLiteral(kb int64) string {
	if kb <= math.MaxInt32 {
		return fmt.Sprintf("%dKB", kb)
	}
	return fmt.Sprintf("%dMB", (kb+1023)/1024)
}

// buildFileDefClause renders a "( NAME = ..., FILENAME = ..., ... )" file
// definition, as used inside CREATE DATABASE's ON PRIMARY/LOG ON clauses.
func buildFileDefClause(spec DatabaseFileSpec) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "( NAME = %s, FILENAME = %s", quoteIdent(spec.Name), QuoteLiteral(spec.Path))
	writeFileSizeClauses(&sb, spec)
	sb.WriteString(" )")
	return sb.String()
}

// buildAddFileStatement builds the ALTER DATABASE ... ADD FILE statement
// for spec. Unexported and side-effect-free so it's unit-testable without
// a server.
func buildAddFileStatement(dbName string, spec DatabaseFileSpec) (string, error) {
	if spec.Name == "" {
		return "", invalidf("gosmo: add file: name is required")
	}
	if spec.Path == "" {
		return "", invalidf("gosmo: add file: path is required")
	}
	isLog := spec.Type == "LOG"
	clause := "ADD FILE"
	if isLog {
		clause = "ADD LOG FILE"
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "ALTER DATABASE %s %s (NAME = %s, FILENAME = %s",
		quoteIdent(dbName), clause, quoteIdent(spec.Name), QuoteLiteral(spec.Path))
	writeFileSizeClauses(&sb, spec)
	sb.WriteString(")")
	if spec.FileGroup != "" && !isLog {
		fmt.Fprintf(&sb, " TO FILEGROUP %s", quoteIdent(spec.FileGroup))
	}
	return sb.String(), nil
}

// FileModify holds the fields to change on an existing file via
// DatabaseFileInfo.Alter.
// Zero-valued fields are left unchanged; NewName renames the file.
type FileModify struct {
	NewName       string
	SizeKB        int64
	GrowthKB      int64
	GrowthPercent int
	// DisableGrowth turns autogrowth off (FILEGROWTH = 0), and takes
	// precedence over GrowthKB/GrowthPercent.
	//
	// It is a separate field because this struct's zero value means "leave
	// this property alone", so GrowthKB = 0 cannot ask for FILEGROWTH = 0 —
	// the one that omits the clause and the one that disables growth are
	// the same value. Without it a UI whose growth control bottoms out at
	// zero produces an ALTER with no FILEGROWTH clause, and if nothing else
	// on the file changed, buildAlterFileStatement returns "" and
	// Alter returns nil: an Apply that reports success and did
	// nothing.
	DisableGrowth bool
	MaxSizeKB     int64 // -1 = UNLIMITED
}

// Alter changes the file's name, size, growth, or max size. A FileModify
// that changes nothing issues no statement.
func (f *DatabaseFileInfo) Alter(ctx context.Context, m FileModify) error {
	stmt, err := buildAlterFileStatement(f.db.Name, f.Name, m)
	if err != nil {
		return err
	}
	if stmt == "" {
		return nil
	}
	if err := f.db.server.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: alter file %q in %q: %w", f.Name, f.db.Name, err)
	}
	if m.NewName != "" {
		setIfApplied(ctx, &f.Name, m.NewName)
	}
	return nil
}

// buildAlterFileStatement builds the ALTER DATABASE ... MODIFY FILE
// statement for the given changes, or "" if m carries no actual change.
func buildAlterFileStatement(dbName, name string, m FileModify) (string, error) {
	if name == "" {
		return "", invalidf("gosmo: alter file: name is required")
	}
	props := []string{"NAME = " + quoteIdent(name)}
	if m.NewName != "" {
		props = append(props, "NEWNAME = "+quoteIdent(m.NewName))
	}
	if m.SizeKB > 0 {
		props = append(props, fmt.Sprintf("SIZE = %dKB", m.SizeKB))
	}
	switch {
	case m.MaxSizeKB < 0:
		props = append(props, "MAXSIZE = UNLIMITED")
	case m.MaxSizeKB > 0:
		props = append(props, fmt.Sprintf("MAXSIZE = %dKB", m.MaxSizeKB))
	}
	switch {
	case m.DisableGrowth:
		props = append(props, "FILEGROWTH = 0")
	case m.GrowthPercent > 0:
		props = append(props, fmt.Sprintf("FILEGROWTH = %d%%", m.GrowthPercent))
	case m.GrowthKB > 0:
		props = append(props, fmt.Sprintf("FILEGROWTH = %dKB", m.GrowthKB))
	}
	if len(props) == 1 {
		return "", nil // only the identifying NAME — nothing to change
	}
	return fmt.Sprintf("ALTER DATABASE %s MODIFY FILE (%s)", quoteIdent(dbName), strings.Join(props, ", ")), nil
}

// Drop removes the file from its database (ALTER DATABASE ... REMOVE FILE).
// The file must be empty (0 bytes of used space) — SQL Server itself
// enforces this, not gosmo.
func (f *DatabaseFileInfo) Drop(ctx context.Context) error {
	q := fmt.Sprintf("ALTER DATABASE %s REMOVE FILE %s", quoteIdent(f.db.Name), quoteIdent(f.Name))
	if err := f.db.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: remove file %q from %q: %w", f.Name, f.db.Name, err)
	}
	return nil
}

// -- Filegroups ----------------------------------------------------------------

// FileGroupRef returns a lightweight handle for a filegroup by name, without
// querying the catalog — the counterpart of Server.DatabaseRef. Every field
// but the name stays at its zero value; FileGroups is what populates them.
// Every write on *FileGroup addresses it by name, so this handle is enough
// for any of them — including on a filegroup AddFileGroup only scripted.
func (d *Database) FileGroupRef(name string) *FileGroup {
	return &FileGroup{db: d, Name: name}
}

// FileGroups returns all filegroups and their files, including a filegroup
// with no files (its Files is empty) — the state ALTER DATABASE ADD FILEGROUP
// leaves one in, and the only state REMOVE FILEGROUP accepts.
func (d *Database) FileGroups(ctx context.Context) ([]*FileGroup, error) {
	const q = `
SELECT fg.name, fg.type_desc, fg.is_default, fg.is_read_only,
       df.file_id, df.name, df.physical_name, df.type_desc, df.state_desc,
       CAST(df.size AS bigint) * 8, df.max_size, df.growth, df.is_percent_growth
FROM   sys.filegroups fg
LEFT   JOIN sys.database_files df ON df.data_space_id = fg.data_space_id
ORDER  BY fg.name, df.file_id`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list filegroups: %w", err)
	}
	defer rows.Close()

	fgMap := make(map[string]*FileGroup)
	var order []string
	for rows.Next() {
		var fgName, fgType string
		var fgDefault, fgReadOnly bool
		// The file columns are NULL on the one row an empty filegroup gets
		// from the LEFT JOIN.
		var fID sql.NullInt64
		var fName, fPath, fType, fState sql.NullString
		var fSize, fMaxSize, fGrowth sql.NullInt64
		var isPctGrowth sql.NullBool
		if err := rows.Scan(&fgName, &fgType, &fgDefault, &fgReadOnly,
			&fID, &fName, &fPath, &fType, &fState, &fSize, &fMaxSize, &fGrowth,
			&isPctGrowth); err != nil {
			return nil, fmt.Errorf("gosmo: list filegroups: %w", err)
		}

		fg, ok := fgMap[fgName]
		if !ok {
			fg = &FileGroup{db: d, Name: fgName, Type: fgType, IsDefault: fgDefault, IsReadOnly: fgReadOnly}
			fgMap[fgName] = fg
			order = append(order, fgName)
		}
		if !fName.Valid {
			continue
		}
		f := &DatabaseFileInfo{
			db: d, FileID: int(fID.Int64), Name: fName.String, PhysicalName: fPath.String,
			Type: fType.String, FileGroup: fgName, State: fState.String,
			SizeKB: fSize.Int64, IsPercentGrowth: isPctGrowth.Bool,
		}
		// max_size and growth are 8 KB pages (growth a percentage when
		// is_percent_growth); normalizeFileGrowth turns them into KB, as
		// Files does.
		f.MaxSizeKB, f.GrowthKB, f.GrowthPercent = normalizeFileGrowth(fMaxSize.Int64, fGrowth.Int64, isPctGrowth.Bool)
		fg.Files = append(fg.Files, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list filegroups: %w", err)
	}

	fgs := make([]*FileGroup, 0, len(order))
	for _, n := range order {
		fgs = append(fgs, fgMap[n])
	}
	return fgs, nil
}

// AddFileGroup adds a new (empty) filegroup to the database.
func (d *Database) AddFileGroup(ctx context.Context, name string) error {
	q := fmt.Sprintf("ALTER DATABASE %s ADD FILEGROUP %s", quoteIdent(d.Name), quoteIdent(name))
	if err := d.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: add filegroup %q to %q: %w", name, d.Name, err)
	}
	return nil
}

// Drop removes the filegroup from its database (ALTER DATABASE ... REMOVE
// FILEGROUP). It must be empty (no files) — SQL Server itself enforces this,
// not gosmo.
func (fg *FileGroup) Drop(ctx context.Context) error {
	d := fg.db
	q := fmt.Sprintf("ALTER DATABASE %s REMOVE FILEGROUP %s", quoteIdent(d.Name), quoteIdent(fg.Name))
	if err := d.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: remove filegroup %q from %q: %w", fg.Name, d.Name, err)
	}
	return nil
}

// SetDefault makes the filegroup its database's default (for its filegroup
// type — see FileGroup.IsDefault).
func (fg *FileGroup) SetDefault(ctx context.Context) error {
	d := fg.db
	q := fmt.Sprintf("ALTER DATABASE %s MODIFY FILEGROUP %s DEFAULT", quoteIdent(d.Name), quoteIdent(fg.Name))
	if err := d.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set default filegroup %q on %q: %w", fg.Name, d.Name, err)
	}
	setIfApplied(ctx, &fg.IsDefault, true)
	return nil
}

// SetReadOnly sets or clears the filegroup's read-only flag.
//
// The keywords are the underscored spellings on purpose. ALTER DATABASE also
// accepts READONLY/READWRITE, but only for backward compatibility — SQL
// Server documents that pair as deprecated and slated for removal, and it is
// the spelling this used to emit.
//
// The change needs exclusive access to the database. This Server's own idle
// sessions are released first (see Server.ReleaseIdleConnections); anyone
// else's are what term decides. MODIFY FILEGROUP parses a WITH ROLLBACK
// IMMEDIATE and ignores it — probed on 17.0: it waited ~20 s behind an open
// transaction and failed Msg 5070 all the same — so
// TerminationRollbackImmediate kills the database's sessions in the same
// batch instead, the form a Managed Instance's forced drop uses. That needs
// ALTER ANY CONNECTION where the ROLLBACK IMMEDIATE of a SET option needs
// only ALTER on the database.
func (fg *FileGroup) SetReadOnly(ctx context.Context, readOnly bool, term Termination) error {
	d, name := fg.db, fg.Name
	mode := "READ_WRITE"
	if readOnly {
		mode = "READ_ONLY"
	}
	if _, err := term.withClause(); err != nil {
		return fmt.Errorf("gosmo: set filegroup %q read-only=%v on %q: %w", name, readOnly, d.Name, err)
	}
	q := fmt.Sprintf("ALTER DATABASE %s MODIFY FILEGROUP %s %s", quoteIdent(d.Name), quoteIdent(name), mode)
	if term == TerminationRollbackImmediate {
		q = killDatabaseSessionsBatch(d.Name) + ";\n" + q + ";"
	}
	d.server.releaseIdle(ctx)
	if err := d.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set filegroup %q read-only=%v on %q: %w", name, readOnly, d.Name, err)
	}
	setIfApplied(ctx, &fg.IsReadOnly, readOnly)
	return nil
}

// normalizeFileGrowth converts sys.database_files' raw max_size/growth
// columns (8KB pages, or a percentage when isPercentGrowth) into the KB/
// percent units DatabaseFileInfo exposes. maxSizePages preserves the -1
// "unlimited" sentinel rather than multiplying it into nonsense.
func normalizeFileGrowth(maxSizePages, growthRaw int64, isPercentGrowth bool) (maxSizeKB, growthKB int64, growthPercent int) {
	if maxSizePages < 0 {
		maxSizeKB = -1
	} else {
		maxSizeKB = maxSizePages * 8
	}
	if isPercentGrowth {
		growthPercent = int(growthRaw)
	} else {
		growthKB = growthRaw * 8
	}
	return maxSizeKB, growthKB, growthPercent
}
