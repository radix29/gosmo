package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ============================================================
// Server (mirrors Microsoft.SqlServer.Management.Smo.Server)
// ============================================================

// Server is the top-level object representing a SQL Server instance.
// Create one with Connect() and use it to enumerate or manage databases,
// logins, server roles, linked servers, and more.
type Server struct {
	db   *sql.DB
	info *ServerInfo
}

// Close releases all resources held by the server connection pool.
func (s *Server) Close() error { return s.db.Close() }

// DB returns the underlying *sql.DB for ad-hoc queries.
func (s *Server) DB() *sql.DB { return s.db }

// Info returns cached server metadata (version, edition, paths ...).
func (s *Server) Info() *ServerInfo { return s.info }

// Name returns the SQL Server instance name.
func (s *Server) Name() string { return s.info.Name }

// CurrentDatabase returns the name of the database the connection is
// currently in — the login's default database when ConnectionOptions.Database
// was left empty at connect time, or whatever a session-level USE has since
// switched to.
func (s *Server) CurrentDatabase() (string, error) {
	return s.CurrentDatabaseContext(context.Background())
}

// CurrentDatabaseContext is the context-aware variant of CurrentDatabase.
func (s *Server) CurrentDatabaseContext(ctx context.Context) (string, error) {
	var name string
	if err := s.queryRowScan(ctx, "SELECT DB_NAME()", nil, &name); err != nil {
		return "", fmt.Errorf("gosmo: current database: %w", err)
	}
	return name, nil
}

// CurrentLogin returns the server login name the connection is
// authenticated as (SUSER_NAME()) — the real login behind the
// connection, which for Windows/Entra auth differs from whatever was
// passed as ConnectionOptions.User (often empty for those methods).
func (s *Server) CurrentLogin() (string, error) {
	return s.CurrentLoginContext(context.Background())
}

// CurrentLoginContext is the context-aware variant of CurrentLogin.
func (s *Server) CurrentLoginContext(ctx context.Context) (string, error) {
	var name string
	if err := s.queryRowScan(ctx, "SELECT SUSER_NAME()", nil, &name); err != nil {
		return "", fmt.Errorf("gosmo: current login: %w", err)
	}
	return name, nil
}

// -- Internal helpers ----------------------------------------------------------

// refusesSingleUser reports whether the instance rejects ALTER DATABASE ... SET
// SINGLE_USER, the statement a forced drop or rename uses to clear a database
// of other connections. A Managed Instance does, with Msg 5008 "This ALTER
// DATABASE statement is not supported" (verified live, 2026-09-11) — and a
// forced drop that led with it failed before reaching the DROP at all.
func (s *Server) refusesSingleUser() bool {
	return s.info != nil && EngineEdition(s.info.EngineEdition) == EngineAzureManagedInst
}

// killDatabaseSessions KILLs every other user session in the database name —
// the Managed Instance stand-in for SET SINGLE_USER WITH ROLLBACK IMMEDIATE —
// and waits, bounded, for them to leave, so the statement after it finds the
// database free.
//
// A session counts when the database is its current one or when it holds a
// database lock there: the second catches a request running in the database
// from a session whose context is elsewhere, which DB_ID alone misses. Each
// KILL is in its own TRY because a session can end between the SELECT and
// its KILL, and the "not an active process ID" that raises is not a failure.
// The wait matters because a KILL returns before the session's rollback
// finishes, and until it does the database is still in use and the DROP
// fails with Msg 3702; ROLLBACK IMMEDIATE waits out the same rollback itself.
//
// One batch, so a caller under WithScript gets it as one statement.
func (s *Server) killDatabaseSessions(ctx context.Context, name string) error {
	lit := "N" + QuoteLiteral(name) // N: a database name can be any Unicode
	return s.execContext(ctx, fmt.Sprintf(`DECLARE @db int = DB_ID(%[1]s), @kill nvarchar(max) = N'', @waits int = 0;
SELECT @kill += N'BEGIN TRY KILL ' + CAST(s.session_id AS nvarchar(10)) + N'; END TRY BEGIN CATCH END CATCH; '
FROM sys.dm_exec_sessions AS s
WHERE s.is_user_process = 1 AND s.session_id <> @@SPID
  AND (s.database_id = @db OR s.session_id IN (
    SELECT l.request_session_id FROM sys.dm_tran_locks AS l
    WHERE l.resource_type = N'DATABASE' AND l.resource_database_id = @db));
EXEC (@kill);
WHILE @waits < 150 AND EXISTS (
    SELECT 1 FROM sys.dm_exec_sessions AS s
    WHERE s.is_user_process = 1 AND s.session_id <> @@SPID AND s.database_id = @db)
BEGIN
    WAITFOR DELAY '00:00:00.200';
    SET @waits += 1;
END`, lit))
}

// multiUserRepairTimeout bounds restoreMultiUser's statement. Short on
// purpose: the caller still holds the single-user slot, so the ALTER has
// nothing to wait for, and a repair that hangs is worse than one that gives up.
const multiUserRepairTimeout = 10 * time.Second

// restoreMultiUser puts a database this package set to SINGLE_USER back to
// MULTI_USER — the release after a rename, and the repair after a detach or a
// drop that failed with the database still there.
//
// The context is derived with context.WithoutCancel because the case this
// exists for is the one where ctx is already dead. SET SINGLE_USER WITH
// ROLLBACK IMMEDIATE waits out the rollback of whatever it killed, so the
// statement before this one is precisely the one likely to have exhausted the
// caller's deadline — and a repair issued on the expired context fails without
// reaching the server, leaving the database locked to a single login for a
// reason nobody asked for. Same shape, and the same reason, as capturePlan's
// deferred SET ... OFF (executionplan.go).
//
// WithoutCancel keeps ctx's values, so a caller under WithScript still captures
// the statement rather than running it.
func (s *Server) restoreMultiUser(ctx context.Context, name string) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), multiUserRepairTimeout)
	defer cancel()
	return s.execContext(rctx, fmt.Sprintf("ALTER DATABASE %s SET MULTI_USER", quoteIdent(name)))
}

// query runs a server-scoped, rows-returning read against the pool,
// retrying once on a transient connection failure (a dropped pooled
// connection, etc.) — the Server-level counterpart of Database.query. A
// single read is idempotent, so it's always safe to re-run on a fresh
// connection; unlike Database.query, there's no USE to redo first, since a
// Server-scoped query never targets a specific database.
func (s *Server) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return withRetry(ctx, func() (*sql.Rows, error) {
		return s.db.QueryContext(ctx, q, args...)
	})
}

// queryRow runs a server-scoped, single-row read and hands the result to
// scan, retrying the whole query+scan as one unit on a transient connection
// failure. Unlike query (and unlike Database.queryRow), this can't hand the
// caller a live *sql.Row to scan later: QueryRowContext itself never
// returns an error — it only ever surfaces at Scan — so the scan has to run
// inside the retried closure to be covered by it at all. scan is usually
// row.Scan(&dest1, &dest2, ...) wrapped in a closure, or — for the several
// row types with a shared scanX(server, row.Scan) helper (see
// agent_schedule.go/agent_alert.go/agent_operator.go) — a closure around
// that call instead.
func (s *Server) queryRow(ctx context.Context, scan func(*sql.Row) error, q string, args ...any) error {
	_, err := withRetry(ctx, func() (struct{}, error) {
		return struct{}{}, scan(s.db.QueryRowContext(ctx, q, args...))
	})
	return err
}

// queryRowScan is a queryRow convenience for the common case of scanning
// straight into a fixed list of destinations, sparing the caller a
// `func(row *sql.Row) error { return row.Scan(dest...) }` closure of their
// own. Callers that need to do something other than a bare Scan (e.g. the
// shared scanAlert/scanOperator/scanSchedule helpers in
// agent_alert.go/agent_operator.go/agent_schedule.go) should keep calling
// queryRow directly.
func (s *Server) queryRowScan(ctx context.Context, q string, args []any, dest ...any) error {
	return s.queryRow(ctx, func(row *sql.Row) error { return row.Scan(dest...) }, q, args...)
}

// loadInfo populates s.info. It runs two statements rather than one, and the
// split is load-bearing: every value in the first is a SERVERPROPERTY or
// @@VERSION call that any login can make, while the second reads
// sys.dm_os_sys_info, which needs VIEW SERVER STATE (VIEW SERVER PERFORMANCE
// STATE on SQL Server 2022 and later). Joined into one statement — as this was
// until 2026-08-25 — the DMV's permission check fails the whole SELECT, and
// since Connect calls loadInfo, a db_owner with no server-level rights could
// not open a connection at all.
//
// Only the first statement's failure is fatal. The second degrades to
// SysInfoUnavailable so the connection still succeeds.
func (s *Server) loadInfo(ctx context.Context) error {
	const q = `
	SELECT
		SERVERPROPERTY('ServerName')           AS server_name,
		SERVERPROPERTY('Edition')              AS edition,
		SERVERPROPERTY('ProductVersion')       AS product_version,
		SERVERPROPERTY('ProductLevel')         AS product_level,
		SERVERPROPERTY('Collation')            AS collation,
		CAST(SERVERPROPERTY('IsClustered')  AS INT),
		CAST(SERVERPROPERTY('IsHadrEnabled') AS INT),
		CAST(SERVERPROPERTY('IsSingleUser') AS INT),
		CAST(SERVERPROPERTY('EngineEdition') AS INT),
		@@VERSION,
		SERVERPROPERTY('InstanceDefaultDataPath'),
		SERVERPROPERTY('InstanceDefaultLogPath'),
		SERVERPROPERTY('InstanceDefaultBackupPath')`

	info := &ServerInfo{}
	var isClustered, isHADR, isSingleUser, engineEdition sql.NullInt64
	var osVer, dataPath, logPath, backupPath sql.NullString

	if err := s.queryRowScan(ctx, q, nil,
		&info.Name, &info.Edition, &info.ProductVersion, &info.ProductLevel,
		&info.Collation, &isClustered, &isHADR, &isSingleUser, &engineEdition, &osVer,
		&dataPath, &logPath, &backupPath,
	); err != nil {
		return fmt.Errorf("gosmo: load server info: %w", err)
	}

	const sysInfoQuery = `
	SELECT osi.physical_memory_kb / 1024, osi.cpu_count
	FROM   sys.dm_os_sys_info osi`

	var memMB, cpuCount sql.NullInt64
	if err := s.queryRowScan(ctx, sysInfoQuery, nil, &memMB, &cpuCount); err != nil {
		info.SysInfoUnavailable = true
	}

	info.IsClustered = isClustered.Int64 == 1
	info.IsHADREnabled = isHADR.Int64 == 1
	info.IsSingleUser = isSingleUser.Int64 == 1
	info.EngineEdition = int(engineEdition.Int64)
	info.OSVersion = osVer.String
	info.Platform = platformFromVersionString(osVer.String)
	info.PhysicalMemoryMB = memMB.Int64
	info.LogicalCPUCount = int(cpuCount.Int64)
	info.DefaultDataPath = dataPath.String
	info.DefaultLogPath = logPath.String
	info.DefaultBackupPath = backupPath.String

	parts := strings.SplitN(info.ProductVersion, ".", 4)
	if len(parts) >= 3 {
		info.VersionMajor, _ = strconv.Atoi(parts[0])
		info.VersionMinor, _ = strconv.Atoi(parts[1])
		info.VersionBuild, _ = strconv.Atoi(parts[2])
	}
	if info.DefaultBackupPath == "" {
		info.DefaultBackupPath = s.backupPathFromRegistry(ctx, info.Platform)
	}
	s.info = info
	return nil
}

// backupPathFromRegistry reads the instance's configured backup directory out
// of the registry. SERVERPROPERTY('InstanceDefaultBackupPath') is SQL Server
// 2019 (15.x) and later and returns NULL before it, while the Data and Log
// properties are populated on every version — so without this, everything that
// defaults a backup location (Server Properties' default locations, Back Up
// Database, the destination browser, New Backup Device, New Audit) comes up
// blank on 2017 and older.
//
// Windows only: there is no registry on Linux, and every caller already copes
// with an empty path, so a failure here — no registry key, or a login without
// the rights to run xp_instance_regread — returns "" rather than failing the
// connection loadInfo is part of.
//
// xp_instance_regread rewrites MSSQLServer to the instance's own key, so this
// is right for a named instance without composing the path by hand.
func (s *Server) backupPathFromRegistry(ctx context.Context, platform string) string {
	if platform != "Windows" {
		return ""
	}
	const q = `
DECLARE @path nvarchar(4000);
EXEC master.dbo.xp_instance_regread N'HKEY_LOCAL_MACHINE',
     N'Software\Microsoft\MSSQLServer\MSSQLServer', N'BackupDirectory', @path OUTPUT;
SELECT @path`

	var path sql.NullString
	if err := s.queryRowScan(ctx, q, nil, &path); err != nil {
		return ""
	}
	return path.String
}

// platformFromVersionString extracts the host OS family from @@VERSION,
// whose last line reads "... (64-bit) on Windows 10 Pro ..." or "...
// (64-bit) on Linux (Ubuntu 24.04) ...".
//
// Azure names no host at all — a Managed Instance's banner is "Microsoft SQL
// Azure (RTM) - 12.0.2000.8 ..." with neither suffix — so without the third
// case every Azure edition reports its platform as unknown. "Azure" is the
// truthful answer there: the host OS is not the caller's to see, and the
// hosting model is what a caller displaying this actually wants to know.
func platformFromVersionString(v string) string {
	switch {
	case strings.Contains(v, " on Windows"):
		return "Windows"
	case strings.Contains(v, " on Linux"):
		return "Linux"
	case strings.Contains(v, "Microsoft SQL Azure"):
		return "Azure"
	}
	return ""
}

// -- Databases -----------------------------------------------------------------

// Databases returns all user-accessible databases on the server.
func (s *Server) Databases() ([]*Database, error) {
	return s.DatabasesContext(context.Background())
}

// DatabasesContext returns all databases, honouring the provided context.
func (s *Server) DatabasesContext(ctx context.Context) ([]*Database, error) {
	const q = `
	SELECT name, database_id, state_desc, recovery_model_desc,
	       compatibility_level, collation_name, is_read_only, create_date,
	       ISNULL(source_database_id, 0)
	FROM sys.databases
	ORDER BY name`

	rows, err := s.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list databases: %w", err)
	}
	defer rows.Close()

	var dbs []*Database
	for rows.Next() {
		d := &Database{server: s}
		var state, recovery, collation sql.NullString
		var compatLevel sql.NullInt64
		if err := rows.Scan(
			&d.Name, &d.ID, &state, &recovery,
			&compatLevel, &collation, &d.IsReadOnly, &d.CreateDate,
			&d.SourceDatabaseID,
		); err != nil {
			return nil, fmt.Errorf("gosmo: list databases: %w", err)
		}
		d.State = state.String
		d.RecoveryModel = RecoveryModel(recovery.String)
		d.CompatibilityLevel = CompatibilityLevel(compatLevel.Int64)
		d.Collation = collation.String
		dbs = append(dbs, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list databases: %w", err)
	}
	return dbs, nil
}

// DatabaseByName returns a single database by name, querying sys.databases
// so the returned handle is verified to exist and has State/RecoveryModel/
// Collation/CompatibilityLevel/etc. populated. Use it when you need to read
// those or to confirm the database is there; use Database when you only
// need a handle to issue further ALTER-style calls against a database you
// already know exists. The two are not interchangeable — see Database.
func (s *Server) DatabaseByName(name string) (*Database, error) {
	return s.DatabaseByNameContext(context.Background(), name)
}

// DatabaseByNameContext is the context-aware variant of DatabaseByName.
func (s *Server) DatabaseByNameContext(ctx context.Context, name string) (*Database, error) {
	const q = `
	SELECT name, database_id, state_desc, recovery_model_desc,
	       compatibility_level, collation_name, is_read_only, create_date,
	       ISNULL(source_database_id, 0)
	FROM sys.databases
	WHERE name = @p1`

	d := &Database{server: s}
	var state, recovery, collation sql.NullString
	var compatLevel sql.NullInt64

	if err := s.queryRowScan(ctx, q, []any{name},
		&d.Name, &d.ID, &state, &recovery,
		&compatLevel, &collation, &d.IsReadOnly, &d.CreateDate,
		&d.SourceDatabaseID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("gosmo: database %q not found", name)
		}
		return nil, fmt.Errorf("gosmo: database by name: %w", err)
	}
	d.State = state.String
	d.RecoveryModel = RecoveryModel(recovery.String)
	d.CompatibilityLevel = CompatibilityLevel(compatLevel.Int64)
	d.Collation = collation.String
	return d, nil
}

// DatabaseRef returns a lightweight handle for name without querying the
// server at all — unlike DatabaseByName/DatabaseByNameContext, it doesn't
// verify the database exists or populate State/RecoveryModel/Collation/
// CompatibilityLevel/etc. (they stay at their zero value). Every write
// method on *Database (AddFileGroupContext, SetDatabaseOptionContext,
// SetOwnerContext, ...) only ever needs the database's name, never those
// cached fields, so this is sufficient for issuing further ALTER-style
// calls against a database the caller already knows exists — most
// commonly one it just created in the same operation. It's also the only
// way to do that under a WithScript-derived context: DatabaseByNameContext's
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

// CreateDatabase creates a new database with the given name and optional options.
func (s *Server) CreateDatabase(name string, opts *CreateDatabaseOptions) error {
	return s.CreateDatabaseContext(context.Background(), name, opts)
}

// CreateDatabaseContext is the context-aware variant of CreateDatabase.
func (s *Server) CreateDatabaseContext(ctx context.Context, name string, opts *CreateDatabaseOptions) error {
	if name == "" {
		return fmt.Errorf("gosmo: create database: name is required")
	}
	if opts == nil {
		opts = &CreateDatabaseOptions{}
	}
	if opts.RecoveryModel != "" && !validRecoveryModel(opts.RecoveryModel) {
		return fmt.Errorf("gosmo: create database %q: unrecognized recovery model %q", name, opts.RecoveryModel)
	}
	if opts.Collation != "" && !isSimpleIdentifier(opts.Collation) {
		return fmt.Errorf("gosmo: create database %q: invalid collation %q", name, opts.Collation)
	}

	if opts.LogFile != nil && opts.PrimaryFile == nil {
		primary, err := defaultPrimaryFile(name, s.info)
		if err != nil {
			return fmt.Errorf("gosmo: create database %q: %w", name, err)
		}
		withPrimary := *opts
		withPrimary.PrimaryFile = primary
		opts = &withPrimary
	}

	if err := s.execContext(ctx, buildCreateDatabaseStatement(name, opts)); err != nil {
		return fmt.Errorf("gosmo: create database %q: %w", name, err)
	}

	if opts.RecoveryModel != "" {
		if err := s.execContext(ctx,
			fmt.Sprintf("ALTER DATABASE %s SET RECOVERY %s", quoteIdent(name), opts.RecoveryModel),
		); err != nil {
			return fmt.Errorf("gosmo: set recovery model for %q: %w", name, err)
		}
	}
	if opts.CompatLevel > 0 {
		if err := s.execContext(ctx,
			fmt.Sprintf("ALTER DATABASE %s SET COMPATIBILITY_LEVEL = %d", quoteIdent(name), opts.CompatLevel),
		); err != nil {
			return fmt.Errorf("gosmo: set compat level for %q: %w", name, err)
		}
	}
	return nil
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
	return &DatabaseFileSpec{Name: name, Path: joinServerPath(info.DefaultDataPath, name+".mdf")}, nil
}

// joinServerPath appends file to dir, a directory on the server's own file
// system — so the separator is the one dir already uses, not the client's.
// SERVERPROPERTY('InstanceDefaultDataPath') ends in one, but a caller-supplied
// directory may not.
func joinServerPath(dir, file string) string {
	if strings.HasSuffix(dir, `\`) || strings.HasSuffix(dir, "/") {
		return dir + file
	}
	if strings.Contains(dir, `\`) {
		return dir + `\` + file
	}
	return dir + "/" + file
}

// buildCreateDatabaseStatement builds the CREATE DATABASE statement for
// name/opts. Unexported and side-effect-free so it's unit-testable without
// a server, mirroring buildAddFileStatement.
func buildCreateDatabaseStatement(name string, opts *CreateDatabaseOptions) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "CREATE DATABASE %s", quoteIdent(name))
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

// CreateDatabaseOptions holds optional parameters for CreateDatabase.
type CreateDatabaseOptions struct {
	Collation     string
	RecoveryModel RecoveryModel
	CompatLevel   CompatibilityLevel

	// PrimaryFile and LogFile customize the database's initial data and
	// log file (name, path, size, growth, max size) via CREATE DATABASE's
	// ON PRIMARY/LOG ON clauses. Leaving either nil lets the server place
	// that file at its own default path/size, exactly like CreateDatabase
	// with a zero-valued CreateDatabaseOptions always has — for a nil
	// PrimaryFile beside a LogFile, by naming that default file explicitly,
	// since SQL Server refuses LOG ON without a data file (see
	// defaultPrimaryFile). FileGroup is
	// ignored on both (PrimaryFile is always PRIMARY; LogFile has none) —
	// additional filegroups and files are added after creation via
	// AddFileGroupContext/AddFileContext, not here.
	PrimaryFile *DatabaseFileSpec
	LogFile     *DatabaseFileSpec
}

// DropDatabase drops the named database.
// When force is true, active connections are terminated first — by SET
// SINGLE_USER WITH ROLLBACK IMMEDIATE, or on a Managed Instance, which refuses
// that statement, by killing the database's sessions (see killDatabaseSessions).
func (s *Server) DropDatabase(name string, force bool) error {
	return s.DropDatabaseContext(context.Background(), name, force)
}

// DropDatabaseContext is the context-aware variant of DropDatabase.
func (s *Server) DropDatabaseContext(ctx context.Context, name string, force bool) error {
	if name == "" {
		return fmt.Errorf("gosmo: drop database: name is required")
	}
	if force && s.refusesSingleUser() {
		if err := s.killDatabaseSessions(ctx, name); err != nil {
			return fmt.Errorf("gosmo: close connections to %q: %w", name, err)
		}
		if err := s.execContext(ctx, fmt.Sprintf("DROP DATABASE %s", quoteIdent(name))); err != nil {
			return fmt.Errorf("gosmo: drop database %q: %w", name, err)
		}
		return nil
	}
	if force {
		if err := s.execContext(ctx,
			fmt.Sprintf("ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE", quoteIdent(name)),
		); err != nil {
			return fmt.Errorf("gosmo: set single user on %q: %w", name, err)
		}
	}
	if err := s.execContext(ctx, fmt.Sprintf("DROP DATABASE %s", quoteIdent(name))); err != nil {
		// A failed DROP leaves the database in place — and, with force, still
		// in the SINGLE_USER this method put it in, unreachable by every other
		// login until someone notices. The drop can genuinely fail after the
		// alter succeeded: another session takes the single-user slot, the
		// database belongs to an availability group, the login may set state
		// but not drop. Best effort, and the drop's own error is what the
		// caller is told about.
		//
		// Only under force: without it nothing here set the access mode, and
		// a MULTI_USER on the way out would silently undo a RESTRICTED_USER or
		// SINGLE_USER the database was deliberately left in.
		if force {
			_ = s.restoreMultiUser(ctx, name)
		}
		return fmt.Errorf("gosmo: drop database %q: %w", name, err)
	}
	return nil
}

// RenameDatabase renames a database (ALTER DATABASE ... MODIFY NAME). The
// server needs exclusive access to it, so any other connection to the
// database fails the statement outright rather than waiting.
//
// When force is true the database is put into SINGLE_USER WITH ROLLBACK
// IMMEDIATE first — terminating those connections and rolling back their
// transactions — and back to MULTI_USER afterwards, including when the
// rename itself fails, so a refused rename never leaves the database
// single-user. A Managed Instance refuses SET SINGLE_USER, so there force
// kills the database's sessions instead and changes no access mode.
func (s *Server) RenameDatabase(oldName, newName string, force bool) error {
	return s.RenameDatabaseContext(context.Background(), oldName, newName, force)
}

// RenameDatabaseContext is the context-aware variant of RenameDatabase.
func (s *Server) RenameDatabaseContext(ctx context.Context, oldName, newName string, force bool) error {
	if oldName == "" || newName == "" {
		return fmt.Errorf("gosmo: rename database: both names are required")
	}
	q := fmt.Sprintf("ALTER DATABASE %s MODIFY NAME = %s", quoteIdent(oldName), quoteIdent(newName))
	if force && s.refusesSingleUser() {
		// Nothing to release afterwards: no access mode was changed.
		if err := s.killDatabaseSessions(ctx, oldName); err != nil {
			return fmt.Errorf("gosmo: close connections to %q: %w", oldName, err)
		}
		if err := s.execContext(ctx, q); err != nil {
			return fmt.Errorf("gosmo: rename database %q to %q: %w", oldName, newName, err)
		}
		return nil
	}
	if force {
		if err := s.execContext(ctx,
			fmt.Sprintf("ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE", quoteIdent(oldName)),
		); err != nil {
			return fmt.Errorf("gosmo: set single user on %q: %w", oldName, err)
		}
	}
	err := s.execContext(ctx, q)
	if force {
		// The name to release is whichever one the database now has.
		name := newName
		if err != nil {
			name = oldName
		}
		if mu := s.restoreMultiUser(ctx, name); mu != nil && err == nil {
			return fmt.Errorf("gosmo: set multi user on %q: %w", name, mu)
		}
	}
	if err != nil {
		return fmt.Errorf("gosmo: rename database %q to %q: %w", oldName, newName, err)
	}
	return nil
}
