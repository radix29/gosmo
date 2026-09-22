//go:build livedb

package gosmo

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// TestLiveCertificateScriptRoundTrip runs ScriptCertificate's CREATE in a
// second database and checks it recreated the same public certificate —
// same thumbprint, owner and ACTIVE FOR BEGIN_DIALOG — with no private key.
// FROM BINARY into the database the certificate came from fails Msg 15232
// (same thumbprint), which is why the target is a second database.
//
// The source certificate is switched off for Service Broker and owned by a
// user, so the script's two optional clauses both have to parse and take
// effect; the owner is created in the target first, as a script's reader
// would have to.
func TestLiveCertificateScriptRoundTrip(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_certscript_src")
	defer dropSrc()
	dst, dropDst := liveScratchDB(t, db, ctx, "gosmo_certscript_dst")
	defer dropDst()

	for _, d := range []*Database{src, dst} {
		if _, err := d.exec(ctx, "CREATE USER cert_owner WITHOUT LOGIN"); err != nil {
			t.Fatalf("create owner in %s: %v", d.Name, err)
		}
	}
	for _, stmt := range []string{
		`CREATE CERTIFICATE [round'trip] AUTHORIZATION cert_owner
		   ENCRYPTION BY PASSWORD = N'R0und!Trip#Pass' WITH SUBJECT = N'gosmo round trip'
		   ACTIVE FOR BEGIN_DIALOG = OFF`,
	} {
		if _, err := src.exec(ctx, stmt); err != nil {
			t.Fatalf("create source certificate: %v", err)
		}
	}

	orig, err := src.CertificateByNameContext(ctx, "round'trip")
	if err != nil || orig == nil {
		t.Fatalf("read source certificate: %v, %v", orig, err)
	}
	if orig.Owner != "cert_owner" || orig.IsActiveForBeginDialog || orig.KeyLength == 0 ||
		orig.IssuerName == "" || orig.SerialNumber == "" || !orig.PvtKeyLastBackupDate.IsZero() {
		t.Errorf("source certificate read back as %+v", orig)
	}

	script, err := NewScripter(src, ScriptOptions{Verb: ScriptCreate}).ScriptCertificateContext(ctx, "round'trip")
	if err != nil {
		t.Fatalf("ScriptCertificateContext: %v", err)
	}
	t.Logf("script:\n%s", script)
	if !strings.Contains(script, "private key cannot be read") {
		t.Errorf("script does not say the private key is missing:\n%s", script)
	}
	for _, batch := range splitGoBatches(script) {
		if _, err := dst.exec(ctx, batch); err != nil {
			t.Fatalf("run script in %s: %v\n%s", dst.Name, err, batch)
		}
	}

	got, err := dst.CertificateByNameContext(ctx, "round'trip")
	if err != nil || got == nil {
		t.Fatalf("read recreated certificate: %v, %v", got, err)
	}
	if !bytes.Equal(got.Thumbprint, orig.Thumbprint) {
		t.Errorf("thumbprint %X, want %X", got.Thumbprint, orig.Thumbprint)
	}
	if got.Owner != orig.Owner || got.IsActiveForBeginDialog != orig.IsActiveForBeginDialog ||
		got.Subject != orig.Subject || got.SerialNumber != orig.SerialNumber ||
		!got.ExpiryDate.Equal(orig.ExpiryDate) {
		t.Errorf("recreated certificate %+v differs from %+v", got, orig)
	}
	if got.HasPrivateKey() {
		t.Errorf("recreated certificate has a private key (%s); the script cannot carry one", got.PvtKeyEncryptionType)
	}

	// DROP from a name-only handle, and the not-found scripter answer.
	if err := dst.CertificateRef("round'trip").DropContext(ctx); err != nil {
		t.Fatalf("drop through CertificateRef: %v", err)
	}
	if _, err := NewScripter(dst, ScriptOptions{}).ScriptCertificateContext(ctx, "round'trip"); !errors.Is(err, ErrNotFound) {
		t.Errorf("scripting a dropped certificate: %v, want a not-found error", err)
	}
}
