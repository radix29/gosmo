package gosmo

import (
	"net/url"
	"strconv"

	// Registers the "krb5" integrated-authentication provider with
	// go-mssqldb. Without this blank import the provider is unknown and a
	// Kerberos connection fails with "provider krb5 not found". The package
	// is pure Go (gokrb5), so it works on every platform gossms targets, not
	// just Windows.
	_ "github.com/microsoft/go-mssqldb/integratedauth/krb5"
)

// ============================================================
// Kerberos (Active Directory integrated auth on any platform)
// ============================================================

// KerberosOptions configures Kerberos authentication for AuthWindows on
// non-Windows hosts (and on Windows when set explicitly, in preference to
// native SSPI). Every field is optional: with all left zero the driver uses
// the ambient Kerberos setup — /etc/krb5.conf (or $KRB5_CONFIG) and the
// credential cache from a prior "kinit" ($KRB5CCNAME or the default cache)
// — which is the usual single-sign-on flow on a domain-joined Linux host.
//
// The three credential sources are mutually exclusive and tried in this
// order of precedence by the driver: an explicit KeytabFile (with User and
// Realm), then a CredCacheFile, then User + Password (with Realm). When
// none is set, the credential cache is discovered from the environment.
type KerberosOptions struct {
	// ConfigFile is the path to krb5.conf. Defaults to $KRB5_CONFIG then
	// /etc/krb5.conf. The file must exist for any Kerberos login.
	ConfigFile string

	// CredCacheFile is the path to a credential cache produced by "kinit".
	// Defaults to $KRB5CCNAME. Use this for single sign-on.
	CredCacheFile string

	// KeytabFile is the path to a keytab holding the client's long-term key,
	// for unattended logins. Requires User and Realm. Defaults to
	// $KRB5_KTNAME or the configured default client keytab.
	KeytabFile string

	// Realm is the Kerberos realm, e.g. "CONTOSO.COM". When empty it is
	// taken from a "user@REALM" User, then from the krb5.conf default realm.
	Realm string

	// DNSLookupKDC, when non-nil, overrides whether KDCs are located via DNS
	// SRV records. The driver defaults to true.
	DNSLookupKDC *bool

	// UDPPreferenceLimit caps the message size sent over UDP before TCP is
	// used. Zero leaves the driver default (1, i.e. effectively always TCP).
	UDPPreferenceLimit int
}

// configured reports whether any Kerberos field is set, i.e. whether the
// caller has asked for Kerberos explicitly rather than relying on the
// platform default.
func (k KerberosOptions) configured() bool {
	return k.ConfigFile != "" || k.CredCacheFile != "" || k.KeytabFile != "" ||
		k.Realm != "" || k.DNSLookupKDC != nil || k.UDPPreferenceLimit != 0
}

// applyDSN writes the krb5-* query parameters for the set fields into q. It
// does not set "authenticator" (buildDSN owns that) or the user id /
// password, which travel as URL userinfo like every other method.
func (k KerberosOptions) applyDSN(q url.Values) {
	if k.ConfigFile != "" {
		q.Set("krb5-configfile", k.ConfigFile)
	}
	if k.CredCacheFile != "" {
		q.Set("krb5-credcachefile", k.CredCacheFile)
	}
	if k.KeytabFile != "" {
		q.Set("krb5-keytabfile", k.KeytabFile)
	}
	if k.Realm != "" {
		q.Set("krb5-realm", k.Realm)
	}
	if k.DNSLookupKDC != nil {
		q.Set("krb5-dnslookupkdc", strconv.FormatBool(*k.DNSLookupKDC))
	}
	if k.UDPPreferenceLimit != 0 {
		q.Set("krb5-udppreferencelimit", strconv.Itoa(k.UDPPreferenceLimit))
	}
}
