package gosmo

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

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
		if !reservedDSNKeys[lk] && !strings.HasPrefix(lk, "krb5-") {
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
