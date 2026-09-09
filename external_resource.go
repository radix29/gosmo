package gosmo

// External resources — the PolyBase and elastic-query objects SSMS groups
// under a database's External Resources folder: external data sources,
// external file formats and external libraries.
//
// Version exposure differs per family, and the three cases are not the same
// shape:
//
//   - sys.external_data_sources and sys.external_file_formats exist on every
//     supported major (13 and later). Two columns on each are later, and are
//     gated per column with colSince.
//   - sys.external_libraries is SQL Server 2017. The whole view is missing on
//     13, which no per-column gate can paper over, so the read is refused
//     with ErrUnsupportedVersion before a statement is sent — the same shape
//     the Query Store wait reads use.
//
// The gates were checked against real instances (majors 13, 14 and 17) rather
// than taken from the documentation: sys.external_file_formats.first_row is
// documented as 2017 and is absent on a 14.0.2130.4 instance, so it is gated
// at 2019 here, where it actually appears.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ============================================================
// External data sources
// ============================================================

// ExternalDataSource mirrors a sys.external_data_sources row.
type ExternalDataSource struct {
	db *Database

	Name         string
	DataSourceID int

	// Location is the connection string of the remote source — an
	// hdfs://, sqlserver://, abfss:// or similar URL depending on Type.
	Location string

	// Type is the source kind, as type_desc reports it: HADOOP,
	// RDBMS, SHARD_MAP_MANAGER, BLOB_STORAGE, EXTERNAL_GENERICS.
	Type string

	// ResourceManagerLocation is the Hadoop resource manager endpoint for a
	// HADOOP source, empty otherwise.
	ResourceManagerLocation string

	// Credential is the database-scoped credential the source authenticates
	// with, empty when it uses none.
	Credential string

	// DatabaseName and ShardMapName are the remote database and shard map
	// for an elastic-query source, empty for the others.
	DatabaseName string
	ShardMapName string

	// ConnectionOptions and PushdownEnabled are SQL Server 2019 and later;
	// on an older instance ConnectionOptions is empty and PushdownEnabled
	// reads as false, rather than the read failing.
	ConnectionOptions string
	PushdownEnabled   bool
}

// Database returns the database the data source belongs to.
func (s *ExternalDataSource) Database() *Database { return s.db }

// externalDataSourceSelect builds the SELECT both the listing and the by-name
// finder use, with the 2019 columns substituted out on an older instance.
//
// pushdown is nvarchar ('ON'/'OFF'), not a bit, so the zero literal is a
// string and the comparison happens in SQL rather than in the scan.
func (d *Database) externalDataSourceSelect() string {
	major := d.serverMajorVersion()
	return `
SELECT s.name, s.data_source_id, s.location,
       ISNULL(s.type_desc, ''),
       ISNULL(s.resource_manager_location, ''),
       ISNULL((SELECT c.name FROM sys.database_scoped_credentials c
               WHERE c.credential_id = s.credential_id), ''),
       ISNULL(s.database_name, ''), ISNULL(s.shard_map_name, ''),
       ISNULL(` + colSince(major, SQLServer2019, "s.connection_options", "CAST('' AS nvarchar(4000))") + `, ''),
       CASE WHEN ` + colSince(major, SQLServer2019, "s.pushdown", "CAST('' AS nvarchar(4))") + ` = 'ON'
            THEN CAST(1 AS bit) ELSE CAST(0 AS bit) END
FROM   sys.external_data_sources s`
}

func scanExternalDataSource(d *Database, scan func(...any) error) (*ExternalDataSource, error) {
	s := &ExternalDataSource{db: d}
	if err := scan(&s.Name, &s.DataSourceID, &s.Location,
		&s.Type, &s.ResourceManagerLocation, &s.Credential,
		&s.DatabaseName, &s.ShardMapName,
		&s.ConnectionOptions, &s.PushdownEnabled); err != nil {
		return nil, err
	}
	return s, nil
}

// ExternalDataSources returns the external data sources defined in the
// database.
func (d *Database) ExternalDataSources() ([]*ExternalDataSource, error) {
	return d.ExternalDataSourcesContext(context.Background())
}

// ExternalDataSourcesContext is the context-aware variant of
// ExternalDataSources.
func (d *Database) ExternalDataSourcesContext(ctx context.Context) ([]*ExternalDataSource, error) {
	q := d.externalDataSourceSelect() + `
ORDER  BY s.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list external data sources in %q: %w", d.name, err)
	}
	defer rows.Close()

	var sources []*ExternalDataSource
	for rows.Next() {
		s, err := scanExternalDataSource(d, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list external data sources in %q: %w", d.name, err)
		}
		sources = append(sources, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list external data sources in %q: %w", d.name, err)
	}
	return sources, nil
}

// ExternalDataSourceByName returns one external data source, or a not-found
// error (errors.Is ErrNotFound) when the database has none by that name.
func (d *Database) ExternalDataSourceByName(name string) (*ExternalDataSource, error) {
	return d.ExternalDataSourceByNameContext(context.Background(), name)
}

// ExternalDataSourceByNameContext is the context-aware variant of
// ExternalDataSourceByName.
func (d *Database) ExternalDataSourceByNameContext(ctx context.Context, name string) (*ExternalDataSource, error) {
	var s *ExternalDataSource
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		s, err = scanExternalDataSource(d, row.Scan)
		return err
	}, d.externalDataSourceSelect()+`
WHERE  s.name = @p1`, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: external data source %q not found in %q", name, d.name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read external data source %q in %q: %w", name, d.name, err)
	}
	return s, nil
}

// DropExternalDataSource drops an external data source by name. One still
// referenced by an external table is refused by the server.
func (d *Database) DropExternalDataSource(name string) error {
	return d.DropExternalDataSourceContext(context.Background(), name)
}

// DropExternalDataSourceContext is the context-aware variant of
// DropExternalDataSource.
func (d *Database) DropExternalDataSourceContext(ctx context.Context, name string) error {
	if _, err := d.exec(ctx, "DROP EXTERNAL DATA SOURCE "+QuoteName(name)); err != nil {
		return fmt.Errorf("gosmo: drop external data source %q in %q: %w", name, d.name, err)
	}
	return nil
}

// Drop drops the external data source.
func (s *ExternalDataSource) Drop() error { return s.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (s *ExternalDataSource) DropContext(ctx context.Context) error {
	return s.db.DropExternalDataSourceContext(ctx, s.Name)
}

// ============================================================
// External file formats
// ============================================================

// ExternalFileFormat mirrors a sys.external_file_formats row.
//
// Most fields apply to one format type only — the delimited-text fields are
// empty on a PARQUET format, and SerDeMethod is set only on a HIVE RCFILE —
// so read FormatType first.
type ExternalFileFormat struct {
	db *Database

	Name         string
	FileFormatID int

	// FormatType is DELIMITEDTEXT, RCFILE, ORC, PARQUET, JSON or DELTA.
	FormatType string

	FieldTerminator string
	StringDelimiter string
	DateFormat      string
	UseTypeDefault  bool
	SerDeMethod     string
	RowTerminator   string
	Encoding        string
	DataCompression string

	// FirstRow and ParserVersion are SQL Server 2019 and later; on an older
	// instance FirstRow is 0 and ParserVersion empty rather than the read
	// failing.
	FirstRow      int
	ParserVersion string
}

// Database returns the database the file format belongs to.
func (f *ExternalFileFormat) Database() *Database { return f.db }

func (d *Database) externalFileFormatSelect() string {
	major := d.serverMajorVersion()
	return `
SELECT f.file_format_id, f.name, f.format_type,
       ISNULL(f.field_terminator, ''), ISNULL(f.string_delimiter, ''),
       ISNULL(f.date_format, ''), ISNULL(f.use_type_default, 0),
       ISNULL(f.serde_method, ''), ISNULL(f.row_terminator, ''),
       ISNULL(f.encoding, ''), ISNULL(f.data_compression, ''),
       ISNULL(` + colSince(major, SQLServer2019, "f.first_row", "CAST(0 AS int)") + `, 0),
       ISNULL(` + colSince(major, SQLServer2019, "f.parser_version", "CAST('' AS nvarchar(10))") + `, '')
FROM   sys.external_file_formats f`
}

func scanExternalFileFormat(d *Database, scan func(...any) error) (*ExternalFileFormat, error) {
	f := &ExternalFileFormat{db: d}
	if err := scan(&f.FileFormatID, &f.Name, &f.FormatType,
		&f.FieldTerminator, &f.StringDelimiter, &f.DateFormat,
		&f.UseTypeDefault, &f.SerDeMethod, &f.RowTerminator,
		&f.Encoding, &f.DataCompression,
		&f.FirstRow, &f.ParserVersion); err != nil {
		return nil, err
	}
	return f, nil
}

// ExternalFileFormats returns the external file formats defined in the
// database.
func (d *Database) ExternalFileFormats() ([]*ExternalFileFormat, error) {
	return d.ExternalFileFormatsContext(context.Background())
}

// ExternalFileFormatsContext is the context-aware variant of
// ExternalFileFormats.
func (d *Database) ExternalFileFormatsContext(ctx context.Context) ([]*ExternalFileFormat, error) {
	q := d.externalFileFormatSelect() + `
ORDER  BY f.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list external file formats in %q: %w", d.name, err)
	}
	defer rows.Close()

	var formats []*ExternalFileFormat
	for rows.Next() {
		f, err := scanExternalFileFormat(d, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list external file formats in %q: %w", d.name, err)
		}
		formats = append(formats, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list external file formats in %q: %w", d.name, err)
	}
	return formats, nil
}

// ExternalFileFormatByName returns one external file format, or a not-found
// error (errors.Is ErrNotFound) when the database has none by that name.
func (d *Database) ExternalFileFormatByName(name string) (*ExternalFileFormat, error) {
	return d.ExternalFileFormatByNameContext(context.Background(), name)
}

// ExternalFileFormatByNameContext is the context-aware variant of
// ExternalFileFormatByName.
func (d *Database) ExternalFileFormatByNameContext(ctx context.Context, name string) (*ExternalFileFormat, error) {
	var f *ExternalFileFormat
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		f, err = scanExternalFileFormat(d, row.Scan)
		return err
	}, d.externalFileFormatSelect()+`
WHERE  f.name = @p1`, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: external file format %q not found in %q", name, d.name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read external file format %q in %q: %w", name, d.name, err)
	}
	return f, nil
}

// DropExternalFileFormat drops an external file format by name. One still
// referenced by an external table is refused by the server.
func (d *Database) DropExternalFileFormat(name string) error {
	return d.DropExternalFileFormatContext(context.Background(), name)
}

// DropExternalFileFormatContext is the context-aware variant of
// DropExternalFileFormat.
func (d *Database) DropExternalFileFormatContext(ctx context.Context, name string) error {
	if _, err := d.exec(ctx, "DROP EXTERNAL FILE FORMAT "+QuoteName(name)); err != nil {
		return fmt.Errorf("gosmo: drop external file format %q in %q: %w", name, d.name, err)
	}
	return nil
}

// Drop drops the external file format.
func (f *ExternalFileFormat) Drop() error { return f.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (f *ExternalFileFormat) DropContext(ctx context.Context) error {
	return f.db.DropExternalFileFormatContext(ctx, f.Name)
}

// ============================================================
// External libraries  (SQL Server 2017+)
// ============================================================

// ExternalLibrary mirrors a sys.external_libraries row — an R or Python
// package uploaded for Machine Learning Services.
//
// The catalog view carries no platform column on any instance gosmo supports:
// the documented platform/platform_desc pair is absent from majors 14 and 17
// alike, so there is no field for it here.
type ExternalLibrary struct {
	db *Database

	Name      string
	LibraryID int

	// Owner is the database principal that owns the library, empty when
	// principal_id names one that no longer exists.
	Owner string

	// Language is the runtime the package belongs to — "R" or "Python".
	Language string

	// Scope is PUBLIC for a library every user can load and PRIVATE for one
	// scoped to its owner.
	Scope string
}

// Database returns the database the library belongs to.
func (l *ExternalLibrary) Database() *Database { return l.db }

// requireExternalLibraries refuses the read on an instance whose catalog has
// no sys.external_libraries at all — SQL Server 2016 and older. The whole
// view is missing there, so there is nothing a column-level gate could
// substitute, and the server's own answer would be "Invalid object name"
// naming a view the caller never wrote.
func (d *Database) requireExternalLibraries() error {
	if major := d.serverMajorVersion(); major != 0 && major < int(SQLServer2017) {
		return unsupportedVersionf(
			"gosmo: external libraries need SQL Server 2017 or later; this instance is major %d", major)
	}
	return nil
}

const externalLibrarySelect = `
SELECT l.external_library_id, ISNULL(l.name, ''),
       ISNULL(USER_NAME(l.principal_id), ''),
       ISNULL(l.language, ''), ISNULL(l.scope_desc, '')
FROM   sys.external_libraries l`

func scanExternalLibrary(d *Database, scan func(...any) error) (*ExternalLibrary, error) {
	l := &ExternalLibrary{db: d}
	if err := scan(&l.LibraryID, &l.Name, &l.Owner, &l.Language, &l.Scope); err != nil {
		return nil, err
	}
	return l, nil
}

// ExternalLibraries returns the external libraries registered in the
// database. It returns an ErrUnsupportedVersion error before SQL Server 2017.
func (d *Database) ExternalLibraries() ([]*ExternalLibrary, error) {
	return d.ExternalLibrariesContext(context.Background())
}

// ExternalLibrariesContext is the context-aware variant of ExternalLibraries.
func (d *Database) ExternalLibrariesContext(ctx context.Context) ([]*ExternalLibrary, error) {
	if err := d.requireExternalLibraries(); err != nil {
		return nil, err
	}
	const q = externalLibrarySelect + `
ORDER  BY l.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list external libraries in %q: %w", d.name, err)
	}
	defer rows.Close()

	var libs []*ExternalLibrary
	for rows.Next() {
		l, err := scanExternalLibrary(d, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list external libraries in %q: %w", d.name, err)
		}
		libs = append(libs, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list external libraries in %q: %w", d.name, err)
	}
	return libs, nil
}

// ExternalLibraryByName returns one external library, or a not-found error
// (errors.Is ErrNotFound) when the database has none by that name. It
// returns an ErrUnsupportedVersion error before SQL Server 2017.
func (d *Database) ExternalLibraryByName(name string) (*ExternalLibrary, error) {
	return d.ExternalLibraryByNameContext(context.Background(), name)
}

// ExternalLibraryByNameContext is the context-aware variant of
// ExternalLibraryByName.
func (d *Database) ExternalLibraryByNameContext(ctx context.Context, name string) (*ExternalLibrary, error) {
	if err := d.requireExternalLibraries(); err != nil {
		return nil, err
	}
	var l *ExternalLibrary
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		l, err = scanExternalLibrary(d, row.Scan)
		return err
	}, externalLibrarySelect+`
WHERE  l.name = @p1`, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: external library %q not found in %q", name, d.name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read external library %q in %q: %w", name, d.name, err)
	}
	return l, nil
}

// DropExternalLibrary drops an external library by name.
func (d *Database) DropExternalLibrary(name string) error {
	return d.DropExternalLibraryContext(context.Background(), name)
}

// DropExternalLibraryContext is the context-aware variant of
// DropExternalLibrary.
func (d *Database) DropExternalLibraryContext(ctx context.Context, name string) error {
	if err := d.requireExternalLibraries(); err != nil {
		return err
	}
	if _, err := d.exec(ctx, "DROP EXTERNAL LIBRARY "+QuoteName(name)); err != nil {
		return fmt.Errorf("gosmo: drop external library %q in %q: %w", name, d.name, err)
	}
	return nil
}

// Drop drops the external library.
func (l *ExternalLibrary) Drop() error { return l.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (l *ExternalLibrary) DropContext(ctx context.Context) error {
	return l.db.DropExternalLibraryContext(ctx, l.Name)
}
