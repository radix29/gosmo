package gosmo

import (
	"strings"
	"testing"
	"time"
)

func TestCreateCertificateStatement(t *testing.T) {
	tests := []struct {
		name string
		spec CreateCertificateRequest
		want string
	}{
		{"generated", CreateCertificateRequest{Name: "ubusql1_Cert", Subject: "gossms endpoint"},
			"CREATE CERTIFICATE [ubusql1_Cert] WITH SUBJECT = N'gossms endpoint'"},
		{"generated with dates", CreateCertificateRequest{
			Name:       "c",
			Subject:    "s",
			StartDate:  time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
			ExpiryDate: time.Date(2036, 8, 11, 0, 0, 0, 0, time.UTC),
		}, "CREATE CERTIFICATE [c] WITH SUBJECT = N's', START_DATE = N'20260811', EXPIRY_DATE = N'20360811'"},
		// The password goes before WITH SUBJECT — the other order is a syntax
		// error, and it is the order the grammar documents.
		{"password protected", CreateCertificateRequest{Name: "c", Subject: "s", EncryptionPassword: "p'w"},
			"CREATE CERTIFICATE [c] ENCRYPTION BY PASSWORD = N'p''w' WITH SUBJECT = N's'"},
		{"imported", CreateCertificateRequest{Name: "ubusql2_Cert", FromBinary: []byte{0x30, 0x82, 0x01, 0xab}},
			"CREATE CERTIFICATE [ubusql2_Cert] FROM BINARY = 0x308201AB"},
		{"imported with an owner", CreateCertificateRequest{
			Name: "ubusql2_Cert", Authorization: "ubusql2_user", FromBinary: []byte{0xde, 0xad},
		}, "CREATE CERTIFICATE [ubusql2_Cert] AUTHORIZATION [ubusql2_user] FROM BINARY = 0xDEAD"},
		{"quoted name", CreateCertificateRequest{Name: "we[i]rd", Subject: "s"},
			"CREATE CERTIFICATE [we[i]]rd] WITH SUBJECT = N's'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.spec.createCertificateStatement()
			if err != nil {
				t.Fatalf("createCertificateStatement() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("createCertificateStatement()\n got %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestCreateCertificateStatementRejects(t *testing.T) {
	tests := []struct {
		name string
		spec CreateCertificateRequest
	}{
		{"no name", CreateCertificateRequest{Subject: "s"}},
		{"no origin", CreateCertificateRequest{Name: "c"}},
		{"both origins", CreateCertificateRequest{Name: "c", Subject: "s", FromBinary: []byte{1}}},
		// An imported certificate has no private key to protect and no
		// validity of its own to set; silently dropping either would produce a
		// statement that does less than it was asked for.
		{"imported with a password", CreateCertificateRequest{Name: "c", FromBinary: []byte{1}, EncryptionPassword: "p"}},
		{"imported with dates", CreateCertificateRequest{
			Name: "c", FromBinary: []byte{1}, ExpiryDate: time.Now(),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.spec.createCertificateStatement(); err == nil {
				t.Errorf("accepted %s", tt.name)
			}
		})
	}
}

func TestCertificateHasPrivateKey(t *testing.T) {
	// The distinction the endpoint flow turns on: an instance can present only
	// a certificate it holds the private key for, and imports its peers'
	// public certificates alone.
	own := &Certificate{PvtKeyEncryptionType: "ENCRYPTED_BY_MASTER_KEY"}
	if !own.HasPrivateKey() {
		t.Error("a master-key-encrypted certificate reports no private key")
	}
	imported := &Certificate{PvtKeyEncryptionType: "NO_PRIVATE_KEY"}
	if imported.HasPrivateKey() {
		t.Error("an imported public certificate reports a private key")
	}
	// An unreported value is not evidence of a private key either.
	if (&Certificate{}).HasPrivateKey() {
		t.Error("a certificate with no reported encryption type reports a private key")
	}
}

func TestCreateCertificateFromBinaryIsUppercaseHex(t *testing.T) {
	// 0x literals are the only varbinary form T-SQL accepts, and the round
	// trip through a query editor is a lot easier to eyeball in one case.
	spec := CreateCertificateRequest{Name: "c", FromBinary: []byte{0x00, 0x0f, 0xff}}
	got, err := spec.createCertificateStatement()
	if err != nil {
		t.Fatalf("createCertificateStatement() error = %v", err)
	}
	if !strings.HasSuffix(got, "FROM BINARY = 0x000FFF") {
		t.Errorf("statement = %s, want it to end with an uppercase 0x literal", got)
	}
}

func TestBuildCertificateScript(t *testing.T) {
	c := &Certificate{
		Name: "o'cert", Owner: "cert_owner",
		PvtKeyEncryptionType: "ENCRYPTED_BY_MASTER_KEY", IsActiveForBeginDialog: true,
	}
	enc := []byte{0x30, 0x82, 0x01, 0xab}
	tests := []struct {
		name string
		c    *Certificate
		opts ScriptOptions
		want string
	}{
		{"create", c, ScriptOptions{Verb: ScriptCreate},
			"/* The certificate's private key cannot be read from the server, so it is\n" +
				"   not scripted: this recreates the public certificate only, which can\n" +
				"   verify signatures and encrypt, but not sign or decrypt. */\n" +
				"CREATE CERTIFICATE [o'cert] AUTHORIZATION [cert_owner]\n" +
				"    FROM BINARY = 0x308201AB;\nGO\n"},
		{"drop", c, ScriptOptions{Verb: ScriptDrop},
			"IF EXISTS (SELECT 1 FROM sys.certificates WHERE name = N'o''cert')\n" +
				"    DROP CERTIFICATE [o'cert];\nGO\n"},
		// An imported public certificate has no private key to lose, so the
		// comment would be wrong; a certificate switched off for Service
		// Broker must stay off, since ON is the default.
		{"public only, inactive, if not exists", &Certificate{Name: "peer",
			PvtKeyEncryptionType: "NO_PRIVATE_KEY"},
			ScriptOptions{Verb: ScriptCreate, IncludeIfNotExists: true},
			"IF NOT EXISTS (SELECT 1 FROM sys.certificates WHERE name = N'peer')\n" +
				"CREATE CERTIFICATE [peer]\n" +
				"    FROM BINARY = 0x308201AB\n" +
				"    ACTIVE FOR BEGIN_DIALOG = OFF;\nGO\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildCertificateScript(tt.c, enc, tt.opts); got != tt.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestBuildCertificateScriptDropAndCreate(t *testing.T) {
	c := &Certificate{Name: "c", IsActiveForBeginDialog: true}
	got := buildCertificateScript(c, []byte{1}, ScriptOptions{Verb: ScriptDropAndCreate})
	drop := strings.Index(got, "DROP CERTIFICATE [c]")
	create := strings.Index(got, "CREATE CERTIFICATE [c]")
	if drop < 0 || create < 0 || drop > create {
		t.Errorf("want DROP then CREATE:\n%s", got)
	}
}
