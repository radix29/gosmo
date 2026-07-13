package gosmo

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	mssql "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/azuread"
)

// goos is runtime.GOOS, indirected so tests can exercise the platform-
// dependent AuthWindows path (native SSPI on Windows, Kerberos elsewhere).
var goos = runtime.GOOS

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

// ConnectionOptions holds every parameter needed to open a connection.
//
// Authentication quick guide:
//
// SQL Server login:
//
//	Auth: AuthSQLServer, User: "sa", Password: "..."
//
// Windows / Kerberos (on-premises, domain-joined host):
//
//	Auth: AuthWindows (no User/Password needed)
//
// On Linux/macOS this uses Kerberos; run "kinit" first for single sign-on,
// or set the Kerberos field for a keytab, realm, or custom krb5.conf.
//
// Azure Managed Identity (system-assigned):
//
//	Auth: AuthEntraMSI, Server: "myserver.database.windows.net"
//
// Azure Managed Identity (user-assigned):
//
//	Auth: AuthEntraMSI, ClientID: "<managed-identity-client-id>"
//
// Service Principal (client secret):
//
//	Auth: AuthEntraServicePrincipal
//	User: "<app-client-id>[@<tenant-id>]", Password: "<client-secret>"
//	TenantID: "<tenant-id>"
//
// Service Principal (certificate):
//
//	Auth: AuthEntraServicePrincipal
//	User: "<app-client-id>[@<tenant-id>]", ClientCertPath: "/path/to/cert.pem"
//
// Default credential chain (env vars -> MSI -> AzCLI):
//
//	Auth: AuthEntraDefault
//
// Azure CLI credential (az login):
//
//	Auth: AuthEntraAzCLI
type ConnectionOptions struct {
	// -- Target ------------------------------------------------------------------

	// Server is the host[:port] or host\instance, e.g. "localhost:1433" or
	// "myserver.database.windows.net". Required.
	Server string

	// Database to connect to initially. Defaults to "master".
	Database string

	// -- Authentication ----------------------------------------------------------

	// Auth selects the authentication strategy. Defaults to AuthSQLServer.
	Auth AuthMethod

	// User is the SQL Server login, Windows UPN, or Entra app client ID,
	// depending on the Auth method chosen.
	User string

	// Password is the SQL Server password, Entra user password, or client secret.
	Password string

	// TenantID is the Entra tenant (directory) ID. Required for service principal
	// methods when the tenant differs from the server tenant.
	TenantID string

	// ClientID selects a user-assigned Managed Identity when Auth=AuthEntraMSI.
	ClientID string

	// ClientCertPath is the path to a PEM/PFX certificate for
	// Auth=AuthEntraServicePrincipal certificate-based auth.
	ClientCertPath string

	// ClientCertPassword is the private-key password for ClientCertPath.
	ClientCertPassword string

	// AccessToken is a pre-acquired bearer token for
	// Auth=AuthEntraServicePrincipalAccessToken or AuthEntraOnBehalfOf.
	// It is embedded once at connect time; prefer AccessTokenProvider when
	// the token can expire during the connection's lifetime.
	AccessToken string

	// AccessTokenProvider, when set, is called to obtain a bearer token for
	// each new pooled connection, so tokens that expire (e.g. Entra tokens,
	// good for ~1 hour) are refreshed automatically rather than embedded
	// once and going stale. It takes precedence over AccessToken and Auth:
	// the token is presented directly to SQL Server, bypassing the fedauth
	// DSN machinery, so it works for any scenario where the caller mints
	// its own tokens. The error it returns aborts the connection attempt.
	AccessTokenProvider func(ctx context.Context) (string, error)

	// ApplicationClientID is the AAD enterprise application client ID registered
	// by the tenant admin to allow interactive / device-code flows.
	ApplicationClientID string

	// ServerSPN overrides the Kerberos service principal name of the target
	// instance (e.g. "MSSQLSvc/host.contoso.com:1433"). Used only with
	// AuthWindows. Leave empty to let the driver derive it from the address,
	// which is correct for most host:port connections.
	ServerSPN string

	// Kerberos configures Active Directory authentication for AuthWindows.
	// It matters mainly on non-Windows hosts, where AuthWindows authenticates
	// via Kerberos rather than native SSPI; see KerberosOptions. The zero
	// value uses the host's ambient Kerberos setup (krb5.conf + kinit cache).
	Kerberos KerberosOptions

	// -- TLS / encryption --------------------------------------------------------

	// Encrypt controls the encryption mode.
	// "" - driver default (true for Azure endpoints, false otherwise)
	// "true" - always encrypt
	// "false" - no encryption
	// "disable" - no encryption (legacy alias)
	// "strict" - TDS 8.0 strict encryption
	Encrypt string

	// TrustServerCertificate disables TLS certificate validation.
	// Handy for dev/local instances; do not use in production.
	TrustServerCertificate bool

	// HostNameInCertificate overrides the expected server name in the TLS cert.
	// Useful when connecting via IP address or when the cert CN differs.
	HostNameInCertificate string

	// -- Entra-specific options --------------------------------------------------

	// DisableInstanceDiscovery disables OIDC instance discovery.
	// Set true only for disconnected or private clouds (e.g. Azure Stack).
	DisableInstanceDiscovery bool

	// SendCertificateChain controls whether the full certificate chain is sent
	// in token requests (needed for Subject Name/Issuer SNI auth).
	SendCertificateChain bool

	// TokenFilePath is the path to the Kubernetes service account token file
	// for Auth=AuthEntraMSI (Workload Identity).
	TokenFilePath string

	// -- Connection pool ---------------------------------------------------------

	// ConnectTimeout is the maximum time to wait for the initial connection.
	// Defaults to 30s when zero.
	ConnectTimeout time.Duration

	// ApplicationName is shown in sys.dm_exec_sessions.program_name.
	// Defaults to "gosmo".
	ApplicationName string

	// MaxOpenConns is the maximum number of open connections in the pool.
	// 0 means unlimited.
	MaxOpenConns int

	// MaxIdleConns is the maximum number of idle connections kept in the pool.
	// Defaults to 2.
	MaxIdleConns int

	// ConnMaxLifetime is the maximum lifetime of a pooled connection.
	// 0 means unlimited.
	ConnMaxLifetime time.Duration

	// SessionInitSQL is T-SQL executed on every pooled connection right
	// after it is reset, before the first query runs on it. Use it to apply
	// SET options that must hold for the whole session (the equivalent of
	// SSMS's Query Execution options), e.g. "SET ARITHABORT ON; SET
	// ANSI_NULLS ON". Leave empty for driver defaults.
	SessionInitSQL string
}

// Connect opens a connection to a SQL Server instance and returns a Server.
// The driver and DSN are chosen automatically based on opts.Auth.
func Connect(opts ConnectionOptions) (*Server, error) {
	ctx := context.Background()
	if opts.ConnectTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.ConnectTimeout)
		defer cancel()
	}
	return ConnectContext(ctx, opts)
}

// ConnectContext is the context-aware variant of Connect.
// The context governs the initial ping and server-info load only;
// subsequent calls each carry their own context.
func ConnectContext(ctx context.Context, opts ConnectionOptions) (*Server, error) {
	applyDefaults(&opts)

	connector, err := buildConnector(opts)
	if err != nil {
		return nil, err
	}
	pool := sql.OpenDB(connector)

	// Pool tuning
	if opts.MaxOpenConns > 0 {
		pool.SetMaxOpenConns(opts.MaxOpenConns)
	}
	pool.SetMaxIdleConns(opts.MaxIdleConns)
	if opts.ConnMaxLifetime > 0 {
		pool.SetConnMaxLifetime(opts.ConnMaxLifetime)
	}

	if err = pool.PingContext(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("gosmo: ping: %w", err)
	}

	s := &Server{db: pool}
	if err = s.loadInfo(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// applyDefaults fills zero-value fields with sensible defaults.
func applyDefaults(opts *ConnectionOptions) {
	if opts.ConnectTimeout == 0 {
		opts.ConnectTimeout = 30 * time.Second
	}
	if opts.ApplicationName == "" {
		opts.ApplicationName = "gosmo"
	}
	if opts.Database == "" {
		opts.Database = "master"
	}
	if opts.MaxIdleConns == 0 {
		opts.MaxIdleConns = 2
	}
}

// ParseServerAddress splits a user-supplied server address into its host,
// named-instance, and port components, accepting every form SSMS itself
// accepts in its "Server name" field:
//
//	host                    host:port                host,port
//	host\instance           host\instance,port
//
// Exported so callers (e.g. a UI layer building its own address/DSN
// preview, or resolving a separate "port" field against a Server string
// that may already carry its own) can reuse the same parsing buildDSN
// relies on, instead of duplicating it.
//
// A malformed trailing port (non-numeric) is left as part of host rather
// than rejected outright — the caller surfaces connection failures via
// the driver's own error, not a separate parse error here.
func ParseServerAddress(server string) (host, instance string, port int) {
	if i := strings.IndexByte(server, '\\'); i >= 0 {
		host, instance = server[:i], server[i+1:]
		// host\instance,port — the port trails the instance name.
		if j := strings.IndexByte(instance, ','); j >= 0 {
			if p, err := strconv.Atoi(instance[j+1:]); err == nil {
				port = p
				instance = instance[:j]
			}
		}
		return host, instance, port
	}

	sep := strings.LastIndexByte(server, ',')
	if sep < 0 {
		sep = strings.LastIndexByte(server, ':')
	}
	if sep < 0 {
		return server, "", 0
	}
	if p, err := strconv.Atoi(server[sep+1:]); err == nil {
		return server[:sep], "", p
	}
	return server, "", 0
}

// buildDSN constructs the DSN URL and a driver-name selector from
// ConnectionOptions. buildConnector uses the selector to pick the matching
// connector constructor: "azuresql" (the go-mssqldb/azuread connector) for
// Entra methods, "sqlserver" (the base connector) for everything else.
func buildDSN(opts ConnectionOptions) (dsn, driverName string, err error) {
	if opts.Server == "" {
		return "", "", fmt.Errorf("gosmo: ConnectionOptions.Server is required")
	}

	// A named instance can't be embedded directly in url.URL.Host — a
	// literal backslash gets percent-escaped and go-mssqldb's own URL-DSN
	// convention (see its splitConnectionStringURL) expects the instance
	// name as a URL path segment instead: sqlserver://host:port/instance.
	host, instance, port := ParseServerAddress(opts.Server)
	dialHost := host
	if port > 0 {
		dialHost = fmt.Sprintf("%s:%d", host, port)
	}

	q := commonDSNValues(opts)

	if opts.Auth.isEntraMethod() {
		// -- Entra ID (Azure AD) path: uses the "azuresql" driver --
		driverName = "azuresql"

		fv, ok := fedauthValue[opts.Auth]
		if !ok {
			return "", "", fmt.Errorf("gosmo: unsupported auth method %d", opts.Auth)
		}
		q.Set("fedauth", fv)

		// Per-method extra parameters
		switch opts.Auth {
		case AuthEntraMSI:
			if opts.ClientID != "" {
				// user-assigned managed identity
				q.Set("user id", opts.ClientID)
			}
			if opts.TokenFilePath != "" {
				q.Set("tokenfilepath", opts.TokenFilePath)
			}

		case AuthEntraServicePrincipal:
			// user id = <clientID>[@<tenantID>]
			if opts.TenantID != "" {
				q.Set("user id", opts.User+"@"+opts.TenantID)
			} else {
				q.Set("user id", opts.User)
			}
			if opts.ClientCertPath != "" {
				q.Set("clientcertpath", opts.ClientCertPath)
				if opts.ClientCertPassword != "" {
					q.Set("password", opts.ClientCertPassword)
				}
			} else {
				q.Set("password", opts.Password)
			}
			if opts.SendCertificateChain {
				q.Set("sendcertificatechain", "true")
			}

		case AuthEntraServicePrincipalAccessToken, AuthEntraOnBehalfOf:
			if opts.AccessToken == "" {
				return "", "", fmt.Errorf("gosmo: AccessToken is required for %d", opts.Auth)
			}
			q.Set("password", opts.AccessToken)

		case AuthEntraPassword:
			q.Set("user id", opts.User)
			q.Set("password", opts.Password)

		case AuthEntraInteractive, AuthEntraDeviceCode:
			if opts.ApplicationClientID != "" {
				q.Set("applicationclientid", opts.ApplicationClientID)
			}

		case AuthEntraAzurePipelines:
			// Reads SYSTEM_ACCESSTOKEN / SYSTEM_OIDCREQUESTURI from env
			if opts.ApplicationClientID != "" {
				q.Set("applicationclientid", opts.ApplicationClientID)
			}
		}

		if opts.DisableInstanceDiscovery {
			q.Set("disableinstancediscovery", "true")
		}
		if opts.TenantID != "" {
			// Some flows need the tenant in the URL query too
			q.Set("tenantid", opts.TenantID)
		}

		u := &url.URL{
			Scheme:   "sqlserver",
			Host:     dialHost,
			RawQuery: q.Encode(),
		}
		if instance != "" {
			u.Path = "/" + instance
		}
		return u.String(), driverName, nil
	}

	// -- Classic path: SQL Server auth or Windows auth --
	driverName = "sqlserver"
	u := &url.URL{
		Scheme: "sqlserver",
		Host:   dialHost,
	}
	if instance != "" {
		u.Path = "/" + instance
	}

	switch opts.Auth {
	case AuthWindows:
		if opts.ServerSPN != "" {
			q.Set("serverspn", opts.ServerSPN)
		}
		// Windows has native SSPI; every other platform authenticates the
		// Windows/AD identity through Kerberos. Kerberos is also used on
		// Windows when the caller configures it explicitly.
		if goos != "windows" || opts.Kerberos.configured() {
			q.Set("authenticator", "krb5")
			opts.Kerberos.applyDSN(q)
			// User + Password selects Kerberos's username/password login;
			// a bare User (optionally "user@REALM") rides along for the
			// keytab / credential-cache logins.
			switch {
			case opts.User != "" && opts.Password != "":
				u.User = url.UserPassword(opts.User, opts.Password)
			case opts.User != "":
				u.User = url.User(opts.User)
			}
		} else if opts.User != "" {
			// Native SSPI; user may optionally supply DOMAIN\user.
			u.User = url.User(opts.User)
		}
	default: // AuthSQLServer
		if opts.User != "" || opts.Password != "" {
			u.User = url.UserPassword(opts.User, opts.Password)
		}
	}

	u.RawQuery = q.Encode()
	return u.String(), driverName, nil
}

// commonDSNValues builds the query parameters shared by every DSN,
// regardless of authentication method: target database, application name,
// connection timeout, and the TLS options.
func commonDSNValues(opts ConnectionOptions) url.Values {
	q := url.Values{}
	q.Set("database", opts.Database)
	q.Set("app name", opts.ApplicationName)
	q.Set("connection timeout", strconv.Itoa(int(opts.ConnectTimeout.Seconds())))

	if opts.TrustServerCertificate {
		q.Set("TrustServerCertificate", "true")
	}
	if opts.Encrypt != "" {
		q.Set("encrypt", opts.Encrypt)
	}
	if opts.HostNameInCertificate != "" {
		q.Set("hostNameInCertificate", opts.HostNameInCertificate)
	}
	return q
}

// baseDSN builds a plain "sqlserver" DSN carrying only the target and the
// common connection parameters, with no authentication. Used for the
// access-token-provider path, where the bearer token is supplied out of
// band by the connector rather than through the DSN.
func baseDSN(opts ConnectionOptions) (string, error) {
	if opts.Server == "" {
		return "", fmt.Errorf("gosmo: ConnectionOptions.Server is required")
	}
	host, instance, port := ParseServerAddress(opts.Server)
	dialHost := host
	if port > 0 {
		dialHost = fmt.Sprintf("%s:%d", host, port)
	}
	u := &url.URL{
		Scheme:   "sqlserver",
		Host:     dialHost,
		RawQuery: commonDSNValues(opts).Encode(),
	}
	if instance != "" {
		u.Path = "/" + instance
	}
	return u.String(), nil
}

// buildConnector builds the driver connector ConnectContext opens the pool
// with. Using a connector (rather than sql.Open on a DSN string) is what
// lets gosmo hand the driver a token-refresh callback and per-session init
// SQL that a string DSN can't express.
//
// The connector is chosen from opts:
//   - AccessTokenProvider set: the caller mints (and refreshes) bearer
//     tokens itself, so the token is presented straight to SQL Server via a
//     security-token connector, bypassing the fedauth DSN machinery. This
//     wins over Auth and AccessToken.
//   - an Entra Auth method: the azuread connector, which reads the fedauth
//     parameters from the DSN.
//   - otherwise: the base sqlserver connector.
func buildConnector(opts ConnectionOptions) (*mssql.Connector, error) {
	var (
		connector *mssql.Connector
		err       error
	)

	switch {
	case opts.AccessTokenProvider != nil:
		var dsn string
		if dsn, err = baseDSN(opts); err != nil {
			return nil, err
		}
		connector, err = mssql.NewConnectorWithAccessTokenProvider(dsn, opts.AccessTokenProvider)

	default:
		var dsn, driverName string
		if dsn, driverName, err = buildDSN(opts); err != nil {
			return nil, err
		}
		if driverName == "azuresql" {
			connector, err = azuread.NewConnector(dsn)
		} else {
			connector, err = mssql.NewConnector(dsn)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: build connector: %w", err)
	}

	connector.SessionInitSQL = opts.SessionInitSQL
	return connector, nil
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
	if err := s.db.QueryRowContext(ctx, "SELECT DB_NAME()").Scan(&name); err != nil {
		return "", fmt.Errorf("gosmo: current database: %w", err)
	}
	return name, nil
}

// -- Internal helpers ----------------------------------------------------------

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
		@@VERSION,
		osi.physical_memory_kb / 1024,
		osi.cpu_count,
		SERVERPROPERTY('InstanceDefaultDataPath'),
		SERVERPROPERTY('InstanceDefaultLogPath'),
		SERVERPROPERTY('InstanceDefaultBackupPath')
	FROM sys.dm_os_sys_info osi`

	row := s.db.QueryRowContext(ctx, q)
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

	parts := strings.SplitN(info.ProductVersion, ".", 4)
	if len(parts) >= 3 {
		info.VersionMajor, _ = strconv.Atoi(parts[0])
		info.VersionMinor, _ = strconv.Atoi(parts[1])
		info.VersionBuild, _ = strconv.Atoi(parts[2])
	}
	s.info = info
	return nil
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
	       compatibility_level, collation_name, is_read_only, create_date
	FROM sys.databases
	ORDER BY name`

	rows, err := s.db.QueryContext(ctx, q)
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

// DatabaseByName returns a single database by name.
func (s *Server) DatabaseByName(name string) (*Database, error) {
	return s.DatabaseByNameContext(context.Background(), name)
}

// DatabaseByNameContext is the context-aware variant of DatabaseByName.
func (s *Server) DatabaseByNameContext(ctx context.Context, name string) (*Database, error) {
	const q = `
	SELECT name, database_id, state_desc, recovery_model_desc,
	       compatibility_level, collation_name, is_read_only, create_date
	FROM sys.databases
	WHERE name = @p1`

	d := &Database{server: s}
	var state, recovery, collation sql.NullString
	var compatLevel sql.NullInt64

	row := s.db.QueryRowContext(ctx, q, name)
	if err := row.Scan(
		&d.name, &d.id, &state, &recovery,
		&compatLevel, &collation, &d.isReadOnly, &d.createDate,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("gosmo: database %q not found", name)
		}
		return nil, fmt.Errorf("gosmo: database by name: %w", err)
	}
	d.state = state.String
	d.recoveryModel = RecoveryModel(recovery.String)
	d.compatLevel = CompatibilityLevel(compatLevel.Int64)
	d.collation = collation.String
	return d, nil
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

	var sb strings.Builder
	fmt.Fprintf(&sb, "CREATE DATABASE %s", quoteIdent(name))
	if opts.Collation != "" {
		fmt.Fprintf(&sb, " COLLATE %s", opts.Collation)
	}
	if err := s.execContext(ctx, sb.String()); err != nil {
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

// CreateDatabaseOptions holds optional parameters for CreateDatabase.
type CreateDatabaseOptions struct {
	Collation     string
	RecoveryModel RecoveryModel
	CompatLevel   CompatibilityLevel
}

// DropDatabase drops the named database.
// When force is true, active connections are terminated first.
func (s *Server) DropDatabase(name string, force bool) error {
	return s.DropDatabaseContext(context.Background(), name, force)
}

// DropDatabaseContext is the context-aware variant of DropDatabase.
func (s *Server) DropDatabaseContext(ctx context.Context, name string, force bool) error {
	if name == "" {
		return fmt.Errorf("gosmo: drop database: name is required")
	}
	if force {
		if err := s.execContext(ctx,
			fmt.Sprintf("ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE", quoteIdent(name)),
		); err != nil {
			return fmt.Errorf("gosmo: set single user on %q: %w", name, err)
		}
	}
	if err := s.execContext(ctx, fmt.Sprintf("DROP DATABASE %s", quoteIdent(name))); err != nil {
		return fmt.Errorf("gosmo: drop database %q: %w", name, err)
	}
	return nil
}

// -- Logins --------------------------------------------------------------------

// Logins returns all server-level logins.
func (s *Server) Logins() ([]*Login, error) {
	return s.LoginsContext(context.Background())
}

// LoginsContext is the context-aware variant of Logins.
func (s *Server) LoginsContext(ctx context.Context) ([]*Login, error) {
	const q = `
	SELECT name, sid, type_desc, is_disabled, default_database_name,
	       create_date, modify_date
	FROM sys.server_principals
	WHERE type IN ('S','U','G')
	ORDER BY name`

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list logins: %w", err)
	}
	defer rows.Close()

	var logins []*Login
	for rows.Next() {
		l := &Login{server: s}
		var defDB sql.NullString
		if err := rows.Scan(&l.Name, &l.SID, &l.LoginType, &l.IsDisabled,
			&defDB, &l.CreateDate, &l.ModifyDate); err != nil {
			return nil, err
		}
		l.DefaultDatabase = defDB.String
		logins = append(logins, l)
	}
	return logins, rows.Err()
}

// LoginByName returns a single server-level login by name.
func (s *Server) LoginByName(name string) (*Login, error) {
	return s.LoginByNameContext(context.Background(), name)
}

// LoginByNameContext is the context-aware variant of LoginByName.
func (s *Server) LoginByNameContext(ctx context.Context, name string) (*Login, error) {
	const q = `
	SELECT name, sid, type_desc, is_disabled, default_database_name,
	       create_date, modify_date
	FROM sys.server_principals
	WHERE type IN ('S','U','G') AND name = @p1`

	l := &Login{server: s}
	var defDB sql.NullString

	row := s.db.QueryRowContext(ctx, q, name)
	if err := row.Scan(&l.Name, &l.SID, &l.LoginType, &l.IsDisabled,
		&defDB, &l.CreateDate, &l.ModifyDate); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("gosmo: login %q not found", name)
		}
		return nil, fmt.Errorf("gosmo: find login %q: %w", name, err)
	}
	l.DefaultDatabase = defDB.String
	return l, nil
}

// CreateLogin creates a SQL Server or Windows login.
// Pass an empty password to create a Windows login (FROM WINDOWS).
func (s *Server) CreateLogin(name, password string, opts *CreateLoginOptions) error {
	return s.CreateLoginContext(context.Background(), name, password, opts)
}

// CreateLoginContext is the context-aware variant of CreateLogin.
//
// Security: the password is never interpolated into the SQL string.
// Instead it is encoded as a UTF-16LE hex literal and passed with the
// HASHED keyword via a pre-computed binary value, which is injection-proof
// regardless of the password content.
func (s *Server) CreateLoginContext(ctx context.Context, name, password string, opts *CreateLoginOptions) error {
	if name == "" {
		return fmt.Errorf("gosmo: create login: name is required")
	}
	if opts == nil {
		opts = &CreateLoginOptions{}
	}

	var sb strings.Builder

	if password != "" {
		// Encode the password as a 0x... hex literal so no quoting or
		// escaping is needed and any byte sequence is safe.
		pwHex := passwordHexLiteral(password)
		fmt.Fprintf(&sb, "CREATE LOGIN %s WITH PASSWORD = %s HASHED", quoteIdent(name), pwHex)
		if opts.MustChange {
			sb.WriteString(" MUST_CHANGE")
		}
		if opts.DefaultDatabase != "" {
			fmt.Fprintf(&sb, ", DEFAULT_DATABASE = %s", quoteIdent(opts.DefaultDatabase))
		}
	} else {
		fmt.Fprintf(&sb, "CREATE LOGIN %s FROM WINDOWS", quoteIdent(name))
		if opts.DefaultDatabase != "" {
			fmt.Fprintf(&sb, " WITH DEFAULT_DATABASE = %s", quoteIdent(opts.DefaultDatabase))
		}
	}

	if err := s.execContext(ctx, sb.String()); err != nil {
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
	return s.DropLoginContext(context.Background(), name)
}

// DropLoginContext is the context-aware variant of DropLogin.
func (s *Server) DropLoginContext(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("gosmo: drop login: name is required")
	}
	if err := s.execContext(ctx, fmt.Sprintf("DROP LOGIN %s", quoteIdent(name))); err != nil {
		return fmt.Errorf("gosmo: drop login %q: %w", name, err)
	}
	return nil
}

// -- Server roles --------------------------------------------------------------

// ServerRole represents a server-level role.
type ServerRole struct {
	Name        string
	IsFixedRole bool
	Members     []string
}

// ServerRoles returns all fixed and user-defined server roles.
func (s *Server) ServerRoles() ([]*ServerRole, error) {
	return s.ServerRolesContext(context.Background())
}

// ServerRolesContext is the context-aware variant of ServerRoles.
func (s *Server) ServerRolesContext(ctx context.Context) ([]*ServerRole, error) {
	const q = `
	SELECT r.name, r.is_fixed_role,
	       STUFF((SELECT ', ' + m.name
	              FROM sys.server_role_members rm
	              JOIN sys.server_principals m ON m.principal_id = rm.member_principal_id
	              WHERE rm.role_principal_id = r.principal_id
	              FOR XML PATH(''), TYPE).value('.','NVARCHAR(MAX)'), 1, 2, '') AS members
	FROM sys.server_principals r
	WHERE r.type = 'R'
	ORDER BY r.name`

	rows, err := s.db.QueryContext(ctx, q)
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

// -- Linked servers ------------------------------------------------------------

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
	return s.LinkedServersContext(context.Background())
}

// LinkedServersContext is the context-aware variant of LinkedServers.
func (s *Server) LinkedServersContext(ctx context.Context) ([]*LinkedServer, error) {
	const q = `
	SELECT name, product, provider, data_source, is_remote_login_enabled
	FROM sys.servers
	WHERE is_linked = 1
	ORDER BY name`

	rows, err := s.db.QueryContext(ctx, q)
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

// -- Password helpers ----------------------------------------------------------

// passwordHexLiteral encodes a plaintext password as a T-SQL 0x... binary
// literal in UTF-16LE, the encoding SQL Server's "WITH PASSWORD = 0x<hex>
// HASHED" form expects for a cleartext password passed as binary. The
// result can be spliced directly into a DDL statement with no quoting or
// escaping needed.
func passwordHexLiteral(password string) string {
	// Encode as UTF-16LE — the wire encoding SQL Server uses internally.
	runes := []rune(password)
	u16 := utf16.Encode(runes)
	buf := make([]byte, len(u16)*2)
	for i, r := range u16 {
		buf[i*2] = byte(r)
		buf[i*2+1] = byte(r >> 8)
	}
	return "0x" + strings.ToUpper(hex.EncodeToString(buf))
}
