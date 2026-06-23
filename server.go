package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "github.com/microsoft/go-mssqldb"
)

// ============================================================
// Server  (mirrors Microsoft.SqlServer.Management.Smo.Server)
// ============================================================

// Server is the top-level object representing a SQL Server instance.
// Create one with Connect() and use it to enumerate or manage databases,
// logins, server roles, linked servers, etc.
type Server struct {
	db   *sql.DB
	info *ServerInfo
	dsn  string
}

// ConnectionOptions holds parameters used when connecting to SQL Server.
type ConnectionOptions struct {
	// Server is the host name or IP, optionally with port: "localhost:1433"
	Server   string
	Database string // leave blank to connect to master
	// SQL Server auth: set User + Password.
	// Windows / integrated auth: leave both empty.
	User     string
	Password string
	// TrustServerCertificate disables certificate validation (handy for dev).
	TrustServerCertificate bool
	// ConnectTimeout defaults to 30 s when zero.
	ConnectTimeout time.Duration
	// ApplicationName appears in sys.dm_exec_sessions.
	ApplicationName string
}

// Connect opens a connection to a SQL Server instance and returns a Server.
func Connect(opts ConnectionOptions) (*Server, error) {
	if opts.ConnectTimeout == 0 {
		opts.ConnectTimeout = 30 * time.Second
	}
	appName := opts.ApplicationName
	if appName == "" {
		appName = "gosmo"
	}
	db := opts.Database
	if db == "" {
		db = "master"
	}

	dsn := fmt.Sprintf(
		"sqlserver://%s:%s@%s?database=%s&connection+timeout=%d&app+name=%s&TrustServerCertificate=%v",
		opts.User, opts.Password,
		opts.Server, db,
		int(opts.ConnectTimeout.Seconds()),
		appName,
		opts.TrustServerCertificate,
	)

	conn, err := sql.Open("sqlserver", dsn)
	if err != nil {
		return nil, fmt.Errorf("gosmo: open connection: %w", err)
	}
	if err = conn.PingContext(context.Background()); err != nil {
		conn.Close()
		return nil, fmt.Errorf("gosmo: ping: %w", err)
	}

	s := &Server{db: conn, dsn: dsn}
	if err = s.loadInfo(); err != nil {
		conn.Close()
		return nil, err
	}
	return s, nil
}

// Close releases all resources held by the server connection.
func (s *Server) Close() error { return s.db.Close() }

// DB returns the underlying *sql.DB for ad-hoc queries.
func (s *Server) DB() *sql.DB { return s.db }

// Info returns cached server metadata (version, edition, paths …).
func (s *Server) Info() *ServerInfo { return s.info }

// Name returns the SQL Server instance name.
func (s *Server) Name() string { return s.info.Name }

// ── Internal helpers ──────────────────────────────────────────────────────────

func (s *Server) loadInfo() error {
	const q = `
SELECT
    SERVERPROPERTY('ServerName')           AS server_name,
    SERVERPROPERTY('Edition')              AS edition,
    SERVERPROPERTY('ProductVersion')       AS product_version,
    SERVERPROPERTY('ProductLevel')         AS product_level,
    SERVERPROPERTY('Collation')            AS collation,
    SERVERPROPERTY('IsClustered')          AS is_clustered,
    SERVERPROPERTY('IsHadrEnabled')        AS is_hadr,
    @@VERSION                              AS os_version,
    (SELECT physical_memory_kb/1024 FROM sys.dm_os_sys_info) AS mem_mb,
    (SELECT cpu_count            FROM sys.dm_os_sys_info) AS cpu_count,
    SERVERPROPERTY('InstanceDefaultDataPath') AS data_path,
    SERVERPROPERTY('InstanceDefaultLogPath')  AS log_path,
    SERVERPROPERTY('InstanceDefaultBackupPath') AS backup_path`

	row := s.db.QueryRowContext(context.Background(), q)
	info := &ServerInfo{}
	var isClustered, isHADR sql.NullInt64
	var osVer, dataPath, logPath, backupPath sql.NullString
	var memMB, cpuCount sql.NullInt64

	if err := row.Scan(
		&info.Name, &info.Edition, &info.ProductVersion, &info.ProductLevel,
		&info.Collation, &isClustered, &isHADR, &osVer,
		&memMB, &cpuCount, &dataPath, &logPath, &backupPath,
	); err != nil {
		return fmt.Errorf("gosmo: load server info: %w", err)
	}

	info.IsClustered = isClustered.Int64 == 1
	info.IsHADREnabled = isHADR.Int64 == 1
	info.OSVersion = osVer.String
	info.PhysicalMemoryMB = memMB.Int64
	info.LogicalCPUCount = int(cpuCount.Int64)
	info.DefaultDataPath = dataPath.String
	info.DefaultLogPath = logPath.String
	info.DefaultBackupPath = backupPath.String

	// Parse "major.minor.build.revision"
	parts := strings.SplitN(info.ProductVersion, ".", 4)
	if len(parts) >= 3 {
		info.VersionMajor, _ = strconv.Atoi(parts[0])
		info.VersionMinor, _ = strconv.Atoi(parts[1])
		info.VersionBuild, _ = strconv.Atoi(parts[2])
	}

	s.info = info
	return nil
}

// ── Databases ─────────────────────────────────────────────────────────────────

// Databases returns all user-accessible databases on the server.
func (s *Server) Databases() ([]*Database, error) {
	const q = `
SELECT name, database_id, state_desc, recovery_model_desc,
       compatibility_level, collation_name, is_read_only, create_date
FROM   sys.databases
ORDER  BY name`

	rows, err := s.db.QueryContext(context.Background(), q)
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
			&d.name, &d.id, &state, &recovery,
			&compatLevel, &collation, &d.isReadOnly, &d.createDate,
		); err != nil {
			return nil, err
		}
		d.state = state.String
		d.recoveryModel = RecoveryModel(recovery.String)
		d.compatLevel = CompatibilityLevel(compatLevel.Int64)
		d.collation = collation.String
		dbs = append(dbs, d)
	}
	return dbs, rows.Err()
}

// DatabaseByName returns a single database by name, or an error if not found.
func (s *Server) DatabaseByName(name string) (*Database, error) {
	dbs, err := s.Databases()
	if err != nil {
		return nil, err
	}
	for _, d := range dbs {
		if strings.EqualFold(d.name, name) {
			return d, nil
		}
	}
	return nil, fmt.Errorf("gosmo: database %q not found", name)
}

// CreateDatabase creates a new database with the given name and optional options.
func (s *Server) CreateDatabase(name string, opts *CreateDatabaseOptions) error {
	if opts == nil {
		opts = &CreateDatabaseOptions{}
	}
	sb := &strings.Builder{}
	fmt.Fprintf(sb, "CREATE DATABASE [%s]", name)
	if opts.Collation != "" {
		fmt.Fprintf(sb, " COLLATE %s", opts.Collation)
	}
	_, err := s.db.ExecContext(context.Background(), sb.String())
	if err != nil {
		return fmt.Errorf("gosmo: create database %q: %w", name, err)
	}
	if opts.RecoveryModel != "" {
		_, err = s.db.ExecContext(context.Background(),
			fmt.Sprintf("ALTER DATABASE [%s] SET RECOVERY %s", name, opts.RecoveryModel))
		if err != nil {
			return fmt.Errorf("gosmo: set recovery model: %w", err)
		}
	}
	return nil
}

// CreateDatabaseOptions holds optional parameters for CreateDatabase.
type CreateDatabaseOptions struct {
	Collation     string
	RecoveryModel RecoveryModel
	CompatLevel   CompatibilityLevel
}

// DropDatabase drops the named database (equivalent to DROP DATABASE).
func (s *Server) DropDatabase(name string, force bool) error {
	if force {
		_, err := s.db.ExecContext(context.Background(),
			fmt.Sprintf("ALTER DATABASE [%s] SET SINGLE_USER WITH ROLLBACK IMMEDIATE", name))
		if err != nil {
			return fmt.Errorf("gosmo: set single user: %w", err)
		}
	}
	_, err := s.db.ExecContext(context.Background(),
		fmt.Sprintf("DROP DATABASE [%s]", name))
	if err != nil {
		return fmt.Errorf("gosmo: drop database %q: %w", name, err)
	}
	return nil
}

// ── Logins ────────────────────────────────────────────────────────────────────

// Logins returns all server-level logins.
func (s *Server) Logins() ([]*Login, error) {
	const q = `
SELECT name, sid, type_desc, is_disabled, default_database_name,
       create_date, modify_date
FROM   sys.server_principals
WHERE  type IN ('S','U','G')
ORDER  BY name`

	rows, err := s.db.QueryContext(context.Background(), q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list logins: %w", err)
	}
	defer rows.Close()

	var logins []*Login
	for rows.Next() {
		l := &Login{server: s}
		var defDB sql.NullString
		if err := rows.Scan(&l.Name, &l.SID, &l.LoginType, &l.IsDisabled, &defDB,
			&l.CreateDate, &l.ModifyDate); err != nil {
			return nil, err
		}
		l.DefaultDatabase = defDB.String
		logins = append(logins, l)
	}
	return logins, rows.Err()
}

// CreateLogin creates a SQL Server login. Pass an empty password for Windows logins.
func (s *Server) CreateLogin(name, password string, opts *CreateLoginOptions) error {
	if opts == nil {
		opts = &CreateLoginOptions{}
	}
	var q string
	if password != "" {
		q = fmt.Sprintf("CREATE LOGIN [%s] WITH PASSWORD = N'%s'", name, escapeSingle(password))
		if opts.MustChange {
			q += " MUST_CHANGE"
		}
		if opts.DefaultDatabase != "" {
			q += fmt.Sprintf(", DEFAULT_DATABASE = [%s]", opts.DefaultDatabase)
		}
	} else {
		q = fmt.Sprintf("CREATE LOGIN [%s] FROM WINDOWS", name)
		if opts.DefaultDatabase != "" {
			q += fmt.Sprintf(" WITH DEFAULT_DATABASE = [%s]", opts.DefaultDatabase)
		}
	}
	_, err := s.db.ExecContext(context.Background(), q)
	if err != nil {
		return fmt.Errorf("gosmo: create login %q: %w", name, err)
	}
	return nil
}

// CreateLoginOptions holds optional parameters for CreateLogin.
type CreateLoginOptions struct {
	DefaultDatabase string
	MustChange      bool
}

// DropLogin drops a server login.
func (s *Server) DropLogin(name string) error {
	_, err := s.db.ExecContext(context.Background(),
		fmt.Sprintf("DROP LOGIN [%s]", name))
	if err != nil {
		return fmt.Errorf("gosmo: drop login %q: %w", name, err)
	}
	return nil
}

// ── Server roles ──────────────────────────────────────────────────────────────

// ServerRole represents a server-level role.
type ServerRole struct {
	Name        string
	IsFixedRole bool
	Members     []string
}

// ServerRoles returns all fixed and user-defined server roles.
func (s *Server) ServerRoles() ([]*ServerRole, error) {
	const q = `
SELECT r.name, r.is_fixed_role,
       STUFF((SELECT ', ' + m.name
              FROM   sys.server_role_members rm
              JOIN   sys.server_principals m ON m.principal_id = rm.member_principal_id
              WHERE  rm.role_principal_id = r.principal_id
              FOR XML PATH('')), 1, 2, '') AS members
FROM   sys.server_principals r
WHERE  r.type = 'R'
ORDER  BY r.name`

	rows, err := s.db.QueryContext(context.Background(), q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list server roles: %w", err)
	}
	defer rows.Close()

	var roles []*ServerRole
	for rows.Next() {
		r := &ServerRole{}
		var members sql.NullString
		if err := rows.Scan(&r.Name, &r.IsFixedRole, &members); err != nil {
			return nil, err
		}
		if members.Valid && members.String != "" {
			r.Members = strings.Split(members.String, ", ")
		}
		roles = append(roles, r)
	}
	return roles, rows.Err()
}

// ── Linked servers ────────────────────────────────────────────────────────────

// LinkedServer represents a linked server definition.
type LinkedServer struct {
	Name       string
	Product    string
	Provider   string
	DataSource string
	IsRemote   bool
}

// LinkedServers returns all linked servers defined on this instance.
func (s *Server) LinkedServers() ([]*LinkedServer, error) {
	const q = `
SELECT name, product, provider, data_source, is_remote_login_enabled
FROM   sys.servers
WHERE  is_linked = 1
ORDER  BY name`

	rows, err := s.db.QueryContext(context.Background(), q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list linked servers: %w", err)
	}
	defer rows.Close()

	var ls []*LinkedServer
	for rows.Next() {
		l := &LinkedServer{}
		var ds sql.NullString
		if err := rows.Scan(&l.Name, &l.Product, &l.Provider, &ds, &l.IsRemote); err != nil {
			return nil, err
		}
		l.DataSource = ds.String
		ls = append(ls, l)
	}
	return ls, rows.Err()
}

// ── Helper ────────────────────────────────────────────────────────────────────

func escapeSingle(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
