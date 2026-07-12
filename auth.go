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
//   AuthEntraPassword       - Entra user + password
//   AuthEntraMSI            - system- or user-assigned Managed Identity
//   AuthEntraServicePrincipal   - client-ID + client-secret (or cert)
//   AuthEntraServicePrincipalAccessToken - pre-acquired bearer token
//   AuthEntraIntegrated     - Windows SSO federated with Entra
//   AuthEntraInteractive    - browser pop-up (human only)
//   AuthEntraDeviceCode     - device-code flow (headless human)
//   AuthEntraAzCLI          - Azure CLI credential (az login)
//   AuthEntraAzureDeveloperCLI - azd credential
//   AuthEntraAzurePipelines - Azure DevOps OIDC
//   AuthEntraOnBehalfOf     - OBO / delegation flow

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
	// Set ConnectionOptions.User (UPN) and ConnectionOptions.Password.
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

	// AuthEntraIntegrated uses Windows SSO federated with Entra (on-premises AD
	// joined to Azure AD). No credentials needed.
	AuthEntraIntegrated

	// AuthEntraInteractive opens a browser for interactive sign-in (human only).
	// Set ConnectionOptions.ApplicationClientID if required by the tenant.
	AuthEntraInteractive

	// AuthEntraDeviceCode prints a device code for human sign-in on another device.
	AuthEntraDeviceCode

	// AuthEntraAzCLI uses the credential from "az login".
	AuthEntraAzCLI

	// AuthEntraAzureDeveloperCLI uses the credential from "azd auth login".
	AuthEntraAzureDeveloperCLI

	// AuthEntraAzurePipelines uses Azure DevOps OIDC federated credentials.
	// Requires SYSTEM_ACCESSTOKEN and SYSTEM_OIDCREQUESTURI env vars.
	AuthEntraAzurePipelines

	// AuthEntraOnBehalfOf uses the OAuth 2.0 on-behalf-of flow.
	// Set ConnectionOptions.AccessToken to the inbound user assertion.
	AuthEntraOnBehalfOf
)

// isEntraMethod reports whether the method requires the azuread sub-driver.
func (m AuthMethod) isEntraMethod() bool {
	return m >= AuthEntraDefault
}

// fedauthValue maps an AuthMethod to the fedauth= query-string value
// expected by github.com/microsoft/go-mssqldb/azuread.
var fedauthValue = map[AuthMethod]string{
	AuthEntraDefault:                      "ActiveDirectoryDefault",
	AuthEntraPassword:                     "ActiveDirectoryPassword",
	AuthEntraMSI:                          "ActiveDirectoryManagedIdentity",
	AuthEntraServicePrincipal:             "ActiveDirectoryServicePrincipal",
	AuthEntraServicePrincipalAccessToken:  "ActiveDirectoryServicePrincipalAccessToken",
	AuthEntraIntegrated:                   "ActiveDirectoryIntegrated",
	AuthEntraInteractive:                  "ActiveDirectoryInteractive",
	AuthEntraDeviceCode:                   "ActiveDirectoryDeviceCode",
	AuthEntraAzCLI:                        "ActiveDirectoryAzCli",
	AuthEntraAzureDeveloperCLI:            "ActiveDirectoryAzureDeveloperCli",
	AuthEntraAzurePipelines:               "ActiveDirectoryAzurePipelines",
	AuthEntraOnBehalfOf:                   "ActiveDirectoryOnBehalfOf",
}
