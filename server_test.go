package gosmo

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/microsoft/go-mssqldb/msdsn"
)

func TestApplyDefaults(t *testing.T) {
	var opts ConnectionOptions
	applyDefaults(&opts)

	if opts.ConnectTimeout != 30*time.Second {
		t.Errorf("ConnectTimeout = %v, want 30s", opts.ConnectTimeout)
	}
	if opts.ApplicationName != "gosmo" {
		t.Errorf("ApplicationName = %q, want gosmo", opts.ApplicationName)
	}
	if opts.Database != "master" {
		t.Errorf("Database = %q, want master", opts.Database)
	}
	if opts.MaxIdleConns != 2 {
		t.Errorf("MaxIdleConns = %d, want 2", opts.MaxIdleConns)
	}
}

func TestApplyDefaultsDoesNotOverwriteExplicitValues(t *testing.T) {
	opts := ConnectionOptions{
		ConnectTimeout:  5 * time.Second,
		ApplicationName: "myapp",
		Database:        "mydb",
		MaxIdleConns:    10,
	}
	applyDefaults(&opts)

	if opts.ConnectTimeout != 5*time.Second {
		t.Errorf("ConnectTimeout = %v, want 5s", opts.ConnectTimeout)
	}
	if opts.ApplicationName != "myapp" {
		t.Errorf("ApplicationName = %q, want myapp", opts.ApplicationName)
	}
	if opts.Database != "mydb" {
		t.Errorf("Database = %q, want mydb", opts.Database)
	}
	if opts.MaxIdleConns != 10 {
		t.Errorf("MaxIdleConns = %d, want 10", opts.MaxIdleConns)
	}
}

func TestBuildDSNRequiresServer(t *testing.T) {
	_, _, err := buildDSN(ConnectionOptions{})
	if err == nil {
		t.Fatal("buildDSN with no Server: want error, got nil")
	}
}

func TestBuildDSNSQLServerAuth(t *testing.T) {
	dsn, driver, err := buildDSN(ConnectionOptions{
		Server:   "localhost:1433",
		Database: "mydb",
		Auth:     AuthSQLServer,
		User:     "sa",
		Password: "p@ss",
	})
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	if driver != "sqlserver" {
		t.Errorf("driver = %q, want sqlserver", driver)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", dsn, err)
	}
	if u.Scheme != "sqlserver" || u.Host != "localhost:1433" {
		t.Errorf("scheme/host = %q/%q, want sqlserver/localhost:1433", u.Scheme, u.Host)
	}
	if user := u.User.Username(); user != "sa" {
		t.Errorf("user = %q, want sa", user)
	}
	if pw, _ := u.User.Password(); pw != "p@ss" {
		t.Errorf("password = %q, want p@ss", pw)
	}
	if got := u.Query().Get("database"); got != "mydb" {
		t.Errorf("database = %q, want mydb", got)
	}
}

func TestParseServerAddress(t *testing.T) {
	cases := []struct {
		server       string
		wantHost     string
		wantInstance string
		wantPort     int
	}{
		{"myserver", "myserver", "", 0},
		{"myserver:1433", "myserver", "", 1433},
		{"myserver,1434", "myserver", "", 1434},
		{`myserver\SQLEXPRESS`, "myserver", "SQLEXPRESS", 0},
		{`myserver\SQLEXPRESS,1434`, "myserver", "SQLEXPRESS", 1434},
	}
	for _, c := range cases {
		host, instance, port := ParseServerAddress(c.server)
		if host != c.wantHost || instance != c.wantInstance || port != c.wantPort {
			t.Errorf("ParseServerAddress(%q) = (%q, %q, %d), want (%q, %q, %d)",
				c.server, host, instance, port, c.wantHost, c.wantInstance, c.wantPort)
		}
	}
}

// TestBuildDSNNamedInstance pins the fix for a real bug: a literal
// backslash in ConnectionOptions.Server used to get percent-escaped by
// url.URL (Host: opts.Server), which go-mssqldb's own DSN parser then
// rejected outright with "invalid URL escape". The instance name must
// instead travel as a URL path segment (host:port/instance).
func TestBuildDSNNamedInstance(t *testing.T) {
	dsn, _, err := buildDSN(ConnectionOptions{
		Server: `myserver\SQLEXPRESS`,
		Auth:   AuthSQLServer,
		User:   "sa", Password: "p@ss",
	})
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", dsn, err)
	}
	if u.Host != "myserver" {
		t.Errorf("Host = %q, want myserver", u.Host)
	}
	if u.Path != "/SQLEXPRESS" {
		t.Errorf("Path = %q, want /SQLEXPRESS", u.Path)
	}
}

// TestBuildDSNCommaPort pins support for the SQL-Server-native
// "host,port" address form (as opposed to the URL-native "host:port",
// which already worked).
func TestBuildDSNCommaPort(t *testing.T) {
	dsn, _, err := buildDSN(ConnectionOptions{Server: "myserver,1434", Auth: AuthSQLServer})
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", dsn, err)
	}
	if u.Host != "myserver:1434" {
		t.Errorf("Host = %q, want myserver:1434", u.Host)
	}
}

// TestBuildDSNNamedInstanceWithPort pins the combined "host\instance,port"
// form.
func TestBuildDSNNamedInstanceWithPort(t *testing.T) {
	dsn, _, err := buildDSN(ConnectionOptions{Server: `myserver\SQLEXPRESS,1434`, Auth: AuthSQLServer})
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", dsn, err)
	}
	if u.Host != "myserver:1434" {
		t.Errorf("Host = %q, want myserver:1434", u.Host)
	}
	if u.Path != "/SQLEXPRESS" {
		t.Errorf("Path = %q, want /SQLEXPRESS", u.Path)
	}
}

// TestBuildDSNNamedInstanceRoundTripsThroughDriver feeds the DSN all the
// way through go-mssqldb's own msdsn.Parse — the exact parser that
// silently failed on a raw backslash before this fix (either erroring on
// "invalid URL escape %5C", or, for the comma-port form, dialing a
// hostname with a literal comma in it) — confirming the driver actually
// resolves host/instance/port as SSMS's "Server name" field would.
func TestBuildDSNNamedInstanceRoundTripsThroughDriver(t *testing.T) {
	cases := []struct {
		name         string
		server       string
		wantHost     string
		wantInstance string
		wantPort     uint64
	}{
		{"plain host", "myserver", "myserver", "", 0},
		{"host:port", "myserver:1433", "myserver", "", 1433},
		{"host,port", "myserver,1434", "myserver", "", 1434},
		{"host\\instance", `myserver\SQLEXPRESS`, "myserver", "SQLEXPRESS", 0},
		{"host\\instance,port", `myserver\SQLEXPRESS,1434`, "myserver", "SQLEXPRESS", 1434},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dsn, _, err := buildDSN(ConnectionOptions{Server: c.server, Auth: AuthSQLServer, User: "sa", Password: "p@ss"})
			if err != nil {
				t.Fatalf("buildDSN: %v", err)
			}
			cfg, err := msdsn.Parse(dsn)
			if err != nil {
				t.Fatalf("msdsn.Parse(%q): %v", dsn, err)
			}
			if cfg.Host != c.wantHost {
				t.Errorf("Host = %q, want %q", cfg.Host, c.wantHost)
			}
			if cfg.Instance != c.wantInstance {
				t.Errorf("Instance = %q, want %q", cfg.Instance, c.wantInstance)
			}
			if cfg.Port != c.wantPort {
				t.Errorf("Port = %d, want %d", cfg.Port, c.wantPort)
			}
		})
	}
}

func TestBuildDSNSQLServerAuthNoCredentials(t *testing.T) {
	dsn, _, err := buildDSN(ConnectionOptions{Server: "localhost", Auth: AuthSQLServer})
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", dsn, err)
	}
	if u.User != nil {
		t.Errorf("User = %v, want nil (no credentials supplied)", u.User)
	}
}

func TestBuildDSNWindowsAuth(t *testing.T) {
	dsn, driver, err := buildDSN(ConnectionOptions{
		Server: "localhost",
		Auth:   AuthWindows,
		User:   `DOMAIN\user`,
	})
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	if driver != "sqlserver" {
		t.Errorf("driver = %q, want sqlserver", driver)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", dsn, err)
	}
	if user := u.User.Username(); user != `DOMAIN\user` {
		t.Errorf("user = %q, want DOMAIN\\user", user)
	}
	if _, hasPW := u.User.Password(); hasPW {
		t.Error("Windows auth DSN should never carry a password")
	}
}

func TestBuildDSNWindowsAuthNoUser(t *testing.T) {
	dsn, _, err := buildDSN(ConnectionOptions{Server: "localhost", Auth: AuthWindows})
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", dsn, err)
	}
	if u.User != nil {
		t.Errorf("User = %v, want nil", u.User)
	}
}

// withGOOS pins the platform buildDSN sees, so the AuthWindows path can be
// exercised deterministically regardless of where the test runs.
func withGOOS(t *testing.T, os string) {
	t.Helper()
	orig := goos
	goos = os
	t.Cleanup(func() { goos = orig })
}

func TestBuildDSNWindowsAuthKerberosOnNonWindows(t *testing.T) {
	withGOOS(t, "linux")
	dsn, driver, err := buildDSN(ConnectionOptions{Server: "sql.contoso.com", Auth: AuthWindows})
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	if driver != "sqlserver" {
		t.Errorf("driver = %q, want sqlserver", driver)
	}
	cfg, err := msdsn.Parse(dsn)
	if err != nil {
		t.Fatalf("msdsn.Parse(%q): %v", dsn, err)
	}
	if got := cfg.Parameters["authenticator"]; got != "krb5" {
		t.Errorf("authenticator = %q, want krb5", got)
	}
}

func TestBuildDSNWindowsAuthSSPIOnWindows(t *testing.T) {
	withGOOS(t, "windows")
	dsn, _, err := buildDSN(ConnectionOptions{Server: "sql.contoso.com", Auth: AuthWindows})
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	u, _ := url.Parse(dsn)
	if _, ok := u.Query()["authenticator"]; ok {
		t.Error("Windows SSPI path should not set authenticator=krb5")
	}
}

func TestBuildDSNWindowsAuthKerberosExplicitOnWindows(t *testing.T) {
	withGOOS(t, "windows")
	dsn, _, err := buildDSN(ConnectionOptions{
		Server:   "sql.contoso.com",
		Auth:     AuthWindows,
		Kerberos: KerberosOptions{Realm: "CONTOSO.COM"},
	})
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	u, _ := url.Parse(dsn)
	if got := u.Query().Get("authenticator"); got != "krb5" {
		t.Errorf("authenticator = %q, want krb5 (explicit Kerberos on Windows)", got)
	}
	if got := u.Query().Get("krb5-realm"); got != "CONTOSO.COM" {
		t.Errorf("krb5-realm = %q, want CONTOSO.COM", got)
	}
}

func TestBuildDSNKerberosUserPassword(t *testing.T) {
	withGOOS(t, "linux")
	dsn, _, err := buildDSN(ConnectionOptions{
		Server:   "sql.contoso.com",
		Auth:     AuthWindows,
		User:     "svc",
		Password: "p@ss",
		Kerberos: KerberosOptions{Realm: "CONTOSO.COM", ConfigFile: "/etc/krb5.conf"},
	})
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	cfg, err := msdsn.Parse(dsn)
	if err != nil {
		t.Fatalf("msdsn.Parse(%q): %v", dsn, err)
	}
	if cfg.User != "svc" || cfg.Password != "p@ss" {
		t.Errorf("user/password = %q/%q, want svc/p@ss", cfg.User, cfg.Password)
	}
	if got := cfg.Parameters["krb5-realm"]; got != "CONTOSO.COM" {
		t.Errorf("krb5-realm = %q, want CONTOSO.COM", got)
	}
	if got := cfg.Parameters["krb5-configfile"]; got != "/etc/krb5.conf" {
		t.Errorf("krb5-configfile = %q, want /etc/krb5.conf", got)
	}
}

func TestBuildDSNServerSPN(t *testing.T) {
	withGOOS(t, "linux")
	dsn, _, err := buildDSN(ConnectionOptions{
		Server:    "sql.contoso.com",
		Auth:      AuthWindows,
		ServerSPN: "MSSQLSvc/sql.contoso.com:1433",
	})
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	cfg, err := msdsn.Parse(dsn)
	if err != nil {
		t.Fatalf("msdsn.Parse(%q): %v", dsn, err)
	}
	if cfg.ServerSPN != "MSSQLSvc/sql.contoso.com:1433" {
		t.Errorf("ServerSPN = %q, want MSSQLSvc/sql.contoso.com:1433", cfg.ServerSPN)
	}
}

// TestBuildConnectorKerberosRegistered proves the krb5 provider is registered
// (via the blank import in kerberos.go): building a connector for an
// AuthWindows Kerberos DSN must succeed rather than fail with "provider krb5
// not found".
func TestBuildConnectorKerberosRegistered(t *testing.T) {
	withGOOS(t, "linux")
	opts := ConnectionOptions{Server: "sql.contoso.com", Auth: AuthWindows}
	applyDefaults(&opts)
	if _, err := buildConnector(opts); err != nil {
		t.Fatalf("buildConnector: %v", err)
	}
}

func TestBuildDSNEntraDefault(t *testing.T) {
	dsn, driver, err := buildDSN(ConnectionOptions{
		Server: "myserver.database.windows.net",
		Auth:   AuthEntraDefault,
	})
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	if driver != "azuresql" {
		t.Errorf("driver = %q, want azuresql", driver)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", dsn, err)
	}
	if got := u.Query().Get("fedauth"); got != "ActiveDirectoryDefault" {
		t.Errorf("fedauth = %q, want ActiveDirectoryDefault", got)
	}
}

func TestBuildDSNEntraMSIWithClientID(t *testing.T) {
	dsn, _, err := buildDSN(ConnectionOptions{
		Server:   "myserver.database.windows.net",
		Auth:     AuthEntraMSI,
		ClientID: "client-123",
	})
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	u, _ := url.Parse(dsn)
	if got := u.Query().Get("user id"); got != "client-123" {
		t.Errorf(`"user id" = %q, want client-123`, got)
	}
}

func TestBuildDSNEntraServicePrincipalWithTenant(t *testing.T) {
	dsn, _, err := buildDSN(ConnectionOptions{
		Server:   "myserver.database.windows.net",
		Auth:     AuthEntraServicePrincipal,
		User:     "app-client-id",
		TenantID: "tenant-id",
		Password: "secret",
	})
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	u, _ := url.Parse(dsn)
	if got := u.Query().Get("user id"); got != "app-client-id@tenant-id" {
		t.Errorf(`"user id" = %q, want app-client-id@tenant-id`, got)
	}
	if got := u.Query().Get("password"); got != "secret" {
		t.Errorf("password = %q, want secret", got)
	}
}

func TestBuildDSNEntraServicePrincipalWithCert(t *testing.T) {
	dsn, _, err := buildDSN(ConnectionOptions{
		Server:             "myserver.database.windows.net",
		Auth:               AuthEntraServicePrincipal,
		User:               "app-client-id",
		ClientCertPath:     "/path/to/cert.pfx",
		ClientCertPassword: "certpw",
	})
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	u, _ := url.Parse(dsn)
	if got := u.Query().Get("clientcertpath"); got != "/path/to/cert.pfx" {
		t.Errorf("clientcertpath = %q, want /path/to/cert.pfx", got)
	}
	if got := u.Query().Get("password"); got != "certpw" {
		t.Errorf("password = %q, want certpw (cert password)", got)
	}
}

func TestBuildDSNEntraServicePrincipalAccessTokenRequiresToken(t *testing.T) {
	_, _, err := buildDSN(ConnectionOptions{
		Server: "myserver.database.windows.net",
		Auth:   AuthEntraServicePrincipalAccessToken,
	})
	if err == nil {
		t.Fatal("want error when AccessToken is empty, got nil")
	}
}

func TestBuildDSNEntraServicePrincipalAccessToken(t *testing.T) {
	dsn, _, err := buildDSN(ConnectionOptions{
		Server:      "myserver.database.windows.net",
		Auth:        AuthEntraServicePrincipalAccessToken,
		AccessToken: "tok-abc",
	})
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	u, _ := url.Parse(dsn)
	if got := u.Query().Get("password"); got != "tok-abc" {
		t.Errorf("password = %q, want tok-abc", got)
	}
}

func TestBuildDSNEntraPassword(t *testing.T) {
	dsn, _, err := buildDSN(ConnectionOptions{
		Server:   "myserver.database.windows.net",
		Auth:     AuthEntraPassword,
		User:     "user@tenant.onmicrosoft.com",
		Password: "pw",
	})
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	u, _ := url.Parse(dsn)
	if got := u.Query().Get("user id"); got != "user@tenant.onmicrosoft.com" {
		t.Errorf(`"user id" = %q, want user@tenant.onmicrosoft.com`, got)
	}
	if got := u.Query().Get("password"); got != "pw" {
		t.Errorf("password = %q, want pw", got)
	}
}

func TestBuildDSNUnsupportedAuthMethod(t *testing.T) {
	_, _, err := buildDSN(ConnectionOptions{Server: "localhost", Auth: AuthMethod(1000)})
	if err == nil {
		t.Fatal("want error for an out-of-range Entra AuthMethod, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported auth method") {
		t.Errorf("err = %q, want it to mention 'unsupported auth method'", err.Error())
	}
}

func TestBuildDSNTrustServerCertificateOmittedWhenFalse(t *testing.T) {
	dsn, _, err := buildDSN(ConnectionOptions{Server: "localhost", TrustServerCertificate: false})
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	u, _ := url.Parse(dsn)
	if _, ok := u.Query()["TrustServerCertificate"]; ok {
		t.Error("TrustServerCertificate should be omitted from the DSN when false")
	}
}

func TestBuildDSNTrustServerCertificateSetWhenTrue(t *testing.T) {
	dsn, _, err := buildDSN(ConnectionOptions{Server: "localhost", TrustServerCertificate: true})
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	u, _ := url.Parse(dsn)
	if got := u.Query().Get("TrustServerCertificate"); got != "true" {
		t.Errorf("TrustServerCertificate = %q, want true", got)
	}
}

func TestBaseDSN(t *testing.T) {
	dsn, err := baseDSN(ConnectionOptions{
		Server:          `myserver\SQLEXPRESS,1434`,
		Database:        "mydb",
		ApplicationName: "gosmo",
		Auth:            AuthEntraServicePrincipalAccessToken, // must be ignored
	})
	if err != nil {
		t.Fatalf("baseDSN: %v", err)
	}
	cfg, err := msdsn.Parse(dsn)
	if err != nil {
		t.Fatalf("msdsn.Parse(%q): %v", dsn, err)
	}
	if cfg.Host != "myserver" || cfg.Instance != "SQLEXPRESS" || cfg.Port != 1434 {
		t.Errorf("host/instance/port = %q/%q/%d, want myserver/SQLEXPRESS/1434", cfg.Host, cfg.Instance, cfg.Port)
	}
	if cfg.Database != "mydb" {
		t.Errorf("Database = %q, want mydb", cfg.Database)
	}
	u, _ := url.Parse(dsn)
	if u.User != nil {
		t.Errorf("User = %v, want nil (no auth in base DSN)", u.User)
	}
	if _, ok := u.Query()["fedauth"]; ok {
		t.Error("base DSN should carry no fedauth parameter")
	}
}

func TestBaseDSNRequiresServer(t *testing.T) {
	if _, err := baseDSN(ConnectionOptions{}); err == nil {
		t.Fatal("want error for empty Server, got nil")
	}
}

func TestBuildConnectorClassic(t *testing.T) {
	opts := ConnectionOptions{Server: "localhost", Auth: AuthSQLServer, User: "sa", Password: "p"}
	applyDefaults(&opts)
	c, err := buildConnector(opts)
	if err != nil {
		t.Fatalf("buildConnector: %v", err)
	}
	if c == nil {
		t.Fatal("connector is nil")
	}
}

func TestBuildConnectorEntra(t *testing.T) {
	opts := ConnectionOptions{Server: "myserver.database.windows.net", Auth: AuthEntraDefault}
	applyDefaults(&opts)
	c, err := buildConnector(opts)
	if err != nil {
		t.Fatalf("buildConnector: %v", err)
	}
	if c == nil {
		t.Fatal("connector is nil")
	}
}

// TestBuildConnectorAccessTokenProviderPrecedence proves the token-provider
// path wins over Auth/AccessToken: an Entra access-token method that would
// otherwise require AccessToken builds fine when a provider is supplied,
// because it routes through the base (no-fedauth) DSN instead.
func TestBuildConnectorAccessTokenProviderPrecedence(t *testing.T) {
	opts := ConnectionOptions{
		Server:              "myserver.database.windows.net",
		Auth:                AuthEntraServicePrincipalAccessToken,
		AccessTokenProvider: func(context.Context) (string, error) { return "tok", nil },
	}
	applyDefaults(&opts)
	c, err := buildConnector(opts)
	if err != nil {
		t.Fatalf("buildConnector: %v", err)
	}
	if c == nil {
		t.Fatal("connector is nil")
	}
}

func TestBuildConnectorSessionInitSQL(t *testing.T) {
	opts := ConnectionOptions{Server: "localhost", Auth: AuthSQLServer, SessionInitSQL: "SET ARITHABORT ON"}
	applyDefaults(&opts)
	c, err := buildConnector(opts)
	if err != nil {
		t.Fatalf("buildConnector: %v", err)
	}
	if c.SessionInitSQL != "SET ARITHABORT ON" {
		t.Errorf("SessionInitSQL = %q, want %q", c.SessionInitSQL, "SET ARITHABORT ON")
	}
}

func TestBuildConnectorInvalidServer(t *testing.T) {
	if _, err := buildConnector(ConnectionOptions{}); err == nil {
		t.Fatal("want error for empty Server, got nil")
	}
}

func TestBuildCreateDatabaseStatement(t *testing.T) {
	cases := []struct {
		name string
		opts *CreateDatabaseOptions
		want string
	}{
		{
			name: "nil opts — bare create, unchanged from before file support existed",
			opts: &CreateDatabaseOptions{},
			want: "CREATE DATABASE [SalesDW]",
		},
		{
			name: "collation only",
			opts: &CreateDatabaseOptions{Collation: "SQL_Latin1_General_CP1_CI_AS"},
			want: "CREATE DATABASE [SalesDW] COLLATE SQL_Latin1_General_CP1_CI_AS",
		},
		{
			name: "primary file only",
			opts: &CreateDatabaseOptions{
				PrimaryFile: &DatabaseFileSpec{
					Name: "SalesDW", Path: `F:\MSSQL\DATA\SalesDW.mdf`,
					SizeKB: 256 * 1024, GrowthKB: 64 * 1024,
				},
			},
			want: "CREATE DATABASE [SalesDW] ON PRIMARY \n" +
				"( NAME = [SalesDW], FILENAME = 'F:\\MSSQL\\DATA\\SalesDW.mdf', SIZE = 262144KB, FILEGROWTH = 65536KB )",
		},
		{
			name: "primary and log files, with collation",
			opts: &CreateDatabaseOptions{
				Collation: "SQL_Latin1_General_CP1_CI_AS",
				PrimaryFile: &DatabaseFileSpec{
					Name: "SalesDW", Path: `F:\MSSQL\DATA\SalesDW.mdf`,
					SizeKB: 256 * 1024, GrowthKB: 64 * 1024, MaxSizeKB: -1,
				},
				LogFile: &DatabaseFileSpec{
					Name: "SalesDW_log", Path: `L:\MSSQL\LOG\SalesDW_log.ldf`,
					SizeKB: 128 * 1024, GrowthPercent: 10,
				},
			},
			want: "CREATE DATABASE [SalesDW] ON PRIMARY \n" +
				"( NAME = [SalesDW], FILENAME = 'F:\\MSSQL\\DATA\\SalesDW.mdf', SIZE = 262144KB, MAXSIZE = UNLIMITED, FILEGROWTH = 65536KB ) \n" +
				"LOG ON \n" +
				"( NAME = [SalesDW_log], FILENAME = 'L:\\MSSQL\\LOG\\SalesDW_log.ldf', SIZE = 131072KB, FILEGROWTH = 10% ) " +
				"COLLATE SQL_Latin1_General_CP1_CI_AS",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildCreateDatabaseStatement("SalesDW", c.opts)
			if got != c.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, c.want)
			}
		})
	}
}
