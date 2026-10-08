package gosmo

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/microsoft/go-mssqldb/msdsn"
)

// TestBuildDSNIPv6RoundTripsThroughDriver pins what the driver reads back for
// each IPv6 form: an unbracketed literal in the URL host is either misread (its
// last group taken for a port) or rejected by url.Parse, and a bracketed one
// with no port keeps its brackets in the dialled host.
func TestBuildDSNIPv6RoundTripsThroughDriver(t *testing.T) {
	cases := []struct {
		server       string
		wantHost     string
		wantInstance string
		wantPort     uint64
	}{
		{"fe80::1", "fe80::1", "", 1433},
		{"2001:db8::abcd", "2001:db8::abcd", "", 1433},
		{"[fe80::1]", "fe80::1", "", 1433},
		{"[fe80::1]:1500", "fe80::1", "", 1500},
		{"fe80::1,1500", "fe80::1", "", 1500},
		{`fe80::1\SQLEXPRESS,1500`, "fe80::1", "SQLEXPRESS", 1500},
		{`[fe80::1]\SQLEXPRESS,1500`, "fe80::1", "SQLEXPRESS", 1500},
	}
	for _, c := range cases {
		t.Run(c.server, func(t *testing.T) {
			dsn, _, err := buildDSN(ConnectionOptions{Server: c.server, User: "sa", Password: "p"})
			if err != nil {
				t.Fatalf("buildDSN: %v", err)
			}
			cfg, err := msdsn.Parse(dsn)
			if err != nil {
				t.Fatalf("msdsn.Parse(%q): %v", dsn, err)
			}
			if cfg.Host != c.wantHost || cfg.Instance != c.wantInstance || cfg.Port != c.wantPort {
				t.Errorf("%q → host/instance/port = %q/%q/%d, want %q/%q/%d", dsn,
					cfg.Host, cfg.Instance, cfg.Port, c.wantHost, c.wantInstance, c.wantPort)
			}
		})
	}
}

// TestBuildDSNAcceptsTheTCPPrefix (S10): "tcp:host,port" is what the Azure
// Portal's ADO.NET string shows and SSMS accepts. Unstripped, the colon made
// the host look like an IPv6 literal, and both Connect and ConnectionString
// failed "cannot mask an unparseable DSN".
func TestBuildDSNAcceptsTheTCPPrefix(t *testing.T) {
	cases := []struct {
		server       string
		wantHost     string
		wantInstance string
		wantPort     uint64
	}{
		{"tcp:x.database.windows.net,1433", "x.database.windows.net", "", 1433},
		{"TCP:myserver", "myserver", "", 0},
		{`tcp:myserver\SQLEXPRESS,1434`, "myserver", "SQLEXPRESS", 1434},
		{"tcp:[fe80::1]:1500", "fe80::1", "", 1500},
		// Not a prefix: a host that happens to be called tcp, on a port.
		{"tcp:1500", "tcp", "", 1500},
	}
	for _, c := range cases {
		t.Run(c.server, func(t *testing.T) {
			dsn, _, err := buildDSN(ConnectionOptions{Server: c.server, User: "sa", Password: "p"})
			if err != nil {
				t.Fatalf("buildDSN: %v", err)
			}
			cfg, err := msdsn.Parse(dsn)
			if err != nil {
				t.Fatalf("msdsn.Parse(%q): %v", dsn, err)
			}
			if cfg.Host != c.wantHost || cfg.Instance != c.wantInstance || cfg.Port != c.wantPort {
				t.Errorf("%q → host/instance/port = %q/%q/%d, want %q/%q/%d", dsn,
					cfg.Host, cfg.Instance, cfg.Port, c.wantHost, c.wantInstance, c.wantPort)
			}
			if _, err := (ConnectionOptions{Server: c.server, User: "sa", Password: "p"}).ConnectionString(true); err != nil {
				t.Errorf("ConnectionString(masked): %v", err)
			}
		})
	}
}

// TestBuildDSNRefusesOtherProtocolsByName (S10): the driver dials TCP only,
// so np:, lpc: and admin: are refused with an error naming the protocol
// rather than bracketed into an IPv6 host that fails somewhere less obvious.
func TestBuildDSNRefusesOtherProtocolsByName(t *testing.T) {
	for server, want := range map[string]string{
		`np:\\myserver\pipe\sql\query`: "named pipes",
		"lpc:myserver":                 "shared memory",
		"Admin:myserver":               "admin:",
	} {
		_, _, err := buildDSN(ConnectionOptions{Server: server})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("buildDSN(%q) err = %v, want one naming %q", server, err, want)
		}
	}
	// "admin" on a port is a host, not the DAC prefix.
	if _, _, err := buildDSN(ConnectionOptions{Server: "admin:1433"}); err != nil {
		t.Errorf("buildDSN(admin:1433): %v", err)
	}
}

// TestConnectTimeoutRoundsUpToWholeSeconds (S10): the driver takes whole
// seconds and reads 0 as no timeout at all, so truncating 500ms turned a
// short timeout into an unbounded one.
func TestConnectTimeoutRoundsUpToWholeSeconds(t *testing.T) {
	for d, want := range map[time.Duration]time.Duration{
		500 * time.Millisecond:  time.Second,
		time.Second:             time.Second,
		1500 * time.Millisecond: 2 * time.Second,
		30 * time.Second:        30 * time.Second,
	} {
		p, err := driverParams(ConnectionOptions{Server: "myserver", User: "sa", Password: "p", ConnectTimeout: d})
		if err != nil {
			t.Fatalf("%v: %v", d, err)
		}
		dsn, _, _ := buildDSN(ConnectionOptions{Server: "myserver", User: "sa", Password: "p", ConnectTimeout: d})
		cfg, err := msdsn.Parse(dsn)
		if err != nil {
			t.Fatalf("msdsn.Parse: %v", err)
		}
		if cfg.ConnTimeout != want {
			t.Errorf("ConnectTimeout %v → driver ConnTimeout %v (param %q), want %v", d, cfg.ConnTimeout, p["connection timeout"], want)
		}
	}
}

// TestConnectTimeoutBoundsTheDial (G1): the driver applies "connection
// timeout" only after the dial, which ran on its own 15 s default, so a 500ms
// ConnectTimeout to an unroutable host took 15 s.
func TestConnectTimeoutBoundsTheDial(t *testing.T) {
	for d, want := range map[time.Duration]time.Duration{
		500 * time.Millisecond:  time.Second,
		1500 * time.Millisecond: 2 * time.Second,
		5 * time.Second:         5 * time.Second,
	} {
		opts := ConnectionOptions{Server: "myserver", User: "sa", Password: "p", ConnectTimeout: d}
		dsn, _, err := buildDSN(opts)
		if err != nil {
			t.Fatalf("%v: buildDSN: %v", d, err)
		}
		cfg, err := msdsn.Parse(dsn)
		if err != nil {
			t.Fatalf("msdsn.Parse: %v", err)
		}
		if cfg.DialTimeout != want || cfg.ConnTimeout != want {
			t.Errorf("ConnectTimeout %v → DialTimeout %v, ConnTimeout %v; want both %v", d, cfg.DialTimeout, cfg.ConnTimeout, want)
		}

		// baseDSN, the access-token-provider path, shares the builder.
		base, err := baseDSN(opts)
		if err != nil {
			t.Fatalf("%v: baseDSN: %v", d, err)
		}
		if cfg, err := msdsn.Parse(base); err != nil || cfg.DialTimeout != want {
			t.Errorf("baseDSN: ConnectTimeout %v → DialTimeout %v (%v), want %v", d, cfg.DialTimeout, err, want)
		}
	}
}

// TestExtraParamsOverrideTheDialTimeout: "dial timeout" is a default derived
// from ConnectTimeout, not a reserved key, so an ExtraParams entry replaces it
// — in any case — and leaves "connection timeout" alone.
func TestExtraParamsOverrideTheDialTimeout(t *testing.T) {
	for _, key := range []string{"dial timeout", "Dial Timeout"} {
		opts := ConnectionOptions{Server: "myserver", User: "sa", Password: "p",
			ConnectTimeout: 30 * time.Second, ExtraParams: url.Values{key: {"3"}}}
		for name, build := range map[string]func() (string, error){
			"buildDSN": func() (string, error) { d, _, err := buildDSN(opts); return d, err },
			"baseDSN":  func() (string, error) { return baseDSN(opts) },
		} {
			dsn, err := build()
			if err != nil {
				t.Fatalf("%s with %q: %v", name, key, err)
			}
			u, _ := url.Parse(dsn)
			n := 0
			for k, vs := range u.Query() {
				if strings.EqualFold(k, "dial timeout") {
					n += len(vs)
				}
			}
			if n != 1 {
				t.Errorf("%s with %q: dial timeout written %d times, want once (%s)", name, key, n, dsn)
			}
			cfg, err := msdsn.Parse(dsn)
			if err != nil {
				t.Fatalf("msdsn.Parse: %v", err)
			}
			if cfg.DialTimeout != 3*time.Second || cfg.ConnTimeout != 30*time.Second {
				t.Errorf("%s with %q: DialTimeout %v, ConnTimeout %v; want 3s, 30s", name, key, cfg.DialTimeout, cfg.ConnTimeout)
			}
		}
	}
}

// TestBuildDSNIPv6InstanceWithoutPortIsAnError: the Browser probe would get
// the bracketed literal as its address, so this must fail up front.
func TestBuildDSNIPv6InstanceWithoutPortIsAnError(t *testing.T) {
	for _, server := range []string{`fe80::1\SQLEXPRESS`, `[fe80::1]\SQLEXPRESS`} {
		_, _, err := buildDSN(ConnectionOptions{Server: server})
		if err == nil || !strings.Contains(err.Error(), "explicit port") {
			t.Errorf("buildDSN(%q) err = %v, want an explicit-port error", server, err)
		}
	}
}

// TestPortReachesTheDriver: ConnectionOptions.Port dials as a port written in
// Server would, a written one wins, and on a named instance it replaces the
// SQL Server Browser lookup. Before Port existed, callers with a separate port
// folded it into Server themselves, and a caller that rewrote Server (an AG
// peer retargeted to its catalog name) lost it.
func TestPortReachesTheDriver(t *testing.T) {
	cases := []struct {
		server       string
		port         int
		wantHost     string
		wantInstance string
		wantPort     uint64
		wantBrowser  bool
	}{
		{"host", 1500, "host", "", 1500, false},
		{"host", 0, "host", "", 0, false}, // the driver's default, 1433
		{`host\SQL2017`, 55253, "host", "SQL2017", 55253, false},
		{`host\SQL2017`, 0, "host", "SQL2017", 0, true},
		{"host,1600", 1500, "host", "", 1600, false},
		{`host\SQL2017,1600`, 1500, "host", "SQL2017", 1600, false},
		{"fe80::1", 1500, "fe80::1", "", 1500, false},
		{`fe80::1\SQLEXPRESS`, 1500, "fe80::1", "SQLEXPRESS", 1500, false},
		{"tcp:host", 1500, "host", "", 1500, false},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s port %d", c.server, c.port), func(t *testing.T) {
			opts := ConnectionOptions{Server: c.server, Port: c.port, User: "sa", Password: "p"}
			dsn, _, err := buildDSN(opts)
			if err != nil {
				t.Fatalf("buildDSN: %v", err)
			}
			cfg, err := msdsn.Parse(dsn)
			if err != nil {
				t.Fatalf("msdsn.Parse(%q): %v", dsn, err)
			}
			if cfg.Host != c.wantHost || cfg.Instance != c.wantInstance || cfg.Port != c.wantPort {
				t.Errorf("%q → host/instance/port = %q/%q/%d, want %q/%q/%d", dsn,
					cfg.Host, cfg.Instance, cfg.Port, c.wantHost, c.wantInstance, c.wantPort)
			}
			if got := dialerFor(opts) != nil; got != c.wantBrowser {
				t.Errorf("dialerFor Browser dialer = %v, want %v", got, c.wantBrowser)
			}
		})
	}
	for _, port := range []int{-1, 65536} {
		_, _, err := buildDSN(ConnectionOptions{Server: "host", Port: port})
		if err == nil || !strings.Contains(err.Error(), "out of range") {
			t.Errorf("buildDSN(Port %d) err = %v, want out of range", port, err)
		}
	}
}

func TestExtraParamsReachTheDriver(t *testing.T) {
	for _, auth := range []AuthMethod{AuthSQLServer, AuthWindows, AuthEntraDefault} {
		dsn, _, err := buildDSN(ConnectionOptions{
			Server:   "myserver",
			Database: "db", // the driver refuses ReadOnly intent without one
			Auth:     auth,
			ExtraParams: url.Values{
				"ApplicationIntent": {"ReadOnly"},
				"packet size":       {"8192"},
			},
		})
		if err != nil {
			t.Fatalf("auth %d: buildDSN: %v", auth, err)
		}
		cfg, err := msdsn.Parse(dsn)
		if err != nil {
			t.Fatalf("auth %d: msdsn.Parse(%q): %v", auth, dsn, err)
		}
		if !cfg.ReadOnlyIntent {
			t.Errorf("auth %d: ReadOnlyIntent = false, want true (%s)", auth, dsn)
		}
		if cfg.PacketSize != 8192 {
			t.Errorf("auth %d: PacketSize = %d, want 8192 (%s)", auth, cfg.PacketSize, dsn)
		}
	}

	dsn, err := baseDSN(ConnectionOptions{Server: "myserver", ExtraParams: url.Values{"packet size": {"8192"}}})
	if err != nil {
		t.Fatalf("baseDSN: %v", err)
	}
	if cfg, err := msdsn.Parse(dsn); err != nil || cfg.PacketSize != 8192 {
		t.Errorf("baseDSN: PacketSize not carried (%s, %v)", dsn, err)
	}
}

func TestExtraParamsRefuseWhatAnOptionControls(t *testing.T) {
	cases := []url.Values{
		{"database": {"other"}},
		{"Database": {"other"}},
		{"initial catalog": {"other"}},
		{"TrustServerCertificate": {"true"}},
		{"trust server certificate": {"true"}},
		{"encrypt": {"disable"}},
		{"user id": {"x"}},
		{"PWD": {"x"}},
		{"server": {"elsewhere"}},
		{"app name": {"x"}},
		{"krb5-realm": {"EXAMPLE.COM"}},
		{"fedauth": {"ActiveDirectoryDefault"}},
		{"  hostNameInCertificate ": {"x"}},
		{"": {"x"}},
		{"packet size": {"4096", "8192"}},
		{"packet size": {"4096"}, "Packet Size": {"8192"}},
	}
	for i, extra := range cases {
		opts := ConnectionOptions{Server: "myserver", User: "sa", Password: "p", ExtraParams: extra}
		_, _, err := buildDSN(opts)
		pe, ok := errors.AsType[*ExtraParamError](err)
		if !ok {
			t.Errorf("buildDSN with ExtraParams %v: err = %v, want an *ExtraParamError", extra, err)
		} else if wantReserved := i < 13; pe.Reserved != wantReserved {
			t.Errorf("ExtraParams %v: Reserved = %v, want %v", extra, pe.Reserved, wantReserved)
		}
		if _, err := baseDSN(opts); err == nil {
			t.Errorf("baseDSN with ExtraParams %v: want an error, got none", extra)
		}
	}
}

// TestReservedDSNKeysCoverEveryKeyGosmoWrites builds a DSN for every auth
// method with every field set and checks that each key in it — plus the two
// the URL form carries outside its query — is one ExtraParams may not set. A
// new key written by buildDSN without a reservedDSNKeys entry would let a
// caller's extra parameter collide with it.
func TestReservedDSNKeysCoverEveryKeyGosmoWrites(t *testing.T) {
	for auth := range fedauthValue {
		checkReservedKeys(t, auth, false)
	}
	checkReservedKeys(t, AuthSQLServer, false)
	for _, kerberos := range []bool{false, true} {
		checkReservedKeys(t, AuthWindows, kerberos)
	}
	for _, k := range []string{"server", "port", "user id", "password"} {
		if !reservedDSNKeys[k] {
			t.Errorf("reservedDSNKeys lacks %q, which the URL form carries in its host/userinfo", k)
		}
	}
}

func checkReservedKeys(t *testing.T, auth AuthMethod, kerberos bool) {
	t.Helper()
	dns := true
	opts := ConnectionOptions{
		Server: "myserver", Database: "db", Auth: auth,
		User: "u", Password: "p", TenantID: "t", ClientID: "c",
		ClientCertPath: "/c.pem", ClientCertPassword: "cp", AccessToken: "tok",
		ApplicationClientID: "a", ServerSPN: "MSSQLSvc/x", Encrypt: "strict",
		TrustServerCertificate: true, HostNameInCertificate: "h",
		DisableInstanceDiscovery: true, SendCertificateChain: true, TokenFilePath: "/t",
	}
	if kerberos {
		opts.Kerberos = KerberosOptions{ConfigFile: "/k", CredCacheFile: "/c", KeytabFile: "/kt",
			Realm: "R", DNSLookupKDC: &dns, UDPPreferenceLimit: 1}
	}
	applyDefaults(&opts)
	dsn, _, err := buildDSN(opts)
	if err != nil {
		t.Fatalf("auth %d: buildDSN: %v", auth, err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("auth %d: url.Parse: %v", auth, err)
	}
	for k := range u.Query() {
		lk := strings.ToLower(k)
		if !reservedDSNKeys[lk] && !overridableDSNKeys[lk] && !strings.HasPrefix(lk, "krb5-") {
			t.Errorf("auth %d: buildDSN writes %q, which reservedDSNKeys does not list", auth, k)
		}
	}
}

func TestConnectionStringIsTheDialledDSN(t *testing.T) {
	opts := ConnectionOptions{Server: `myserver\SQLEXPRESS`, User: "sa", Password: "p@ss&x", Encrypt: "mandatory"}
	got, err := opts.ConnectionString(false)
	if err != nil {
		t.Fatalf("ConnectionString: %v", err)
	}
	withDefaults := opts
	applyDefaults(&withDefaults)
	want, _, err := buildDSN(withDefaults)
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	if got != want {
		t.Errorf("ConnectionString(false) = %q, want buildDSN's %q", got, want)
	}
	cfg, err := msdsn.Parse(got)
	if err != nil {
		t.Fatalf("msdsn.Parse: %v", err)
	}
	if cfg.Database != "master" || cfg.AppName != "gosmo" || cfg.Password != "p@ss&x" {
		t.Errorf("database/app/password = %q/%q/%q, want master/gosmo/p@ss&x", cfg.Database, cfg.AppName, cfg.Password)
	}
	if opts.Database != "" {
		t.Error("ConnectionString changed the receiver's Database")
	}
}

func TestConnectionStringMasksEverySecret(t *testing.T) {
	cases := []struct {
		name   string
		opts   ConnectionOptions
		secret string
	}{
		{"sql login", ConnectionOptions{Server: "s", User: "sa", Password: "hunter2"}, "hunter2"},
		{"kerberos password", ConnectionOptions{Server: "s", Auth: AuthWindows, User: "u@R", Password: "hunter2",
			Kerberos: KerberosOptions{Realm: "R"}}, "hunter2"},
		{"entra password", ConnectionOptions{Server: "s", Auth: AuthEntraPassword, User: "u", Password: "hunter2"}, "hunter2"},
		{"client secret", ConnectionOptions{Server: "s", Auth: AuthEntraServicePrincipal, User: "app", Password: "hunter2"}, "hunter2"},
		{"cert password", ConnectionOptions{Server: "s", Auth: AuthEntraServicePrincipal, User: "app",
			ClientCertPath: "/c.pfx", ClientCertPassword: "hunter2"}, "hunter2"},
		{"access token", ConnectionOptions{Server: "s", Auth: AuthEntraServicePrincipalAccessToken, AccessToken: "hunter2"}, "hunter2"},
		{"obo user assertion", ConnectionOptions{Server: "s", Auth: AuthEntraOnBehalfOf, User: "app",
			AccessToken: "hunter2", ClientCertPath: "/c.pfx"}, "hunter2"},
		{"pipelines system token", ConnectionOptions{Server: "s", Auth: AuthEntraAzurePipelines, User: "app",
			ExtraParams: url.Values{"SystemToken": {"hunter2"}}}, "hunter2"},
		{"obo client assertion", ConnectionOptions{Server: "s", Auth: AuthEntraOnBehalfOf, User: "app",
			AccessToken: "a", ExtraParams: url.Values{"clientassertion": {"hunter2"}}}, "hunter2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plain, err := c.opts.ConnectionString(false)
			if err != nil {
				t.Fatalf("ConnectionString(false): %v", err)
			}
			if !strings.Contains(plain, c.secret) {
				t.Fatalf("unmasked %q lacks the secret — the case tests nothing", plain)
			}
			masked, err := c.opts.ConnectionString(true)
			if err != nil {
				t.Fatalf("ConnectionString(true): %v", err)
			}
			if strings.Contains(masked, c.secret) {
				t.Errorf("masked %q still carries the secret", masked)
			}
			if !strings.Contains(masked, maskedSecret) {
				t.Errorf("masked %q carries no placeholder, so it hides that a secret is set", masked)
			}
		})
	}

	// No secret, no placeholder: the masked form must not claim one is set.
	masked, err := ConnectionOptions{Server: "s", User: "sa"}.ConnectionString(true)
	if err != nil {
		t.Fatalf("ConnectionString: %v", err)
	}
	if strings.Contains(masked, maskedSecret) {
		t.Errorf("masked %q shows a placeholder for an empty password", masked)
	}
}

func TestConnectionStringReportsConnectsErrors(t *testing.T) {
	if _, err := (ConnectionOptions{}).ConnectionString(true); err == nil {
		t.Error("empty Server: want an error")
	}
	bad := ConnectionOptions{Server: "s", ExtraParams: url.Values{"database": {"x"}}}
	if _, err := bad.ConnectionString(true); err == nil {
		t.Error("reserved extra parameter: want an error")
	}
	tp := ConnectionOptions{Server: "s", User: "sa", Password: "hunter2",
		AccessTokenProvider: func(context.Context) (string, error) { return "tok", nil }}
	got, err := tp.ConnectionString(false)
	if err != nil {
		t.Fatalf("token provider: %v", err)
	}
	if strings.Contains(got, "hunter2") || strings.Contains(got, "tok") {
		t.Errorf("token-provider DSN %q carries credentials the provider path never sends", got)
	}
}

// TestExtraParamsADOSynonymsMatchTheDriver gives ExtraParams every keyword
// Microsoft.Data.SqlClient documents and requires the driver to read gosmo's
// URL exactly as it reads the same keyword appended to the key=value form,
// where go-mssqldb applies its (unexported) ADO.NET synonym map. The URL form
// does not translate synonyms, so before adoDSNSynonyms "Application
// Intent=ReadOnly" parsed without error and without effect. A synonym a
// future driver adds to a keyword listed here fails this test instead of
// silently diverging. Keywords reservedDSNKeys refuses are skipped: they never
// reach the DSN.
func TestExtraParamsADOSynonymsMatchTheDriver(t *testing.T) {
	keywords := []string{
		"Addr", "Address", "App", "Application Intent", "ApplicationIntent", "Application Name",
		"Async", "Asynchronous Processing", "Attestation Protocol", "AttachDBFilename",
		"Authentication", "Column Encryption Setting", "Command Timeout",
		"Connect Retry Count", "ConnectRetryCount", "Connect Retry Interval", "ConnectRetryInterval",
		"Connect Timeout", "Connection Lifetime", "Connection Timeout", "Context Connection",
		"Current Language", "Data Source", "Database", "Enclave Attestation Url", "Encrypt",
		"Enlist", "Extended Properties", "Failover Partner", "Failover Partner SPN",
		"FailoverPartnerSPN", "Host Name In Certificate", "HostNameInCertificate",
		"Initial Catalog", "Initial File Name", "Integrated Security", "IP Address Preference",
		"IPAddressPreference", "Language", "Load Balance Timeout", "Max Pool Size",
		"Min Pool Size", "Multiple Active Result Sets", "MultipleActiveResultSets",
		"Multi Subnet Failover", "MultiSubnetFailover", "Net", "Network Address",
		"Network Library", "Packet Size", "Password", "Persist Security Info",
		"PersistSecurityInfo", "Pool Blocking Period", "PoolBlockingPeriod", "Pooling", "PWD",
		"Replication", "Server", "Server Certificate", "ServerCertificate", "Server SPN",
		"ServerSPN", "Timeout", "Transaction Binding", "Trust Server Certificate",
		"TrustServerCertificate", "Trusted_Connection", "Type System Version", "UID", "User",
		"User ID", "User Instance", "Workstation ID", "WSID",
	}
	// A value that moves the driver's Config off its default where it reads
	// the key at all. "server certificate" names a missing file, so the driver
	// fails to parse it — in both forms, if both read it.
	sample := map[string]string{
		"application intent": "ReadOnly", "applicationintent": "ReadOnly",
		"column encryption setting": "Enabled",
		"failover partner":          "fp,1500", "failover partner spn": "MSSQLSvc/fp",
		"failoverpartnerspn":    "MSSQLSvc/fp",
		"multi subnet failover": "false", "multisubnetfailover": "false",
		"server certificate": "/nonexistent/gosmo-test.pem", "servercertificate": "/nonexistent/gosmo-test.pem",
		"workstation id": "ws1", "wsid": "ws1", "packet size": "8192",
	}
	base := ConnectionOptions{Server: "myserver", Database: "db", User: "sa", Password: "p"}
	applyDefaults(&base)
	baseDSN, _, err := buildDSN(base)
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	baseCfg, err := msdsn.Parse(baseDSN)
	if err != nil {
		t.Fatalf("msdsn.Parse(%q): %v", baseDSN, err)
	}
	var ado strings.Builder
	for k, v := range baseCfg.Parameters {
		fmt.Fprintf(&ado, "%s=%s;", k, adoQuote(v))
	}

	for _, kw := range keywords {
		val, ok := sample[strings.ToLower(kw)]
		if !ok {
			val = "1"
		}
		opts := base
		opts.ExtraParams = url.Values{kw: {val}}
		dsn, _, err := buildDSN(opts)
		if pe, ok := errors.AsType[*ExtraParamError](err); ok && pe.Reserved {
			continue
		} else if err != nil {
			t.Errorf("%s: buildDSN: %v", kw, err)
			continue
		}
		urlCfg, urlErr := msdsn.Parse(dsn)
		adoCfg, adoErr := msdsn.Parse(ado.String() + kw + "=" + adoQuote(val))
		switch {
		case (urlErr == nil) != (adoErr == nil):
			t.Errorf("%s=%s: driver parse error differs: URL form %v, key=value form %v", kw, val, urlErr, adoErr)
		case urlErr == nil:
			if u, a := comparableConfig(urlCfg), comparableConfig(adoCfg); !reflect.DeepEqual(u, a) {
				t.Errorf("%s=%s: driver reads the URL form as\n%+v\nbut the key=value form as\n%+v", kw, val, u, a)
			}
		}
	}

	// A synonym beside its canonical spelling is one key given twice.
	opts := base
	opts.ExtraParams = url.Values{"Application Intent": {"ReadOnly"}, "ApplicationIntent": {"ReadWrite"}}
	if _, _, err := buildDSN(opts); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Errorf("synonym and canonical key together: err = %v, want \"given more than once\"", err)
	}
}

func adoQuote(v string) string {
	if strings.ContainsAny(v, `;"`) {
		return `"` + strings.ReplaceAll(v, `"`, `""`) + `"`
	}
	return v
}

// comparableConfig drops the msdsn.Config fields that differ between two
// parses of the same settings: the random ActivityID, the TLS config (its
// pointer and callbacks; ServerName and InsecureSkipVerify are kept) and the
// raw Parameters map, whose keys follow the DSN spelling.
func comparableConfig(c msdsn.Config) msdsn.Config {
	c.ActivityID = nil
	c.Parameters = nil
	if c.TLSConfig != nil {
		c.TLSConfig = &tls.Config{ServerName: c.TLSConfig.ServerName, InsecureSkipVerify: c.TLSConfig.InsecureSkipVerify}
	}
	return c
}
