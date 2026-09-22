//go:build livedb

package gosmo

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
)

// TestLiveKeyOps drives the key and certificate writes key_ops_write_test.go
// pins as text: certificate backup, private-key removal, owner changes, the
// database master key's reads and writes (opened by password and not), and
// module signatures.
//
// Every run leaves its backup files behind in C:\temp on the server, named
// gosmo_keyops_<pid>_*. SQL Server writes a certificate or master key backup
// with an ACL that denies even its own service account delete and WRITE_DAC,
// so neither xp_cmdshell's del nor icacls /grant can remove them (2026-09-22,
// on 13, 14 and 17); an administrator on the host has to.
func TestLiveKeyOps(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_keyops")
	defer drop()

	tag := fmt.Sprintf("gosmo_keyops_%d", os.Getpid())
	file := func(ext string) string { return `C:\temp\` + tag + ext }

	run := func(q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, "USE [gosmo_keyops];\n"+q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	const mk, cp = "Mk!111aaaBBB", "Cp!111aaaBBB"
	if err := d.CreateMasterKeyContext(ctx, mk); err != nil {
		t.Fatal(err)
	}
	run("CREATE CERTIFICATE c1 WITH SUBJECT = 'c1'")
	run("CREATE CERTIFICATE c2 ENCRYPTION BY PASSWORD = '" + cp + "' WITH SUBJECT = 'c2'")
	run("CREATE ASYMMETRIC KEY a1 WITH ALGORITHM = RSA_2048")
	run("CREATE SYMMETRIC KEY s1 WITH ALGORITHM = AES_256 ENCRYPTION BY CERTIFICATE c1")
	run("CREATE USER u1 WITHOUT LOGIN")
	run("EXEC('CREATE PROCEDURE dbo.p1 AS SELECT 1')")
	run("EXEC('CREATE FUNCTION dbo.f1() RETURNS int AS BEGIN RETURN 1 END')")

	// -- Certificate backup: public only, with a master-key-protected private
	// key, and with a password-protected one.
	c1, c2 := d.CertificateRef("c1"), d.CertificateRef("c2")
	if err := c1.BackupContext(ctx, CertificateBackupSpec{File: file("_c1pub.cer")}); err != nil {
		t.Fatal(err)
	}
	if err := c1.BackupContext(ctx, CertificateBackupSpec{File: file("_c1.cer"), PrivateKeyFile: file("_c1.pvk"), EncryptionPassword: "Bk!111aaaBBB"}); err != nil {
		t.Fatal(err)
	}
	if err := c2.BackupContext(ctx, CertificateBackupSpec{File: file("_c2.cer"), PrivateKeyFile: file("_c2.pvk"),
		EncryptionPassword: "Bk!111aaaBBB", DecryptionPassword: cp}); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.CertificateByNameContext(ctx, "c2"); got == nil || got.PvtKeyLastBackupDate.IsZero() {
		t.Error("c2's private key backup date was not set")
	}

	// -- Signatures, by a master-key certificate, a password certificate (its
	// password), and an asymmetric key; then read three ways.
	if err := d.AddSignatureContext(ctx, "dbo", "p1", Signer{Kind: SignerCertificate, Name: "c1"}, false); err != nil {
		t.Fatal(err)
	}
	if err := d.AddSignatureContext(ctx, "dbo", "p1", Signer{Kind: SignerCertificate, Name: "c2", Password: cp}, false); err != nil {
		t.Fatal(err)
	}
	if err := d.AddSignatureContext(ctx, "dbo", "f1", Signer{Kind: SignerAsymmetricKey, Name: "a1"}, true); err != nil {
		t.Fatal(err)
	}
	all, err := d.ModuleSignaturesContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range all {
		got = append(got, fmt.Sprintf("%s.%s %s %s %v %s", s.Schema, s.Module, s.Kind, s.Signer, s.Counter, s.ModuleType))
	}
	want := []string{
		"dbo.f1 ASYMMETRIC KEY a1 true SQL_SCALAR_FUNCTION",
		"dbo.p1 CERTIFICATE c1 false SQL_STORED_PROCEDURE",
		"dbo.p1 CERTIFICATE c2 false SQL_STORED_PROCEDURE",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ModuleSignatures:\n got %q\nwant %q", got, want)
	}
	run("EXEC('CREATE FUNCTION dbo.itvf() RETURNS TABLE AS RETURN SELECT 1 x')")
	if mods, err := d.SignableModulesContext(ctx); err != nil || len(mods) != 2 || mods[0].Name != "f1" || mods[1].Name != "p1" {
		t.Errorf("SignableModules = %v, %v; want f1 and p1, the inline function left out", mods, err)
	}
	if on, err := d.SignaturesOnContext(ctx, "dbo", "p1"); err != nil || len(on) != 2 {
		t.Errorf("SignaturesOn(p1) = %d, %v; want 2", len(on), err)
	}
	if by, err := c1.SignedModulesContext(ctx); err != nil || len(by) != 1 || by[0].Module != "p1" {
		t.Errorf("c1.SignedModules = %v, %v", by, err)
	}
	if by, err := d.AsymmetricKeyRef("a1").SignedModulesContext(ctx); err != nil || len(by) != 1 || by[0].Module != "f1" || !by[0].Counter {
		t.Errorf("a1.SignedModules = %v, %v", by, err)
	}
	if err := d.AddSignatureContext(ctx, "dbo", "p1", Signer{Kind: SignerCertificate, Name: "c1"}, false); liveMsg(err) != 15557 {
		t.Errorf("a second signature by c1: %v, want Msg 15557", err)
	}
	if err := d.DropSignatureContext(ctx, "dbo", "p1", Signer{Kind: SignerCertificate, Name: "c2", Password: "ignored"}, false); err != nil {
		t.Fatal(err)
	}
	if err := d.DropSignatureContext(ctx, "dbo", "f1", Signer{Kind: SignerAsymmetricKey, Name: "a1"}, true); err != nil {
		t.Fatal(err)
	}
	if on, _ := d.SignaturesOnContext(ctx, "dbo", "p1"); len(on) != 1 || on[0].Signer != "c1" {
		t.Errorf("after the drops p1 is signed by %v, want c1 alone", on)
	}

	// -- Private key removal.
	if err := c2.RemovePrivateKeyContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.AsymmetricKeyRef("a1").RemovePrivateKeyContext(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.CertificateByNameContext(ctx, "c2"); got == nil || got.HasPrivateKey() {
		t.Error("c2 still has a private key")
	}
	if got, _ := d.AsymmetricKeyByNameContext(ctx, "a1"); got == nil || got.HasPrivateKey() {
		t.Error("a1 still has a private key")
	}

	// -- Owner changes, and the permissions they drop.
	run("GRANT VIEW DEFINITION ON CERTIFICATE::c1 TO u1")
	if err := c1.ChangeOwnerContext(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	if err := d.AsymmetricKeyRef("a1").ChangeOwnerContext(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	if err := d.SymmetricKeyRef("s1").ChangeOwnerContext(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	if c1.Owner != "u1" {
		t.Errorf("c1's handle owner = %q, want the change mirrored", c1.Owner)
	}
	var perms int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM gosmo_keyops.sys.database_permissions WHERE class = 25").Scan(&perms); err != nil || perms != 0 {
		t.Errorf("certificate permissions after the owner change: %d, %v; want 0", perms, err)
	}
	for _, q := range []string{
		"SELECT p.name FROM gosmo_keyops.sys.certificates k JOIN gosmo_keyops.sys.database_principals p ON p.principal_id = k.principal_id WHERE k.name = 'c1'",
		"SELECT p.name FROM gosmo_keyops.sys.asymmetric_keys k JOIN gosmo_keyops.sys.database_principals p ON p.principal_id = k.principal_id WHERE k.name = 'a1'",
		"SELECT p.name FROM gosmo_keyops.sys.symmetric_keys k JOIN gosmo_keyops.sys.database_principals p ON p.principal_id = k.principal_id WHERE k.name = 's1'",
	} {
		var owner sql.NullString
		if err := db.QueryRowContext(ctx, q).Scan(&owner); err != nil || owner.String != "u1" {
			t.Errorf("%s = %q, %v; want u1", q, owner.String, err)
		}
	}

	// -- The database master key.
	m, err := d.MasterKeyContext(ctx)
	if err != nil || m == nil {
		t.Fatalf("MasterKey = %v, %v", m, err)
	}
	if !m.EncryptedByServer || m.Algorithm != "AES_256" || len(m.Encryptions) != 2 {
		t.Errorf("master key = %+v", m)
	}
	if err := m.DropEncryptionContext(ctx, MasterKeyEncryptor{ServiceMasterKey: true}, ""); err != nil {
		t.Fatal(err)
	}
	// Not encrypted by the service master key any more, so every write needs
	// it opened by password.
	if err := m.AddEncryptionContext(ctx, MasterKeyEncryptor{ServiceMasterKey: true}, ""); liveMsg(err) != 15581 {
		t.Errorf("adding the SMK encryption unopened: %v, want Msg 15581", err)
	}
	if err := m.AddEncryptionContext(ctx, MasterKeyEncryptor{ServiceMasterKey: true}, "wrong"); liveMsg(err) != 15313 {
		t.Errorf("adding the SMK encryption with a wrong password: %v, want Msg 15313", err)
	}
	if err := m.BackupContext(ctx, file("_dmk_open.key"), "Bk!111aaaBBB", mk); err != nil {
		t.Fatal(err)
	}
	if err := m.AddEncryptionContext(ctx, MasterKeyEncryptor{ServiceMasterKey: true}, mk); err != nil {
		t.Fatal(err)
	}
	if m, _ := d.MasterKeyContext(ctx); m == nil || !m.EncryptedByServer {
		t.Error("the SMK encryption did not come back")
	}
	if err := m.AddEncryptionContext(ctx, MasterKeyEncryptor{Password: "Mk!222cccDDD"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.DropEncryptionContext(ctx, MasterKeyEncryptor{Password: mk}, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.BackupContext(ctx, file("_dmk.key"), "Bk!111aaaBBB", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.RegenerateContext(ctx, "Mk!333eeeFFF", false, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.DropEncryptionContext(ctx, MasterKeyEncryptor{Password: "Mk!333eeeFFF"}, ""); liveMsg(err) != 15558 {
		t.Errorf("dropping the last password: %v, want Msg 15558", err)
	}
	if err := m.DropContext(ctx); liveMsg(err) != 15580 {
		t.Errorf("dropping a master key that protects c1: %v, want Msg 15580", err)
	}
	run("DROP SYMMETRIC KEY s1; DROP SIGNATURE FROM dbo.p1 BY CERTIFICATE c1; DROP CERTIFICATE c1; DROP CERTIFICATE c2; DROP ASYMMETRIC KEY a1")
	if err := m.DropContext(ctx); err != nil {
		t.Fatal(err)
	}
	if m, err := d.MasterKeyContext(ctx); m != nil || err != nil {
		t.Errorf("after the drop MasterKey = %v, %v; want (nil, nil)", m, err)
	}
}
