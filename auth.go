package gosmo

// auth.go defines the AuthMethod type and every authentication strategy
// supported by github.com/microsoft/go-mssqldb.
//
// Quick reference:
//
//   SQL Server / Windows
//   ---------------------
//   AuthSQLServer           - classic user + password (SQL auth)
//   AuthWindows             - Windows/AD identity: native SSPI on Windows,
//                             Kerberos elsewhere (see ConnectionOptions.Kerberos)
//
//   Microsoft Entra ID (Azure AD) -- require the azuread sub-driver
//   -------------------------------------------------------------------
//   AuthEntraDefault        - chained credential (env -> MSI -> AzCLI)
//   AuthEntraPassword       - Entra user + password (no MFA; deprecated upstream)
//   AuthEntraMSI            - system- or user-assigned Managed Identity
//   AuthEntraServicePrincipal   - client-ID + client-secret (or cert)
//   AuthEntraServicePrincipalAccessToken - pre-acquired bearer token
//   AuthEntraIntegrated     - same as AuthEntraDefault (no Windows SSO yet)
//   AuthEntraInteractive    - browser pop-up (human only)
//   AuthEntraDeviceCode     - device-code flow (headless human)
//   AuthEntraAzCLI          - Azure CLI credential (az login)
//   AuthEntraAzureDeveloperCLI - azd credential
//   AuthEntraAzurePipelines - Azure DevOps OIDC
//   AuthEntraOnBehalfOf     - OBO / delegation flow

import "fmt"

// AuthMethod selects the authentication strategy for Connect().
type AuthMethod int

const (
	// AuthSQLServer uses a SQL Server login and password.
	// Set ConnectionOptions.User and ConnectionOptions.Password.
	AuthSQLServer AuthMethod = iota

	// AuthWindows uses the Windows/Active Directory identity (on-premises,
	// domain-joined host). On Windows it uses native SSPI. On every other
	// platform it authenticates via Kerberos: with no credentials it uses the
	// ambient kinit credential cache (single sign-on); set ConnectionOptions.
	// Kerberos for a keytab, an explicit realm, or a non-default krb5.conf,
	// or set User+Password for a username/password Kerberos login.
	AuthWindows

	// AuthEntraDefault uses DefaultAzureCredential (env vars -> MSI -> AzCLI).
	// Ideal for code that must work both locally and in Azure without changes.
	AuthEntraDefault

	// AuthEntraPassword uses an Entra user UPN + password.
	// Set ConnectionOptions.User (UPN) and ConnectionOptions.Password;
	// ConnectionOptions.ApplicationClientID optionally names the public client
	// app to sign in through (default: 04b07795-8ddb-461a-bbee-02f9e1bf7b46,
	// the public client azidentity itself defaults to).
	//
	// It cannot satisfy multifactor authentication, so it fails for any user
	// MFA applies to; azidentity deprecates the underlying credential for that
	// reason. Prefer AuthEntraInteractive or AuthEntraDeviceCode for people.
	AuthEntraPassword

	// AuthEntraMSI uses a system-assigned Managed Identity.
	// Set ConnectionOptions.ClientID to select a user-assigned identity.
	AuthEntraMSI

	// AuthEntraServicePrincipal uses an app registration client-secret or cert.
	// Set ConnectionOptions.User to the application (client) ID,
	// ConnectionOptions.TenantID, and either:
	//   - ConnectionOptions.Password (client secret), or
	//   - ConnectionOptions.ClientCertPath + optionally ClientCertPassword (cert).
	AuthEntraServicePrincipal

	// AuthEntraServicePrincipalAccessToken presents a pre-acquired bearer token.
	// Set ConnectionOptions.AccessToken.
	AuthEntraServicePrincipalAccessToken

	// AuthEntraIntegrated is meant for Windows SSO federated with Entra, but
	// go-mssqldb has no such credential: it runs the same
	// DefaultAzureCredential chain as AuthEntraDefault. No credentials needed.
	AuthEntraIntegrated

	// AuthEntraInteractive opens a browser for interactive sign-in (human only).
	// ConnectionOptions.User is an optional login hint (the user's UPN).
	// ConnectionOptions.ApplicationClientID optionally names the public client
	// app to sign in through (default: 04b07795-8ddb-461a-bbee-02f9e1bf7b46,
	// the public client azidentity itself defaults to).
	AuthEntraInteractive

	// AuthEntraDeviceCode prints a device code for human sign-in on another
	// device. ConnectionOptions.ApplicationClientID optionally names the public
	// client app (azidentity defaults it to the same public client as
	// AuthEntraInteractive). The code goes to ConnectionOptions.DeviceCodePrompt,
	// or to the process's standard output when that is nil.
	AuthEntraDeviceCode

	// AuthEntraAzCLI uses the credential from "az login".
	AuthEntraAzCLI

	// AuthEntraAzureDeveloperCLI uses the credential from "azd auth login".
	AuthEntraAzureDeveloperCLI

	// AuthEntraAzurePipelines uses Azure DevOps OIDC federated credentials
	// (an Azure Resource Manager service connection).
	// Set ConnectionOptions.User to the service connection's client ID and
	// ConnectionOptions.TenantID to its tenant, or leave both empty to use
	// AZURESUBSCRIPTION_CLIENT_ID / AZURESUBSCRIPTION_TENANT_ID. The service
	// connection ID and the pipeline's System.AccessToken come from the
	// "serviceconnectionid" and "systemtoken" ExtraParams, or else from
	// AZURESUBSCRIPTION_SERVICE_CONNECTION_ID and SYSTEM_ACCESSTOKEN. The
	// OIDC request URL comes from SYSTEM_OIDCREQUESTURI, which Azure Pipelines
	// sets on every job.
	AuthEntraAzurePipelines

	// AuthEntraOnBehalfOf uses the OAuth 2.0 on-behalf-of flow: a middle-tier
	// app exchanges the token it received from a user for one to SQL Server.
	// Set ConnectionOptions.User to the app's client ID, optionally
	// ConnectionOptions.TenantID, ConnectionOptions.AccessToken to the inbound
	// user assertion, and the app's credential: ConnectionOptions.Password (a
	// client secret) or ConnectionOptions.ClientCertPath (+ ClientCertPassword),
	// or a "clientassertion" ExtraParams entry.
	AuthEntraOnBehalfOf
)

// defaultPublicClientID is the client ID gosmo signs in through for
// AuthEntraPassword and AuthEntraInteractive when ApplicationClientID is
// empty. go-mssqldb requires one for both, although azidentity itself would
// default it; this is the public client (Microsoft's first-party Azure CLI
// app) azidentity falls back to for its interactive-browser and device-code
// credentials, so all three human flows sign in through the same app.
const defaultPublicClientID = "04b07795-8ddb-461a-bbee-02f9e1bf7b46"

var authMethodNames = map[AuthMethod]string{
	AuthSQLServer:                        "AuthSQLServer",
	AuthWindows:                          "AuthWindows",
	AuthEntraDefault:                     "AuthEntraDefault",
	AuthEntraPassword:                    "AuthEntraPassword",
	AuthEntraMSI:                         "AuthEntraMSI",
	AuthEntraServicePrincipal:            "AuthEntraServicePrincipal",
	AuthEntraServicePrincipalAccessToken: "AuthEntraServicePrincipalAccessToken",
	AuthEntraIntegrated:                  "AuthEntraIntegrated",
	AuthEntraInteractive:                 "AuthEntraInteractive",
	AuthEntraDeviceCode:                  "AuthEntraDeviceCode",
	AuthEntraAzCLI:                       "AuthEntraAzCLI",
	AuthEntraAzureDeveloperCLI:           "AuthEntraAzureDeveloperCLI",
	AuthEntraAzurePipelines:              "AuthEntraAzurePipelines",
	AuthEntraOnBehalfOf:                  "AuthEntraOnBehalfOf",
}

// String returns the constant's Go name ("AuthEntraPassword"), or
// "AuthMethod(<n>)" for a value that names no method.
func (m AuthMethod) String() string {
	if s, ok := authMethodNames[m]; ok {
		return s
	}
	return fmt.Sprintf("AuthMethod(%d)", int(m))
}

// isEntraMethod reports whether the method requires the azuread sub-driver.
func (m AuthMethod) isEntraMethod() bool {
	return m >= AuthEntraDefault
}

// fedauthValue maps an AuthMethod to the fedauth= query-string value
// expected by github.com/microsoft/go-mssqldb/azuread.
var fedauthValue = map[AuthMethod]string{
	AuthEntraDefault:                     "ActiveDirectoryDefault",
	AuthEntraPassword:                    "ActiveDirectoryPassword",
	AuthEntraMSI:                         "ActiveDirectoryManagedIdentity",
	AuthEntraServicePrincipal:            "ActiveDirectoryServicePrincipal",
	AuthEntraServicePrincipalAccessToken: "ActiveDirectoryServicePrincipalAccessToken",
	AuthEntraIntegrated:                  "ActiveDirectoryIntegrated",
	AuthEntraInteractive:                 "ActiveDirectoryInteractive",
	AuthEntraDeviceCode:                  "ActiveDirectoryDeviceCode",
	AuthEntraAzCLI:                       "ActiveDirectoryAzCli",
	AuthEntraAzureDeveloperCLI:           "ActiveDirectoryAzureDeveloperCli",
	AuthEntraAzurePipelines:              "ActiveDirectoryAzurePipelines",
	AuthEntraOnBehalfOf:                  "ActiveDirectoryOnBehalfOf",
}
