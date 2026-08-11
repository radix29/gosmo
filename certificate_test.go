package gosmo

import (
	"strings"
	"testing"
	"time"
)

func TestCreateCertificateStatement(t *testing.T) {
	tests := []struct {
		name string
		spec CertificateSpec
		want string
	}{
		{"generated", CertificateSpec{Name: "ubusql1_Cert", Subject: "gossms endpoint"},
			"CREATE CERTIFICATE [ubusql1_Cert] WITH SUBJECT = N'gossms endpoint'"},
		{"generated with dates", CertificateSpec{
			Name:       "c",
			Subject:    "s",
			StartDate:  time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
			ExpiryDate: time.Date(2036, 8, 11, 0, 0, 0, 0, time.UTC),
		}, "CREATE CERTIFICATE [c] WITH SUBJECT = N's', START_DATE = N'20260811', EXPIRY_DATE = N'20360811'"},
		// The password goes before WITH SUBJECT — the other order is a syntax
		// error, and it is the order the grammar documents.
		{"password protected", CertificateSpec{Name: "c", Subject: "s", EncryptionPassword: "p'w"},
			"CREATE CERTIFICATE [c] ENCRYPTION BY PASSWORD = N'p''w' WITH SUBJECT = N's'"},
		{"imported", CertificateSpec{Name: "ubusql2_Cert", FromBinary: []byte{0x30, 0x82, 0x01, 0xab}},
			"CREATE CERTIFICATE [ubusql2_Cert] FROM BINARY = 0x308201AB"},
		{"imported with an owner", CertificateSpec{
			Name: "ubusql2_Cert", Authorization: "ubusql2_user", FromBinary: []byte{0xde, 0xad},
		}, "CREATE CERTIFICATE [ubusql2_Cert] AUTHORIZATION [ubusql2_user] FROM BINARY = 0xDEAD"},
		{"quoted name", CertificateSpec{Name: "we[i]rd", Subject: "s"},
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
		spec CertificateSpec
	}{
		{"no name", CertificateSpec{Subject: "s"}},
		{"no origin", CertificateSpec{Name: "c"}},
		{"both origins", CertificateSpec{Name: "c", Subject: "s", FromBinary: []byte{1}}},
		// An imported certificate has no private key to protect and no
		// validity of its own to set; silently dropping either would produce a
		// statement that does less than it was asked for.
		{"imported with a password", CertificateSpec{Name: "c", FromBinary: []byte{1}, EncryptionPassword: "p"}},
		{"imported with dates", CertificateSpec{
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
	spec := CertificateSpec{Name: "c", FromBinary: []byte{0x00, 0x0f, 0xff}}
	got, err := spec.createCertificateStatement()
	if err != nil {
		t.Fatalf("createCertificateStatement() error = %v", err)
	}
	if !strings.HasSuffix(got, "FROM BINARY = 0x000FFF") {
		t.Errorf("statement = %s, want it to end with an uppercase 0x literal", got)
	}
}
