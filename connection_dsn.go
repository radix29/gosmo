package gosmo

import (
	"cmp"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

// unsupportedProtocols names each protocol prefix gosmo cannot dial, for
// dsnHost's refusal. go-mssqldb speaks TDS over TCP only.
var unsupportedProtocols = map[string]string{
	"np":    "named pipes (np:) are not supported; connect over TCP — host, host\\instance or host,port",
	"lpc":   "shared memory (lpc:) is not supported; connect over TCP — host, host\\instance or host,port",
	"admin": "the admin: prefix is not supported; to reach the dedicated admin connection, name its TCP port (host,port)",
}

// buildDSN constructs the DSN URL and a driver-name selector from
// ConnectionOptions. buildConnector uses the selector to pick the matching
// connector constructor: "azuresql" (the go-mssqldb/azuread connector) for
// Entra methods, "sqlserver" (the base connector) for everything else.
func buildDSN(opts ConnectionOptions) (dsn, driverName string, err error) {
	if opts.Server == "" {
		return "", "", invalidf("gosmo: ConnectionOptions.Server is required")
	}

	// A named instance can't be embedded directly in url.URL.Host — a
	// literal backslash gets percent-escaped and go-mssqldb's own URL-DSN
	// convention (see its splitConnectionStringURL) expects the instance
	// name as a URL path segment instead: sqlserver://host:port/instance.
	dialHost, instance, err := dsnHost(opts)
	if err != nil {
		return "", "", err
	}

	q := commonDSNValues(opts)

	if opts.Auth.isEntraMethod() {
		// -- Entra ID (Azure AD) path: uses the "azuresql" driver --
		driverName = "azuresql"

		fv, ok := fedauthValue[opts.Auth]
		if !ok {
			return "", "", invalidf("gosmo: unsupported auth method %d", opts.Auth)
		}
		q.Set("fedauth", fv)

		if err := checkEntraFields(opts); err != nil {
			return "", "", err
		}

		// Per-method extra parameters. Every key here is one go-mssqldb's
		// azuread driver reads for that workflow (azuread/configuration.go,
		// validateParameters); auth_test.go runs each through it.
		switch opts.Auth {
		case AuthEntraMSI:
			if opts.ClientID != "" {
				// user-assigned managed identity
				q.Set("user id", opts.ClientID)
			}
			if opts.TokenFilePath != "" {
				q.Set("tokenfilepath", opts.TokenFilePath)
			}

		case AuthEntraServicePrincipal:
			userID, err := entraClientAtTenant(opts)
			if err != nil {
				return "", "", err
			}
			q.Set("user id", userID)
			setEntraAppCredential(q, opts)

		case AuthEntraServicePrincipalAccessToken:
			q.Set("password", opts.AccessToken)

		case AuthEntraOnBehalfOf:
			userID, err := entraClientAtTenant(opts)
			if err != nil {
				return "", "", err
			}
			q.Set("user id", userID)
			q.Set("userassertion", opts.AccessToken)
			setEntraAppCredential(q, opts)

		case AuthEntraPassword:
			q.Set("user id", opts.User)
			q.Set("password", opts.Password)
			q.Set("applicationclientid", cmp.Or(opts.ApplicationClientID, defaultPublicClientID))

		case AuthEntraInteractive:
			q.Set("applicationclientid", cmp.Or(opts.ApplicationClientID, defaultPublicClientID))
			if opts.User != "" {
				q.Set("user id", opts.User) // login hint
			}

		case AuthEntraDeviceCode:
			if opts.ApplicationClientID != "" {
				q.Set("applicationclientid", opts.ApplicationClientID)
			}

		case AuthEntraAzurePipelines:
			// With no User the driver takes the client and tenant from
			// AZURESUBSCRIPTION_CLIENT_ID / _TENANT_ID. "serviceconnectionid"
			// and "systemtoken" are deliberately left to ExtraParams (or the
			// environment) and not reserved: that is the one route by which
			// this method has always connected.
			if opts.User != "" {
				userID, err := entraClientAtTenant(opts)
				if err != nil {
					return "", "", err
				}
				q.Set("user id", userID)
			}
		}

		if opts.DisableInstanceDiscovery {
			q.Set("disableinstancediscovery", "true")
		}
		if opts.TenantID != "" {
			// go-mssqldb's own azuread connector never reads this key (its
			// credential gets a tenant only from "user id"); gosmo's credential
			// does (parseEntraConfig), which is how TenantID reaches the
			// methods with no "user id" to carry it.
			q.Set("tenantid", opts.TenantID)
		}
		if err := mergeExtraParams(q, opts.ExtraParams); err != nil {
			return "", "", err
		}

		u := &url.URL{
			Scheme:   "sqlserver",
			Host:     dialHost,
			RawQuery: q.Encode(),
		}
		if instance != "" {
			u.Path = "/" + instance
		}
		return u.String(), driverName, nil
	}

	// -- Classic path: SQL Server auth or Windows auth --
	driverName = "sqlserver"
	u := &url.URL{
		Scheme: "sqlserver",
		Host:   dialHost,
	}
	if instance != "" {
		u.Path = "/" + instance
	}

	switch opts.Auth {
	case AuthWindows:
		if opts.ServerSPN != "" {
			q.Set("serverspn", opts.ServerSPN)
		}
		// Windows has native SSPI; every other platform authenticates the
		// Windows/AD identity through Kerberos. Kerberos is also used on
		// Windows when the caller configures it explicitly.
		if goos != "windows" || opts.Kerberos.configured() {
			q.Set("authenticator", "krb5")
			opts.Kerberos.applyDSN(q)
			// User + Password selects Kerberos's username/password login;
			// a bare User (optionally "user@REALM") rides along for the
			// keytab / credential-cache logins.
			switch {
			case opts.User != "" && opts.Password != "":
				u.User = url.UserPassword(opts.User, opts.Password)
			case opts.User != "":
				u.User = url.User(opts.User)
			}
		} else if opts.User != "" {
			// Native SSPI; user may optionally supply DOMAIN\user.
			u.User = url.User(opts.User)
		}
	default: // AuthSQLServer
		if opts.User != "" || opts.Password != "" {
			u.User = url.UserPassword(opts.User, opts.Password)
		}
	}

	if err := mergeExtraParams(q, opts.ExtraParams); err != nil {
		return "", "", err
	}
	u.RawQuery = q.Encode()
	return u.String(), driverName, nil
}

// checkEntraFields refuses, in ConnectionOptions vocabulary, options an Entra
// method cannot connect with. Left to the driver, the same mistakes come back
// worded in DSN keys the caller never wrote ("Must provide 'client id[@tenant
// id]' as username parameter").
func checkEntraFields(opts ConnectionOptions) error {
	const appClientID = "User (the application's client ID)"
	var missing string
	switch opts.Auth {
	case AuthEntraPassword:
		switch {
		case opts.User == "":
			missing = "User (the user's UPN)"
		case opts.Password == "":
			missing = "Password"
		}
	case AuthEntraServicePrincipal:
		switch {
		case opts.User == "":
			missing = appClientID
		case opts.Password == "" && opts.ClientCertPath == "":
			missing = "Password (a client secret) or ClientCertPath"
		}
	case AuthEntraServicePrincipalAccessToken:
		if opts.AccessToken == "" {
			missing = "AccessToken"
		}
	case AuthEntraOnBehalfOf:
		switch {
		case opts.User == "":
			missing = appClientID
		case opts.AccessToken == "":
			missing = "AccessToken (the inbound user assertion)"
		case opts.Password == "" && opts.ClientCertPath == "" && !hasExtraParam(opts.ExtraParams, "clientassertion"):
			missing = `Password (a client secret), ClientCertPath, or a "clientassertion" ExtraParams entry`
		}
	case AuthEntraAzurePipelines:
		// The client ID may come from the environment instead, but a TenantID
		// with no User has nowhere to go: the driver reads a tenant only as
		// the "@tenant" suffix of "user id".
		if opts.User == "" && opts.TenantID != "" {
			missing = appClientID + " when TenantID is set"
		}
	}
	if missing != "" {
		return invalidf("gosmo: %s requires %s", opts.Auth, missing)
	}
	return nil
}

// hasExtraParam reports whether extra carries a non-empty key, matched as the
// driver matches it (case-insensitively).
func hasExtraParam(extra url.Values, key string) bool {
	for k, vs := range extra {
		if strings.EqualFold(strings.TrimSpace(k), key) && len(vs) > 0 && vs[0] != "" {
			return true
		}
	}
	return false
}

// entraClientAtTenant renders the driver's "user id" for the methods that
// read it as "<client id>[@<tenant id>]" (service principal, on-behalf-of,
// Azure Pipelines). The driver splits it at the first '@' — when that is
// neither the first nor the last character — so a User that already carries
// a tenant must not have TenantID appended again ("app@T@T" signs in to
// tenant "T@T"), and one naming a different tenant from TenantID is refused
// rather than silently preferring either.
func entraClientAtTenant(opts ConnectionOptions) (string, error) {
	if at := strings.IndexByte(opts.User, '@'); at >= 1 && at < len(opts.User)-1 {
		if tenant := opts.User[at+1:]; opts.TenantID != "" && !strings.EqualFold(tenant, opts.TenantID) {
			return "", invalidf("gosmo: %s: User %q names tenant %q, but TenantID is %q",
				opts.Auth, opts.User, tenant, opts.TenantID)
		}
		return opts.User, nil
	}
	if opts.TenantID != "" {
		return opts.User + "@" + opts.TenantID, nil
	}
	return opts.User, nil
}

// setEntraAppCredential writes an application's own credential — a
// certificate (whose private-key password travels as "password") or else a
// client secret — for the service principal and on-behalf-of workflows,
// which read the same keys for it, including "sendcertificatechain".
func setEntraAppCredential(q url.Values, opts ConnectionOptions) {
	switch {
	case opts.ClientCertPath != "":
		q.Set("clientcertpath", opts.ClientCertPath)
		if opts.ClientCertPassword != "" {
			q.Set("password", opts.ClientCertPassword)
		}
	case opts.Password != "":
		q.Set("password", opts.Password)
	}
	if opts.SendCertificateChain {
		q.Set("sendcertificatechain", "true")
	}
}

// commonDSNValues builds the query parameters shared by every DSN,
// regardless of authentication method: target database, application name,
// connection timeout, and the TLS options.
func commonDSNValues(opts ConnectionOptions) url.Values {
	q := url.Values{}
	q.Set("database", opts.Database)
	q.Set("app name", opts.ApplicationName)
	// Rounded up: the driver takes whole seconds and reads 0 as "no timeout",
	// so truncating turned a 500ms timeout into none at all.
	timeout := strconv.Itoa(int((opts.ConnectTimeout + time.Second - 1) / time.Second))
	q.Set("connection timeout", timeout)
	// The driver applies "connection timeout" only to I/O after the dial; the
	// dial runs on its own 15 s default, so without this a 500ms timeout to an
	// unroutable host took 15 s. A caller's ExtraParams entry replaces it.
	q.Set("dial timeout", timeout)

	if opts.TrustServerCertificate {
		q.Set("TrustServerCertificate", "true")
	}
	if opts.Encrypt != "" {
		q.Set("encrypt", opts.Encrypt)
	}
	if opts.HostNameInCertificate != "" {
		q.Set("hostNameInCertificate", opts.HostNameInCertificate)
	}
	return q
}

// baseDSN builds a plain "sqlserver" DSN carrying only the target and the
// common connection parameters, with no authentication. Used for the
// access-token-provider path, where the bearer token is supplied out of
// band by the connector rather than through the DSN.
func baseDSN(opts ConnectionOptions) (string, error) {
	if opts.Server == "" {
		return "", invalidf("gosmo: ConnectionOptions.Server is required")
	}
	dialHost, instance, err := dsnHost(opts)
	if err != nil {
		return "", err
	}
	q := commonDSNValues(opts)
	if err := mergeExtraParams(q, opts.ExtraParams); err != nil {
		return "", err
	}
	u := &url.URL{
		Scheme:   "sqlserver",
		Host:     dialHost,
		RawQuery: q.Encode(),
	}
	if instance != "" {
		u.Path = "/" + instance
	}
	return u.String(), nil
}

// dsnHost renders opts' Server address (and Port) as the Host of a
// sqlserver:// URL, plus the instance name that travels as its path.
//
// An IPv6 literal is bracketed there: unbracketed, url.Parse takes its last
// group for a port, or rejects it outright when that group is not numeric.
// The driver removes the brackets again only when a port follows them (it
// splits with net.SplitHostPort), so a literal with no port is given the
// default 1433 explicitly — the port the driver would have dialled anyway.
// A literal with a named instance and no port has no URL form the driver
// reads back correctly: it would carry the brackets into the SQL Browser
// probe's address. That is an error naming the fix, not a dial that fails
// somewhere less obvious.
func dsnHost(opts ConnectionOptions) (host, instance string, err error) {
	server := opts.Server
	if proto, _ := splitProtocolPrefix(server); unsupportedProtocols[proto] != "" {
		return "", "", invalidf("gosmo: server %q: %s", server, unsupportedProtocols[proto])
	}
	if opts.Port < 0 || opts.Port > 65535 {
		return "", "", invalidf("gosmo: ConnectionOptions.Port %d out of range 0-65535", opts.Port)
	}
	host, instance, port := opts.address()
	if strings.ContainsRune(host, ':') {
		if !strings.HasPrefix(host, "[") {
			host = "[" + host + "]"
		}
		if port == 0 {
			if instance != "" {
				return "", "", invalidf("gosmo: server %q: an IPv6 address with a named instance needs "+
					"an explicit port (%s\\%s,<port>)", server, strings.Trim(host, "[]"), instance)
			}
			port = 1433
		}
	}
	if port > 0 {
		host = fmt.Sprintf("%s:%d", host, port)
	}
	return host, instance, nil
}

// reservedDSNKeys are the driver parameters a ConnectionOptions field
// controls, lower-cased as the driver compares them — see
// ConnectionOptions.ExtraParams, which may not set any of them. The ADO.NET
// synonyms go-mssqldb maps onto these keys are listed too: the URL form does
// not translate them, so "initial catalog" beside "database" would be an
// unknown key silently ignored rather than an override, which is no better.
// Every key buildDSN, baseDSN and KerberosOptions.applyDSN writes must be
// here; TestReservedDSNKeysCoverEveryKeyGosmoWrites pins it.
var reservedDSNKeys = map[string]bool{
	"server": true, "port": true, "database": true, "user id": true, "password": true,
	"app name": true, "connection timeout": true,
	"encrypt": true, "trustservercertificate": true, "hostnameincertificate": true,
	"fedauth": true, "authenticator": true, "serverspn": true, "tenantid": true,
	"applicationclientid": true, "clientcertpath": true, "tokenfilepath": true,
	"sendcertificatechain": true, "disableinstancediscovery": true, "userassertion": true,

	// ADO.NET synonyms of the above.
	"data source": true, "address": true, "network address": true, "addr": true,
	"initial catalog": true, "user": true, "uid": true, "pwd": true,
	"app": true, "application name": true, "connect timeout": true, "timeout": true,
	"trust server certificate": true, "host name in certificate": true, "server spn": true,
}

// overridableDSNKeys are the driver parameters gosmo writes as a default
// derived from a ConnectionOptions field, which an ExtraParams entry may
// replace rather than being refused: "dial timeout" follows ConnectTimeout
// unless the caller wants the dial bounded differently from the login.
var overridableDSNKeys = map[string]bool{"dial timeout": true}

// ExtraParamError is the error Connect and ConnectionString return for a
// ConnectionOptions.ExtraParams entry they refuse. Key is the name as the
// caller gave it.
type ExtraParamError struct {
	Key string
	// Reserved reports that a ConnectionOptions field controls Key — the
	// caller should set that field instead. When false the entry itself is
	// malformed: an empty name, more than one value, or the same key twice in
	// different case.
	Reserved bool
	reason   string
}

func (e *ExtraParamError) Error() string {
	return fmt.Sprintf("gosmo: extra connection parameter %q %s", e.Key, e.reason)
}

// mergeExtraParams adds ConnectionOptions.ExtraParams to the DSN query q,
// refusing a key a ConnectionOptions field controls (reservedDSNKeys, plus the
// krb5-* family), a key q already carries, and a key given more than one
// value or given twice in different case — the driver itself rejects the
// last two with a message that does not say where the duplicate came from.
// Every refusal is an *ExtraParamError.
func mergeExtraParams(q, extra url.Values) error {
	const reserved = "is set through a ConnectionOptions field, not ExtraParams"
	seen := make(map[string]bool, len(extra))
	for k, vs := range extra {
		key := strings.ToLower(strings.TrimSpace(k))
		switch {
		case key == "":
			return &ExtraParamError{Key: k, reason: "has an empty name"}
		case reservedDSNKeys[key] || strings.HasPrefix(key, "krb5-"):
			return &ExtraParamError{Key: k, Reserved: true, reason: reserved}
		case seen[key]:
			return &ExtraParamError{Key: k, reason: "is given more than once"}
		case len(vs) != 1:
			return &ExtraParamError{Key: k, reason: fmt.Sprintf("needs exactly one value, has %d", len(vs))}
		}
		for existing := range q {
			if strings.EqualFold(existing, key) {
				if !overridableDSNKeys[key] {
					return &ExtraParamError{Key: k, Reserved: true, reason: reserved}
				}
				q.Del(existing)
			}
		}
		seen[key] = true
		q.Set(key, vs[0])
	}
	return nil
}

// buildConnector builds the driver connector Connect opens the pool
// with. Using a connector (rather than sql.Open on a DSN string) is what
// lets gosmo hand the driver a token-refresh callback and per-session init
// SQL that a string DSN can't express.
//
// The connector is chosen from opts:
//   - AccessTokenProvider set: the caller mints (and refreshes) bearer
//     tokens itself, so the token is presented straight to SQL Server via a
//     security-token connector, bypassing the fedauth DSN machinery. This
//     wins over Auth and AccessToken.
//   - an Entra Auth method: a token connector whose credential comes from
//     opts.EntraCache (see entra.go), after the azuread parser has validated
//     the DSN.
//   - otherwise: the base sqlserver connector.
func buildConnector(opts ConnectionOptions) (*mssql.Connector, error) {
	var (
		connector *mssql.Connector
		err       error
	)

	switch {
	case opts.AccessTokenProvider != nil:
		var dsn string
		if dsn, err = baseDSN(opts); err != nil {
			return nil, err
		}
		connector, err = mssql.NewConnectorWithAccessTokenProvider(dsn, opts.AccessTokenProvider)

	default:
		var dsn, driverName string
		if dsn, driverName, err = buildDSN(opts); err != nil {
			return nil, err
		}
		if driverName == "azuresql" {
			cache := opts.EntraCache
			if cache == nil {
				cache = NewEntraCache()
			}
			connector, err = newEntraConnector(dsn, cache, opts.DeviceCodePrompt)
		} else {
			connector, err = mssql.NewConnector(dsn)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: build connector: %w", err)
	}

	connector.SessionInitSQL = opts.SessionInitSQL
	connector.Dialer = dialerFor(opts)
	return connector, nil
}

// maskedSecret stands in for every secret in a masked ConnectionString. A
// fixed placeholder rather than a run of '*' as long as the secret, so the
// masked form does not leak the secret's length.
const maskedSecret = "XXXXX"

// ConnectionString renders the go-mssqldb connection string Connect would
// dial for o: the same builder, after the same defaults, so the result
// carries "database=master", the application name and the connect timeout
// exactly as they are sent, and fails with the error Connect would fail with
// for the same options. Use it to show a user what is being dialled, to log
// it, or to hand it to another go-mssqldb client.
//
// With maskSecrets, every password, client secret, certificate password,
// access token and user assertion in it is replaced by a fixed placeholder, so the result is safe
// to display — and no longer connects. Unmasked, it is a live credential. The
// tokens an AccessTokenProvider mints never appear either way: they are
// fetched per connection and are not part of the string.
func (o ConnectionOptions) ConnectionString(maskSecrets bool) (string, error) {
	applyDefaults(&o)
	var (
		dsn string
		err error
	)
	if o.AccessTokenProvider != nil {
		dsn, err = baseDSN(o)
	} else {
		dsn, _, err = buildDSN(o)
	}
	if err != nil || !maskSecrets {
		return dsn, err
	}

	u, err := url.Parse(dsn)
	if err != nil {
		// Never include dsn or the parse error: both carry the secrets this
		// call was asked to hide.
		return "", fmt.Errorf("gosmo: connection string: cannot mask an unparseable DSN")
	}
	if u.User != nil {
		if p, ok := u.User.Password(); ok && p != "" {
			u.User = url.UserPassword(u.User.Username(), maskedSecret)
		}
	}
	q := u.Query()
	masked := false
	for _, k := range secretDSNKeys {
		if q.Get(k) != "" {
			q.Set(k, maskedSecret)
			masked = true
		}
	}
	if masked {
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// secretDSNKeys are the query parameters a masked ConnectionString hides:
// "password" (every password, client secret, certificate password and access
// token gosmo writes), the on-behalf-of user assertion, and the two Entra
// secrets only ExtraParams can carry (mergeExtraParams lower-cases keys, so
// these match however the caller spelled them).
var secretDSNKeys = []string{"password", "userassertion", "systemtoken", "clientassertion"}
