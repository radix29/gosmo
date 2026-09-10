//go:build livedb

// Live coverage for Warm's probe: the half-login that asks a server which
// token its Entra login wants, before anyone signs in.
//
// No unit test can reach it — the SPN and STS URL come from the server's
// FEDAUTHINFO token, part-way through a real login. It needs no Entra
// identity: the probe abandons the login before a token is requested, so the
// SQL login in the DSN is used only for its host, port and TLS settings.
//
//	go test -tags livedb . -run TestLiveEntraProbe -v \
//	  -livedb 'sqlserver://testgo:PASS@t-qmi-01…:3342?TrustServerCertificate=true'
//
// Nothing is created. On an instance with no Entra support the probe must fail
// with the server's own refusal rather than report an authority.
package gosmo

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestLiveEntraProbe(t *testing.T) {
	if *liveDSN == "" {
		t.Skip("no -livedb DSN given")
	}
	u, err := url.Parse(*liveDSN)
	if err != nil {
		t.Fatalf("parse -livedb: %v", err)
	}
	q := u.Query()
	opts := ConnectionOptions{
		Server:                 u.Host,
		Auth:                   AuthEntraInteractive,
		TrustServerCertificate: strings.EqualFold(q.Get("TrustServerCertificate"), "true"),
		Encrypt:                "mandatory",
	}
	applyDefaults(&opts)
	cfg, err := entraConfigFor(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	srv, err := probeServerSignIn(ctx, opts, cfg)
	if !strings.Contains(strings.ToLower(u.Host), ".database.") {
		if err == nil {
			t.Fatalf("non-Azure server announced %+v; expected a refusal", srv)
		}
		t.Logf("non-Azure server refused the Entra login, as expected: %v", err)
		return
	}
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	t.Logf("SPN %q, STS %q", srv.spn, srv.stsURL)
	authority, tenant := splitSTSURL(srv.stsURL)
	if srv.spn == "" || authority == "" || tenant == "" {
		t.Errorf("probe = %+v, want an SPN and an STS URL ending in the tenant", srv)
	}
}
