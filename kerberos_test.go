package gosmo

import (
	"net/url"
	"testing"
)

func TestKerberosConfigured(t *testing.T) {
	yes := true
	cases := []struct {
		name string
		k    KerberosOptions
		want bool
	}{
		{"zero", KerberosOptions{}, false},
		{"config file", KerberosOptions{ConfigFile: "/etc/krb5.conf"}, true},
		{"cred cache", KerberosOptions{CredCacheFile: "/tmp/krb5cc_1000"}, true},
		{"keytab", KerberosOptions{KeytabFile: "/etc/krb5.keytab"}, true},
		{"realm", KerberosOptions{Realm: "CONTOSO.COM"}, true},
		{"dns lookup", KerberosOptions{DNSLookupKDC: &yes}, true},
		{"udp limit", KerberosOptions{UDPPreferenceLimit: 1500}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.k.configured(); got != c.want {
				t.Errorf("configured() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestKerberosApplyDSN(t *testing.T) {
	no := false
	q := url.Values{}
	KerberosOptions{
		ConfigFile:         "/custom/krb5.conf",
		CredCacheFile:      "/tmp/cc",
		KeytabFile:         "/etc/app.keytab",
		Realm:              "CONTOSO.COM",
		DNSLookupKDC:       &no,
		UDPPreferenceLimit: 2048,
	}.applyDSN(q)

	want := map[string]string{
		"krb5-configfile":         "/custom/krb5.conf",
		"krb5-credcachefile":      "/tmp/cc",
		"krb5-keytabfile":         "/etc/app.keytab",
		"krb5-realm":              "CONTOSO.COM",
		"krb5-dnslookupkdc":       "false",
		"krb5-udppreferencelimit": "2048",
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestKerberosApplyDSNOmitsUnset(t *testing.T) {
	q := url.Values{}
	KerberosOptions{Realm: "CONTOSO.COM"}.applyDSN(q)
	if _, ok := q["krb5-keytabfile"]; ok {
		t.Error("krb5-keytabfile should be omitted when KeytabFile is empty")
	}
	if _, ok := q["krb5-dnslookupkdc"]; ok {
		t.Error("krb5-dnslookupkdc should be omitted when DNSLookupKDC is nil")
	}
	if _, ok := q["krb5-udppreferencelimit"]; ok {
		t.Error("krb5-udppreferencelimit should be omitted when zero")
	}
}
