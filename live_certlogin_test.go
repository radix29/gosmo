//go:build livedb

// Live verification of the certificate-mapped login path: that the CREATE
// LOGIN statement gosmo builds is accepted, that the login reads back as the
// mapped type with its certificate resolvable, and that the script generated
// from it recreates the same login.
//
// The unit tests pin the statement text; only a live run settles what SQL
// Server accepts. It rejected the first shape tried here — a mapped login
// cannot have DEFAULT_DATABASE in CREATE *or* ALTER, which is why
// CreateLoginContext refuses the option rather than sending it.
//
//	go test -tags livedb . -run TestLiveCertificateLogin -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway certificate and login; touches nothing
// else.
package gosmo

import (
	"strings"
	"testing"
)

func TestLiveCertificateLoginCreateReadScript(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	s, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	master := s.Database("master")
	// CREATE CERTIFICATE encrypts the private key with master's DMK.
	defer ensureMasterKey(t, s, ctx)()

	const certName = "gosmo_live_cert"
	const loginName = "gosmo_live_certlogin"

	cleanup := func() {
		dropLoginIfPresent(t, s, loginName)
		if c, err := master.CertificateByNameContext(ctx, certName); err == nil && c != nil {
			if err := c.DropContext(ctx); err != nil {
				t.Logf("cleanup of certificate %q: %v", certName, err)
			}
		}
	}
	cleanup()
	defer cleanup()

	if err := master.CreateCertificateContext(ctx, CertificateSpec{
		Name: certName, Subject: "gosmo live test certificate",
	}); err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	if err := s.CreateLoginContext(ctx, loginName, "", &CreateLoginOptions{
		Source: LoginSourceCertificate, CertificateName: certName,
	}); err != nil {
		t.Fatalf("create certificate login: %v", err)
	}

	l, err := s.LoginByNameContext(ctx, loginName)
	if err != nil {
		t.Fatalf("read the login back: %v", err)
	}
	if l.LoginType != "CERTIFICATE_MAPPED_LOGIN" {
		t.Fatalf("LoginType = %q, want CERTIFICATE_MAPPED_LOGIN", l.LoginType)
	}
	if err := l.ResolveMappingContext(ctx); err != nil {
		t.Fatalf("ResolveMappingContext: %v", err)
	}
	if l.MappedObject != certName {
		t.Errorf("MappedObject = %q, want %q", l.MappedObject, certName)
	}

	// SQL Server reports a default database for a mapped login but refuses to
	// set one, so the script must not carry it back.
	script, err := NewServerScripter(s, DefaultScriptOptions()).ScriptLoginContext(ctx, loginName)
	if err != nil {
		t.Fatalf("script the login: %v", err)
	}
	if !strings.Contains(script, "FROM CERTIFICATE ["+certName+"]") {
		t.Fatalf("script does not name the certificate:\n%s", script)
	}
	if strings.Contains(script, "DEFAULT_DATABASE") {
		t.Fatalf("a mapped login's script must not set a default database:\n%s", script)
	}

	// Recreate it from its own script — the check the statement text cannot
	// make.
	if err := s.DropLoginContext(ctx, loginName); err != nil {
		t.Fatalf("drop before replay: %v", err)
	}
	runScript(t, s, script)
	again, err := s.LoginByNameContext(ctx, loginName)
	if err != nil {
		t.Fatalf("read back after replaying the script: %v", err)
	}
	if again.LoginType != "CERTIFICATE_MAPPED_LOGIN" {
		t.Errorf("after replay LoginType = %q, want CERTIFICATE_MAPPED_LOGIN", again.LoginType)
	}
}

// A mapped login has no default database SQL Server will accept: the CREATE
// fails, and so does the ALTER an external login's default database goes
// through. CreateLoginContext refuses it before either is sent — this pins
// that the server really does reject it, so the refusal is not just gosmo
// being cautious.
func TestLiveCertificateLoginRejectsADefaultDatabase(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	s, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	master := s.Database("master")
	// CREATE CERTIFICATE encrypts the private key with master's DMK.
	defer ensureMasterKey(t, s, ctx)()

	const certName = "gosmo_live_cert_dd"
	const loginName = "gosmo_live_certlogin_dd"

	cleanup := func() {
		dropLoginIfPresent(t, s, loginName)
		if c, err := master.CertificateByNameContext(ctx, certName); err == nil && c != nil {
			c.DropContext(ctx)
		}
	}
	cleanup()
	defer cleanup()

	if err := master.CreateCertificateContext(ctx, CertificateSpec{
		Name: certName, Subject: "gosmo live test certificate",
	}); err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	// gosmo refuses it up front.
	if err := s.CreateLoginContext(ctx, loginName, "", &CreateLoginOptions{
		Source: LoginSourceCertificate, CertificateName: certName, DefaultDatabase: "tempdb",
	}); err == nil {
		t.Fatal("a mapped login with a default database: want an error, got none")
	}

	// And the server refuses it too, on the ALTER the external-login path
	// would have used.
	if err := s.CreateLoginContext(ctx, loginName, "", &CreateLoginOptions{
		Source: LoginSourceCertificate, CertificateName: certName,
	}); err != nil {
		t.Fatalf("create certificate login: %v", err)
	}
	_, err = s.db.Exec("ALTER LOGIN " + quoteIdent(loginName) + " WITH DEFAULT_DATABASE = [tempdb]")
	if err == nil {
		t.Error("ALTER LOGIN ... DEFAULT_DATABASE on a mapped login succeeded; gosmo refuses it for nothing")
	} else if !strings.Contains(err.Error(), "DEFAULT_DATABASE") {
		t.Logf("rejected, as expected, with: %v", err)
	}
}
