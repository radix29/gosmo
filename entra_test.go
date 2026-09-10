package gosmo

import (
	"cmp"
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/microsoft/go-mssqldb/azuread"
)

// entraConfigFor is the entraConfig buildConnector would give opts' token
// callback.
func entraConfigFor(opts ConnectionOptions) (*entraConfig, error) {
	applyDefaults(&opts)
	dsn, _, err := buildDSN(opts)
	if err != nil {
		return nil, err
	}
	return parseEntraConfig(dsn, opts.DeviceCodePrompt)
}

// specTenant is the tenant s hands its azidentity constructor, positionally
// or through its options.
func specTenant(s entraCredSpec) string {
	switch o := s.options.(type) {
	case *azidentity.DefaultAzureCredentialOptions:
		return o.TenantID
	case *azidentity.InteractiveBrowserCredentialOptions:
		return o.TenantID
	case *azidentity.DeviceCodeCredentialOptions:
		return o.TenantID
	case *azidentity.AzureCLICredentialOptions:
		return o.TenantID
	case *azidentity.AzureDeveloperCLICredentialOptions:
		return o.TenantID
	case *azidentity.ManagedIdentityCredentialOptions:
		return ""
	}
	return s.tenant // the constructors that take it positionally
}

// fakeCred is an azcore.TokenCredential that counts the tokens it issues.
type fakeCred struct {
	calls   atomic.Int32
	expires time.Duration // token lifetime; 0 means an hour
	gate    chan struct{} // when non-nil, GetToken waits for it to close

	mu     sync.Mutex
	scopes []string
}

func (f *fakeCred) GetToken(ctx context.Context, o policy.TokenRequestOptions) (azcore.AccessToken, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.scopes = append(f.scopes, o.Scopes...)
	f.mu.Unlock()
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return azcore.AccessToken{}, ctx.Err()
		}
	}
	return azcore.AccessToken{Token: "tok", ExpiresOn: now().Add(cmp.Or(f.expires, time.Hour))}, nil
}

// fakeCache is an EntraCache whose credentials are one shared fakeCred, with
// every spec it was asked to build recorded.
type fakeCache struct {
	*EntraCache
	cred *fakeCred

	mu     sync.Mutex
	specs  []entraCredSpec
	probed []string // the Server of every probe, in order
	// probeErr, when set, is what every probe fails with.
	probeErr error
}

// newFakeCache's probe answers what an Azure SQL login announces (testSPN,
// testSTS) without dialling.
func newFakeCache() *fakeCache {
	f := &fakeCache{EntraCache: NewEntraCache(), cred: &fakeCred{}}
	f.newCredential = func(s entraCredSpec) (azcore.TokenCredential, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.specs = append(f.specs, s)
		return f.cred, nil
	}
	f.probe = func(_ context.Context, opts ConnectionOptions, _ *entraConfig) (serverSignIn, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.probed = append(f.probed, opts.Server)
		if f.probeErr != nil {
			return serverSignIn{}, f.probeErr
		}
		return serverSignIn{spn: testSPN, stsURL: testSTS}, nil
	}
	return f
}

func (f *fakeCache) constructions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.specs)
}

const (
	testSPN    = "https://database.windows.net/"
	testTenant = "99999999-0000-0000-0000-000000000000"
	testSTS    = "https://login.windows.net/" + testTenant
)

func mustEntraConfig(t *testing.T, opts ConnectionOptions) *entraConfig {
	t.Helper()
	cfg, err := entraConfigFor(opts)
	if err != nil {
		t.Fatalf("entraConfigFor: %v", err)
	}
	return cfg
}

// TestEntraCacheSignsInOncePerIdentity: many physical connections — from one
// pool at once, from several pools, to several servers — authenticating as
// the same identity construct one credential and fetch one token (plan G9,
// S5). The driver's own connector constructs one per connection.
func TestEntraCacheSignsInOncePerIdentity(t *testing.T) {
	fc := newFakeCache()
	ctx := context.Background()
	var wg sync.WaitGroup
	for _, server := range []string{"a.database.windows.net", "b.database.windows.net"} {
		cfg := mustEntraConfig(t, ConnectionOptions{Server: server, Auth: AuthEntraInteractive})
		for range 20 {
			wg.Go(func() {
				if _, err := fc.token(ctx, cfg, testSPN, testSTS); err != nil {
					t.Error(err)
				}
			})
		}
	}
	wg.Wait()
	if n := fc.constructions(); n != 1 {
		t.Errorf("credentials constructed = %d, want 1", n)
	}
	if n := fc.cred.calls.Load(); n != 1 {
		t.Errorf("tokens fetched = %d, want 1", n)
	}
}

// TestEntraCacheSeparatesIdentities: what decides who signs in decides the
// credential — including the server's tenant for a method that falls back to
// it, and the secret, so a corrected secret is not answered by the cached,
// failing credential. Clear forgets them all.
func TestEntraCacheSeparatesIdentities(t *testing.T) {
	fc := newFakeCache()
	ctx := context.Background()
	sp := ConnectionOptions{Server: "a.database.windows.net", Auth: AuthEntraServicePrincipal, User: "app", Password: "s1"}
	get := func(opts ConnectionOptions, sts string) {
		t.Helper()
		if _, err := fc.token(ctx, mustEntraConfig(t, opts), testSPN, sts); err != nil {
			t.Fatal(err)
		}
	}
	get(sp, "https://login.windows.net/tenant-a")
	get(sp, "https://login.windows.net/tenant-a")
	if n := fc.constructions(); n != 1 {
		t.Fatalf("same identity: constructions = %d, want 1", n)
	}
	get(sp, "https://login.windows.net/tenant-b")
	if n := fc.constructions(); n != 2 {
		t.Errorf("another server tenant: constructions = %d, want 2", n)
	}
	sp2 := sp
	sp2.Password = "s2"
	get(sp2, "https://login.windows.net/tenant-a")
	if n := fc.constructions(); n != 3 {
		t.Errorf("another secret: constructions = %d, want 3", n)
	}
	fc.Clear()
	get(sp, "https://login.windows.net/tenant-a")
	if n := fc.constructions(); n != 4 {
		t.Errorf("after Clear: constructions = %d, want 4", n)
	}

	// The public cloud's authority aliases are one authority.
	in := ConnectionOptions{Server: "a.database.windows.net", Auth: AuthEntraInteractive}
	get(in, "https://login.windows.net/t")
	get(in, "https://login.microsoftonline.com/t")
	if n := fc.constructions(); n != 5 {
		t.Errorf("authority aliases: constructions = %d, want 5", n)
	}
}

// TestEntraCacheRenewsAnExpiringToken: a token within five minutes of expiry
// is not handed to a new connection; a fresh one is.
func TestEntraCacheRenewsAnExpiringToken(t *testing.T) {
	fc := newFakeCache()
	fc.cred.expires = 4 * time.Minute
	cfg := mustEntraConfig(t, ConnectionOptions{Server: "a.database.windows.net", Auth: AuthEntraAzCLI})
	for range 2 {
		if _, err := fc.token(context.Background(), cfg, testSPN, testSTS); err != nil {
			t.Fatal(err)
		}
	}
	if n := fc.cred.calls.Load(); n != 2 {
		t.Errorf("tokens fetched = %d, want 2 (the first was too close to expiry to reuse)", n)
	}
}

// TestEntraTokenWaitHonoursContext: a connection waiting behind another's
// sign-in gives up when its own context ends, rather than waiting out a
// sign-in that may take minutes.
func TestEntraTokenWaitHonoursContext(t *testing.T) {
	fc := newFakeCache()
	fc.cred.gate = make(chan struct{})
	cfg := mustEntraConfig(t, ConnectionOptions{Server: "a.database.windows.net", Auth: AuthEntraInteractive})

	first := make(chan error, 1)
	go func() {
		_, err := fc.token(context.Background(), cfg, testSPN, testSTS)
		first <- err
	}()
	for fc.cred.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := fc.token(ctx, cfg, testSPN, testSTS); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiter err = %v, want context.DeadlineExceeded", err)
	}
	close(fc.cred.gate)
	if err := <-first; err != nil {
		t.Errorf("first sign-in: %v", err)
	}
	if n := fc.cred.calls.Load(); n != 1 {
		t.Errorf("tokens fetched = %d, want 1", n)
	}
}

// TestEntraCredentialSpecPerMethod: each method reaches the azidentity
// constructor the driver would call for it, with the options that decide who
// signs in.
func TestEntraCredentialSpecPerMethod(t *testing.T) {
	const tenant = "T"
	cases := []struct {
		name  string
		opts  ConnectionOptions
		kind  entraCredKind
		check func(t *testing.T, s entraCredSpec)
	}{
		{"Default", ConnectionOptions{Auth: AuthEntraDefault}, credDefault, nil},
		{"Integrated", ConnectionOptions{Auth: AuthEntraIntegrated}, credDefault, nil},
		{
			"Password", ConnectionOptions{Auth: AuthEntraPassword, User: "alice@contoso.com", Password: "pw"},
			credUsernamePassword, func(t *testing.T, s entraCredSpec) {
				if s.clientID != defaultPublicClientID || s.user != "alice@contoso.com" || s.password != "pw" {
					t.Errorf("client, user, password = %q, %q, %q", s.clientID, s.user, s.password)
				}
				if s.tenant != "server-tenant" {
					t.Errorf("tenant = %q, want the server's when TenantID is empty", s.tenant)
				}
			},
		},
		{
			"MSI user-assigned", ConnectionOptions{Auth: AuthEntraMSI, ClientID: "mi"},
			credManagedIdentity, func(t *testing.T, s entraCredSpec) {
				if id := s.options.(*azidentity.ManagedIdentityCredentialOptions).ID; id != azidentity.ClientID("mi") {
					t.Errorf("ID = %v, want ClientID(mi)", id)
				}
			},
		},
		{
			"Service Principal secret", ConnectionOptions{Auth: AuthEntraServicePrincipal, User: "app", Password: "s"},
			credClientSecret, func(t *testing.T, s entraCredSpec) {
				if s.clientID != "app" || s.secret != "s" || s.tenant != "server-tenant" {
					t.Errorf("client, secret, tenant = %q, %q, %q", s.clientID, s.secret, s.tenant)
				}
			},
		},
		{
			"Service Principal certificate", ConnectionOptions{Auth: AuthEntraServicePrincipal, User: "app",
				TenantID: tenant, ClientCertPath: "/c.pem", ClientCertPassword: "cp", SendCertificateChain: true},
			credClientCertificate, func(t *testing.T, s entraCredSpec) {
				o := s.options.(*azidentity.ClientCertificateCredentialOptions)
				if s.certPath != "/c.pem" || s.secret != "cp" || s.tenant != tenant || !o.SendCertificateChain {
					t.Errorf("cert, password, tenant, chain = %q, %q, %q, %v", s.certPath, s.secret, s.tenant, o.SendCertificateChain)
				}
			},
		},
		{
			"Interactive", ConnectionOptions{Auth: AuthEntraInteractive, User: "alice@contoso.com"},
			credInteractiveBrowser, func(t *testing.T, s entraCredSpec) {
				o := s.options.(*azidentity.InteractiveBrowserCredentialOptions)
				if o.ClientID != defaultPublicClientID || o.LoginHint != "alice@contoso.com" {
					t.Errorf("ClientID, LoginHint = %q, %q", o.ClientID, o.LoginHint)
				}
				if got := o.Cloud.ActiveDirectoryAuthorityHost; got != "https://login.microsoftonline.com/" {
					t.Errorf("authority = %q, want the server's, canonicalised", got)
				}
				// Not azidentity's "organizations": a personal Microsoft
				// account that is a member of the server's tenant is refused
				// there.
				if o.TenantID != "server-tenant" {
					t.Errorf("TenantID = %q, want the server's when TenantID is empty", o.TenantID)
				}
			},
		},
		{
			"Device Code", ConnectionOptions{Auth: AuthEntraDeviceCode, ApplicationClientID: "app-id"},
			credDeviceCode, func(t *testing.T, s entraCredSpec) {
				o := s.options.(*azidentity.DeviceCodeCredentialOptions)
				if o.ClientID != "app-id" || o.Cloud.ActiveDirectoryAuthorityHost == "" || o.TenantID != "server-tenant" {
					t.Errorf("ClientID, authority, TenantID = %q, %q, %q", o.ClientID, o.Cloud.ActiveDirectoryAuthorityHost, o.TenantID)
				}
			},
		},
		{"Azure CLI", ConnectionOptions{Auth: AuthEntraAzCLI}, credAzureCLI, nil},
		{"Azure Developer CLI", ConnectionOptions{Auth: AuthEntraAzureDeveloperCLI}, credAzureDeveloperCLI, nil},
		{
			"Azure Pipelines", ConnectionOptions{Auth: AuthEntraAzurePipelines, User: "app", TenantID: tenant,
				ExtraParams: url.Values{"serviceconnectionid": {"sc"}, "systemtoken": {"st"}}},
			credAzurePipelines, func(t *testing.T, s entraCredSpec) {
				if s.clientID != "app" || s.tenant != tenant || s.serviceConnectionID != "sc" || s.systemToken != "st" {
					t.Errorf("client, tenant, connection, token = %q, %q, %q, %q",
						s.clientID, s.tenant, s.serviceConnectionID, s.systemToken)
				}
			},
		},
		{
			"On-Behalf-Of secret", ConnectionOptions{Auth: AuthEntraOnBehalfOf, User: "app", AccessToken: "ua", Password: "s"},
			credOnBehalfOfSecret, func(t *testing.T, s entraCredSpec) {
				if s.userAssertion != "ua" || s.secret != "s" {
					t.Errorf("assertion, secret = %q, %q", s.userAssertion, s.secret)
				}
			},
		},
		{
			"On-Behalf-Of certificate", ConnectionOptions{Auth: AuthEntraOnBehalfOf, User: "app", AccessToken: "ua",
				ClientCertPath: "/c.pem"},
			credOnBehalfOfCertificate, nil,
		},
		{
			"On-Behalf-Of client assertion", ConnectionOptions{Auth: AuthEntraOnBehalfOf, User: "app", AccessToken: "ua",
				ExtraParams: url.Values{"clientassertion": {"ca"}}},
			credOnBehalfOfAssertion, func(t *testing.T, s entraCredSpec) {
				if s.clientAssertion != "ca" {
					t.Errorf("clientAssertion = %q", s.clientAssertion)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearEntraEnv(t)
			c.opts.Server = "a.database.windows.net"
			s := mustEntraConfig(t, c.opts).credentialSpec("https://login.windows.net", "server-tenant")
			if s.kind != c.kind {
				t.Fatalf("kind = %d, want %d", s.kind, c.kind)
			}
			if c.check != nil {
				c.check(t, s)
			}
			// The real constructor accepts the spec — none of them dials. The
			// certificate kinds fail reading a file that does not exist; Azure
			// Pipelines needs the variable every pipeline job sets.
			t.Setenv("SYSTEM_OIDCREQUESTURI", "https://example.invalid/oidc")
			if _, err := buildEntraCredential(s); err != nil && s.certPath == "" {
				t.Errorf("buildEntraCredential: %v", err)
			}
		})
	}
}

// TestEntraExtraParamsReachTheCredential: the Entra keys a caller could pass
// through ExtraParams to the driver's own connector mean the same to gosmo's.
func TestEntraExtraParamsReachTheCredential(t *testing.T) {
	spec := func(opts ConnectionOptions) entraCredSpec {
		t.Helper()
		opts.Server = "a.database.windows.net"
		return mustEntraConfig(t, opts).credentialSpec("", "")
	}
	s := spec(ConnectionOptions{Auth: AuthEntraMSI, ClientID: "mi", ExtraParams: url.Values{"resource id": {"/r"}}})
	if id := s.options.(*azidentity.ManagedIdentityCredentialOptions).ID; id != azidentity.ResourceID("/r") {
		t.Errorf("MSI ID = %v, want ResourceID(/r) — the driver prefers it to the client ID", id)
	}
	s = spec(ConnectionOptions{Auth: AuthEntraDefault, DisableInstanceDiscovery: true,
		ExtraParams: url.Values{"AdditionallyAllowedTenants": {"a; b"}}})
	o := s.options.(*azidentity.DefaultAzureCredentialOptions)
	if !slices.Equal(o.AdditionallyAllowedTenants, []string{"a", "b"}) || !o.DisableInstanceDiscovery {
		t.Errorf("AdditionallyAllowedTenants, DisableInstanceDiscovery = %q, %v", o.AdditionallyAllowedTenants, o.DisableInstanceDiscovery)
	}
}

// TestEveryDriverWorkflowIsMappedOrRefused cross-checks gosmo's credential
// mapping against the workflows the azuread driver accepts, so a driver bump
// that adds one is noticed rather than silently unreachable.
func TestEveryDriverWorkflowIsMappedOrRefused(t *testing.T) {
	_, err := azuread.NewConnector("sqlserver://h?fedauth=NoSuchWorkflow")
	if err == nil {
		t.Fatal("driver accepted an unknown workflow")
	}
	msg := err.Error()
	i, j := strings.LastIndexByte(msg, '['), strings.LastIndexByte(msg, ']')
	if i < 0 || j < i {
		t.Fatalf("driver's unknown-workflow error no longer lists the workflows: %v", err)
	}
	// The driver's list omits the access-token workflow, which it accepts.
	accepted := append(strings.Fields(msg[i+1:j]), azuread.ActiveDirectoryServicePrincipalAccessToken)

	mapped := map[string]bool{}
	for _, v := range fedauthValue {
		mapped[v] = true
	}
	refused := map[string]string{
		azuread.ActiveDirectoryApplication:      "synonym of ServicePrincipal; gosmo writes the canonical name",
		azuread.ActiveDirectoryMSI:              "synonym of ManagedIdentity; gosmo writes the canonical name",
		azuread.ActiveDirectoryEnvironment:      "no AuthMethod (plan G15)",
		azuread.ActiveDirectoryWorkloadIdentity: "no AuthMethod (plan G7, G15)",
		azuread.ActiveDirectoryClientAssertion:  "no AuthMethod (plan G15)",
	}
	for _, w := range accepted {
		switch {
		case mapped[w]:
			if _, err := parseEntraConfig("sqlserver://h?fedauth="+w, nil); err != nil {
				t.Errorf("%s: mapped by fedauthValue but parseEntraConfig refuses it: %v", w, err)
			}
		case refused[w] != "":
			if _, err := parseEntraConfig("sqlserver://h?fedauth="+w, nil); err == nil {
				t.Errorf("%s: meant to be refused (%s) but parseEntraConfig accepts it", w, refused[w])
			}
		default:
			t.Errorf("driver workflow %s is neither mapped to an AuthMethod nor deliberately refused", w)
		}
	}
	for w := range mapped {
		if !slices.Contains(accepted, w) {
			t.Errorf("fedauthValue writes %s, which the driver does not accept", w)
		}
	}
}

// TestDeviceCodePromptReplacesStdout: the device code reaches the caller's
// DeviceCodePrompt, in gosmo's own type, and a shared credential uses the
// most recent connection's prompt (plan G11).
func TestDeviceCodePromptReplacesStdout(t *testing.T) {
	fc := newFakeCache()
	var got []string
	prompt := func(tag string) func(context.Context, DeviceCodeMessage) error {
		return func(_ context.Context, m DeviceCodeMessage) error {
			got = append(got, tag+":"+m.UserCode+"@"+m.VerificationURL)
			return nil
		}
	}
	opts := ConnectionOptions{Server: "a.database.windows.net", Auth: AuthEntraDeviceCode, DeviceCodePrompt: prompt("first")}
	if _, err := fc.token(context.Background(), mustEntraConfig(t, opts), testSPN, testSTS); err != nil {
		t.Fatal(err)
	}
	userPrompt := fc.specs[0].options.(*azidentity.DeviceCodeCredentialOptions).UserPrompt
	m := azidentity.DeviceCodeMessage{UserCode: "ABC", VerificationURL: "https://microsoft.com/devicelogin"}
	if err := userPrompt(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	opts.DeviceCodePrompt = prompt("second")
	if _, err := fc.token(context.Background(), mustEntraConfig(t, opts), testSPN, testSTS); err != nil {
		t.Fatal(err)
	}
	if err := userPrompt(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	want := []string{"first:ABC@https://microsoft.com/devicelogin", "second:ABC@https://microsoft.com/devicelogin"}
	if !slices.Equal(got, want) {
		t.Errorf("prompts = %q, want %q", got, want)
	}
}

// TestWarmSignsInBeforeTheConnection: Warm fetches the token the server's
// login will ask for — scope and tenant as the server announced them — so the
// connection itself neither constructs a credential nor signs in (plan G10,
// S4, S6).
func TestWarmSignsInBeforeTheConnection(t *testing.T) {
	fc := newFakeCache()
	opts := ConnectionOptions{Server: "srv.database.windows.net", Auth: AuthEntraInteractive, EntraCache: fc.EntraCache}
	if err := fc.Warm(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if n := fc.cred.calls.Load(); n != 1 {
		t.Fatalf("Warm fetched %d tokens, want 1", n)
	}
	if want := testSPN + "/.default"; fc.cred.scopes[0] != want {
		t.Errorf("Warm scope = %q, want %q", fc.cred.scopes[0], want)
	}
	if got := specTenant(fc.specs[0]); got != testTenant {
		t.Errorf("Warm tenant = %q, want the server's %q", got, testTenant)
	}
	// What an Azure SQL login announces.
	if _, err := fc.token(context.Background(), mustEntraConfig(t, opts), testSPN, testSTS); err != nil {
		t.Fatal(err)
	}
	if n, c := fc.cred.calls.Load(), fc.constructions(); n != 1 || c != 1 {
		t.Errorf("after the connection: tokens = %d, constructions = %d, want 1 and 1", n, c)
	}
}

// TestWarmAsksEachServerOnce: the probe runs once per server, whatever the
// method or identity, and again only after Clear. A server behind a custom DNS
// name, and a service principal with no TenantID — both no-ops when Warm
// guessed from the DNS suffix — are warmed like any other.
func TestWarmAsksEachServerOnce(t *testing.T) {
	fc := newFakeCache()
	ctx := context.Background()
	warm := func(opts ConnectionOptions) {
		t.Helper()
		if err := fc.Warm(ctx, opts); err != nil {
			t.Fatal(err)
		}
	}
	warm(ConnectionOptions{Server: "sql.contoso.com", Auth: AuthEntraInteractive})
	warm(ConnectionOptions{Server: "SQL.contoso.com", Auth: AuthEntraDeviceCode})
	warm(ConnectionOptions{Server: "sql.contoso.com", Auth: AuthEntraServicePrincipal, User: "app", Password: "s"})
	warm(ConnectionOptions{Server: "other.contoso.com", Auth: AuthEntraInteractive})
	fc.Clear()
	warm(ConnectionOptions{Server: "sql.contoso.com", Auth: AuthEntraInteractive})
	if want := []string{"sql.contoso.com", "other.contoso.com", "sql.contoso.com"}; !slices.Equal(fc.probed, want) {
		t.Errorf("probed = %q, want %q", fc.probed, want)
	}
	if got := fc.specs[2].tenant; got != testTenant {
		t.Errorf("service principal tenant = %q, want the server's %q", got, testTenant)
	}
}

// TestWarmReportsAProbeFailure: a server that cannot be asked fails Warm, with
// the server named, and nothing signs in.
func TestWarmReportsAProbeFailure(t *testing.T) {
	fc := newFakeCache()
	fc.probeErr = errors.New("dial tcp: connection refused")
	err := fc.Warm(context.Background(), ConnectionOptions{Server: "srv.database.windows.net", Auth: AuthEntraInteractive})
	if err == nil || !strings.Contains(err.Error(), "srv.database.windows.net") || !errors.Is(err, fc.probeErr) {
		t.Errorf("Warm = %v, want the probe's error, naming the server", err)
	}
	if c := fc.constructions(); c != 0 {
		t.Errorf("constructions = %d, want 0", c)
	}
	// A failure is not remembered: the next Warm asks again.
	fc.probeErr = nil
	if err := fc.Warm(context.Background(), ConnectionOptions{Server: "srv.database.windows.net", Auth: AuthEntraInteractive}); err != nil {
		t.Fatal(err)
	}
	if len(fc.probed) != 2 {
		t.Errorf("probes = %d, want 2", len(fc.probed))
	}
}

// TestWarmIsANoOpWithoutASignIn: nothing to sign in, nothing probed.
func TestWarmIsANoOpWithoutASignIn(t *testing.T) {
	for name, opts := range map[string]ConnectionOptions{
		"SQL auth":                    {Server: "srv.database.windows.net", Auth: AuthSQLServer, User: "u", Password: "p"},
		"access token":                {Server: "srv.database.windows.net", Auth: AuthEntraServicePrincipalAccessToken, AccessToken: "t"},
		"caller mints its own tokens": {Server: "srv.database.windows.net", Auth: AuthEntraInteractive, AccessTokenProvider: func(context.Context) (string, error) { return "", nil }},
	} {
		t.Run(name, func(t *testing.T) {
			fc := newFakeCache()
			if err := fc.Warm(context.Background(), opts); err != nil {
				t.Fatal(err)
			}
			if c, p := fc.constructions(), len(fc.probed); c != 0 || p != 0 {
				t.Errorf("constructions = %d, probes = %d, want 0 and 0", c, p)
			}
		})
	}
}

// TestBuildConnectorEntraAccessToken: the pre-acquired token still builds a
// security-token connector, with no credential or cache involved.
func TestBuildConnectorEntraAccessToken(t *testing.T) {
	fc := newFakeCache()
	opts := ConnectionOptions{Server: "a.database.windows.net", Auth: AuthEntraServicePrincipalAccessToken,
		AccessToken: "tok", EntraCache: fc.EntraCache}
	applyDefaults(&opts)
	if _, err := buildConnector(opts); err != nil {
		t.Fatal(err)
	}
	if c := fc.constructions(); c != 0 {
		t.Errorf("constructions = %d, want 0", c)
	}
}
