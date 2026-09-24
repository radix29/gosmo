package gosmo

import (
	"context"
	"strings"
	"testing"
)

// The key and certificate writes added for goSSMS's open threads N4-N8:
// backup, private-key removal, owner change, FROM PROVIDER, the database
// master key and module signatures. Each was run live on 13 and 17 before
// being written down here (live_keyops_test.go), except FROM PROVIDER, which
// no test instance can run.

// capture runs each write under WithScript and returns what it collected.
func capture(t *testing.T, writes ...func(ctx context.Context) error) []string {
	t.Helper()
	ctx, col := WithScript(context.Background())
	for i, w := range writes {
		if err := w(ctx); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	return col.Statements()
}

func assertStatements(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("captured %d statements, want %d:\n%s", len(got), len(want), strings.Join(got, "\n---\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("statement %d:\ngot:\n%s\nwant:\n%s", i, got[i], want[i])
		}
	}
}

func TestCertificateBackupRemoveKeyAndOwner(t *testing.T) {
	d := (&Server{}).DatabaseRef("AppDB")
	c := d.CertificateRef("c]1")
	got := capture(t,
		func(ctx context.Context) error {
			return c.Backup(ctx, CertificateBackupSpec{File: `C:\b\it's.cer`})
		},
		func(ctx context.Context) error {
			return c.Backup(ctx, CertificateBackupSpec{File: `C:\b\c.cer`,
				PrivateKeyFile: `C:\b\c.pvk`, EncryptionPassword: "enc"})
		},
		func(ctx context.Context) error {
			return c.Backup(ctx, CertificateBackupSpec{File: `C:\b\c.cer`,
				PrivateKeyFile: `C:\b\c.pvk`, EncryptionPassword: "enc", DecryptionPassword: "d'ec"})
		},
		c.RemovePrivateKey,
		func(ctx context.Context) error { return c.ChangeOwner(ctx, "u]1") },
	)
	assertStatements(t, got, []string{
		useAppDB + `BACKUP CERTIFICATE [c]]1] TO FILE = N'C:\b\it''s.cer'`,
		useAppDB + `BACKUP CERTIFICATE [c]]1] TO FILE = N'C:\b\c.cer' WITH PRIVATE KEY (FILE = N'C:\b\c.pvk', ENCRYPTION BY PASSWORD = N'enc')`,
		useAppDB + `BACKUP CERTIFICATE [c]]1] TO FILE = N'C:\b\c.cer' WITH PRIVATE KEY (FILE = N'C:\b\c.pvk', ENCRYPTION BY PASSWORD = N'enc', DECRYPTION BY PASSWORD = N'd''ec')`,
		useAppDB + "ALTER CERTIFICATE [c]]1] REMOVE PRIVATE KEY",
		useAppDB + "ALTER AUTHORIZATION ON CERTIFICATE::[c]]1] TO [u]]1]",
	})
	// Nothing ran, so the handle must not claim the key is gone or the owner
	// changed.
	if c.PvtKeyEncryptionType != "" || c.Owner != "" {
		t.Errorf("a scripted write mirrored onto the handle: %+v", c)
	}
}

func TestCertificateBackupRejects(t *testing.T) {
	c := (&Server{}).DatabaseRef("AppDB").CertificateRef("c")
	for _, spec := range []CertificateBackupSpec{
		{},
		{File: "f", PrivateKeyFile: "p"},
		{File: "f", EncryptionPassword: "e"},
		{File: "f", DecryptionPassword: "d"},
	} {
		if _, err := c.backupStatement(spec); err == nil {
			t.Errorf("%+v: accepted", spec)
		}
	}
}

func TestAsymmetricAndSymmetricKeyOwnerAndPrivateKey(t *testing.T) {
	d := (&Server{}).DatabaseRef("AppDB")
	a, s := d.AsymmetricKeyRef("a"), d.SymmetricKeyRef("s")
	got := capture(t,
		a.RemovePrivateKey,
		func(ctx context.Context) error { return a.ChangeOwner(ctx, "u") },
		func(ctx context.Context) error { return s.ChangeOwner(ctx, "u") },
	)
	assertStatements(t, got, []string{
		useAppDB + "ALTER ASYMMETRIC KEY [a] REMOVE PRIVATE KEY",
		useAppDB + "ALTER AUTHORIZATION ON ASYMMETRIC KEY::[a] TO [u]",
		useAppDB + "ALTER AUTHORIZATION ON SYMMETRIC KEY::[s] TO [u]",
	})
}

func TestProviderKeyStatements(t *testing.T) {
	prov := func(d ProviderKeyDisposition) *ProviderKey {
		return &ProviderKey{Provider: "EKM]P", KeyName: "k'1", Disposition: d}
	}
	for _, tc := range []struct {
		got  func() (string, error)
		want string
	}{
		{func() (string, error) {
			return CreateAsymmetricKeyRequest{Name: "a", Algorithm: AsymmetricKeyRSA2048, FromProvider: prov(ProviderCreateNew)}.createAsymmetricKeyStatement()
		}, "CREATE ASYMMETRIC KEY [a] FROM PROVIDER [EKM]]P] WITH ALGORITHM = RSA_2048, PROVIDER_KEY_NAME = N'k''1', CREATION_DISPOSITION = CREATE_NEW"},
		{func() (string, error) {
			return CreateAsymmetricKeyRequest{Name: "a", Authorization: "o", FromProvider: prov(ProviderOpenExisting)}.createAsymmetricKeyStatement()
		}, "CREATE ASYMMETRIC KEY [a] AUTHORIZATION [o] FROM PROVIDER [EKM]]P] WITH PROVIDER_KEY_NAME = N'k''1', CREATION_DISPOSITION = OPEN_EXISTING"},
		{func() (string, error) {
			return CreateSymmetricKeyRequest{Name: "s", Algorithm: SymmetricKeyAES256, FromProvider: prov("")}.createSymmetricKeyStatement()
		}, "CREATE SYMMETRIC KEY [s] FROM PROVIDER [EKM]]P] WITH ALGORITHM = AES_256, PROVIDER_KEY_NAME = N'k''1'"},
		{func() (string, error) {
			return CreateSymmetricKeyRequest{Name: "s", FromProvider: prov(ProviderOpenExisting)}.createSymmetricKeyStatement()
		}, "CREATE SYMMETRIC KEY [s] FROM PROVIDER [EKM]]P] WITH PROVIDER_KEY_NAME = N'k''1', CREATION_DISPOSITION = OPEN_EXISTING"},
	} {
		got, err := tc.got()
		if err != nil {
			t.Errorf("%s: %v", tc.want, err)
			continue
		}
		if got != tc.want {
			t.Errorf("got:\n%s\nwant:\n%s", got, tc.want)
		}
	}
}

func TestProviderKeyStatementsReject(t *testing.T) {
	p := &ProviderKey{Provider: "P", KeyName: "k"}
	for name, f := range map[string]func() (string, error){
		"asym with password": func() (string, error) {
			return CreateAsymmetricKeyRequest{Name: "a", Algorithm: AsymmetricKeyRSA2048, EncryptionPassword: "x", FromProvider: p}.createAsymmetricKeyStatement()
		},
		"asym CREATE_NEW without algorithm": func() (string, error) {
			return CreateAsymmetricKeyRequest{Name: "a", FromProvider: p}.createAsymmetricKeyStatement()
		},
		"asym no provider": func() (string, error) {
			return CreateAsymmetricKeyRequest{Name: "a", Algorithm: AsymmetricKeyRSA2048, FromProvider: &ProviderKey{KeyName: "k"}}.createAsymmetricKeyStatement()
		},
		"asym no key name": func() (string, error) {
			return CreateAsymmetricKeyRequest{Name: "a", Algorithm: AsymmetricKeyRSA2048, FromProvider: &ProviderKey{Provider: "P"}}.createAsymmetricKeyStatement()
		},
		"asym bad disposition": func() (string, error) {
			return CreateAsymmetricKeyRequest{Name: "a", Algorithm: AsymmetricKeyRSA2048, FromProvider: &ProviderKey{Provider: "P", KeyName: "k", Disposition: "X; DROP"}}.createAsymmetricKeyStatement()
		},
		"sym with encryption": func() (string, error) {
			return CreateSymmetricKeyRequest{Name: "s", Algorithm: SymmetricKeyAES256, FromProvider: p,
				Encryptions: []SymmetricKeyEncryptor{{Kind: SymmetricKeyByPassword, Password: "x"}}}.createSymmetricKeyStatement()
		},
		"sym with key source": func() (string, error) {
			return CreateSymmetricKeyRequest{Name: "s", Algorithm: SymmetricKeyAES256, FromProvider: p, KeySource: "k"}.createSymmetricKeyStatement()
		},
	} {
		if s, err := f(); err == nil {
			t.Errorf("%s: accepted as %s", name, s)
		}
	}
}

func TestMasterKeyWrites(t *testing.T) {
	m := (&Server{}).DatabaseRef("AppDB").MasterKeyRef()
	opened := func(stmt string) string {
		return useAppDB + "BEGIN TRY\nOPEN MASTER KEY DECRYPTION BY PASSWORD = N'op';\n" + stmt + ";\nCLOSE MASTER KEY;\n" +
			"END TRY\nBEGIN CATCH\n" +
			"IF EXISTS (SELECT 1 FROM sys.openkeys WHERE database_id = DB_ID() AND key_name = N'##MS_DatabaseMasterKey##')\n" +
			"    CLOSE MASTER KEY;\nTHROW;\nEND CATCH;"
	}
	got := capture(t,
		func(ctx context.Context) error { return m.Regenerate(ctx, "new", false, "") },
		func(ctx context.Context) error { return m.Regenerate(ctx, "new", true, "op") },
		func(ctx context.Context) error {
			return m.AddEncryption(ctx, MasterKeyEncryptor{ServiceMasterKey: true}, "op")
		},
		func(ctx context.Context) error {
			return m.DropEncryption(ctx, MasterKeyEncryptor{ServiceMasterKey: true}, "")
		},
		func(ctx context.Context) error {
			return m.AddEncryption(ctx, MasterKeyEncryptor{Password: "p'2"}, "")
		},
		func(ctx context.Context) error {
			return m.DropEncryption(ctx, MasterKeyEncryptor{Password: "p2"}, "")
		},
		func(ctx context.Context) error { return m.Backup(ctx, `C:\b\dmk.key`, "enc", "") },
		m.Drop,
	)
	assertStatements(t, got, []string{
		useAppDB + "ALTER MASTER KEY REGENERATE WITH ENCRYPTION BY PASSWORD = N'new'",
		opened("ALTER MASTER KEY FORCE REGENERATE WITH ENCRYPTION BY PASSWORD = N'new'"),
		opened("ALTER MASTER KEY ADD ENCRYPTION BY SERVICE MASTER KEY"),
		useAppDB + "ALTER MASTER KEY DROP ENCRYPTION BY SERVICE MASTER KEY",
		useAppDB + "ALTER MASTER KEY ADD ENCRYPTION BY PASSWORD = N'p''2'",
		useAppDB + "ALTER MASTER KEY DROP ENCRYPTION BY PASSWORD = N'p2'",
		useAppDB + `BACKUP MASTER KEY TO FILE = N'C:\b\dmk.key' ENCRYPTION BY PASSWORD = N'enc'`,
		useAppDB + "DROP MASTER KEY",
	})
}

func TestMasterKeyWritesReject(t *testing.T) {
	ctx, col := WithScript(context.Background())
	m := (&Server{}).DatabaseRef("AppDB").MasterKeyRef()
	for _, err := range []error{
		m.Regenerate(ctx, "", false, ""),
		m.AddEncryption(ctx, MasterKeyEncryptor{}, ""),
		m.AddEncryption(ctx, MasterKeyEncryptor{ServiceMasterKey: true, Password: "p"}, ""),
		m.DropEncryption(ctx, MasterKeyEncryptor{}, ""),
		m.Backup(ctx, "", "enc", ""),
		m.Backup(ctx, "f", "", ""),
	} {
		if err == nil {
			t.Error("an invalid master key write was accepted")
		}
	}
	if len(col.Statements()) != 0 {
		t.Errorf("a refused write still sent: %q", col.Statements())
	}
}

func TestSignatureKind(t *testing.T) {
	for desc, want := range map[string]struct {
		kind    SignerKind
		counter bool
	}{
		"SIGNATURE BY CERTIFICATE":            {SignerCertificate, false},
		"SIGNATURE BY ASYMMETRIC KEY":         {SignerAsymmetricKey, false},
		"COUNTER SIGNATURE BY CERTIFICATE":    {SignerCertificate, true},
		"COUNTER SIGNATURE BY ASYMMETRIC KEY": {SignerAsymmetricKey, true},
		"SIGNATURE BY SOMETHING NEW":          {"", false},
		"":                                    {"", false},
	} {
		kind, counter := signatureKind(desc)
		if kind != want.kind || counter != want.counter {
			t.Errorf("%q: got (%q, %v), want (%q, %v)", desc, kind, counter, want.kind, want.counter)
		}
	}
}

func TestSignatureWrites(t *testing.T) {
	d := (&Server{}).DatabaseRef("AppDB")
	cert := Signer{Kind: SignerCertificate, Name: "c", Password: "p'w"}
	asym := Signer{Kind: SignerAsymmetricKey, Name: "a"}
	got := capture(t,
		func(ctx context.Context) error { return d.AddSignature(ctx, "dbo", "p]1", cert, false) },
		func(ctx context.Context) error { return d.AddSignature(ctx, "dbo", "f", asym, true) },
		// DROP takes no password, even when the signer has one.
		func(ctx context.Context) error { return d.DropSignature(ctx, "dbo", "p]1", cert, false) },
		func(ctx context.Context) error { return d.DropSignature(ctx, "dbo", "f", asym, true) },
	)
	assertStatements(t, got, []string{
		useAppDB + "ADD SIGNATURE TO [dbo].[p]]1] BY CERTIFICATE [c] WITH PASSWORD = N'p''w'",
		useAppDB + "ADD COUNTER SIGNATURE TO [dbo].[f] BY ASYMMETRIC KEY [a]",
		useAppDB + "DROP SIGNATURE FROM [dbo].[p]]1] BY CERTIFICATE [c]",
		useAppDB + "DROP COUNTER SIGNATURE FROM [dbo].[f] BY ASYMMETRIC KEY [a]",
	})

	ctx, col := WithScript(context.Background())
	for _, err := range []error{
		d.AddSignature(ctx, "dbo", "", cert, false),
		d.AddSignature(ctx, "dbo", "p", Signer{Kind: "SYMMETRIC KEY", Name: "s"}, false),
		d.AddSignature(ctx, "dbo", "p", Signer{Kind: SignerCertificate}, false),
	} {
		if err == nil {
			t.Error("an invalid signature write was accepted")
		}
	}
	if len(col.Statements()) != 0 {
		t.Errorf("a refused write still sent: %q", col.Statements())
	}
}
