package gosmo

// entra.go builds the Microsoft Entra ID credential for every Entra
// AuthMethod itself, rather than leaving it to go-mssqldb's azuread
// connector.
//
// The azuread connector constructs a fresh azidentity credential inside its
// token callback, which the driver calls once per physical connection: a
// browser sign-in per pooled connection for AuthEntraInteractive, a new code
// per connection for AuthEntraDeviceCode, an "az" subprocess per connection
// for AuthEntraAzCLI. Its credential construction is unexported, so the only
// fix short of a fork is to do that part here: the DSN is still built by
// buildDSN and still validated by the azuread parser, and the parameters are
// read back from it under the driver's own key names — so the Entra keys a
// caller passes through ExtraParams ("resource id",
// "additionallyallowedtenants", "serviceconnectionid", "systemtoken",
// "clientassertion") keep meaning what they meant — but the credential comes
// from an EntraCache and is built once per identity, not once per connection.
//
// entraConfig.credentialSpec mirrors go-mssqldb v1.11.0's
// azuread/configuration.go provideActiveDirectoryToken, workflow by workflow,
// with four deliberate differences: TenantID reaches every credential that
// takes a tenant (the driver drops it for all but the client-credential
// workflows); with no TenantID, the human flows sign in to the tenant the
// server names, as SSMS does (the driver leaves them on azidentity's
// "organizations", where a personal Microsoft account that is a member of the
// server's tenant is refused — "does not exist in tenant 'Microsoft
// Services'"); AuthEntraInteractive's login hint is passed (the driver parses
// and discards it); and AuthEntraDeviceCode's prompt can be redirected
// (DeviceCodePrompt). A driver bump that adds or renames a workflow fails
// TestEveryDriverWorkflowIsMappedOrRefused.

import (
	"cmp"
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	mssql "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/azuread"
	"github.com/microsoft/go-mssqldb/msdsn"
)

// DeviceCodeMessage is what AuthEntraDeviceCode needs the user to see: a code
// to enter at a verification URL, on any device.
type DeviceCodeMessage struct {
	// UserCode is the code the user types in.
	UserCode string
	// VerificationURL is the page the user types it into.
	VerificationURL string
	// Message is Microsoft's own one-line instruction carrying both, suitable
	// for display as it is.
	Message string
}

// EntraCache holds Microsoft Entra credentials and the access tokens they
// have issued, so that every connection authenticating as the same identity
// signs in once rather than once per physical connection. Share one across
// every ConnectContext call that should share a sign-in — typically one per
// process. It holds tokens and, for the methods that take one, secrets, in
// memory only; nothing is written to disk, so a new process signs in again.
//
// An identity is the auth method with the fields that decide who signs in —
// tenant, client and application IDs, user or login hint, certificate path,
// sign-in authority, and a digest of the secrets — never the server, so one
// sign-in covers every server in the tenant, as it does in SSMS. A cached
// token is reused until five minutes before it expires (or the refresh time
// the issuer suggests), then renewed through the same credential, which for
// the interactive flows is normally silent.
//
// The zero value is not usable; call NewEntraCache. An EntraCache is safe for
// concurrent use.
type EntraCache struct {
	mu      sync.Mutex
	creds   map[entraKey]*entraCredential
	servers map[string]serverSignIn // by lower-cased ConnectionOptions.Server

	// newCredential builds the azidentity credential a spec describes. Tests
	// replace it; nil means buildEntraCredential.
	newCredential func(entraCredSpec) (azcore.TokenCredential, error)
	// probe asks a server what its login will ask for. Tests replace it; nil
	// means probeServerSignIn.
	probe func(context.Context, ConnectionOptions, *entraConfig) (serverSignIn, error)
}

// serverSignIn is what a server announces at an Entra login: the service
// principal name the token is for, and the STS URL — sign-in authority and
// tenant — that issues it.
type serverSignIn struct{ spn, stsURL string }

// NewEntraCache returns an empty EntraCache.
func NewEntraCache() *EntraCache {
	return &EntraCache{creds: make(map[entraKey]*entraCredential), servers: make(map[string]serverSignIn)}
}

// Clear forgets every credential and token c holds. The next connection
// through c signs in again — the way to switch accounts. Pools already open
// keep the connections they have; only new physical connections are
// affected.
func (c *EntraCache) Clear() {
	c.mu.Lock()
	clear(c.creds)
	clear(c.servers)
	c.mu.Unlock()
}

// Warm signs in for opts ahead of any connection, filling c with the token
// the connection will need, so that a human sign-in (AuthEntraInteractive,
// AuthEntraDeviceCode) runs under ctx — which may be long and cancellable —
// rather than under a connect timeout, inside the TDS login handshake. Pass
// the same options, with EntraCache set to c, to ConnectContext afterwards.
// opts.EntraCache itself is ignored here.
//
// The token's scope, sign-in authority and — when TenantID is empty — tenant
// are the server's to announce, and it announces them only part-way through a
// login. So Warm first opens a login to Server and abandons it as soon as the
// server has said them, before any token is sent; c remembers the answer per
// Server, so later Warms for the same server skip it. That probe runs under
// ctx and at most opts.ConnectTimeout, and its failure — an unreachable
// server, one that does not accept Entra logins — is Warm's error.
//
// Warm does nothing and returns nil when there is no sign-in to do: a
// non-Entra method, AccessTokenProvider, or
// AuthEntraServicePrincipalAccessToken.
func (c *EntraCache) Warm(ctx context.Context, opts ConnectionOptions) error {
	if !opts.Auth.isEntraMethod() || opts.AccessTokenProvider != nil ||
		opts.Auth == AuthEntraServicePrincipalAccessToken {
		return nil
	}
	applyDefaults(&opts)
	dsn, _, err := buildDSN(opts)
	if err != nil {
		return err
	}
	cfg, err := parseEntraConfig(dsn, opts.DeviceCodePrompt)
	if err != nil {
		return fmt.Errorf("gosmo: entra sign-in: %w", err)
	}
	srv, err := c.askServer(ctx, opts, cfg)
	if err != nil {
		return fmt.Errorf("gosmo: entra sign-in: asking %s for its sign-in authority: %w", opts.Server, err)
	}
	if _, err := c.token(ctx, cfg, srv.spn, srv.stsURL); err != nil {
		return fmt.Errorf("gosmo: entra sign-in: %w", err)
	}
	return nil
}

// askServer returns what opts' server announces at an Entra login, probing
// it the first time c is asked.
func (c *EntraCache) askServer(ctx context.Context, opts ConnectionOptions, cfg *entraConfig) (serverSignIn, error) {
	key := strings.ToLower(opts.Server)
	c.mu.Lock()
	srv, ok := c.servers[key]
	c.mu.Unlock()
	if ok {
		return srv, nil
	}
	probe := c.probe
	if probe == nil {
		probe = probeServerSignIn
	}
	srv, err := probe(ctx, opts, cfg)
	if err != nil {
		return serverSignIn{}, err
	}
	c.mu.Lock()
	c.servers[key] = srv
	c.mu.Unlock()
	return srv, nil
}

// errProbed ends a probe login once the server has announced its SPN and STS
// URL: the token callback returns it instead of a token, and the driver
// abandons the login.
var errProbed = errors.New("gosmo: entra probe complete")

// probeServerSignIn opens an Entra login to opts' server with a token
// callback that records what the server asks for and then fails, so no token
// is ever requested or sent. The server sees a client that went away
// mid-login.
func probeServerSignIn(ctx context.Context, opts ConnectionOptions, cfg *entraConfig) (serverSignIn, error) {
	var srv serverSignIn
	connector, err := mssql.NewActiveDirectoryTokenConnector(cfg.mssql, cfg.adalWorkflow,
		func(_ context.Context, serverSPN, stsURL string) (string, error) {
			srv = serverSignIn{spn: serverSPN, stsURL: stsURL}
			return "", errProbed
		})
	if err != nil {
		return serverSignIn{}, err
	}
	connector.Dialer = dialerFor(opts)
	ctx, cancel := context.WithTimeout(ctx, opts.ConnectTimeout)
	defer cancel()
	conn, err := connector.Connect(ctx)
	if err == nil { // on failure conn is a typed nil, whose Close panics
		conn.Close()
	}
	switch {
	case srv.stsURL != "":
		return srv, nil
	case err != nil:
		return serverSignIn{}, err
	}
	return serverSignIn{}, errors.New("the server completed a login without asking for an Entra token")
}

// newEntraConnector builds the connector for an Entra DSN: the azuread parser
// validates it (so a malformed DSN fails exactly as it always has), then the
// token callback draws on cache instead of building a credential per
// connection.
func newEntraConnector(dsn string, cache *EntraCache, prompt func(context.Context, DeviceCodeMessage) error) (*mssql.Connector, error) {
	if _, err := azuread.NewConnector(dsn); err != nil {
		return nil, err
	}
	cfg, err := parseEntraConfig(dsn, prompt)
	if err != nil {
		return nil, err
	}
	if cfg.workflow == azuread.ActiveDirectoryServicePrincipalAccessToken {
		token := cfg.password
		return mssql.NewSecurityTokenConnector(cfg.mssql, func(context.Context) (string, error) {
			return token, nil
		})
	}
	return mssql.NewActiveDirectoryTokenConnector(cfg.mssql, cfg.adalWorkflow,
		func(ctx context.Context, serverSPN, stsURL string) (string, error) {
			return cache.token(ctx, cfg, serverSPN, stsURL)
		})
}

// entraConfig is an Entra DSN's parameters, read under the azuread driver's
// key names and with its environment fallbacks.
type entraConfig struct {
	mssql        msdsn.Config
	workflow     string // the fedauth value, as the driver spells it
	adalWorkflow byte

	clientID, tenantID     string
	clientSecret, certPath string // clientSecret doubles as the certificate's password
	user, password         string
	appClientID            string
	resourceID             string
	serviceConnectionID    string
	systemToken            string
	userAssertion          string
	clientAssertion        string

	additionallyAllowedTenants []string
	disableInstanceDiscovery   bool
	sendCertificateChain       bool

	prompt func(context.Context, DeviceCodeMessage) error
}

// parseEntraConfig reads dsn — one buildDSN wrote for an Entra method — as
// azuread/configuration.go validateParameters does. It does not repeat the
// driver's validation; newEntraConnector runs the driver's own first.
func parseEntraConfig(dsn string, prompt func(context.Context, DeviceCodeMessage) error) (*entraConfig, error) {
	m, err := msdsn.Parse(dsn)
	if err != nil {
		return nil, err
	}
	p := m.Parameters
	cfg := &entraConfig{
		mssql:        m,
		workflow:     p["fedauth"],
		adalWorkflow: mssql.FedAuthADALWorkflowPassword,
		appClientID:  p["applicationclientid"],
		// Only gosmo writes "tenantid" (it is reserved): the caller's TenantID.
		tenantID: p["tenantid"],
		prompt:   prompt,
	}
	userTenant := func() {
		var t string
		cfg.clientID, t = splitClientAndTenant(p["user id"])
		cfg.tenantID = cmp.Or(t, cfg.tenantID)
	}
	switch cfg.workflow {
	case azuread.ActiveDirectoryDefault, azuread.ActiveDirectoryAzCli,
		azuread.ActiveDirectoryAzureDeveloperCli:
	case azuread.ActiveDirectoryIntegrated:
		cfg.adalWorkflow = mssql.FedAuthADALWorkflowIntegrated
	case azuread.ActiveDirectoryPassword:
		cfg.user, cfg.password = p["user id"], p["password"]
	case azuread.ActiveDirectoryManagedIdentity:
		cfg.adalWorkflow = mssql.FedAuthADALWorkflowMSI
		cfg.resourceID = p["resource id"]
		cfg.clientID, _ = splitClientAndTenant(p["user id"])
	case azuread.ActiveDirectoryServicePrincipal:
		userTenant()
		cfg.clientSecret, cfg.certPath = p["password"], p["clientcertpath"]
	case azuread.ActiveDirectoryInteractive:
		cfg.user = p["user id"] // login hint
	case azuread.ActiveDirectoryDeviceCode:
	case azuread.ActiveDirectoryServicePrincipalAccessToken:
		cfg.adalWorkflow = mssql.FedAuthADALWorkflowNone
		cfg.password = p["password"]
	case azuread.ActiveDirectoryAzurePipelines:
		userTenant()
		cfg.clientID = cmp.Or(cfg.clientID, os.Getenv("AZURESUBSCRIPTION_CLIENT_ID"))
		cfg.tenantID = cmp.Or(cfg.tenantID, os.Getenv("AZURESUBSCRIPTION_TENANT_ID"))
		cfg.serviceConnectionID = cmp.Or(p["serviceconnectionid"], os.Getenv("AZURESUBSCRIPTION_SERVICE_CONNECTION_ID"))
		cfg.systemToken = cmp.Or(p["systemtoken"], os.Getenv("SYSTEM_ACCESSTOKEN"))
	case azuread.ActiveDirectoryOnBehalfOf:
		userTenant()
		cfg.userAssertion = p["userassertion"]
		cfg.clientSecret, cfg.certPath = p["password"], p["clientcertpath"]
		cfg.clientAssertion = p["clientassertion"]
	default:
		// Every fedauth value gosmo writes is handled above, and "fedauth" is
		// reserved, so this is reachable only through a new fedauthValue entry
		// without a case here.
		return nil, fmt.Errorf("gosmo: no Entra credential mapping for fedauth %q", cfg.workflow)
	}
	if v := p["additionallyallowedtenants"]; v != "" {
		for t := range strings.FieldsFuncSeq(v, func(r rune) bool { return r == ',' || r == ';' }) {
			if t = strings.TrimSpace(t); t != "" {
				cfg.additionallyAllowedTenants = append(cfg.additionallyAllowedTenants, t)
			}
		}
	}
	cfg.disableInstanceDiscovery = isTrueParam(p["disableinstancediscovery"])
	cfg.sendCertificateChain = isTrueParam(p["sendcertificatechain"])
	return cfg, nil
}

func isTrueParam(v string) bool { return strings.EqualFold(v, "true") || v == "1" }

// splitClientAndTenant splits "client[@tenant]" as the driver does: at the
// first '@', and only when it is neither the first nor the last character.
func splitClientAndTenant(user string) (client, tenant string) {
	at := strings.IndexByte(user, '@')
	if at < 1 || at >= len(user)-1 {
		return user, ""
	}
	return user[:at], user[at+1:]
}

// usesServerTenant reports whether the workflow's credential falls back to
// the tenant named by the server's STS URL when the caller gave none, as the
// driver does for the client-credential and password workflows and gosmo
// also does for the human flows (see this file's header).
func (cfg *entraConfig) usesServerTenant() bool {
	switch cfg.workflow {
	case azuread.ActiveDirectoryServicePrincipal, azuread.ActiveDirectoryPassword,
		azuread.ActiveDirectoryAzurePipelines, azuread.ActiveDirectoryOnBehalfOf,
		azuread.ActiveDirectoryInteractive, azuread.ActiveDirectoryDeviceCode:
		return true
	}
	return false
}

// usesAuthority reports whether the workflow's credential signs in at the
// authority host the server names. The driver does it for Interactive; gosmo
// does it for Device Code too, the other public-client human flow, so both
// reach a sovereign cloud's sign-in page.
func (cfg *entraConfig) usesAuthority() bool {
	return cfg.workflow == azuread.ActiveDirectoryInteractive ||
		cfg.workflow == azuread.ActiveDirectoryDeviceCode
}

// entraCredKind names the azidentity constructor an entraCredSpec calls.
type entraCredKind int

const (
	credDefault entraCredKind = iota
	credClientSecret
	credClientCertificate
	credUsernamePassword
	credManagedIdentity
	credInteractiveBrowser
	credDeviceCode
	credAzureCLI
	credAzureDeveloperCLI
	credAzurePipelines
	credOnBehalfOfSecret
	credOnBehalfOfCertificate
	credOnBehalfOfAssertion
)

// entraCredSpec is one azidentity constructor call, fully decided: which
// constructor, its positional arguments, and its options struct (a pointer to
// the matching azidentity *…Options type). Separating the decision from the
// call is what lets tests check the mapping without a network.
type entraCredSpec struct {
	kind                             entraCredKind
	tenant, clientID                 string
	secret                           string // client secret, or the certificate's password
	certPath                         string
	user, password                   string
	userAssertion, clientAssertion   string
	serviceConnectionID, systemToken string
	options                          any
}

// credentialSpec decides the credential for cfg against a server whose STS
// URL split into authority and serverTenant. Mirrors the driver's
// provideActiveDirectoryToken; see this file's header for the differences.
func (cfg *entraConfig) credentialSpec(authority, serverTenant string) entraCredSpec {
	tenant := cfg.tenantID
	if cfg.usesServerTenant() {
		tenant = cmp.Or(tenant, serverTenant)
	}
	var clientOpts azcore.ClientOptions
	if cfg.usesAuthority() && authority != "" {
		clientOpts.Cloud = cloud.Configuration{ActiveDirectoryAuthorityHost: "https://" + canonicalAuthority(authority) + "/"}
	}
	s := entraCredSpec{tenant: tenant, clientID: cfg.clientID}
	allowed, noDiscovery := cfg.additionallyAllowedTenants, cfg.disableInstanceDiscovery

	switch cfg.workflow {
	case azuread.ActiveDirectoryServicePrincipal:
		if cfg.certPath != "" {
			s.kind, s.certPath, s.secret = credClientCertificate, cfg.certPath, cfg.clientSecret
			s.options = &azidentity.ClientCertificateCredentialOptions{AdditionallyAllowedTenants: allowed,
				DisableInstanceDiscovery: noDiscovery, SendCertificateChain: cfg.sendCertificateChain}
		} else {
			s.kind, s.secret = credClientSecret, cfg.clientSecret
			s.options = &azidentity.ClientSecretCredentialOptions{AdditionallyAllowedTenants: allowed,
				DisableInstanceDiscovery: noDiscovery}
		}
	case azuread.ActiveDirectoryPassword:
		s.kind, s.clientID, s.user, s.password = credUsernamePassword, cfg.appClientID, cfg.user, cfg.password
		s.options = &azidentity.UsernamePasswordCredentialOptions{AdditionallyAllowedTenants: allowed,
			DisableInstanceDiscovery: noDiscovery}
	case azuread.ActiveDirectoryManagedIdentity:
		s.kind, s.tenant = credManagedIdentity, ""
		o := &azidentity.ManagedIdentityCredentialOptions{}
		switch {
		case cfg.resourceID != "":
			o.ID = azidentity.ResourceID(cfg.resourceID)
		case cfg.clientID != "":
			o.ID = azidentity.ClientID(cfg.clientID)
		}
		s.options = o
	case azuread.ActiveDirectoryInteractive:
		s.kind, s.clientID, s.user = credInteractiveBrowser, cfg.appClientID, cfg.user
		s.options = &azidentity.InteractiveBrowserCredentialOptions{ClientOptions: clientOpts,
			ClientID: cfg.appClientID, TenantID: tenant, LoginHint: cfg.user,
			AdditionallyAllowedTenants: allowed, DisableInstanceDiscovery: noDiscovery}
	case azuread.ActiveDirectoryDeviceCode:
		s.kind, s.clientID = credDeviceCode, cfg.appClientID
		s.options = &azidentity.DeviceCodeCredentialOptions{ClientOptions: clientOpts,
			ClientID: cfg.appClientID, TenantID: tenant,
			AdditionallyAllowedTenants: allowed, DisableInstanceDiscovery: noDiscovery}
	case azuread.ActiveDirectoryAzCli:
		s.kind = credAzureCLI
		s.options = &azidentity.AzureCLICredentialOptions{TenantID: tenant, AdditionallyAllowedTenants: allowed}
	case azuread.ActiveDirectoryAzureDeveloperCli:
		s.kind = credAzureDeveloperCLI
		s.options = &azidentity.AzureDeveloperCLICredentialOptions{TenantID: tenant, AdditionallyAllowedTenants: allowed}
	case azuread.ActiveDirectoryAzurePipelines:
		s.kind, s.serviceConnectionID, s.systemToken = credAzurePipelines, cfg.serviceConnectionID, cfg.systemToken
		s.options = &azidentity.AzurePipelinesCredentialOptions{AdditionallyAllowedTenants: allowed,
			DisableInstanceDiscovery: noDiscovery}
	case azuread.ActiveDirectoryOnBehalfOf:
		s.userAssertion = cfg.userAssertion
		o := &azidentity.OnBehalfOfCredentialOptions{AdditionallyAllowedTenants: allowed,
			DisableInstanceDiscovery: noDiscovery}
		switch {
		case cfg.certPath != "":
			s.kind, s.certPath, s.secret = credOnBehalfOfCertificate, cfg.certPath, cfg.clientSecret
			o.SendCertificateChain = cfg.sendCertificateChain
		case cfg.clientAssertion != "":
			s.kind, s.clientAssertion = credOnBehalfOfAssertion, cfg.clientAssertion
		default:
			s.kind, s.secret = credOnBehalfOfSecret, cfg.clientSecret
		}
		s.options = o
	default: // ActiveDirectoryDefault, and ActiveDirectoryIntegrated, which the driver also runs as Default
		s.kind, s.clientID = credDefault, ""
		s.options = &azidentity.DefaultAzureCredentialOptions{TenantID: tenant,
			AdditionallyAllowedTenants: allowed, DisableInstanceDiscovery: noDiscovery}
	}
	return s
}

// buildEntraCredential calls the azidentity constructor s describes.
func buildEntraCredential(s entraCredSpec) (azcore.TokenCredential, error) {
	switch s.kind {
	case credClientSecret:
		return azidentity.NewClientSecretCredential(s.tenant, s.clientID, s.secret,
			s.options.(*azidentity.ClientSecretCredentialOptions))
	case credClientCertificate:
		certs, key, err := readEntraCertificate(s.certPath, s.secret)
		if err != nil {
			return nil, err
		}
		return azidentity.NewClientCertificateCredential(s.tenant, s.clientID, certs, key,
			s.options.(*azidentity.ClientCertificateCredentialOptions))
	case credUsernamePassword:
		// Deprecated upstream (no MFA); AuthEntraPassword documents it and is kept.
		return azidentity.NewUsernamePasswordCredential(s.tenant, s.clientID, s.user, s.password,
			s.options.(*azidentity.UsernamePasswordCredentialOptions))
	case credManagedIdentity:
		return azidentity.NewManagedIdentityCredential(s.options.(*azidentity.ManagedIdentityCredentialOptions))
	case credInteractiveBrowser:
		return azidentity.NewInteractiveBrowserCredential(s.options.(*azidentity.InteractiveBrowserCredentialOptions))
	case credDeviceCode:
		return azidentity.NewDeviceCodeCredential(s.options.(*azidentity.DeviceCodeCredentialOptions))
	case credAzureCLI:
		return azidentity.NewAzureCLICredential(s.options.(*azidentity.AzureCLICredentialOptions))
	case credAzureDeveloperCLI:
		return azidentity.NewAzureDeveloperCLICredential(s.options.(*azidentity.AzureDeveloperCLICredentialOptions))
	case credAzurePipelines:
		return azidentity.NewAzurePipelinesCredential(s.tenant, s.clientID, s.serviceConnectionID, s.systemToken,
			s.options.(*azidentity.AzurePipelinesCredentialOptions))
	case credOnBehalfOfSecret:
		return azidentity.NewOnBehalfOfCredentialWithSecret(s.tenant, s.clientID, s.userAssertion, s.secret,
			s.options.(*azidentity.OnBehalfOfCredentialOptions))
	case credOnBehalfOfCertificate:
		certs, key, err := readEntraCertificate(s.certPath, s.secret)
		if err != nil {
			return nil, err
		}
		return azidentity.NewOnBehalfOfCredentialWithCertificate(s.tenant, s.clientID, s.userAssertion, certs, key,
			s.options.(*azidentity.OnBehalfOfCredentialOptions))
	case credOnBehalfOfAssertion:
		assertion := s.clientAssertion
		return azidentity.NewOnBehalfOfCredentialWithClientAssertions(s.tenant, s.clientID, s.userAssertion,
			func(context.Context) (string, error) { return assertion, nil },
			s.options.(*azidentity.OnBehalfOfCredentialOptions))
	default:
		return azidentity.NewDefaultAzureCredential(s.options.(*azidentity.DefaultAzureCredentialOptions))
	}
}

func readEntraCertificate(path, password string) ([]*x509.Certificate, crypto.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	return azidentity.ParseCertificates(data, []byte(password))
}

// entraKey identifies one credential in an EntraCache: the spec's
// identity-deciding fields, the sign-in authority when the workflow uses one,
// and a digest of every secret, so a corrected secret is a new credential
// rather than the cached, failing one.
type entraKey struct {
	workflow, tenant, clientID, user, certPath string
	resourceID, serviceConnectionID, authority string
	allowedTenants                             string
	disableInstanceDiscovery, sendCertChain    bool
	secrets                                    [sha256.Size]byte
}

func (cfg *entraConfig) key(s entraCredSpec, authority string) entraKey {
	k := entraKey{
		workflow: cfg.workflow, tenant: s.tenant, clientID: s.clientID, user: s.user, certPath: s.certPath,
		resourceID: cfg.resourceID, serviceConnectionID: s.serviceConnectionID,
		allowedTenants:           strings.Join(cfg.additionallyAllowedTenants, ","),
		disableInstanceDiscovery: cfg.disableInstanceDiscovery, sendCertChain: cfg.sendCertificateChain,
	}
	if cfg.usesAuthority() {
		k.authority = canonicalAuthority(authority)
	}
	h := sha256.New()
	for _, v := range []string{s.secret, s.password, s.userAssertion, s.clientAssertion, s.systemToken} {
		fmt.Fprintf(h, "%d:%s;", len(v), v)
	}
	h.Sum(k.secrets[:0])
	return k
}

// canonicalAuthority reduces an authority URL to the host that identifies
// its cloud, folding the public cloud's aliases together: Azure SQL announces
// its STS as login.windows.net, which azidentity itself calls
// login.microsoftonline.com.
func canonicalAuthority(authority string) string {
	host := authority
	if u, err := url.Parse(authority); err == nil && u.Host != "" {
		host = u.Host
	}
	host = strings.ToLower(host)
	switch host {
	case "login.windows.net", "login.microsoft.com", "sts.windows.net":
		return "login.microsoftonline.com"
	}
	return host
}

// splitSTSURL splits the STS URL a server announces
// ("https://login.windows.net/<tenant>") into its authority and tenant.
func splitSTSURL(stsURL string) (authority, tenant string) {
	i := strings.LastIndexByte(stsURL, '/')
	if i < 0 {
		return stsURL, ""
	}
	return stsURL[:i], stsURL[i+1:]
}

// token returns an access token for cfg at a server that announced serverSPN
// and stsURL, from c's cached credential for that identity.
func (c *EntraCache) token(ctx context.Context, cfg *entraConfig, serverSPN, stsURL string) (string, error) {
	authority, serverTenant := splitSTSURL(stsURL)
	cred, err := c.credential(cfg, authority, serverTenant)
	if err != nil {
		return "", err
	}
	scope := serverSPN
	if !strings.HasSuffix(scope, "/.default") {
		scope += "/.default"
	}
	return cred.token(ctx, scope)
}

// credential returns c's credential for cfg's identity, creating it — without
// signing in; that happens on its first token — if c has none yet.
func (c *EntraCache) credential(cfg *entraConfig, authority, serverTenant string) (*entraCredential, error) {
	spec := cfg.credentialSpec(authority, serverTenant)
	key := cfg.key(spec, authority)

	c.mu.Lock()
	defer c.mu.Unlock()
	if ec, ok := c.creds[key]; ok {
		ec.setPrompt(cfg.prompt)
		return ec, nil
	}
	ec := &entraCredential{sem: make(chan struct{}, 1), tokens: map[string]azcore.AccessToken{}}
	ec.setPrompt(cfg.prompt)
	if o, ok := spec.options.(*azidentity.DeviceCodeCredentialOptions); ok {
		o.UserPrompt = ec.showDeviceCode
	}
	newCred := c.newCredential
	if newCred == nil {
		newCred = buildEntraCredential
	}
	cred, err := newCred(spec)
	if err != nil {
		return nil, err
	}
	ec.cred = cred
	c.creds[key] = ec
	return ec, nil
}

// entraCredential is one cached credential and the tokens it has issued, per
// scope. Token requests are serialised: when a pool opens many connections at
// once, the first signs in and the rest wait for — and then reuse — its token,
// rather than each starting a sign-in of its own.
type entraCredential struct {
	cred   azcore.TokenCredential
	sem    chan struct{} // held while a token is being fetched
	tokens map[string]azcore.AccessToken
	prompt atomic.Pointer[func(context.Context, DeviceCodeMessage) error]
}

// entraTokenMargin is how long before expiry a cached token stops being
// handed out, so a connection is never opened with a token that lapses
// during the login handshake.
const entraTokenMargin = 5 * time.Minute

// now is time.Now, replaceable by tests.
var now = time.Now

func (ec *entraCredential) token(ctx context.Context, scope string) (string, error) {
	select {
	case ec.sem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-ec.sem }()

	if tk, ok := ec.tokens[scope]; ok && tokenFresh(tk) {
		return tk.Token, nil
	}
	tk, err := ec.cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{scope}})
	if err != nil {
		return "", err
	}
	ec.tokens[scope] = tk
	return tk.Token, nil
}

func tokenFresh(tk azcore.AccessToken) bool {
	t := now()
	if !tk.RefreshOn.IsZero() && !t.Before(tk.RefreshOn) {
		return false
	}
	return t.Before(tk.ExpiresOn.Add(-entraTokenMargin))
}

// setPrompt records the device-code prompt the latest connection through ec
// asked for; nil leaves the one already recorded.
func (ec *entraCredential) setPrompt(p func(context.Context, DeviceCodeMessage) error) {
	if p != nil {
		ec.prompt.Store(&p)
	}
}

// showDeviceCode is AuthEntraDeviceCode's azidentity UserPrompt: the caller's
// DeviceCodePrompt when one was given, else azidentity's own default, which
// prints the message to standard output.
func (ec *entraCredential) showDeviceCode(ctx context.Context, m azidentity.DeviceCodeMessage) error {
	if p := ec.prompt.Load(); p != nil {
		return (*p)(ctx, DeviceCodeMessage{UserCode: m.UserCode, VerificationURL: m.VerificationURL, Message: m.Message})
	}
	fmt.Println(m.Message)
	return nil
}
