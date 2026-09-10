package gosmo

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
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
//	User: "<app-client-id>", TenantID: "<tenant-id>", Password: "<client-secret>"
//
// (or User: "<app-client-id>@<tenant-id>" with no TenantID — not both, unless
// they name the same tenant).
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

	// User is the SQL Server login, Windows UPN, Entra user UPN (the sign-in
	// name for AuthEntraPassword, a login hint for AuthEntraInteractive), or
	// Entra application client ID (service principal, on-behalf-of, Azure
	// Pipelines), depending on the Auth method chosen.
	User string

	// Password is the SQL Server password, Entra user password, or client secret.
	Password string

	// TenantID is the Entra tenant (directory) ID to sign in to, for every
	// Entra method whose credential takes one — all but AuthEntraMSI and
	// AuthEntraServicePrincipalAccessToken, which ignore it. Left empty,
	// AuthEntraServicePrincipal, AuthEntraOnBehalfOf, AuthEntraAzurePipelines,
	// AuthEntraPassword, AuthEntraInteractive and AuthEntraDeviceCode sign in
	// to the tenant the server names at login, as SSMS does; the rest use
	// their credential's own default (the CLI's signed-in tenant, or the
	// environment's). For the first three it also travels in the DSN as the
	// "@tenant" suffix of the client ID.
	TenantID string

	// ClientID selects a user-assigned Managed Identity when Auth=AuthEntraMSI.
	// It is not the application client ID of the other Entra methods: that is
	// User for service principal, on-behalf-of and Azure Pipelines, and
	// ApplicationClientID for the public-client (human) flows.
	ClientID string

	// ClientCertPath is the path to a PEM/PFX certificate for
	// Auth=AuthEntraServicePrincipal certificate-based auth.
	ClientCertPath string

	// ClientCertPassword is the private-key password for ClientCertPath.
	ClientCertPassword string

	// AccessToken is a pre-acquired bearer token for
	// Auth=AuthEntraServicePrincipalAccessToken, or the inbound user assertion
	// for AuthEntraOnBehalfOf. The former is embedded once at connect time;
	// prefer AccessTokenProvider when the token can expire during the
	// connection's lifetime.
	AccessToken string

	// AccessTokenProvider, when set, is called to obtain a bearer token for
	// each new pooled connection, so tokens that expire (e.g. Entra tokens,
	// good for ~1 hour) are refreshed automatically rather than embedded
	// once and going stale. It takes precedence over AccessToken and Auth:
	// the token is presented directly to SQL Server, bypassing the fedauth
	// DSN machinery, so it works for any scenario where the caller mints
	// its own tokens. The error it returns aborts the connection attempt.
	AccessTokenProvider func(ctx context.Context) (string, error)

	// ApplicationClientID is the client ID of the public client application
	// the human sign-in flows go through: AuthEntraPassword,
	// AuthEntraInteractive and AuthEntraDeviceCode. Set it when the tenant
	// requires its own app registration; empty uses the public client
	// azidentity defaults to (04b07795-8ddb-461a-bbee-02f9e1bf7b46). Ignored
	// by every other method.
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
	// "true" / "mandatory" - always encrypt
	// "false" / "optional" - encrypt the login packet only
	// "disable" - no encryption at all, not even the login
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

	// TokenFilePath is the path to a Kubernetes service account token file
	// for Workload Identity. go-mssqldb reads it only for its
	// ActiveDirectoryWorkloadIdentity workflow, which no AuthMethod selects
	// yet: it is written to the DSN for AuthEntraMSI, and has no effect there.
	TokenFilePath string

	// EntraCache holds the Entra credentials and tokens connections share, so
	// that an Entra method signs in once per identity rather than once per
	// physical connection — the difference between one browser sign-in and one
	// per pooled connection for AuthEntraInteractive. Share one across every
	// Connect that should share a sign-in, and call its Warm first to sign in
	// under a context of your choosing. Nil gives the Server a private cache of
	// its own: its connections share a sign-in, other Servers' do not.
	EntraCache *EntraCache

	// DeviceCodePrompt, when set, is called with the code and URL the user must
	// visit for AuthEntraDeviceCode, in place of azidentity's default of
	// printing them to standard output — which a terminal UI, a GUI or a
	// service cannot show. It runs on the goroutine that is connecting (or
	// warming the cache) and should return promptly; sign-in completes, or
	// times out with that goroutine's context, after it returns. A credential
	// an EntraCache shares uses the prompt of the most recent connection
	// through it that set one.
	DeviceCodePrompt func(ctx context.Context, m DeviceCodeMessage) error

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

	// ConnMaxIdleTime is the maximum time a pooled connection may sit idle
	// before it's closed and evicted rather than handed out again. This is
	// what guards against a connection silently dropped while idle — a
	// firewall/NAT idle timeout, a load balancer, or the server itself
	// closing a long-idle session — sitting in the pool looking usable
	// until something actually tries it and fails. Defaults to 5 minutes
	// when zero.
	ConnMaxIdleTime time.Duration

	// Dialer, when set, is used for every network operation the driver
	// performs — the TDS connection itself and the SQL Server Browser probe
	// that resolves a named instance's port. Use it to route connections
	// through a proxy or an SSH tunnel, or to control address selection.
	// A dialer that also implements mssql.HostDialer has DNS resolved on its
	// own network.
	//
	// Leave it nil for the default, which is the driver's own dialer except
	// when Server names an instance with no port: that case needs a Browser
	// probe, and gosmo substitutes a dialer that sends it to every resolved
	// address rather than only the first, and reuses a reply for up to two
	// minutes rather than probing for every new pooled connection — any
	// failed connection attempt discards it (see dialer.go).
	Dialer mssql.Dialer

	// SessionInitSQL is T-SQL executed on every pooled connection right
	// after it is reset, before the first query runs on it. Use it to apply
	// SET options that must hold for the whole session (the equivalent of
	// SSMS's Query Execution options), e.g. "SET ARITHABORT ON; SET
	// ANSI_NULLS ON". Leave empty for driver defaults.
	SessionInitSQL string

	// -- Driver pass-through -------------------------------------------------------

	// ExtraParams carries go-mssqldb connection-string parameters that have
	// no ConnectionOptions field — "packet size", "ApplicationIntent",
	// "MultiSubnetFailover", "dial timeout", "keepalive" and the like. Keys are
	// case-insensitive, as the driver reads them; each key takes exactly one
	// value.
	//
	// A parameter that a ConnectionOptions field controls is refused, never
	// merged: server, port, database, user id and password, app name,
	// connection timeout, the TLS settings, and every authentication
	// parameter (fedauth, authenticator, the krb5-* family, tenant, client and
	// certificate settings), along with their ADO.NET synonyms ("initial
	// catalog", "uid", "trust server certificate", …). Otherwise one of the
	// two would silently override the other, and which one won would depend
	// on the driver's parsing order rather than on anything the caller wrote.
	// A refused key fails Connect and ConnectionString with an error naming it.
	ExtraParams url.Values
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
	pool := sql.OpenDB(poolConnector(connector, opts))

	// Pool tuning
	if opts.MaxOpenConns > 0 {
		pool.SetMaxOpenConns(opts.MaxOpenConns)
	}
	pool.SetMaxIdleConns(opts.MaxIdleConns)
	if opts.ConnMaxLifetime > 0 {
		pool.SetConnMaxLifetime(opts.ConnMaxLifetime)
	}
	pool.SetConnMaxIdleTime(opts.ConnMaxIdleTime)

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

// NewServer wraps an already-open *sql.DB as a Server, loading the same
// server metadata ConnectContext loads. Use it when the pool is not gosmo's
// to open: a connection shared with the rest of an application, a driver
// wrapped for tracing or retries, or a fake driver in a test.
//
// db must be a SQL Server pool — gosmo builds T-SQL and reads system views,
// and nothing here checks the dialect. Ownership passes to the returned
// Server: Close closes db, as it does for a pool Connect opened.
//
// This is the inverse of DB(), and the seam that makes gosmo's read and
// write paths reachable from a caller's tests. Without it a Server can only
// come from a real network connection, so any code that takes one — an
// application's whole database layer — is testable only against a live
// instance.
func NewServer(ctx context.Context, db *sql.DB) (*Server, error) {
	if db == nil {
		return nil, fmt.Errorf("gosmo: new server: db is nil")
	}
	s := &Server{db: db}
	if err := s.loadInfo(ctx); err != nil {
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
	if opts.ConnMaxIdleTime == 0 {
		opts.ConnMaxIdleTime = 5 * time.Minute
	}
}

// ParseServerAddress splits a user-supplied server address into its host,
// named-instance, and port components, accepting every form SSMS itself
// accepts in its "Server name" field:
//
//	host                    host:port                host,port
//	host\instance           host\instance,port
//
// An IPv6 literal is accepted bare (fe80::1), bracketed ([fe80::1]), or
// with a port as [fe80::1]:1433 or fe80::1,1433. A colon is read as a port
// separator only when what precedes it is a host that can carry one — a
// name, an IPv4 address or a bracketed literal — so the last group of a bare
// literal is never mistaken for a port ("2001:db8::5" is a host, not
// "2001:db8:" on port 5). A bracketed host is returned with its brackets.
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
		// Only a host with no colon of its own, or a bracketed IPv6 literal,
		// can be followed by ":port"; in a bare literal every colon is part of
		// the address.
		if sep >= 0 {
			if h := server[:sep]; strings.ContainsRune(h, ':') &&
				!(strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]")) {
				sep = -1
			}
		}
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
	dialHost, instance, err := dsnHost(opts.Server)
	if err != nil {
		return "", "", err
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

		if err := checkEntraFields(opts); err != nil {
			return "", "", err
		}

		// Per-method extra parameters. Every key here is one go-mssqldb's
		// azuread driver reads for that workflow (azuread/configuration.go,
		// validateParameters); auth_test.go runs each through it.
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
			userID, err := entraClientAtTenant(opts)
			if err != nil {
				return "", "", err
			}
			q.Set("user id", userID)
			setEntraAppCredential(q, opts)

		case AuthEntraServicePrincipalAccessToken:
			q.Set("password", opts.AccessToken)

		case AuthEntraOnBehalfOf:
			userID, err := entraClientAtTenant(opts)
			if err != nil {
				return "", "", err
			}
			q.Set("user id", userID)
			q.Set("userassertion", opts.AccessToken)
			setEntraAppCredential(q, opts)

		case AuthEntraPassword:
			q.Set("user id", opts.User)
			q.Set("password", opts.Password)
			q.Set("applicationclientid", cmp.Or(opts.ApplicationClientID, defaultPublicClientID))

		case AuthEntraInteractive:
			q.Set("applicationclientid", cmp.Or(opts.ApplicationClientID, defaultPublicClientID))
			if opts.User != "" {
				q.Set("user id", opts.User) // login hint
			}

		case AuthEntraDeviceCode:
			if opts.ApplicationClientID != "" {
				q.Set("applicationclientid", opts.ApplicationClientID)
			}

		case AuthEntraAzurePipelines:
			// With no User the driver takes the client and tenant from
			// AZURESUBSCRIPTION_CLIENT_ID / _TENANT_ID. "serviceconnectionid"
			// and "systemtoken" are deliberately left to ExtraParams (or the
			// environment) and not reserved: that is the one route by which
			// this method has always connected.
			if opts.User != "" {
				userID, err := entraClientAtTenant(opts)
				if err != nil {
					return "", "", err
				}
				q.Set("user id", userID)
			}
		}

		if opts.DisableInstanceDiscovery {
			q.Set("disableinstancediscovery", "true")
		}
		if opts.TenantID != "" {
			// go-mssqldb's own azuread connector never reads this key (its
			// credential gets a tenant only from "user id"); gosmo's credential
			// does (parseEntraConfig), which is how TenantID reaches the
			// methods with no "user id" to carry it.
			q.Set("tenantid", opts.TenantID)
		}
		if err := mergeExtraParams(q, opts.ExtraParams); err != nil {
			return "", "", err
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

	if err := mergeExtraParams(q, opts.ExtraParams); err != nil {
		return "", "", err
	}
	u.RawQuery = q.Encode()
	return u.String(), driverName, nil
}

// checkEntraFields refuses, in ConnectionOptions vocabulary, options an Entra
// method cannot connect with. Left to the driver, the same mistakes come back
// worded in DSN keys the caller never wrote ("Must provide 'client id[@tenant
// id]' as username parameter").
func checkEntraFields(opts ConnectionOptions) error {
	const appClientID = "User (the application's client ID)"
	var missing string
	switch opts.Auth {
	case AuthEntraPassword:
		switch {
		case opts.User == "":
			missing = "User (the user's UPN)"
		case opts.Password == "":
			missing = "Password"
		}
	case AuthEntraServicePrincipal:
		switch {
		case opts.User == "":
			missing = appClientID
		case opts.Password == "" && opts.ClientCertPath == "":
			missing = "Password (a client secret) or ClientCertPath"
		}
	case AuthEntraServicePrincipalAccessToken:
		if opts.AccessToken == "" {
			missing = "AccessToken"
		}
	case AuthEntraOnBehalfOf:
		switch {
		case opts.User == "":
			missing = appClientID
		case opts.AccessToken == "":
			missing = "AccessToken (the inbound user assertion)"
		case opts.Password == "" && opts.ClientCertPath == "" && !hasExtraParam(opts.ExtraParams, "clientassertion"):
			missing = `Password (a client secret), ClientCertPath, or a "clientassertion" ExtraParams entry`
		}
	case AuthEntraAzurePipelines:
		// The client ID may come from the environment instead, but a TenantID
		// with no User has nowhere to go: the driver reads a tenant only as
		// the "@tenant" suffix of "user id".
		if opts.User == "" && opts.TenantID != "" {
			missing = appClientID + " when TenantID is set"
		}
	}
	if missing != "" {
		return fmt.Errorf("gosmo: %s requires %s", opts.Auth, missing)
	}
	return nil
}

// hasExtraParam reports whether extra carries a non-empty key, matched as the
// driver matches it (case-insensitively).
func hasExtraParam(extra url.Values, key string) bool {
	for k, vs := range extra {
		if strings.EqualFold(strings.TrimSpace(k), key) && len(vs) > 0 && vs[0] != "" {
			return true
		}
	}
	return false
}

// entraClientAtTenant renders the driver's "user id" for the methods that
// read it as "<client id>[@<tenant id>]" (service principal, on-behalf-of,
// Azure Pipelines). The driver splits it at the first '@' — when that is
// neither the first nor the last character — so a User that already carries
// a tenant must not have TenantID appended again ("app@T@T" signs in to
// tenant "T@T"), and one naming a different tenant from TenantID is refused
// rather than silently preferring either.
func entraClientAtTenant(opts ConnectionOptions) (string, error) {
	if at := strings.IndexByte(opts.User, '@'); at >= 1 && at < len(opts.User)-1 {
		if tenant := opts.User[at+1:]; opts.TenantID != "" && !strings.EqualFold(tenant, opts.TenantID) {
			return "", fmt.Errorf("gosmo: %s: User %q names tenant %q, but TenantID is %q",
				opts.Auth, opts.User, tenant, opts.TenantID)
		}
		return opts.User, nil
	}
	if opts.TenantID != "" {
		return opts.User + "@" + opts.TenantID, nil
	}
	return opts.User, nil
}

// setEntraAppCredential writes an application's own credential — a
// certificate (whose private-key password travels as "password") or else a
// client secret — for the service principal and on-behalf-of workflows,
// which read the same keys for it, including "sendcertificatechain".
func setEntraAppCredential(q url.Values, opts ConnectionOptions) {
	switch {
	case opts.ClientCertPath != "":
		q.Set("clientcertpath", opts.ClientCertPath)
		if opts.ClientCertPassword != "" {
			q.Set("password", opts.ClientCertPassword)
		}
	case opts.Password != "":
		q.Set("password", opts.Password)
	}
	if opts.SendCertificateChain {
		q.Set("sendcertificatechain", "true")
	}
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
	dialHost, instance, err := dsnHost(opts.Server)
	if err != nil {
		return "", err
	}
	q := commonDSNValues(opts)
	if err := mergeExtraParams(q, opts.ExtraParams); err != nil {
		return "", err
	}
	u := &url.URL{
		Scheme:   "sqlserver",
		Host:     dialHost,
		RawQuery: q.Encode(),
	}
	if instance != "" {
		u.Path = "/" + instance
	}
	return u.String(), nil
}

// dsnHost renders a ConnectionOptions.Server address as the Host of a
// sqlserver:// URL, plus the instance name that travels as its path.
//
// An IPv6 literal is bracketed there: unbracketed, url.Parse takes its last
// group for a port, or rejects it outright when that group is not numeric.
// The driver removes the brackets again only when a port follows them (it
// splits with net.SplitHostPort), so a literal with no port is given the
// default 1433 explicitly — the port the driver would have dialled anyway.
// A literal with a named instance and no port has no URL form the driver
// reads back correctly: it would carry the brackets into the SQL Browser
// probe's address. That is an error naming the fix, not a dial that fails
// somewhere less obvious.
func dsnHost(server string) (host, instance string, err error) {
	host, instance, port := ParseServerAddress(server)
	if strings.ContainsRune(host, ':') {
		if !strings.HasPrefix(host, "[") {
			host = "[" + host + "]"
		}
		if port == 0 {
			if instance != "" {
				return "", "", fmt.Errorf("gosmo: server %q: an IPv6 address with a named instance needs "+
					"an explicit port (%s\\%s,<port>)", server, strings.Trim(host, "[]"), instance)
			}
			port = 1433
		}
	}
	if port > 0 {
		host = fmt.Sprintf("%s:%d", host, port)
	}
	return host, instance, nil
}

// reservedDSNKeys are the driver parameters a ConnectionOptions field
// controls, lower-cased as the driver compares them — see
// ConnectionOptions.ExtraParams, which may not set any of them. The ADO.NET
// synonyms go-mssqldb maps onto these keys are listed too: the URL form does
// not translate them, so "initial catalog" beside "database" would be an
// unknown key silently ignored rather than an override, which is no better.
// Every key buildDSN, baseDSN and KerberosOptions.applyDSN writes must be
// here; TestReservedDSNKeysCoverEveryKeyGosmoWrites pins it.
var reservedDSNKeys = map[string]bool{
	"server": true, "port": true, "database": true, "user id": true, "password": true,
	"app name": true, "connection timeout": true,
	"encrypt": true, "trustservercertificate": true, "hostnameincertificate": true,
	"fedauth": true, "authenticator": true, "serverspn": true, "tenantid": true,
	"applicationclientid": true, "clientcertpath": true, "tokenfilepath": true,
	"sendcertificatechain": true, "disableinstancediscovery": true, "userassertion": true,

	// ADO.NET synonyms of the above.
	"data source": true, "address": true, "network address": true, "addr": true,
	"initial catalog": true, "user": true, "uid": true, "pwd": true,
	"app": true, "application name": true, "connect timeout": true, "timeout": true,
	"trust server certificate": true, "host name in certificate": true, "server spn": true,
}

// ExtraParamError is the error Connect and ConnectionString return for a
// ConnectionOptions.ExtraParams entry they refuse. Key is the name as the
// caller gave it.
type ExtraParamError struct {
	Key string
	// Reserved reports that a ConnectionOptions field controls Key — the
	// caller should set that field instead. When false the entry itself is
	// malformed: an empty name, more than one value, or the same key twice in
	// different case.
	Reserved bool
	reason   string
}

func (e *ExtraParamError) Error() string {
	return fmt.Sprintf("gosmo: extra connection parameter %q %s", e.Key, e.reason)
}

// mergeExtraParams adds ConnectionOptions.ExtraParams to the DSN query q,
// refusing a key a ConnectionOptions field controls (reservedDSNKeys, plus the
// krb5-* family), a key q already carries, and a key given more than one
// value or given twice in different case — the driver itself rejects the
// last two with a message that does not say where the duplicate came from.
// Every refusal is an *ExtraParamError.
func mergeExtraParams(q, extra url.Values) error {
	const reserved = "is set through a ConnectionOptions field, not ExtraParams"
	seen := make(map[string]bool, len(extra))
	for k, vs := range extra {
		key := strings.ToLower(strings.TrimSpace(k))
		switch {
		case key == "":
			return &ExtraParamError{Key: k, reason: "has an empty name"}
		case reservedDSNKeys[key] || strings.HasPrefix(key, "krb5-"):
			return &ExtraParamError{Key: k, Reserved: true, reason: reserved}
		case seen[key]:
			return &ExtraParamError{Key: k, reason: "is given more than once"}
		case len(vs) != 1:
			return &ExtraParamError{Key: k, reason: fmt.Sprintf("needs exactly one value, has %d", len(vs))}
		}
		for existing := range q {
			if strings.EqualFold(existing, key) {
				return &ExtraParamError{Key: k, Reserved: true, reason: reserved}
			}
		}
		seen[key] = true
		q.Set(key, vs[0])
	}
	return nil
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
//   - an Entra Auth method: a token connector whose credential comes from
//     opts.EntraCache (see entra.go), after the azuread parser has validated
//     the DSN.
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
			cache := opts.EntraCache
			if cache == nil {
				cache = NewEntraCache()
			}
			connector, err = newEntraConnector(dsn, cache, opts.DeviceCodePrompt)
		} else {
			connector, err = mssql.NewConnector(dsn)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: build connector: %w", err)
	}

	connector.SessionInitSQL = opts.SessionInitSQL
	connector.Dialer = dialerFor(opts)
	return connector, nil
}

// maskedSecret stands in for every secret in a masked ConnectionString. A
// fixed placeholder rather than a run of '*' as long as the secret, so the
// masked form does not leak the secret's length.
const maskedSecret = "XXXXX"

// ConnectionString renders the go-mssqldb connection string Connect would
// dial for o: the same builder, after the same defaults, so the result
// carries "database=master", the application name and the connect timeout
// exactly as they are sent, and fails with the error Connect would fail with
// for the same options. Use it to show a user what is being dialled, to log
// it, or to hand it to another go-mssqldb client.
//
// With maskSecrets, every password, client secret, certificate password,
// access token and user assertion in it is replaced by a fixed placeholder, so the result is safe
// to display — and no longer connects. Unmasked, it is a live credential. The
// tokens an AccessTokenProvider mints never appear either way: they are
// fetched per connection and are not part of the string.
func (o ConnectionOptions) ConnectionString(maskSecrets bool) (string, error) {
	applyDefaults(&o)
	var (
		dsn string
		err error
	)
	if o.AccessTokenProvider != nil {
		dsn, err = baseDSN(o)
	} else {
		dsn, _, err = buildDSN(o)
	}
	if err != nil || !maskSecrets {
		return dsn, err
	}

	u, err := url.Parse(dsn)
	if err != nil {
		// Never include dsn or the parse error: both carry the secrets this
		// call was asked to hide.
		return "", fmt.Errorf("gosmo: connection string: cannot mask an unparseable DSN")
	}
	if u.User != nil {
		if p, ok := u.User.Password(); ok && p != "" {
			u.User = url.UserPassword(u.User.Username(), maskedSecret)
		}
	}
	q := u.Query()
	masked := false
	for _, k := range secretDSNKeys {
		if q.Get(k) != "" {
			q.Set(k, maskedSecret)
			masked = true
		}
	}
	if masked {
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// secretDSNKeys are the query parameters a masked ConnectionString hides:
// "password" (every password, client secret, certificate password and access
// token gosmo writes), the on-behalf-of user assertion, and the two Entra
// secrets only ExtraParams can carry (mergeExtraParams lower-cases keys, so
// these match however the caller spelled them).
var secretDSNKeys = []string{"password", "userassertion", "systemtoken", "clientassertion"}

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
			&d.name, &d.id, &state, &recovery,
			&compatLevel, &collation, &d.isReadOnly, &d.createDate,
			&d.sourceDatabaseID,
		); err != nil {
			return nil, fmt.Errorf("gosmo: list databases: %w", err)
		}
		d.state = state.String
		d.recoveryModel = RecoveryModel(recovery.String)
		d.compatLevel = CompatibilityLevel(compatLevel.Int64)
		d.collation = collation.String
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
		&d.name, &d.id, &state, &recovery,
		&compatLevel, &collation, &d.isReadOnly, &d.createDate,
		&d.sourceDatabaseID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("gosmo: database %q not found", name)
		}
		return nil, fmt.Errorf("gosmo: database by name: %w", err)
	}
	d.state = state.String
	d.recoveryModel = RecoveryModel(recovery.String)
	d.compatLevel = CompatibilityLevel(compatLevel.Int64)
	d.collation = collation.String
	return d, nil
}

// Database returns a lightweight handle for name without querying the
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
func (s *Server) Database(name string) *Database {
	return &Database{server: s, name: name}
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
	// with a zero-valued CreateDatabaseOptions always has. FileGroup is
	// ignored on both (PrimaryFile is always PRIMARY; LogFile has none) —
	// additional filegroups and files are added after creation via
	// AddFileGroupContext/AddFileContext, not here.
	PrimaryFile *DatabaseFileSpec
	LogFile     *DatabaseFileSpec
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
// single-user.
func (s *Server) RenameDatabase(oldName, newName string, force bool) error {
	return s.RenameDatabaseContext(context.Background(), oldName, newName, force)
}

// RenameDatabaseContext is the context-aware variant of RenameDatabase.
func (s *Server) RenameDatabaseContext(ctx context.Context, oldName, newName string, force bool) error {
	if oldName == "" || newName == "" {
		return fmt.Errorf("gosmo: rename database: both names are required")
	}
	if force {
		if err := s.execContext(ctx,
			fmt.Sprintf("ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE", quoteIdent(oldName)),
		); err != nil {
			return fmt.Errorf("gosmo: set single user on %q: %w", oldName, err)
		}
	}
	q := fmt.Sprintf("ALTER DATABASE %s MODIFY NAME = %s", quoteIdent(oldName), quoteIdent(newName))
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

// -- Logins --------------------------------------------------------------------

// Logins returns all server-level logins.
func (s *Server) Logins() ([]*Login, error) {
	return s.LoginsContext(context.Background())
}

// LoginsContext is the context-aware variant of Logins.
//
// Every server-level login is listed, not just the SQL/Windows ones: the
// type filter also admits Entra ('E','X') and the certificate- and
// asymmetric-key-mapped logins ('C','K') that hold permissions for signed
// code, which is what SSMS's Logins folder shows.
func (s *Server) LoginsContext(ctx context.Context) ([]*Login, error) {
	const q = `
	SELECT name, sid, type_desc, is_disabled, default_database_name,
	       create_date, modify_date
	FROM sys.server_principals
	WHERE type IN ('S','U','G','E','X','C','K')
	ORDER BY name`

	rows, err := s.query(ctx, q)
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
			return nil, fmt.Errorf("gosmo: list logins: %w", err)
		}
		l.DefaultDatabase = defDB.String
		logins = append(logins, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list logins: %w", err)
	}
	return logins, nil
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
	WHERE type IN ('S','U','G','E','X','C','K') AND name = @p1`

	l := &Login{server: s}
	var defDB sql.NullString

	if err := s.queryRowScan(ctx, q, []any{name},
		&l.Name, &l.SID, &l.LoginType, &l.IsDisabled, &defDB, &l.CreateDate, &l.ModifyDate,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("gosmo: login %q not found", name)
		}
		return nil, fmt.Errorf("gosmo: find login %q: %w", name, err)
	}
	l.DefaultDatabase = defDB.String
	return l, nil
}

// Login returns a lightweight handle for name without querying the server
// at all — unlike LoginByName/LoginByNameContext, it doesn't verify the
// login exists or populate SID/LoginType/IsDisabled/etc. (they stay at
// their zero value). Every write method on *Login (AddServerRoleMemberContext,
// DisableContext, ChangePasswordContext, ...) only ever needs the login's
// name, never those cached fields, so this is sufficient for issuing
// further ALTER-style calls against a login the caller already knows
// exists — most commonly one it just created in the same operation. See
// Server.Database's doc comment for why this also matters under a
// WithScript-derived context.
func (s *Server) Login(name string) *Login {
	return &Login{server: s, Name: name}
}

// CreateLogin creates a login. With no CreateLoginOptions.Source, an empty
// password means a Windows login (FROM WINDOWS) and a non-empty one a SQL
// login; set Source to create any of the other kinds.
func (s *Server) CreateLogin(name, password string, opts *CreateLoginOptions) error {
	return s.CreateLoginContext(context.Background(), name, password, opts)
}

// CreateLoginContext is the context-aware variant of CreateLogin.
//
// Security: the password is never string-concatenated raw into the SQL
// text — it's quoted via nStringLiteral (N'...', doubling any embedded
// quote), the same escaping every other literal in this package uses.
// HASHED is deliberately not used here: it tells SQL Server the value is
// already one of its own password-hash formats, not a cleartext password,
// so passing an arbitrary hex encoding of the cleartext under HASHED
// either fails outright or creates a login nothing can ever authenticate
// as.
//
// DefaultDatabase reaches an external-provider login through a following
// ALTER LOGIN: OBJECT_ID is the only WITH option FROM EXTERNAL PROVIDER
// accepts, and DEFAULT_DATABASE alongside it does not parse. A
// certificate- or asymmetric-key-mapped login cannot have one at all —
// SQL Server rejects DEFAULT_DATABASE for those in both CREATE and ALTER
// ("Cannot use the parameter DEFAULT_DATABASE for a certificate or
// asymmetric key login", verified live) — so asking for one is an error
// rather than a statement the server will refuse.
func (s *Server) CreateLoginContext(ctx context.Context, name, password string, opts *CreateLoginOptions) error {
	if name == "" {
		return fmt.Errorf("gosmo: create login: name is required")
	}
	if opts == nil {
		opts = &CreateLoginOptions{}
	}

	src := opts.Source
	if src == LoginSourceAuto {
		if password == "" {
			src = LoginSourceWindows
		} else {
			src = LoginSourceSQL
		}
	}
	stmt, alterDefaultDB, err := createLoginStatement(name, password, src, opts)
	if err != nil {
		return fmt.Errorf("gosmo: create login %q: %w", name, err)
	}
	if err := s.execContext(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: create login %q: %w", name, err)
	}
	if alterDefaultDB {
		q := fmt.Sprintf("ALTER LOGIN %s WITH DEFAULT_DATABASE = %s",
			quoteIdent(name), quoteIdent(opts.DefaultDatabase))
		if err := s.execContext(ctx, q); err != nil {
			return fmt.Errorf("gosmo: create login %q: set default database: %w", name, err)
		}
	}
	return nil
}

// createLoginStatement builds the CREATE LOGIN statement for one resolved
// source, and reports whether DefaultDatabase still has to be applied by a
// following ALTER LOGIN — CERTIFICATE and ASYMMETRIC KEY take no WITH option
// list in CREATE LOGIN and EXTERNAL PROVIDER takes only OBJECT_ID, so naming
// DEFAULT_DATABASE there is a syntax error. A mapped login has no default database at all; see
// CreateLoginContext.
func createLoginStatement(name, password string, src LoginSource, opts *CreateLoginOptions) (string, bool, error) {
	if src != LoginSourceSQL && password != "" {
		return "", false, fmt.Errorf("a %s login takes no password", src)
	}
	if opts.MustChange && src != LoginSourceSQL {
		return "", false, fmt.Errorf("MustChange applies to a SQL login only, not a %s login", src)
	}
	if opts.DefaultDatabase != "" && (src == LoginSourceCertificate || src == LoginSourceAsymmetricKey) {
		return "", false, fmt.Errorf("a %s login cannot have a default database", src)
	}
	if opts.ObjectID != "" && src != LoginSourceExternalProvider {
		return "", false, fmt.Errorf("ObjectID applies to an external provider login only, not a %s login", src)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "CREATE LOGIN %s", quoteIdent(name))

	switch src {
	case LoginSourceSQL:
		if password == "" {
			return "", false, fmt.Errorf("a SQL login requires a password")
		}
		fmt.Fprintf(&sb, " WITH PASSWORD = %s", nStringLiteral(password))
		if opts.MustChange {
			// MUST_CHANGE requires CHECK_EXPIRATION = ON (and CHECK_POLICY =
			// ON, already the server default) — SQL Server rejects
			// MUST_CHANGE otherwise.
			sb.WriteString(" MUST_CHANGE, CHECK_EXPIRATION = ON")
		}
		if opts.DefaultDatabase != "" {
			fmt.Fprintf(&sb, ", DEFAULT_DATABASE = %s", quoteIdent(opts.DefaultDatabase))
		}
	case LoginSourceWindows:
		sb.WriteString(" FROM WINDOWS")
		if opts.DefaultDatabase != "" {
			fmt.Fprintf(&sb, " WITH DEFAULT_DATABASE = %s", quoteIdent(opts.DefaultDatabase))
		}
	case LoginSourceExternalProvider:
		sb.WriteString(" FROM EXTERNAL PROVIDER")
		if opts.ObjectID != "" {
			// The one WITH option FROM EXTERNAL PROVIDER does take, and it is
			// not part of the general option list: OBJECT_ID names the Entra
			// principal directly, so DEFAULT_DATABASE still cannot join it
			// here and stays on the following ALTER LOGIN.
			fmt.Fprintf(&sb, " WITH OBJECT_ID = %s", nStringLiteral(opts.ObjectID))
		}
		return sb.String(), opts.DefaultDatabase != "", nil
	case LoginSourceCertificate:
		if opts.CertificateName == "" {
			return "", false, fmt.Errorf("a certificate login requires CertificateName")
		}
		fmt.Fprintf(&sb, " FROM CERTIFICATE %s", quoteIdent(opts.CertificateName))
		return sb.String(), false, nil
	case LoginSourceAsymmetricKey:
		if opts.AsymmetricKeyName == "" {
			return "", false, fmt.Errorf("an asymmetric key login requires AsymmetricKeyName")
		}
		fmt.Fprintf(&sb, " FROM ASYMMETRIC KEY %s", quoteIdent(opts.AsymmetricKeyName))
		return sb.String(), false, nil
	default:
		return "", false, fmt.Errorf("unknown login source %d", int(src))
	}
	return sb.String(), false, nil
}

// LoginSource names what a new login authenticates from — the FROM clause of
// CREATE LOGIN, or WITH PASSWORD for a SQL login.
type LoginSource int

const (
	// LoginSourceAuto resolves from the password CreateLogin is given: empty
	// means a Windows login, non-empty a SQL login. It is the zero value, so
	// a CreateLoginOptions written before LoginSource existed behaves exactly
	// as it did.
	LoginSourceAuto LoginSource = iota
	// LoginSourceSQL is a SQL Server login (WITH PASSWORD).
	LoginSourceSQL
	// LoginSourceWindows is a Windows user or group login (FROM WINDOWS).
	LoginSourceWindows
	// LoginSourceExternalProvider is a Microsoft Entra ID (Azure AD) login
	// (FROM EXTERNAL PROVIDER) — SQL Server 2022 and later, Azure SQL
	// Managed Instance, and Azure SQL Database.
	LoginSourceExternalProvider
	// LoginSourceCertificate maps the login to a certificate in master
	// (FROM CERTIFICATE). Nothing authenticates as such a login; it exists
	// to hold permissions for code signed by the certificate.
	LoginSourceCertificate
	// LoginSourceAsymmetricKey maps the login to an asymmetric key in master
	// (FROM ASYMMETRIC KEY), the asymmetric-key counterpart of
	// LoginSourceCertificate.
	LoginSourceAsymmetricKey
)

// String renders the source as the words used in error messages.
func (src LoginSource) String() string {
	switch src {
	case LoginSourceAuto:
		return "auto"
	case LoginSourceSQL:
		return "SQL"
	case LoginSourceWindows:
		return "Windows"
	case LoginSourceExternalProvider:
		return "external provider"
	case LoginSourceCertificate:
		return "certificate"
	case LoginSourceAsymmetricKey:
		return "asymmetric key"
	}
	return fmt.Sprintf("LoginSource(%d)", int(src))
}

// CreateLoginOptions holds optional parameters for CreateLogin.
type CreateLoginOptions struct {
	DefaultDatabase string
	MustChange      bool

	// Source selects what the login authenticates from. The zero value
	// (LoginSourceAuto) keeps CreateLogin's original behaviour: a SQL login
	// when a password is given, a Windows login when it is empty.
	Source LoginSource

	// CertificateName is the master certificate a LoginSourceCertificate
	// login maps to; required for that source and ignored otherwise.
	CertificateName string

	// AsymmetricKeyName is the master asymmetric key a
	// LoginSourceAsymmetricKey login maps to; required for that source and
	// ignored otherwise.
	AsymmetricKeyName string

	// ObjectID is the Microsoft Entra ID object id (a GUID) a
	// LoginSourceExternalProvider login names explicitly, emitted as
	// CREATE LOGIN ... FROM EXTERNAL PROVIDER WITH OBJECT_ID = '...'.
	// SQL Server 2022 and later. It resolves a display name that is
	// ambiguous in the directory — with no object id the server looks the
	// login name up itself, which is the ordinary case. Naming it for any
	// other source is an error rather than a silently ignored field.
	ObjectID string
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
	server      *Server
	Name        string
	ID          int
	IsFixedRole bool
	Owner       string
	Members     []string
	SID         []byte
	CreateDate  time.Time
	ModifyDate  time.Time
}

// ServerRoles returns all fixed and user-defined server roles.
func (s *Server) ServerRoles() ([]*ServerRole, error) {
	return s.ServerRolesContext(context.Background())
}

// ServerRolesContext is the context-aware variant of ServerRoles.
func (s *Server) ServerRolesContext(ctx context.Context) ([]*ServerRole, error) {
	const q = `
	SELECT r.name, r.principal_id, r.is_fixed_role, ISNULL(p.name, ''),
	       STUFF((SELECT ', ' + m.name
	              FROM sys.server_role_members rm
	              JOIN sys.server_principals m ON m.principal_id = rm.member_principal_id
	              WHERE rm.role_principal_id = r.principal_id
	              FOR XML PATH(''), TYPE).value('.','NVARCHAR(MAX)'), 1, 2, '') AS members
	FROM sys.server_principals r
	LEFT JOIN sys.server_principals p ON p.principal_id = r.owning_principal_id
	WHERE r.type = 'R'
	ORDER BY r.name`

	rows, err := s.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list server roles: %w", err)
	}
	defer rows.Close()

	var roles []*ServerRole
	for rows.Next() {
		r := &ServerRole{server: s}
		var members sql.NullString
		if err := rows.Scan(&r.Name, &r.ID, &r.IsFixedRole, &r.Owner, &members); err != nil {
			return nil, fmt.Errorf("gosmo: list server roles: %w", err)
		}
		if members.Valid && members.String != "" {
			r.Members = strings.Split(members.String, ", ")
		}
		roles = append(roles, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list server roles: %w", err)
	}
	return roles, nil
}

// ServerRoleByName returns a single server role by name, with its
// principal detail (SID, create/modify dates) filled in —
// ServerRolesContext leaves these out since Object Explorer's tree listing
// never needs them.
func (s *Server) ServerRoleByName(name string) (*ServerRole, error) {
	return s.ServerRoleByNameContext(context.Background(), name)
}

// ServerRoleByNameContext is the context-aware variant of ServerRoleByName.
func (s *Server) ServerRoleByNameContext(ctx context.Context, name string) (*ServerRole, error) {
	const q = `
	SELECT r.principal_id, r.is_fixed_role, ISNULL(p.name, ''),
	       r.sid, r.create_date, r.modify_date,
	       STUFF((SELECT ', ' + m.name
	              FROM sys.server_role_members rm
	              JOIN sys.server_principals m ON m.principal_id = rm.member_principal_id
	              WHERE rm.role_principal_id = r.principal_id
	              FOR XML PATH(''), TYPE).value('.','NVARCHAR(MAX)'), 1, 2, '') AS members
	FROM sys.server_principals r
	LEFT JOIN sys.server_principals p ON p.principal_id = r.owning_principal_id
	WHERE r.type = 'R' AND r.name = @p1`

	r := &ServerRole{server: s, Name: name}
	var members sql.NullString
	if err := s.queryRowScan(ctx, q, []any{name},
		&r.ID, &r.IsFixedRole, &r.Owner, &r.SID, &r.CreateDate, &r.ModifyDate, &members,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("gosmo: server role %q not found", name)
		}
		return nil, fmt.Errorf("gosmo: find server role %q: %w", name, err)
	}
	if members.Valid && members.String != "" {
		r.Members = strings.Split(members.String, ", ")
	}
	return r, nil
}

// DropServerRole drops a user-defined server role. A fixed role, or one
// that still owns another role, is refused by the server, not here.
func (s *Server) DropServerRole(name string) error {
	return s.DropServerRoleContext(context.Background(), name)
}

// DropServerRoleContext is the context-aware variant of DropServerRole.
func (s *Server) DropServerRoleContext(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("gosmo: drop server role: name is required")
	}
	if err := s.execContext(ctx, "DROP SERVER ROLE "+quoteIdent(name)); err != nil {
		return fmt.Errorf("gosmo: drop server role %q: %w", name, err)
	}
	return nil
}

// Drop drops this server role.
func (r *ServerRole) Drop() error { return r.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (r *ServerRole) DropContext(ctx context.Context) error {
	return r.server.DropServerRoleContext(ctx, r.Name)
}

// Rename changes the server role's name.
func (r *ServerRole) Rename(newName string) error {
	return r.RenameContext(context.Background(), newName)
}

// RenameContext is the context-aware variant of Rename.
func (r *ServerRole) RenameContext(ctx context.Context, newName string) error {
	q := fmt.Sprintf("ALTER SERVER ROLE %s WITH NAME = %s", quoteIdent(r.Name), quoteIdent(newName))
	if err := r.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: rename server role %q to %q: %w", r.Name, newName, err)
	}
	setIfApplied(ctx, &r.Name, newName)
	return nil
}

// ChangeOwner transfers ownership of the server role to a new principal.
func (r *ServerRole) ChangeOwner(newOwner string) error {
	return r.ChangeOwnerContext(context.Background(), newOwner)
}

// ChangeOwnerContext is the context-aware variant of ChangeOwner.
func (r *ServerRole) ChangeOwnerContext(ctx context.Context, newOwner string) error {
	q := fmt.Sprintf("ALTER AUTHORIZATION ON SERVER ROLE::%s TO %s", quoteIdent(r.Name), quoteIdent(newOwner))
	if err := r.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: change server role %q owner to %q: %w", r.Name, newOwner, err)
	}
	setIfApplied(ctx, &r.Owner, newOwner)
	return nil
}

// ServerRoleMembers returns the direct members of a server role (logins or
// other server roles), with each member's principal type —
// ServerRolesContext/ServerRoleByNameContext only return member names,
// concatenated, with no type.
func (s *Server) ServerRoleMembers(roleName string) ([]*RoleMember, error) {
	return s.ServerRoleMembersContext(context.Background(), roleName)
}

// ServerRoleMembersContext is the context-aware variant of ServerRoleMembers.
func (s *Server) ServerRoleMembersContext(ctx context.Context, roleName string) ([]*RoleMember, error) {
	const q = `
SELECT m.name, m.type_desc
FROM   sys.server_role_members rm
JOIN   sys.server_principals r ON r.principal_id = rm.role_principal_id
JOIN   sys.server_principals m ON m.principal_id = rm.member_principal_id
WHERE  r.name = @p1
ORDER  BY m.name`

	rows, err := s.query(ctx, q, roleName)
	if err != nil {
		return nil, fmt.Errorf("gosmo: members of server role %q: %w", roleName, err)
	}
	defer rows.Close()

	var members []*RoleMember
	for rows.Next() {
		m := &RoleMember{}
		if err := rows.Scan(&m.Name, &m.Type); err != nil {
			return nil, fmt.Errorf("gosmo: members of server role %q: %w", roleName, err)
		}
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: members of server role %q: %w", roleName, err)
	}
	return members, nil
}

// AddServerRoleMember adds member (a login or another server role, by
// name) to a server role.
func (s *Server) AddServerRoleMember(roleName, memberName string) error {
	return s.AddServerRoleMemberContext(context.Background(), roleName, memberName)
}

// AddServerRoleMemberContext is the context-aware variant of AddServerRoleMember.
func (s *Server) AddServerRoleMemberContext(ctx context.Context, roleName, memberName string) error {
	q := fmt.Sprintf("ALTER SERVER ROLE %s ADD MEMBER %s", quoteIdent(roleName), quoteIdent(memberName))
	if err := s.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: add %q to server role %q: %w", memberName, roleName, err)
	}
	return nil
}

// RemoveServerRoleMember removes member from a server role.
func (s *Server) RemoveServerRoleMember(roleName, memberName string) error {
	return s.RemoveServerRoleMemberContext(context.Background(), roleName, memberName)
}

// RemoveServerRoleMemberContext is the context-aware variant of RemoveServerRoleMember.
func (s *Server) RemoveServerRoleMemberContext(ctx context.Context, roleName, memberName string) error {
	q := fmt.Sprintf("ALTER SERVER ROLE %s DROP MEMBER %s", quoteIdent(roleName), quoteIdent(memberName))
	if err := s.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: remove %q from server role %q: %w", memberName, roleName, err)
	}
	return nil
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

	rows, err := s.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list linked servers: %w", err)
	}
	defer rows.Close()

	var ls []*LinkedServer
	for rows.Next() {
		l := &LinkedServer{}
		var ds sql.NullString
		if err := rows.Scan(&l.Name, &l.Product, &l.Provider, &ds, &l.IsRemote); err != nil {
			return nil, fmt.Errorf("gosmo: list linked servers: %w", err)
		}
		l.DataSource = ds.String
		ls = append(ls, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list linked servers: %w", err)
	}
	return ls, nil
}
