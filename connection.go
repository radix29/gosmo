package gosmo

import (
	"cmp"
	"context"
	"database/sql"
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
