package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// -- Databases -----------------------------------------------------------------

// Databases returns all user-accessible databases on the server.
func (s *Server) Databases(ctx context.Context) ([]*Database, error) {
	rows, err := s.query(ctx, s.databaseSelect()+`
	ORDER BY name`)
	return scanRows(rows, err, "list databases", func(scan func(...any) error) (*Database, error) {
		return scanDatabase(s, scan)
	})
}

// databaseSelect is the sys.databases read a populated *Database is built
// from, without its WHERE or ORDER BY; scanDatabase reads its row.
func (s *Server) databaseSelect() string {
	return `
	SELECT name, database_id, state_desc, recovery_model_desc,
	       compatibility_level, collation_name, ` + catalogCollationExpr(s.serverMajorVersion()) + `,
	       is_read_only, create_date, ISNULL(source_database_id, 0), is_distributor
	FROM sys.databases`
}

// scanDatabase builds a *Database from one databaseSelect row.
func scanDatabase(s *Server, scan func(...any) error) (*Database, error) {
	d := &Database{server: s}
	var state, recovery, collation, catalogCollation sql.NullString
	var compatLevel sql.NullInt64
	if err := scan(
		&d.Name, &d.ID, &state, &recovery,
		&compatLevel, &collation, &catalogCollation, &d.IsReadOnly, &d.CreateDate,
		&d.SourceDatabaseID, &d.IsDistributor,
	); err != nil {
		return nil, err
	}
	d.State = DatabaseState(state.String)
	d.RecoveryModel = RecoveryModel(recovery.String)
	d.CompatibilityLevel = CompatibilityLevel(compatLevel.Int64)
	d.Collation = collation.String
	d.CatalogCollation = catalogCollation.String
	return d, nil
}

// containedCatalogCollation is the fixed collation of a partially contained
// database's catalog, whatever its data collation.
const containedCatalogCollation = "Latin1_General_100_CI_AS_KS_WS_SC"

// catalogCollationExpr is the sys.databases expression for
// Database.CatalogCollation. catalog_collation_type_desc names the collation
// outright when it is not the database's own — "Latin1_General_100_CI_AS_KS_WS_SC"
// for a contained database, "SQL_Latin1_General_CP1_CI_AS" for an Azure SQL
// Database created WITH CATALOG_COLLATION — and says DATABASE_DEFAULT
// otherwise. The column is 2019+ (2017 lacks it, probed 2026-10-04), so older
// instances fall back to containment, the only way one of them has a catalog
// collation of its own.
func catalogCollationExpr(major int) string {
	desc := colSince(major, SQLServer2019, "catalog_collation_type_desc", "CAST(NULL AS nvarchar(60))")
	return `CASE WHEN ` + desc + ` NOT IN ('DATABASE_DEFAULT', 'NOT_APPLICABLE') THEN ` + desc + `
	            WHEN containment = 1 THEN N'` + containedCatalogCollation + `'
	            ELSE collation_name END`
}

// DatabaseByName returns a single database by name, querying sys.databases
// so the returned handle is verified to exist and has State/RecoveryModel/
// Collation/CompatibilityLevel/etc. populated. Use it when you need to read
// those or to confirm the database is there; use DatabaseRef when you
// only need a handle to issue further ALTER-style calls against a database you
// already know exists. The two are not interchangeable — see DatabaseRef.
func (s *Server) DatabaseByName(ctx context.Context, name string) (*Database, error) {
	return readByName(ctx, s, scanDatabase, s.databaseSelect()+`
	WHERE name = @p1`, []any{name},
		notFoundf("gosmo: database %q not found", name), fmt.Sprintf("read database %q", name))
}

// DatabaseRef returns a lightweight handle for name without querying the
// server at all — unlike DatabaseByName, it doesn't
// verify the database exists or populate State/RecoveryModel/Collation/
// CompatibilityLevel/etc. (they stay at their zero value). Every write
// method on *Database (AddFileGroup, SetDatabaseOption,
// SetOwner, ...) only ever needs the database's name, never those
// cached fields, so this is sufficient for issuing further ALTER-style
// calls against a database the caller already knows exists — most
// commonly one it just created in the same operation. It's also the only
// way to do that under a WithScript-derived context: DatabaseByName's
// own lookup query is a real read, not a write, so it isn't captured by
// ScriptCollector and would fail outright (or return stale data) for a
// database whose CREATE DATABASE was itself only scripted, not actually
// run.
//
// IsSystem and IsSnapshot are derived from ID and SourceDatabaseID, so both
// answer false on a handle — DatabaseRef("master").IsSystem() is false. A
// caller that needs either answer needs DatabaseByName.
func (s *Server) DatabaseRef(name string) *Database {
	return &Database{server: s, Name: name}
}

// CreateDatabase creates a new database, and returns it read back from
// sys.databases — or, under Scripting(ctx), the DatabaseRef handle, since
// nothing ran.
func (s *Server) CreateDatabase(ctx context.Context, req CreateDatabaseRequest) (*Database, error) {
	name, opts := req.Name, &req
	if name == "" {
		return nil, invalidf("gosmo: create database: name is required")
	}
	if opts.RecoveryModel != "" && !validRecoveryModel(opts.RecoveryModel) {
		return nil, invalidf("gosmo: create database %q: unrecognized recovery model %q", name, opts.RecoveryModel)
	}
	if opts.Collation != "" && !isSimpleIdentifier(opts.Collation) {
		return nil, invalidf("gosmo: create database %q: invalid collation %q", name, opts.Collation)
	}

	if opts.LogFile != nil && opts.PrimaryFile == nil {
		primary, err := defaultPrimaryFile(name, s.info)
		if err != nil {
			return nil, fmt.Errorf("gosmo: create database %q: %w", name, err)
		}
		withPrimary := *opts
		withPrimary.PrimaryFile = primary
		opts = &withPrimary
	}
	var dataDir, logDir string
	if s.info != nil {
		dataDir, logDir = s.info.DefaultDataPath, s.info.DefaultLogPath
	}
	primary, err := placeUnpathedFile(opts.PrimaryFile, dataDir, ".mdf", "data")
	if err != nil {
		return nil, fmt.Errorf("gosmo: create database %q: %w", name, err)
	}
	logFile, err := placeUnpathedFile(opts.LogFile, logDir, ".ldf", "log")
	if err != nil {
		return nil, fmt.Errorf("gosmo: create database %q: %w", name, err)
	}
	if primary != opts.PrimaryFile || logFile != opts.LogFile {
		placed := *opts
		placed.PrimaryFile, placed.LogFile = primary, logFile
		opts = &placed
	}

	if _, err := s.exec(ctx, buildCreateDatabaseStatement(name, opts)); err != nil {
		return nil, fmt.Errorf("gosmo: create database %q: %w", name, err)
	}

	if opts.RecoveryModel != "" {
		if _, err := s.exec(ctx,
			fmt.Sprintf("ALTER DATABASE %s SET RECOVERY %s", QuoteName(name), opts.RecoveryModel),
		); err != nil {
			return nil, fmt.Errorf("gosmo: set recovery model for %q: %w", name, err)
		}
	}
	if opts.CompatLevel > 0 {
		if _, err := s.exec(ctx,
			fmt.Sprintf("ALTER DATABASE %s SET COMPATIBILITY_LEVEL = %d", QuoteName(name), opts.CompatLevel),
		); err != nil {
			return nil, fmt.Errorf("gosmo: set compat level for %q: %w", name, err)
		}
	}
	return createdByName(ctx, s.DatabaseRef(name), s.DatabaseByName, name)
}

// defaultPrimaryFile is the data file CREATE DATABASE would have made on its
// own — logical name name, file name.mdf in the instance's default data
// directory, every size and growth left to model — spelled out because the
// caller has asked for a log file.
//
// SQL Server takes a LOG ON clause only after an ON clause naming at least one
// data file: "Cannot specify a log file in a CREATE DATABASE statement without
// also specifying at least one data file" (verified live). Omitting the data
// file is how a caller says "the server's default", so a request to customise
// only the log would otherwise fail outright.
func defaultPrimaryFile(name string, info *ServerInfo) (*DatabaseFileSpec, error) {
	if info == nil || info.DefaultDataPath == "" {
		return nil, fmt.Errorf("a log file needs a data file beside it, and the instance reports no default data path to place one in")
	}
	return &DatabaseFileSpec{Name: name, Path: JoinServerPath(info.DefaultDataPath, name+".mdf")}, nil
}

// placeUnpathedFile returns spec, or for a spec with no Path a copy placed in
// dir, the instance's default directory for the kind, as spec.Name + ext —
// the file CREATE DATABASE would have made there itself.
//
// CREATE DATABASE has no way to leave a named file's location to the server:
// FILENAME is required beside NAME (Msg 1036, verified live), and a bare file
// name is not resolved against the default directory but refused (Msg 5105).
// So an empty Path is either placed here or refused before anything is sent.
func placeUnpathedFile(spec *DatabaseFileSpec, dir, ext, kind string) (*DatabaseFileSpec, error) {
	if spec == nil || spec.Path != "" {
		return spec, nil
	}
	if dir == "" {
		return nil, invalidf("%s file %q has no path, and the instance reports no default %s directory to place it in", kind, spec.Name, kind)
	}
	placed := *spec
	placed.Path = JoinServerPath(dir, spec.Name+ext)
	return &placed, nil
}

// buildCreateDatabaseStatement builds the CREATE DATABASE statement for
// name/opts. Unexported and side-effect-free so it's unit-testable without
// a server, mirroring buildAddFileStatement.
func buildCreateDatabaseStatement(name string, opts *CreateDatabaseRequest) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "CREATE DATABASE %s", QuoteName(name))
	if opts.PrimaryFile != nil {
		fmt.Fprintf(&sb, " ON PRIMARY \n%s", buildFileDefClause(*opts.PrimaryFile))
	}
	if opts.LogFile != nil {
		fmt.Fprintf(&sb, " \nLOG ON \n%s", buildFileDefClause(*opts.LogFile))
	}
	if opts.Collation != "" {
		fmt.Fprintf(&sb, " COLLATE %s", opts.Collation)
	}
	return sb.String()
}

// CreateDatabaseRequest describes a new database. Only Name is required.
type CreateDatabaseRequest struct {
	Name string

	Collation     string
	RecoveryModel RecoveryModel
	CompatLevel   CompatibilityLevel

	// PrimaryFile and LogFile customize the database's initial data and
	// log file (name, path, size, growth, max size) via CREATE DATABASE's
	// ON PRIMARY/LOG ON clauses. A spec with an empty Path is placed in the
	// instance's default data or log directory as Name + ".mdf"/".ldf",
	// and refused if the instance reports none (see placeUnpathedFile).
	// Leaving either nil lets the server place
	// that file at its own default path/size, exactly like CreateDatabase
	// with only a Name always has — for a nil
	// PrimaryFile beside a LogFile, by naming that default file explicitly,
	// since SQL Server refuses LOG ON without a data file (see
	// defaultPrimaryFile). FileGroup is
	// ignored on both (PrimaryFile is always PRIMARY; LogFile has none) —
	// additional filegroups and files are added after creation via
	// AddFileGroup/AddFile, not here.
	PrimaryFile *DatabaseFileSpec
	LogFile     *DatabaseFileSpec
}
