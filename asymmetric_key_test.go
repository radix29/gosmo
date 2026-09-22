package gosmo

import (
	"context"
	"strings"
	"testing"
)

func TestCreateAsymmetricKeyStatement(t *testing.T) {
	tests := []struct {
		name string
		spec AsymmetricKeySpec
		want string
	}{
		{"master key protected", AsymmetricKeySpec{Name: "k", Algorithm: AsymmetricKeyRSA2048},
			"CREATE ASYMMETRIC KEY [k] WITH ALGORITHM = RSA_2048"},
		// The password goes after WITH ALGORITHM — the reverse of CREATE
		// CERTIFICATE's order, and the one the grammar documents.
		{"password protected, owned", AsymmetricKeySpec{Name: "k", Authorization: "o]wner",
			Algorithm: AsymmetricKeyRSA4096, EncryptionPassword: "p'w"},
			"CREATE ASYMMETRIC KEY [k] AUTHORIZATION [o]]wner] WITH ALGORITHM = RSA_4096 ENCRYPTION BY PASSWORD = N'p''w'"},
		{"quoted name", AsymmetricKeySpec{Name: "we[i]rd", Algorithm: AsymmetricKeyRSA3072},
			"CREATE ASYMMETRIC KEY [we[i]]rd] WITH ALGORITHM = RSA_3072"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.spec.createAsymmetricKeyStatement()
			if err != nil {
				t.Fatalf("createAsymmetricKeyStatement() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

// The algorithm is written into the statement unquoted, so anything outside
// the known set — including a well-meant lower-case spelling — is refused
// before it reaches the server.
func TestCreateAsymmetricKeyStatementRejects(t *testing.T) {
	for _, spec := range []AsymmetricKeySpec{
		{Name: " ", Algorithm: AsymmetricKeyRSA2048},
		{Name: "k"},
		{Name: "k", Algorithm: "rsa_2048"},
		{Name: "k", Algorithm: "RSA_2048; DROP TABLE t"},
	} {
		if got, err := spec.createAsymmetricKeyStatement(); err == nil {
			t.Errorf("%+v: got %q, want an error", spec, got)
		}
	}
}

func TestCreateAsymmetricKeyUnderScript(t *testing.T) {
	ctx, col := WithScript(context.Background())
	d := (&Server{}).DatabaseRef("AppDB")
	if err := d.CreateAsymmetricKeyContext(ctx, AsymmetricKeySpec{Name: "k", Algorithm: AsymmetricKeyRSA2048}); err != nil {
		t.Fatal(err)
	}
	want := useAppDB + "CREATE ASYMMETRIC KEY [k] WITH ALGORITHM = RSA_2048"
	if len(col.Statements) != 1 || col.Statements[0] != want {
		t.Errorf("got %q, want [%q]", col.Statements, want)
	}
}

const asymKeyNewPairComment = "/* An asymmetric key cannot be recreated from what the server exposes, so\n" +
	"   this creates a NEW key pair with the same algorithm, not this key: what\n" +
	"   the original signed or encrypted will not verify or decrypt with it."

func TestBuildAsymmetricKeyScript(t *testing.T) {
	k := &AsymmetricKey{Name: "o'key", Owner: "key_owner", Algorithm: "RSA_2048",
		PvtKeyEncryptionType: "ENCRYPTED_BY_MASTER_KEY"}
	tests := []struct {
		name string
		k    *AsymmetricKey
		opts ScriptOptions
		want string
	}{
		{"create", k, ScriptOptions{Verb: ScriptCreate},
			asymKeyNewPairComment + " */\n" +
				"CREATE ASYMMETRIC KEY [o'key] AUTHORIZATION [key_owner]\n" +
				"    WITH ALGORITHM = RSA_2048;\nGO\n"},
		{"drop", k, ScriptOptions{Verb: ScriptDrop},
			"IF EXISTS (SELECT 1 FROM sys.asymmetric_keys WHERE name = N'o''key')\n" +
				"    DROP ASYMMETRIC KEY [o'key];\nGO\n"},
		// A password-protected key keeps its protection, as a placeholder.
		{"password, if not exists", &AsymmetricKey{Name: "pk", Algorithm: "RSA_4096",
			PvtKeyEncryptionType: "ENCRYPTED_BY_PASSWORD"},
			ScriptOptions{Verb: ScriptCreate, IncludeIfNotExists: true},
			asymKeyNewPairComment + " */\n" +
				"IF NOT EXISTS (SELECT 1 FROM sys.asymmetric_keys WHERE name = N'pk')\n" +
				"CREATE ASYMMETRIC KEY [pk]\n" +
				"    WITH ALGORITHM = RSA_4096\n" +
				"    ENCRYPTION BY PASSWORD = N'<insert password here>';\nGO\n"},
		// A public-only EKM key: both differences are said, and an algorithm
		// WITH ALGORITHM does not take is left to the reader.
		{"public only, provider", &AsymmetricKey{Name: "ekm", Algorithm: "RSA_OAEP",
			PvtKeyEncryptionType: "NO_PRIVATE_KEY", ProviderType: "CRYPTOGRAPHIC PROVIDER"},
			ScriptOptions{Verb: ScriptCreate},
			asymKeyNewPairComment + "\n" +
				"   The original holds only a public key; the result has a private key too.\n" +
				"   The original is held by an EKM provider (FROM PROVIDER); the result is not. */\n" +
				"CREATE ASYMMETRIC KEY [ekm]\n" +
				"    WITH ALGORITHM = <algorithm>;\nGO\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildAsymmetricKeyScript(tt.k, tt.opts); got != tt.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestBuildAsymmetricKeyScriptDropAndCreate(t *testing.T) {
	k := &AsymmetricKey{Name: "k", Algorithm: "RSA_2048", PvtKeyEncryptionType: "ENCRYPTED_BY_MASTER_KEY"}
	got := buildAsymmetricKeyScript(k, ScriptOptions{Verb: ScriptDropAndCreate})
	drop := strings.Index(got, "DROP ASYMMETRIC KEY [k]")
	create := strings.Index(got, "CREATE ASYMMETRIC KEY [k]")
	if drop < 0 || create < 0 || drop > create {
		t.Errorf("want DROP then CREATE:\n%s", got)
	}
}
