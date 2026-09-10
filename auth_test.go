package gosmo

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/microsoft/go-mssqldb/msdsn"
)

func TestAuthMethodIsEntraMethod(t *testing.T) {
	nonEntra := []AuthMethod{AuthSQLServer, AuthWindows}
	for _, m := range nonEntra {
		if m.isEntraMethod() {
			t.Errorf("AuthMethod(%d).isEntraMethod() = true, want false", m)
		}
	}

	entra := []AuthMethod{
		AuthEntraDefault, AuthEntraPassword, AuthEntraMSI,
		AuthEntraServicePrincipal, AuthEntraServicePrincipalAccessToken,
		AuthEntraIntegrated, AuthEntraInteractive, AuthEntraDeviceCode,
		AuthEntraAzCLI, AuthEntraAzureDeveloperCLI, AuthEntraAzurePipelines,
		AuthEntraOnBehalfOf,
	}
	for _, m := range entra {
		if !m.isEntraMethod() {
			t.Errorf("AuthMethod(%d).isEntraMethod() = false, want true", m)
		}
	}
}

// TestFedauthValueCoversAllEntraMethods guards against a new AuthEntra*
// constant being added to auth.go without a matching fedauthValue entry —
// buildDSN would otherwise silently fail with "unsupported auth method"
// for every caller of the new method.
func TestFedauthValueCoversAllEntraMethods(t *testing.T) {
	entra := []AuthMethod{
		AuthEntraDefault, AuthEntraPassword, AuthEntraMSI,
		AuthEntraServicePrincipal, AuthEntraServicePrincipalAccessToken,
		AuthEntraIntegrated, AuthEntraInteractive, AuthEntraDeviceCode,
		AuthEntraAzCLI, AuthEntraAzureDeveloperCLI, AuthEntraAzurePipelines,
		AuthEntraOnBehalfOf,
	}
	for _, m := range entra {
		if _, ok := fedauthValue[m]; !ok {
			t.Errorf("fedauthValue is missing an entry for AuthMethod(%d)", m)
		}
	}
	if len(fedauthValue) != len(entra) {
		t.Errorf("fedauthValue has %d entries, want %d (one per Entra AuthMethod)", len(fedauthValue), len(entra))
	}
}

// knownBroken marks a table case that pins a finding from gossms's
// docs/entra-auth-plan.md which is not fixed yet. Such a case must still fail,
// and with failsWith in its message, so the suite stays green while the bug
// stands; once a fix lands it fails with "remove its knownBroken marker",
// which is what makes the pin impossible to forget. A case failing for any
// other reason is reported as an ordinary failure — including a fix that
// uncovers the next finding behind it (Password's tenant case moves from G1
// to G5 once G1 is fixed): update the marker, don't delete it. No case carries
// one since plan Phase 2 closed G5; the mechanism stays for the next finding.
type knownBroken struct {
	finding   string // plan finding ID, e.g. "G1"
	failsWith string // substring of today's failure
}

// checkCase applies c's knownBroken marker (if any) to problem, the case's
// failure or nil.
func checkCase(t *testing.T, kb *knownBroken, problem error) {
	t.Helper()
	switch {
	case kb == nil && problem != nil:
		t.Error(problem)
	case kb == nil:
	case problem == nil:
		t.Errorf("%s now passes — remove its knownBroken marker", kb.finding)
	case !strings.Contains(problem.Error(), kb.failsWith):
		t.Errorf("known broken (%s), but failing for a different reason than %q: %v",
			kb.finding, kb.failsWith, problem)
	default:
		t.Logf("known broken (%s): %v", kb.finding, problem)
	}
}

// clearEntraEnv empties the environment variables go-mssqldb's azuread
// driver falls back to, so a case sees only what its ConnectionOptions say.
func clearEntraEnv(t *testing.T) {
	for _, k := range []string{
		"AZURESUBSCRIPTION_CLIENT_ID", "AZURESUBSCRIPTION_TENANT_ID",
		"AZURESUBSCRIPTION_SERVICE_CONNECTION_ID", "SYSTEM_ACCESSTOKEN",
	} {
		t.Setenv(k, "")
	}
}

// driverParams builds opts' connector — which for an Entra method runs the
// DSN through the azuread driver's own parser and validator, without dialling
// — and returns the parameters the driver reads from that DSN.
func driverParams(opts ConnectionOptions) (map[string]string, error) {
	applyDefaults(&opts)
	if _, err := buildConnector(opts); err != nil {
		return nil, err
	}
	dsn, _, err := buildDSN(opts)
	if err != nil {
		return nil, err
	}
	cfg, err := msdsn.Parse(dsn)
	if err != nil {
		return nil, err
	}
	return cfg.Parameters, nil
}

// TestEveryEntraMethodBuildsWithItsDocumentedFields gives each Entra method
// the minimal fields its documentation asks for and requires the driver to
// accept the result. Asserting only on the query string gosmo writes is how
// four methods shipped unable to connect: the DSN looked right and the driver
// refused it (plan findings G1–G4).
func TestEveryEntraMethodBuildsWithItsDocumentedFields(t *testing.T) {
	const server = "myserver.database.windows.net"
	cases := []struct {
		name       string
		opts       ConnectionOptions
		wantParams map[string]string // driver-read values, beyond building at all
		broken     *knownBroken
	}{
		{name: "Default", opts: ConnectionOptions{Auth: AuthEntraDefault}},
		{
			name: "Password",
			opts: ConnectionOptions{Auth: AuthEntraPassword, User: "alice@contoso.com", Password: "pw"},
			wantParams: map[string]string{"user id": "alice@contoso.com", "password": "pw",
				"applicationclientid": defaultPublicClientID},
		},
		{
			name: "Password with ApplicationClientID",
			opts: ConnectionOptions{Auth: AuthEntraPassword, User: "alice@contoso.com", Password: "pw",
				ApplicationClientID: "app-id"},
			wantParams: map[string]string{"applicationclientid": "app-id"},
		},
		{name: "MSI system-assigned", opts: ConnectionOptions{Auth: AuthEntraMSI}},
		{
			name:       "MSI user-assigned",
			opts:       ConnectionOptions{Auth: AuthEntraMSI, ClientID: "mi-client"},
			wantParams: map[string]string{"user id": "mi-client"},
		},
		{
			name: "Service Principal secret",
			opts: ConnectionOptions{Auth: AuthEntraServicePrincipal, User: "app", Password: "secret"},
		},
		{
			name: "Service Principal certificate",
			opts: ConnectionOptions{Auth: AuthEntraServicePrincipal, User: "app",
				ClientCertPath: "/path/to/cert.pem"},
		},
		{
			name: "Service Principal access token",
			opts: ConnectionOptions{Auth: AuthEntraServicePrincipalAccessToken, AccessToken: "tok"},
		},
		{name: "Integrated", opts: ConnectionOptions{Auth: AuthEntraIntegrated}},
		{
			name:       "Interactive",
			opts:       ConnectionOptions{Auth: AuthEntraInteractive},
			wantParams: map[string]string{"applicationclientid": defaultPublicClientID, "user id": ""},
		},
		{
			name:       "Interactive with ApplicationClientID",
			opts:       ConnectionOptions{Auth: AuthEntraInteractive, ApplicationClientID: "app-id"},
			wantParams: map[string]string{"applicationclientid": "app-id"},
		},
		{
			name: "Interactive with login hint",
			opts: ConnectionOptions{Auth: AuthEntraInteractive, ApplicationClientID: "app-id",
				User: "alice@contoso.com"},
			wantParams: map[string]string{"user id": "alice@contoso.com"},
		},
		{
			// azidentity defaults the device-code client itself; gosmo leaves
			// the key unset rather than duplicating that default.
			name:       "Device Code",
			opts:       ConnectionOptions{Auth: AuthEntraDeviceCode},
			wantParams: map[string]string{"applicationclientid": ""},
		},
		{
			name:       "Device Code with ApplicationClientID",
			opts:       ConnectionOptions{Auth: AuthEntraDeviceCode, ApplicationClientID: "app-id"},
			wantParams: map[string]string{"applicationclientid": "app-id"},
		},
		{name: "Azure CLI", opts: ConnectionOptions{Auth: AuthEntraAzCLI}},
		{name: "Azure Developer CLI", opts: ConnectionOptions{Auth: AuthEntraAzureDeveloperCLI}},
		{
			// serviceconnectionid and systemtoken deliberately stay with
			// ExtraParams / the environment (plan Phase 1.4).
			name: "Azure Pipelines",
			opts: ConnectionOptions{Auth: AuthEntraAzurePipelines, User: "app", TenantID: "T",
				ExtraParams: url.Values{"serviceconnectionid": {"sc"}, "systemtoken": {"st"}}},
			wantParams: map[string]string{"user id": "app@T", "applicationclientid": ""},
		},
		{
			name: "Azure Pipelines, ApplicationClientID ignored",
			opts: ConnectionOptions{Auth: AuthEntraAzurePipelines, User: "app", ApplicationClientID: "other",
				ExtraParams: url.Values{"serviceconnectionid": {"sc"}, "systemtoken": {"st"}}},
			wantParams: map[string]string{"user id": "app", "applicationclientid": ""},
		},
		{
			name: "On-Behalf-Of",
			opts: ConnectionOptions{Auth: AuthEntraOnBehalfOf, User: "app", TenantID: "T",
				AccessToken: "user-assertion", Password: "secret"},
			wantParams: map[string]string{"user id": "app@T", "userassertion": "user-assertion",
				"password": "secret"},
		},
		{
			name: "On-Behalf-Of certificate",
			opts: ConnectionOptions{Auth: AuthEntraOnBehalfOf, User: "app", AccessToken: "user-assertion",
				ClientCertPath: "/path/to/cert.pem", ClientCertPassword: "certpw"},
			wantParams: map[string]string{"clientcertpath": "/path/to/cert.pem", "password": "certpw"},
		},
		{
			name: "On-Behalf-Of client assertion",
			opts: ConnectionOptions{Auth: AuthEntraOnBehalfOf, User: "app", AccessToken: "user-assertion",
				ExtraParams: url.Values{"ClientAssertion": {"ca"}}},
			wantParams: map[string]string{"clientassertion": "ca", "password": ""},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearEntraEnv(t)
			c.opts.Server = server
			params, err := driverParams(c.opts)
			if err == nil {
				for k, want := range c.wantParams {
					if got := params[k]; got != want {
						err = fmt.Errorf("%q = %q, want %q", k, got, want)
						break
					}
				}
			}
			checkCase(t, c.broken, err)
		})
	}
}

// TestAzurePipelinesEnvironmentPathKeepsWorking pins the one route by which
// AuthEntraAzurePipelines connects today — everything from the environment —
// so the Phase 1 fix cannot break it by reserving or overwriting a key.
func TestAzurePipelinesEnvironmentPathKeepsWorking(t *testing.T) {
	t.Setenv("AZURESUBSCRIPTION_CLIENT_ID", "app")
	t.Setenv("AZURESUBSCRIPTION_TENANT_ID", "T")
	t.Setenv("AZURESUBSCRIPTION_SERVICE_CONNECTION_ID", "sc")
	t.Setenv("SYSTEM_ACCESSTOKEN", "st")
	if _, err := driverParams(ConnectionOptions{
		Server: "myserver.database.windows.net", Auth: AuthEntraAzurePipelines,
	}); err != nil {
		t.Fatalf("buildConnector: %v", err)
	}
}

// tenantReachingCredential reports the tenant the credential for opts would be
// built with, when that is decided by opts rather than by the server's STS URL
// at login time ("" then): the tenant the azidentity constructor gosmo calls
// receives, positionally or in its options.
func tenantReachingCredential(opts ConnectionOptions) (string, error) {
	if _, err := driverParams(opts); err != nil {
		return "", err
	}
	cfg, err := entraConfigFor(opts)
	if err != nil {
		return "", err
	}
	return specTenant(cfg.credentialSpec("https://login.windows.net", "")), nil
}

// TestTenantIDReachesTheCredential: a TenantID set on ConnectionOptions must
// be the tenant the credential signs in to, for every method whose azidentity
// credential takes one (plan findings G5, G6). Managed Identity and the
// pre-acquired access token have no tenant to choose and are absent.
func TestTenantIDReachesTheCredential(t *testing.T) {
	const tenant = "11111111-2222-3333-4444-555555555555"
	cases := []struct {
		name   string
		opts   ConnectionOptions
		broken *knownBroken
	}{
		{"Service Principal", ConnectionOptions{Auth: AuthEntraServicePrincipal, User: "app", Password: "s"}, nil},
		{
			name: "Service Principal, tenant also in User",
			opts: ConnectionOptions{Auth: AuthEntraServicePrincipal, User: "app@" + tenant, Password: "s"},
		},
		{
			name: "On-Behalf-Of",
			opts: ConnectionOptions{Auth: AuthEntraOnBehalfOf, User: "app", AccessToken: "a", Password: "s"},
		},
		{
			name: "Azure Pipelines",
			opts: ConnectionOptions{Auth: AuthEntraAzurePipelines, User: "app",
				ExtraParams: url.Values{"serviceconnectionid": {"sc"}, "systemtoken": {"st"}}},
		},
		{"Default", ConnectionOptions{Auth: AuthEntraDefault}, nil},
		{"Integrated", ConnectionOptions{Auth: AuthEntraIntegrated}, nil},
		{
			name: "Password",
			opts: ConnectionOptions{Auth: AuthEntraPassword, User: "alice@contoso.com", Password: "pw",
				ApplicationClientID: "app-id"},
		},
		{"Interactive", ConnectionOptions{Auth: AuthEntraInteractive, ApplicationClientID: "app-id"}, nil},
		{"Device Code", ConnectionOptions{Auth: AuthEntraDeviceCode}, nil},
		{"Azure CLI", ConnectionOptions{Auth: AuthEntraAzCLI}, nil},
		{"Azure Developer CLI", ConnectionOptions{Auth: AuthEntraAzureDeveloperCLI}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearEntraEnv(t)
			c.opts.Server = "myserver.database.windows.net"
			c.opts.TenantID = tenant
			got, err := tenantReachingCredential(c.opts)
			if err == nil && got != tenant {
				err = fmt.Errorf("tenant = %q, want %q", got, tenant)
			}
			checkCase(t, c.broken, err)
		})
	}
}

// TestConflictingTenantsAreRefused: a Service Principal User already carrying
// "@tenant" plus a different TenantID is ambiguous; gosmo must refuse it
// rather than send either (plan finding G6).
func TestConflictingTenantsAreRefused(t *testing.T) {
	clearEntraEnv(t)
	_, err := driverParams(ConnectionOptions{
		Server: "myserver.database.windows.net", Auth: AuthEntraServicePrincipal,
		User: "app@tenant-a", TenantID: "tenant-b", Password: "s",
	})
	if err == nil {
		t.Fatal("conflicting tenants accepted, want an error")
	}
	for _, want := range []string{"AuthEntraServicePrincipal", "tenant-a", "tenant-b"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to name %q", err, want)
		}
	}
	// The same tenant twice, in any case, is not a conflict.
	params, err := driverParams(ConnectionOptions{
		Server: "myserver.database.windows.net", Auth: AuthEntraServicePrincipal,
		User: "app@Tenant-A", TenantID: "tenant-a", Password: "s",
	})
	if err != nil {
		t.Fatalf("same tenant in User and TenantID: %v", err)
	}
	if got := params["user id"]; got != "app@Tenant-A" {
		t.Errorf(`"user id" = %q, want app@Tenant-A`, got)
	}
}

// TestEntraRequiredFieldsAreNamedInGosmoTerms: an Entra method missing a field
// it cannot connect without fails before the driver sees it, naming the
// method and the ConnectionOptions field rather than a DSN key (plan G13).
func TestEntraRequiredFieldsAreNamedInGosmoTerms(t *testing.T) {
	cases := []struct {
		name string
		opts ConnectionOptions
		want string
	}{
		{"Password without User", ConnectionOptions{Auth: AuthEntraPassword, Password: "pw"},
			"AuthEntraPassword requires User"},
		{"Password without Password", ConnectionOptions{Auth: AuthEntraPassword, User: "u"},
			"AuthEntraPassword requires Password"},
		{"Service Principal without User", ConnectionOptions{Auth: AuthEntraServicePrincipal, Password: "s"},
			"AuthEntraServicePrincipal requires User"},
		{"Service Principal without secret", ConnectionOptions{Auth: AuthEntraServicePrincipal, User: "app"},
			"AuthEntraServicePrincipal requires Password (a client secret) or ClientCertPath"},
		{"access token without AccessToken", ConnectionOptions{Auth: AuthEntraServicePrincipalAccessToken},
			"AuthEntraServicePrincipalAccessToken requires AccessToken"},
		{"On-Behalf-Of without User", ConnectionOptions{Auth: AuthEntraOnBehalfOf, AccessToken: "a", Password: "s"},
			"AuthEntraOnBehalfOf requires User"},
		{"On-Behalf-Of without assertion", ConnectionOptions{Auth: AuthEntraOnBehalfOf, User: "app", Password: "s"},
			"AuthEntraOnBehalfOf requires AccessToken"},
		{"On-Behalf-Of without app credential", ConnectionOptions{Auth: AuthEntraOnBehalfOf, User: "app", AccessToken: "a"},
			"AuthEntraOnBehalfOf requires Password (a client secret), ClientCertPath"},
		{"Azure Pipelines TenantID without User", ConnectionOptions{Auth: AuthEntraAzurePipelines, TenantID: "T"},
			"AuthEntraAzurePipelines requires User"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearEntraEnv(t)
			c.opts.Server = "myserver.database.windows.net"
			_, err := driverParams(c.opts)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestAuthMethodString(t *testing.T) {
	for m, want := range map[AuthMethod]string{
		AuthSQLServer:       "AuthSQLServer",
		AuthEntraPassword:   "AuthEntraPassword",
		AuthEntraOnBehalfOf: "AuthEntraOnBehalfOf",
		AuthMethod(1000):    "AuthMethod(1000)",
	} {
		if got := m.String(); got != want {
			t.Errorf("AuthMethod(%d).String() = %q, want %q", int(m), got, want)
		}
	}
	// Every constant has a name: a new one added without an authMethodNames
	// entry would print as a number in every error that names it.
	for m := AuthSQLServer; m <= AuthEntraOnBehalfOf; m++ {
		if strings.HasPrefix(m.String(), "AuthMethod(") {
			t.Errorf("AuthMethod(%d) has no authMethodNames entry", int(m))
		}
	}
}
