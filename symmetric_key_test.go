package gosmo

import (
	"context"
	"strings"
	"testing"
)

// The crypt_type code for one encryption differs by major (EPUC on 13 and 14,
// C256 on 17; ESKP on 13, ESP2 later), so classification goes by the
// description's prefix. Every description step 0 of the keys plan saw on
// 13, 14, 17 and Managed Instance is here.
func TestSymmetricKeyEncryptionKind(t *testing.T) {
	for desc, want := range map[string]SymmetricKeyEncryptionKind{
		"ENCRYPTION BY CERTIFICATE":            SymmetricKeyByCertificate,
		"ENCRYPTION BY CERTIFICATE OAEP256":    SymmetricKeyByCertificate,
		"ENCRYPTION BY ASYMMETRIC KEY":         SymmetricKeyByAsymmetricKey,
		"ENCRYPTION BY ASYMMETRIC KEY OAEP256": SymmetricKeyByAsymmetricKey,
		"ENCRYPTION BY SYMMETRIC KEY":          SymmetricKeyBySymmetricKey,
		"ENCRYPTION BY PASSWORD":               SymmetricKeyByPassword,
		"ENCRYPTION BY PASSWORD V2":            SymmetricKeyByPassword,
		"ENCRYPTION BY MASTER KEY":             SymmetricKeyByMasterKey,
		"ENCRYPTION BY SOMETHING NEW":          "",
		"":                                     "",
	} {
		if got := symmetricKeyEncryptionKind(desc); got != want {
			t.Errorf("%q: got %q, want %q", desc, got, want)
		}
	}
}

func TestSortSymmetricKeyEncryptions(t *testing.T) {
	es := []SymmetricKeyEncryption{
		{Kind: SymmetricKeyByAsymmetricKey, Name: "a"},
		{Kind: SymmetricKeyByPassword, Thumbprint: []byte{2}},
		{Kind: ""},
		{Kind: SymmetricKeyByCertificate, Name: "z"},
		{Kind: SymmetricKeyBySymmetricKey, Name: "s"},
		{Kind: SymmetricKeyByPassword, Thumbprint: []byte{1}},
		{Kind: SymmetricKeyByCertificate, Name: "b"},
	}
	sortSymmetricKeyEncryptions(es)
	var got []string
	for _, e := range es {
		got = append(got, string(e.Kind)+":"+e.Name+":"+string(e.Thumbprint))
	}
	want := []string{"CERTIFICATE:b:", "CERTIFICATE:z:", "PASSWORD::\x01", "PASSWORD::\x02",
		"SYMMETRIC KEY:s:", "ASYMMETRIC KEY:a:", "::"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

const symKeyNewKeyComment = "/* A symmetric key's material cannot be read from the server, so this\n" +
	"   creates a NEW key with the same algorithm and encryptions, not this key:\n" +
	"   data encrypted with the original will not decrypt with it. Only a key\n" +
	"   created with KEY_SOURCE and IDENTITY_VALUE can be recreated, by adding\n" +
	"   both to the WITH clause with their original values."

func TestBuildSymmetricKeyScript(t *testing.T) {
	k := &SymmetricKey{Name: "o'key", Owner: "key_owner", Algorithm: "AES_256",
		Encryptions: []SymmetricKeyEncryption{
			{Kind: SymmetricKeyByCertificate, Name: "c]1"},
			{Kind: SymmetricKeyByPassword},
			{Kind: SymmetricKeyByAsymmetricKey, Name: "ak"},
		}}
	tests := []struct {
		name string
		k    *SymmetricKey
		opts ScriptOptions
		want string
	}{
		{"create", k, ScriptOptions{Verb: ScriptCreate},
			symKeyNewKeyComment + " */\n" +
				"CREATE SYMMETRIC KEY [o'key] AUTHORIZATION [key_owner]\n" +
				"    WITH ALGORITHM = AES_256\n" +
				"    ENCRYPTION BY CERTIFICATE [c]]1],\n" +
				"        PASSWORD = N'<insert password here>',\n" +
				"        ASYMMETRIC KEY [ak];\nGO\n"},
		{"drop", k, ScriptOptions{Verb: ScriptDrop},
			"IF EXISTS (SELECT 1 FROM sys.symmetric_keys WHERE name = N'o''key')\n" +
				"    DROP SYMMETRIC KEY [o'key];\nGO\n"},
		{"drop and create, if not exists", &SymmetricKey{Name: "k", Algorithm: "AES_128",
			Encryptions: []SymmetricKeyEncryption{{Kind: SymmetricKeyByPassword}}},
			ScriptOptions{Verb: ScriptDropAndCreate, IncludeIfNotExists: true},
			"IF EXISTS (SELECT 1 FROM sys.symmetric_keys WHERE name = N'k')\n" +
				"    DROP SYMMETRIC KEY [k];\nGO\n\n" +
				symKeyNewKeyComment + " */\n" +
				"IF NOT EXISTS (SELECT 1 FROM sys.symmetric_keys WHERE name = N'k')\n" +
				"CREATE SYMMETRIC KEY [k]\n" +
				"    WITH ALGORITHM = AES_128\n" +
				"    ENCRYPTION BY PASSWORD = N'<insert password here>';\nGO\n"},
		// A symmetric-key encryptor must be open for the CREATE to run; an
		// encryptor the reader cannot see keeps its place as a placeholder.
		{"parent key, hidden certificate", &SymmetricKey{Name: "child", Algorithm: "AES_256",
			Encryptions: []SymmetricKeyEncryption{
				{Kind: SymmetricKeyByCertificate},
				{Kind: SymmetricKeyBySymmetricKey, Name: "parent"},
			}},
			ScriptOptions{Verb: ScriptCreate},
			symKeyNewKeyComment +
				"\n   [parent] must be open in this session first: OPEN SYMMETRIC KEY [parent] DECRYPTION BY <decryptor>. */\n" +
				"CREATE SYMMETRIC KEY [child]\n" +
				"    WITH ALGORITHM = AES_256\n" +
				"    ENCRYPTION BY CERTIFICATE <certificate name>,\n" +
				"        SYMMETRIC KEY [parent];\nGO\n"},
		// An algorithm outside the known set is not written unquoted into the
		// script, and a key with no readable encryption still gets one, as a
		// placeholder — CREATE SYMMETRIC KEY needs at least one.
		{"unknown algorithm, no encryption", &SymmetricKey{Name: "k", Algorithm: "AES_512; --"},
			ScriptOptions{Verb: ScriptCreate},
			symKeyNewKeyComment +
				"\n   No encryption of the original could be read; the one below is a placeholder. */\n" +
				"CREATE SYMMETRIC KEY [k]\n" +
				"    WITH ALGORITHM = <algorithm>\n" +
				"    ENCRYPTION BY PASSWORD = N'<insert password here>';\nGO\n"},
		{"EKM", &SymmetricKey{Name: "k", Algorithm: "AES_256", ProviderType: "CRYPTOGRAPHIC PROVIDER"},
			ScriptOptions{Verb: ScriptCreate},
			symKeyNewKeyComment +
				"\n   The original is held by an EKM provider (FROM PROVIDER); the result is not. */\n" +
				"CREATE SYMMETRIC KEY [k]\n" +
				"    WITH ALGORITHM = AES_256;\nGO\n"},
		{"unrecognised encryption", &SymmetricKey{Name: "k", Algorithm: "AES_256",
			Encryptions: []SymmetricKeyEncryption{{CryptTypeDesc: "ENCRYPTION BY SOMETHING NEW"}}},
			ScriptOptions{Verb: ScriptCreate},
			symKeyNewKeyComment + " */\n" +
				"CREATE SYMMETRIC KEY [k]\n" +
				"    WITH ALGORITHM = AES_256\n" +
				"    ENCRYPTION BY <ENCRYPTION BY SOMETHING NEW>;\nGO\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildSymmetricKeyScript(tt.k, tt.opts); got != tt.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestSymmetricKeyRef(t *testing.T) {
	d := (&Server{}).DatabaseRef("AppDB")
	k := d.SymmetricKeyRef("k")
	if k.Name != "k" || k.Database() != d || k.KeyID != 0 || k.Encryptions != nil {
		t.Errorf("SymmetricKeyRef(\"k\") = %+v", k)
	}
}

// catchClose is the guarded CLOSE a wrapped write's CATCH carries per key.
func catchClose(name string) string {
	return "IF EXISTS (SELECT 1 FROM sys.openkeys WHERE database_id = DB_ID() AND key_name = N'" +
		strings.ReplaceAll(name, "'", "''") + "')\n    CLOSE SYMMETRIC KEY [" + name + "];\n"
}

func TestCreateSymmetricKeyStatement(t *testing.T) {
	parent := SymmetricKeyEncryptor{Kind: SymmetricKeyBySymmetricKey, Name: "p'k",
		Open: &SymmetricKeyDecryptor{Kind: SymmetricKeyByCertificate, Name: "c", Password: "cp"}}
	tests := []struct {
		name string
		spec SymmetricKeySpec
		want string
	}{
		{"certificate", SymmetricKeySpec{Name: "k", Algorithm: SymmetricKeyAES256,
			Encryptions: []SymmetricKeyEncryptor{{Kind: SymmetricKeyByCertificate, Name: "c"}}},
			"CREATE SYMMETRIC KEY [k] WITH ALGORITHM = AES_256 ENCRYPTION BY CERTIFICATE [c]"},
		// Every option and every non-symmetric encryptor, in the order given;
		// the secrets are escaped literals.
		{"all options", SymmetricKeySpec{Name: "k", Authorization: "o]wner", Algorithm: SymmetricKeyAES128,
			KeySource: "s'rc", IdentityValue: "id",
			Encryptions: []SymmetricKeyEncryptor{
				{Kind: SymmetricKeyByPassword, Password: "p'w"},
				{Kind: SymmetricKeyByAsymmetricKey, Name: "a"},
				{Kind: SymmetricKeyByCertificate, Name: "c", Password: "ignored"},
			}},
			"CREATE SYMMETRIC KEY [k] AUTHORIZATION [o]]wner] WITH ALGORITHM = AES_128, " +
				"KEY_SOURCE = N's''rc', IDENTITY_VALUE = N'id' " +
				"ENCRYPTION BY PASSWORD = N'p''w', ASYMMETRIC KEY [a], CERTIFICATE [c]"},
		{"identity value alone", SymmetricKeySpec{Name: "k", Algorithm: SymmetricKeyAES192, IdentityValue: "id",
			Encryptions: []SymmetricKeyEncryptor{{Kind: SymmetricKeyByPassword, Password: "pw"}}},
			"CREATE SYMMETRIC KEY [k] WITH ALGORITHM = AES_192, IDENTITY_VALUE = N'id' ENCRYPTION BY PASSWORD = N'pw'"},
		// A symmetric-key encryptor is opened first and closed after, with
		// the CATCH closing it too.
		{"by symmetric key", SymmetricKeySpec{Name: "k", Algorithm: SymmetricKeyAES256,
			Encryptions: []SymmetricKeyEncryptor{parent}},
			"BEGIN TRY\n" +
				"OPEN SYMMETRIC KEY [p'k] DECRYPTION BY CERTIFICATE [c] WITH PASSWORD = N'cp';\n" +
				"CREATE SYMMETRIC KEY [k] WITH ALGORITHM = AES_256 ENCRYPTION BY SYMMETRIC KEY [p'k];\n" +
				"CLOSE SYMMETRIC KEY [p'k];\n" +
				"END TRY\nBEGIN CATCH\n" + catchClose("p'k") + "THROW;\nEND CATCH;"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.spec.createSymmetricKeyStatement()
			if err != nil {
				t.Fatalf("createSymmetricKeyStatement() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

// The algorithm is written unquoted, a password encryptor needs a password, a
// symmetric-key encryptor needs a way to open it, and the master key never
// encrypts a symmetric key — each is refused before reaching the server.
func TestCreateSymmetricKeyStatementRejects(t *testing.T) {
	pw := []SymmetricKeyEncryptor{{Kind: SymmetricKeyByPassword, Password: "pw"}}
	cyclic := &SymmetricKeyDecryptor{Kind: SymmetricKeyBySymmetricKey, Name: "a"}
	cyclic.Open = &SymmetricKeyDecryptor{Kind: SymmetricKeyBySymmetricKey, Name: "b", Open: cyclic}
	for _, spec := range []SymmetricKeySpec{
		{Name: " ", Algorithm: SymmetricKeyAES256, Encryptions: pw},
		{Name: "k", Encryptions: pw},
		{Name: "k", Algorithm: "aes_256", Encryptions: pw},
		{Name: "k", Algorithm: "AES_256; DROP TABLE t", Encryptions: pw},
		{Name: "k", Algorithm: SymmetricKeyAES256},
		{Name: "k", Algorithm: SymmetricKeyAES256, Encryptions: []SymmetricKeyEncryptor{{Kind: SymmetricKeyByPassword}}},
		{Name: "k", Algorithm: SymmetricKeyAES256, Encryptions: []SymmetricKeyEncryptor{{Kind: SymmetricKeyByCertificate}}},
		{Name: "k", Algorithm: SymmetricKeyAES256, Encryptions: []SymmetricKeyEncryptor{{Kind: SymmetricKeyByMasterKey}}},
		{Name: "k", Algorithm: SymmetricKeyAES256, Encryptions: []SymmetricKeyEncryptor{{Kind: ""}}},
		{Name: "k", Algorithm: SymmetricKeyAES256, Encryptions: []SymmetricKeyEncryptor{{Kind: SymmetricKeyBySymmetricKey, Name: "p"}}},
		{Name: "k", Algorithm: SymmetricKeyAES256, Encryptions: []SymmetricKeyEncryptor{{Kind: SymmetricKeyBySymmetricKey, Name: "p",
			Open: &SymmetricKeyDecryptor{Kind: SymmetricKeyByPassword}}}},
		{Name: "k", Algorithm: SymmetricKeyAES256, Encryptions: []SymmetricKeyEncryptor{{Kind: SymmetricKeyBySymmetricKey, Name: "p",
			Open: cyclic}}},
	} {
		if got, err := spec.createSymmetricKeyStatement(); err == nil {
			t.Errorf("%+v: got %q, want an error", spec, got)
		}
	}
}

func TestSymmetricKeyWritesUnderScript(t *testing.T) {
	ctx, col := WithScript(context.Background())
	d := (&Server{}).DatabaseRef("AppDB")
	k := d.SymmetricKeyRef("k")
	byPassword := SymmetricKeyDecryptor{Kind: SymmetricKeyByPassword, Password: "old"}
	// The parent is opened by another symmetric key in turn: the chain opens
	// outermost first and closes innermost first.
	grand := &SymmetricKeyDecryptor{Kind: SymmetricKeyByAsymmetricKey, Name: "a"}
	bySym := SymmetricKeyEncryptor{Kind: SymmetricKeyBySymmetricKey, Name: "p",
		Open: &SymmetricKeyDecryptor{Kind: SymmetricKeyBySymmetricKey, Name: "g", Open: grand}}

	for _, w := range []func() error{
		func() error {
			return d.CreateSymmetricKey(ctx, SymmetricKeySpec{Name: "k", Algorithm: SymmetricKeyAES256,
				Encryptions: []SymmetricKeyEncryptor{{Kind: SymmetricKeyByPassword, Password: "old"}}})
		},
		func() error {
			return k.AddEncryption(ctx, SymmetricKeyEncryptor{Kind: SymmetricKeyByCertificate, Name: "c"}, byPassword)
		},
		func() error { return k.AddEncryption(ctx, bySym, byPassword) },
		// The key opened by the encryption being removed.
		func() error {
			return k.DropEncryption(ctx, SymmetricKeyEncryptor{Kind: SymmetricKeyByPassword, Password: "old"}, byPassword)
		},
		func() error { return k.Drop(ctx) },
	} {
		if err := w(); err != nil {
			t.Fatal(err)
		}
	}
	openK := "OPEN SYMMETRIC KEY [k] DECRYPTION BY PASSWORD = N'old';\n"
	want := []string{
		useAppDB + "CREATE SYMMETRIC KEY [k] WITH ALGORITHM = AES_256 ENCRYPTION BY PASSWORD = N'old'",
		useAppDB + "BEGIN TRY\n" + openK +
			"ALTER SYMMETRIC KEY [k] ADD ENCRYPTION BY CERTIFICATE [c];\n" +
			"CLOSE SYMMETRIC KEY [k];\n" +
			"END TRY\nBEGIN CATCH\n" + catchClose("k") + "THROW;\nEND CATCH;",
		useAppDB + "BEGIN TRY\n" + openK +
			"OPEN SYMMETRIC KEY [g] DECRYPTION BY ASYMMETRIC KEY [a];\n" +
			"OPEN SYMMETRIC KEY [p] DECRYPTION BY SYMMETRIC KEY [g];\n" +
			"ALTER SYMMETRIC KEY [k] ADD ENCRYPTION BY SYMMETRIC KEY [p];\n" +
			"CLOSE SYMMETRIC KEY [p];\nCLOSE SYMMETRIC KEY [g];\nCLOSE SYMMETRIC KEY [k];\n" +
			"END TRY\nBEGIN CATCH\n" + catchClose("p") + catchClose("g") + catchClose("k") + "THROW;\nEND CATCH;",
		useAppDB + "BEGIN TRY\n" + openK +
			"ALTER SYMMETRIC KEY [k] DROP ENCRYPTION BY PASSWORD = N'old';\n" +
			"CLOSE SYMMETRIC KEY [k];\n" +
			"END TRY\nBEGIN CATCH\n" + catchClose("k") + "THROW;\nEND CATCH;",
		useAppDB + "DROP SYMMETRIC KEY [k]",
	}
	if len(col.Statements()) != len(want) {
		t.Fatalf("captured %d statements, want %d:\n%s", len(col.Statements()), len(want), strings.Join(col.Statements(), "\n---\n"))
	}
	for i := range want {
		if col.Statements()[i] != want[i] {
			t.Errorf("statement %d:\ngot:\n%s\nwant:\n%s", i, col.Statements()[i], want[i])
		}
	}
}

// A key cannot be altered without a way to open it, and a decryptor that
// cannot be rendered is refused before anything is sent.
func TestSymmetricKeyAlterEncryptionRejects(t *testing.T) {
	ctx, col := WithScript(context.Background())
	k := (&Server{}).DatabaseRef("AppDB").SymmetricKeyRef("k")
	cert := SymmetricKeyEncryptor{Kind: SymmetricKeyByCertificate, Name: "c"}
	for _, err := range []error{
		k.AddEncryption(ctx, cert, SymmetricKeyDecryptor{}),
		k.AddEncryption(ctx, cert, SymmetricKeyDecryptor{Kind: SymmetricKeyByMasterKey}),
		k.AddEncryption(ctx, cert, SymmetricKeyDecryptor{Kind: SymmetricKeyBySymmetricKey, Name: "p"}),
		k.DropEncryption(ctx, SymmetricKeyEncryptor{Kind: SymmetricKeyByPassword},
			SymmetricKeyDecryptor{Kind: SymmetricKeyByPassword, Password: "pw"}),
	} {
		if err == nil {
			t.Error("want an error")
		}
	}
	if len(col.Statements()) != 0 {
		t.Errorf("captured %q, want nothing", col.Statements())
	}
}
