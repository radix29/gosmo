package gosmo

import (
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
	// "myserver.database.windows.net" — any ParseServerAddress form. Required.
	Server string

	// Port is the TCP port to dial when Server writes none. Zero leaves the
	// default: 1433 for a default instance, SQL Server Browser's answer for a
	// named one. Set on a named instance, it skips the Browser lookup, as
	// "host\instance,port" does. A port written in Server wins over Port.
	// Outside 0-65535 is an error.
	Port int

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

	// ConnectTimeout is the maximum time to wait for the initial connection,
	// the TCP dial included. Defaults to 30s when zero. It is rounded up to
	// whole seconds, which is all the driver takes. A "dial timeout" entry in
	// ExtraParams overrides it for the dial alone.
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
//
// ctx governs the initial ping and server-info load only; subsequent calls
// each carry their own context. opts.ConnectTimeout bounds each connection attempt, not the
// whole call: an interactive Entra sign-in during the dial may take longer, so
// a caller that wants an overall limit puts a deadline on ctx.
func Connect(ctx context.Context, opts ConnectionOptions) (*Server, error) {
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

	s := newServer(pool)
	if err = s.loadInfo(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// NewServer wraps an already-open *sql.DB as a Server, loading the same
// server metadata Connect loads. Use it when the pool is not gosmo's
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
	s := newServer(db)
	if err := s.loadInfo(ctx); err != nil {
		s.cancel()
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

// address is Server split by ParseServerAddress, with Port standing in for a
// port Server does not write. Every reader of the dial target goes through it,
// so a Port-field port and a written one behave alike.
func (o ConnectionOptions) address() (host, instance string, port int) {
	host, instance, port = ParseServerAddress(o.Server)
	if port == 0 {
		port = o.Port
	}
	return host, instance, port
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
//
// A leading "tcp:" protocol prefix — the form the Azure Portal's ADO.NET
// connection string shows, "tcp:x.database.windows.net,1433", and SSMS
// accepts — is dropped, since TCP is the only protocol there is here. The
// other prefixes (np:, lpc:, admin:) are left in place, as part of host;
// Connect and ConnectionString refuse them by name.
func ParseServerAddress(server string) (host, instance string, port int) {
	if proto, rest := splitProtocolPrefix(server); proto == "tcp" {
		server = rest
	}
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

// splitProtocolPrefix splits a SQL Server protocol prefix — tcp:, np:, lpc:,
// admin: (any case) — off server, returning it lower-cased without its colon
// and the address after it; proto is "" when there is none. A prefix followed
// by nothing but digits is not one: "admin:1433" is the host "admin" on port
// 1433.
func splitProtocolPrefix(server string) (proto, rest string) {
	i := strings.IndexByte(server, ':')
	if i <= 0 {
		return "", server
	}
	p := strings.ToLower(server[:i])
	switch p {
	case "tcp", "np", "lpc", "admin":
	default:
		return "", server
	}
	rest = server[i+1:]
	if strings.Trim(rest, "0123456789") == "" {
		return "", server
	}
	return p, rest
}
