package gosmo

import (
	"context"
	"fmt"
	"strings"
)

// ============================================================
// Database files  (sys.database_files — SSMS's Database Properties >
// Files page)
// ============================================================

// DatabaseFileInfo describes a single database file, including log files —
// unlike FileGroups/FileGroupsContext (database.go), which only sees files
// that belong to a filegroup and so omits the log. Sizes are normalized to
// KB (FileGroupsContext's MaxSize/Growth fields are not, for backward
// compatibility with existing callers).
type DatabaseFileInfo struct {
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

// Files returns every file in the database, data and log alike.
func (d *Database) Files() ([]*DatabaseFileInfo, error) {
	return d.FilesContext(context.Background())
}

// FilesContext is the context-aware variant of Files.
func (d *Database) FilesContext(ctx context.Context) ([]*DatabaseFileInfo, error) {
	const q = `
SELECT df.file_id, df.name, df.physical_name, df.type_desc,
       ISNULL(fg.name, ''), df.state_desc,
       df.size * 8, df.max_size, df.growth, df.is_percent_growth
FROM   sys.database_files df
LEFT   JOIN sys.filegroups fg ON fg.data_space_id = df.data_space_id
ORDER  BY df.type_desc, df.file_id`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list files in %q: %w", d.name, err)
	}
	defer rows.Close()

	var files []*DatabaseFileInfo
	for rows.Next() {
		f := &DatabaseFileInfo{}
		var maxSizePages, growthRaw int64
		if err := rows.Scan(&f.FileID, &f.Name, &f.PhysicalName, &f.Type, &f.FileGroup, &f.State,
			&f.SizeKB, &maxSizePages, &growthRaw, &f.IsPercentGrowth); err != nil {
			return nil, err
		}
		f.MaxSizeKB, f.GrowthKB, f.GrowthPercent = normalizeFileGrowth(maxSizePages, growthRaw, f.IsPercentGrowth)
		files = append(files, f)
	}
	return files, rows.Err()
}

// DatabaseFileSpec describes a file to add via AddFile.
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
	// MaxSizeKB: 0 omits MAXSIZE (server default), -1 means UNLIMITED,
	// >0 is the cap in KB.
	MaxSizeKB int64
}

// AddFile adds a new data or log file to the database.
func (d *Database) AddFile(spec DatabaseFileSpec) error {
	return d.AddFileContext(context.Background(), spec)
}

// AddFileContext is the context-aware variant of AddFile.
func (d *Database) AddFileContext(ctx context.Context, spec DatabaseFileSpec) error {
	stmt, err := buildAddFileStatement(d.name, spec)
	if err != nil {
		return err
	}
	if err := d.server.execContext(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: add file %q to %q: %w", spec.Name, d.name, err)
	}
	return nil
}

// buildAddFileStatement builds the ALTER DATABASE ... ADD FILE statement
// for spec. Unexported and side-effect-free so it's unit-testable without
// a server.
func buildAddFileStatement(dbName string, spec DatabaseFileSpec) (string, error) {
	if spec.Name == "" {
		return "", fmt.Errorf("gosmo: add file: name is required")
	}
	if spec.Path == "" {
		return "", fmt.Errorf("gosmo: add file: path is required")
	}
	isLog := spec.Type == "LOG"
	clause := "ADD FILE"
	if isLog {
		clause = "ADD LOG FILE"
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "ALTER DATABASE %s %s (NAME = %s, FILENAME = %s",
		quoteIdent(dbName), clause, quoteIdent(spec.Name), QuoteLiteral(spec.Path))
	if spec.SizeKB > 0 {
		fmt.Fprintf(&sb, ", SIZE = %dKB", spec.SizeKB)
	}
	switch {
	case spec.MaxSizeKB < 0:
		sb.WriteString(", MAXSIZE = UNLIMITED")
	case spec.MaxSizeKB > 0:
		fmt.Fprintf(&sb, ", MAXSIZE = %dKB", spec.MaxSizeKB)
	}
	switch {
	case spec.GrowthPercent > 0:
		fmt.Fprintf(&sb, ", FILEGROWTH = %d%%", spec.GrowthPercent)
	case spec.GrowthKB > 0:
		fmt.Fprintf(&sb, ", FILEGROWTH = %dKB", spec.GrowthKB)
	}
	sb.WriteString(")")
	if spec.FileGroup != "" && !isLog {
		fmt.Fprintf(&sb, " TO FILEGROUP %s", quoteIdent(spec.FileGroup))
	}
	return sb.String(), nil
}

// FileModify holds the fields to change on an existing file via AlterFile.
// Zero-valued fields are left unchanged; NewName renames the file.
type FileModify struct {
	NewName       string
	SizeKB        int64
	GrowthKB      int64
	GrowthPercent int
	MaxSizeKB     int64 // -1 = UNLIMITED
}

// AlterFile changes an existing file's name, size, growth, or max size.
func (d *Database) AlterFile(name string, m FileModify) error {
	return d.AlterFileContext(context.Background(), name, m)
}

// AlterFileContext is the context-aware variant of AlterFile.
func (d *Database) AlterFileContext(ctx context.Context, name string, m FileModify) error {
	stmt, err := buildAlterFileStatement(d.name, name, m)
	if err != nil {
		return err
	}
	if stmt == "" {
		return nil
	}
	if err := d.server.execContext(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: alter file %q in %q: %w", name, d.name, err)
	}
	return nil
}

// buildAlterFileStatement builds the ALTER DATABASE ... MODIFY FILE
// statement for the given changes, or "" if m carries no actual change.
func buildAlterFileStatement(dbName, name string, m FileModify) (string, error) {
	if name == "" {
		return "", fmt.Errorf("gosmo: alter file: name is required")
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

// RemoveFile drops a file from the database. The file must be empty (0
// bytes of used space) — SQL Server itself enforces this, not gosmo.
func (d *Database) RemoveFile(name string) error {
	return d.RemoveFileContext(context.Background(), name)
}

// RemoveFileContext is the context-aware variant of RemoveFile.
func (d *Database) RemoveFileContext(ctx context.Context, name string) error {
	q := fmt.Sprintf("ALTER DATABASE %s REMOVE FILE %s", quoteIdent(d.name), quoteIdent(name))
	if err := d.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: remove file %q from %q: %w", name, d.name, err)
	}
	return nil
}

// AddFileGroup adds a new (empty) filegroup to the database.
func (d *Database) AddFileGroup(name string) error {
	return d.AddFileGroupContext(context.Background(), name)
}

// AddFileGroupContext is the context-aware variant of AddFileGroup.
func (d *Database) AddFileGroupContext(ctx context.Context, name string) error {
	q := fmt.Sprintf("ALTER DATABASE %s ADD FILEGROUP %s", quoteIdent(d.name), quoteIdent(name))
	if err := d.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: add filegroup %q to %q: %w", name, d.name, err)
	}
	return nil
}

// RemoveFileGroup drops a filegroup. It must be empty (no files) — SQL
// Server itself enforces this, not gosmo.
func (d *Database) RemoveFileGroup(name string) error {
	return d.RemoveFileGroupContext(context.Background(), name)
}

// RemoveFileGroupContext is the context-aware variant of RemoveFileGroup.
func (d *Database) RemoveFileGroupContext(ctx context.Context, name string) error {
	q := fmt.Sprintf("ALTER DATABASE %s REMOVE FILEGROUP %s", quoteIdent(d.name), quoteIdent(name))
	if err := d.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: remove filegroup %q from %q: %w", name, d.name, err)
	}
	return nil
}

// SetDefaultFileGroup marks a filegroup as the database's default.
func (d *Database) SetDefaultFileGroup(name string) error {
	return d.SetDefaultFileGroupContext(context.Background(), name)
}

// SetDefaultFileGroupContext is the context-aware variant of SetDefaultFileGroup.
func (d *Database) SetDefaultFileGroupContext(ctx context.Context, name string) error {
	q := fmt.Sprintf("ALTER DATABASE %s MODIFY FILEGROUP %s DEFAULT", quoteIdent(d.name), quoteIdent(name))
	if err := d.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set default filegroup %q on %q: %w", name, d.name, err)
	}
	return nil
}

// SetFileGroupReadOnly sets or clears a filegroup's read-only flag.
func (d *Database) SetFileGroupReadOnly(name string, readOnly bool) error {
	return d.SetFileGroupReadOnlyContext(context.Background(), name, readOnly)
}

// SetFileGroupReadOnlyContext is the context-aware variant of SetFileGroupReadOnly.
func (d *Database) SetFileGroupReadOnlyContext(ctx context.Context, name string, readOnly bool) error {
	mode := "READWRITE"
	if readOnly {
		mode = "READONLY"
	}
	q := fmt.Sprintf("ALTER DATABASE %s MODIFY FILEGROUP %s %s", quoteIdent(d.name), quoteIdent(name), mode)
	if err := d.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set filegroup %q read-only=%v on %q: %w", name, readOnly, d.name, err)
	}
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
