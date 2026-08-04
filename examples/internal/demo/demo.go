// Package demo holds the connection factory and output helpers the gosmo
// example programs share, so each example file is only about its own topic.
//
// Every example reads the same environment variables:
//
//	MSSQL_SERVER     host[:port] or host\instance   (default localhost:1433)
//	MSSQL_DATABASE   initial database               (default master)
//	MSSQL_AUTH       authentication method          (default sql)
//	MSSQL_USER       login / client ID
//	MSSQL_PASSWORD   password / client secret
//	MSSQL_ENCRYPT    "", "true", "false", or "strict"
//	MSSQL_TRUST_CERT "true" to skip TLS validation (self-signed dev certs)
//
// See Connect for the full list of MSSQL_AUTH values.
package demo

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	gosmo "github.com/radix29/gosmo"
)

// Connect reads the environment variables above and returns a connected
// server. It exits the process on failure — these are examples, not
// production code.
//
// Supported MSSQL_AUTH values:
//
//	""  / "sql"   - SQL Server auth (default)
//	"windows"     - Windows/Kerberos auth (on-premises)
//	"msi"         - Managed Identity (system-assigned)
//	"msi-user"    - Managed Identity (user-assigned, needs AZURE_CLIENT_ID)
//	"sp"          - Service Principal (needs AZURE_TENANT_ID, AZURE_CLIENT_ID,
//	                  AZURE_CLIENT_SECRET)
//	"sp-cert"     - Service Principal + certificate (needs AZURE_TENANT_ID,
//	                  AZURE_CLIENT_ID, AZURE_CLIENT_CERT_PATH)
//	"token"       - a bearer token minted by the caller, via
//	                  ConnectionOptions.AccessTokenProvider
//	"default"     - DefaultAzureCredential chain
//	"azcli"       - Azure CLI credential
//	"azd"         - Azure Developer CLI credential
//	"password"    - Entra user + password
//	"interactive" - Browser interactive sign-in
//	"devicecode"  - Device-code flow
func Connect() *gosmo.Server {
	authStr := strings.ToLower(EnvOr("MSSQL_AUTH", "sql"))

	opts := gosmo.ConnectionOptions{
		Server:                 EnvOr("MSSQL_SERVER", "localhost:1433"),
		Database:               EnvOr("MSSQL_DATABASE", "master"),
		ApplicationName:        "gosmo-examples",
		Encrypt:                os.Getenv("MSSQL_ENCRYPT"),
		TrustServerCertificate: os.Getenv("MSSQL_TRUST_CERT") == "true",
	}

	switch authStr {
	case "", "sql":
		opts.Auth = gosmo.AuthSQLServer
		opts.User = EnvOr("MSSQL_USER", "sa")
		opts.Password = os.Getenv("MSSQL_PASSWORD")
		fmt.Printf("Auth: SQL Server (%s)\n", opts.User)

	case "windows":
		opts.Auth = gosmo.AuthWindows
		fmt.Println("Auth: Windows / Kerberos")

	case "msi":
		opts.Auth = gosmo.AuthEntraMSI
		fmt.Println("Auth: Managed Identity (system-assigned)")

	case "msi-user":
		opts.Auth = gosmo.AuthEntraMSI
		opts.ClientID = MustEnv("AZURE_CLIENT_ID")
		fmt.Printf("Auth: Managed Identity (user-assigned: %s)\n", opts.ClientID)

	case "sp":
		opts.Auth = gosmo.AuthEntraServicePrincipal
		opts.TenantID = MustEnv("AZURE_TENANT_ID")
		opts.User = MustEnv("AZURE_CLIENT_ID")
		opts.Password = MustEnv("AZURE_CLIENT_SECRET")
		fmt.Printf("Auth: Service Principal (client %s)\n", opts.User)

	case "sp-cert":
		opts.Auth = gosmo.AuthEntraServicePrincipal
		opts.TenantID = MustEnv("AZURE_TENANT_ID")
		opts.User = MustEnv("AZURE_CLIENT_ID")
		opts.ClientCertPath = MustEnv("AZURE_CLIENT_CERT_PATH")
		opts.ClientCertPassword = os.Getenv("AZURE_CLIENT_CERT_PASSWORD")
		fmt.Printf("Auth: Service Principal + certificate (client %s)\n", opts.User)

	case "token":
		// AccessTokenProvider is called for every new pooled connection, so a
		// token that expires mid-session is refreshed instead of going stale.
		// A real provider would call into MSAL, an Azure SDK credential, or a
		// sidecar; this one just re-reads the variable.
		opts.AccessTokenProvider = func(ctx context.Context) (string, error) {
			tok := os.Getenv("MSSQL_ACCESS_TOKEN")
			if tok == "" {
				return "", errors.New("MSSQL_ACCESS_TOKEN is not set")
			}
			return tok, nil
		}
		fmt.Println("Auth: caller-supplied bearer token")

	case "default":
		opts.Auth = gosmo.AuthEntraDefault
		fmt.Println("Auth: DefaultAzureCredential chain")

	case "azcli":
		opts.Auth = gosmo.AuthEntraAzCLI
		fmt.Println("Auth: Azure CLI (az login)")

	case "azd":
		opts.Auth = gosmo.AuthEntraAzureDeveloperCLI
		fmt.Println("Auth: Azure Developer CLI (azd auth login)")

	case "password":
		opts.Auth = gosmo.AuthEntraPassword
		opts.User = MustEnv("AZURE_USER")
		opts.Password = MustEnv("AZURE_PASSWORD")
		fmt.Printf("Auth: Entra password (%s)\n", opts.User)

	case "interactive":
		opts.Auth = gosmo.AuthEntraInteractive
		opts.ApplicationClientID = os.Getenv("AZURE_APPLICATION_CLIENT_ID")
		fmt.Println("Auth: Entra interactive (browser)")

	case "devicecode":
		opts.Auth = gosmo.AuthEntraDeviceCode
		opts.ApplicationClientID = os.Getenv("AZURE_APPLICATION_CLIENT_ID")
		fmt.Println("Auth: Entra device code")

	default:
		log.Fatalf("unknown MSSQL_AUTH value: %q", authStr)
	}

	srv, err := gosmo.Connect(opts)
	Must(err)
	return srv
}

// TempDatabase creates a throwaway database, dropping any leftover of the
// same name first, and returns it with the function that drops it again.
// Examples never write to a database they didn't create.
func TempDatabase(srv *gosmo.Server, name string) (*gosmo.Database, func()) {
	_ = srv.DropDatabase(name, true)
	Must(srv.CreateDatabase(name, &gosmo.CreateDatabaseOptions{
		RecoveryModel: gosmo.RecoveryModelSimple,
		CompatLevel:   gosmo.CompatLevel2019,
	}))
	db, err := srv.DatabaseByName(name)
	Must(err)
	fmt.Printf("Created throwaway database [%s]\n", name)

	return db, func() {
		if err := srv.DropDatabase(name, true); err != nil {
			log.Printf("cleanup: dropping [%s]: %v", name, err)
			return
		}
		fmt.Printf("\nDropped [%s]\n", name)
	}
}

// ServerPath joins a directory reported by the server (Info().DefaultDataPath
// and friends) with a file name, using the separator that directory itself
// uses — the server's OS is not necessarily the client's.
func ServerPath(dir, name string) string {
	sep := "/"
	if strings.Contains(dir, `\`) {
		sep = `\`
	}
	return strings.TrimRight(dir, `/\`) + sep + name
}

// Section prints a headed section separator.
func Section(title string) {
	fmt.Printf("\n=== %s ===\n", title)
}

// fatal is what Must panics with, so Exit can tell an example giving up on
// an error from a genuine bug.
type fatal struct{ err error }

// Must aborts the example on error.
//
// It panics rather than calling os.Exit so that deferred cleanup — dropping
// the throwaway database, the disposable login, the demo job — still runs.
// Every example's main pairs it with "defer demo.Exit()" as its first
// statement, which turns the panic back into a plain message and exit 1
// after those defers have run.
func Must(err error) {
	if err != nil {
		panic(fatal{err})
	}
}

// Exit ends an example that called Must with a failing error, after the
// deferred cleanup registered below it has run. Defer it first in main. A
// panic from anything other than Must is re-raised with its stack intact.
func Exit() {
	r := recover()
	if r == nil {
		return
	}
	f, ok := r.(fatal)
	if !ok {
		panic(r)
	}
	fmt.Fprintf(os.Stderr, "ERROR: %v\n", f.err)
	os.Exit(1)
}

// Value unwraps a (T, error) pair, exiting on error — it turns the usual
// three-line call/check/use into one expression, which keeps the examples
// about gosmo rather than about error plumbing.
func Value[T any](v T, err error) T {
	Must(err)
	return v
}

// EnvOr returns the environment variable, or fallback when it is unset.
func EnvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// MustEnv returns the environment variable, exiting when it is unset.
func MustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required environment variable %s is not set", key)
	}
	return v
}
