package gosmo

import (
	"context"
	"strings"
	"testing"
)

// secretWrite is one write carrying secrets, run against s.
type secretWrite struct {
	name    string
	secrets []string // every secret the write sends; each contains "s3cr3t"
	write   func(ctx context.Context, s *Server) error
}

// secretWrites is every write method that sends a password, credential
// secret or key source. A new one goes here: TestSecretsNeverCapturedOrObserved
// is what holds it to the redaction policy. A symmetric key's
// IDENTITY_VALUE counts: with its KEY_SOURCE it re-creates the key.
func secretWrites() []secretWrite {
	db := func(s *Server) *Database { return s.DatabaseRef("AppDB") }
	pw := func(n string) string { return "s3cr3t'" + n }
	return []secretWrite{
		{"CreateLogin", []string{pw("login")}, func(ctx context.Context, s *Server) error {
			return errOnly(s.CreateLogin(ctx, CreateLoginRequest{Name: "app", Password: pw("login")}))
		}},
		{"CreateLogin with defaults", []string{pw("login")}, func(ctx context.Context, s *Server) error {
			return errOnly(s.CreateLogin(ctx, CreateLoginRequest{Name: "app", Password: pw("login"),
				DefaultDatabase: "AppDB"}))
		}},
		{"Login.ChangePassword", []string{pw("change")}, func(ctx context.Context, s *Server) error {
			return s.LoginRef("app").ChangePassword(ctx, pw("change"), ChangePasswordOptions{Unlock: true})
		}},
		{"CreateCredential", []string{pw("cred")}, func(ctx context.Context, s *Server) error {
			return errOnly(s.CreateCredential(ctx, CreateCredentialRequest{Name: "c", Identity: "id", Secret: pw("cred")}))
		}},
		{"Credential.Alter", []string{pw("cred")}, func(ctx context.Context, s *Server) error {
			sec := pw("cred")
			return s.CredentialRef("c").Alter(ctx, CredentialOptions{Identity: "id", Secret: &sec})
		}},
		{"CreateDatabaseScopedCredential", []string{pw("dbcred")}, func(ctx context.Context, s *Server) error {
			return errOnly(db(s).CreateDatabaseScopedCredential(ctx, CreateDatabaseScopedCredentialRequest{
				Name: "c", Identity: "id", Secret: pw("dbcred")}))
		}},
		{"DatabaseScopedCredential.Alter", []string{pw("dbcred")}, func(ctx context.Context, s *Server) error {
			sec := pw("dbcred")
			return db(s).DatabaseScopedCredentialRef("c").Alter(ctx, CredentialOptions{Identity: "id", Secret: &sec})
		}},
		{"CreateUser with password", []string{pw("user")}, func(ctx context.Context, s *Server) error {
			return errOnly(db(s).CreateUser(ctx, CreateUserRequest{Name: "u", Kind: UserWithPassword, Password: pw("user")}))
		}},
		{"CreateCertificate", []string{pw("cert")}, func(ctx context.Context, s *Server) error {
			return errOnly(db(s).CreateCertificate(ctx, CreateCertificateRequest{Name: "c", Subject: "s",
				EncryptionPassword: pw("cert")}))
		}},
		{"Certificate.Backup", []string{pw("enc"), pw("dec")}, func(ctx context.Context, s *Server) error {
			return db(s).CertificateRef("c").Backup(ctx, CertificateBackupSpec{File: "f", PrivateKeyFile: "k",
				EncryptionPassword: pw("enc"), DecryptionPassword: pw("dec")})
		}},
		{"CreateAsymmetricKey", []string{pw("asym")}, func(ctx context.Context, s *Server) error {
			return errOnly(db(s).CreateAsymmetricKey(ctx, CreateAsymmetricKeyRequest{Name: "k",
				Algorithm: AsymmetricKeyRSA2048, EncryptionPassword: pw("asym")}))
		}},
		{"CreateMasterKey", []string{pw("dmk")}, func(ctx context.Context, s *Server) error {
			return errOnly(db(s).CreateMasterKey(ctx, CreateMasterKeyRequest{Password: pw("dmk")}))
		}},
		{"MasterKey.Regenerate", []string{pw("new"), pw("open")}, func(ctx context.Context, s *Server) error {
			return db(s).MasterKeyRef().Regenerate(ctx, pw("new"), false, pw("open"))
		}},
		{"MasterKey.AddEncryption", []string{pw("add"), pw("open")}, func(ctx context.Context, s *Server) error {
			return db(s).MasterKeyRef().AddEncryption(ctx, MasterKeyEncryptor{Password: pw("add")}, pw("open"))
		}},
		{"MasterKey.DropEncryption", []string{pw("drop")}, func(ctx context.Context, s *Server) error {
			return db(s).MasterKeyRef().DropEncryption(ctx, MasterKeyEncryptor{Password: pw("drop")}, "")
		}},
		{"MasterKey.Backup", []string{pw("enc"), pw("open")}, func(ctx context.Context, s *Server) error {
			return db(s).MasterKeyRef().Backup(ctx, "f", pw("enc"), pw("open"))
		}},
		{"CreateSymmetricKey", []string{pw("sym"), pw("src"), pw("id"), pw("parent")}, func(ctx context.Context, s *Server) error {
			return errOnly(db(s).CreateSymmetricKey(ctx, CreateSymmetricKeyRequest{Name: "k", Algorithm: SymmetricKeyAES256,
				KeySource: pw("src"), IdentityValue: pw("id"),
				Encryptions: []SymmetricKeyEncryptor{
					{Kind: SymmetricKeyByPassword, Password: pw("sym")},
					{Kind: SymmetricKeyBySymmetricKey, Name: "p",
						Open: &SymmetricKeyDecryptor{Kind: SymmetricKeyByPassword, Password: pw("parent")}},
				}}))
		}},
		{"SymmetricKey.AddEncryption", []string{pw("add"), pw("cert")}, func(ctx context.Context, s *Server) error {
			return db(s).SymmetricKeyRef("k").AddEncryption(ctx,
				SymmetricKeyEncryptor{Kind: SymmetricKeyByPassword, Password: pw("add")},
				SymmetricKeyDecryptor{Kind: SymmetricKeyBySymmetricKey, Name: "g",
					Open: &SymmetricKeyDecryptor{Kind: SymmetricKeyByCertificate, Name: "c", Password: pw("cert")}})
		}},
		{"SymmetricKey.DropEncryption", []string{pw("drop"), pw("open")}, func(ctx context.Context, s *Server) error {
			return db(s).SymmetricKeyRef("k").DropEncryption(ctx,
				SymmetricKeyEncryptor{Kind: SymmetricKeyByPassword, Password: pw("drop")},
				SymmetricKeyDecryptor{Kind: SymmetricKeyByPassword, Password: pw("open")})
		}},
		{"AddSignature", []string{pw("sig")}, func(ctx context.Context, s *Server) error {
			return db(s).AddSignature(ctx, "dbo", "p", Signer{Kind: SignerCertificate, Name: "c", Password: pw("sig")}, false)
		}},
		{"CreateMailAccount", []string{pw("mail")}, func(ctx context.Context, s *Server) error {
			return errOnly(s.CreateMailAccount(ctx, CreateMailAccountRequest{Name: "a", EmailAddress: "a@b", ServerName: "x",
				Credentials: MailCredentials{Authentication: MailAuthBasic, UserName: "u", Password: pw("mail")}}))
		}},
	}
}

// A secret reaches the server and nothing else: a WithScript capture and a
// statement observer see a placeholder in its place. WithScriptSecrets puts
// it back in the capture, and only there.
func TestSecretsNeverCapturedOrObserved(t *testing.T) {
	// Azure SQL Database takes contained users without the containment read
	// CreateUser otherwise makes — reads go to the server even under
	// WithScript, and these servers have none.
	azure := &ServerInfo{EngineEdition: int(EngineAzureSQLDatabase)}
	for _, w := range secretWrites() {
		t.Run(w.name, func(t *testing.T) {
			// Captured: redacted by default.
			ctx, col := WithScript(context.Background())
			if err := w.write(ctx, &Server{info: azure}); err != nil {
				t.Fatalf("under WithScript: %v", err)
			}
			script := col.String()
			if strings.Contains(script, "s3cr3t") {
				t.Errorf("captured script carries a secret:\n%s", script)
			}
			if !strings.Contains(script, "<insert ") {
				t.Errorf("captured script has no placeholder:\n%s", script)
			}

			// Captured with WithScriptSecrets: every secret as sent.
			ctx, col = WithScript(WithScriptSecrets(context.Background()))
			if err := w.write(ctx, &Server{info: azure}); err != nil {
				t.Fatalf("under WithScriptSecrets: %v", err)
			}
			script = col.String()
			for _, sec := range w.secrets {
				if !strings.Contains(script, QuoteLiteral(sec)) {
					t.Errorf("WithScriptSecrets script lacks %s:\n%s", QuoteLiteral(sec), script)
				}
			}

			// Run: the server gets every secret, the observer none. A Create
			// fails reading its object back from the capture driver, after
			// the write ran — which is all this checks.
			s := captureServer(t, 17)
			s.info.EngineEdition = azure.EngineEdition
			octx, got := observed(WithScriptSecrets(context.Background()))
			_ = w.write(octx, s)
			for _, sec := range w.secrets {
				if captured.find(QuoteLiteral(sec)) == "" {
					t.Errorf("no statement run carried %s; ran %q", QuoteLiteral(sec), captured.qs)
				}
			}
			if len(*got) == 0 {
				t.Fatalf("nothing observed; ran %q", captured.qs)
			}
			for _, e := range *got {
				if strings.Contains(e.SQL, "s3cr3t") {
					t.Errorf("observer saw a secret: %q", e.SQL)
				}
			}
		})
	}
}

func TestRedactSecrets(t *testing.T) {
	const P = "N'<p>'"
	cases := []struct {
		name, stmt string
		secrets    []string
		want       string
	}{
		{"whole literal", "PASSWORD = N'pw'", []string{"pw"}, "PASSWORD = " + P},
		{"ansi literal", "PASSWORD = 'pw'", []string{"pw"}, "PASSWORD = " + P},
		{"lower-case prefix", "PASSWORD = n'pw'", []string{"pw"}, "PASSWORD = " + P},
		{"escaped quote", "PASSWORD = N'p''w'", []string{"p'w"}, "PASSWORD = " + P},
		{"every occurrence", "A = N'pw', B = N'pw'", []string{"pw"}, "A = " + P + ", B = " + P},
		{"several secrets", "A = N'a', B = N'b', C = N'c'", []string{"a", "c"}, "A = " + P + ", B = N'b', C = " + P},
		// A text replace of N'a' would tear these literals apart.
		{"secret is a prefix of another literal", "F = N'a''b', P = N'a'", []string{"a"}, "F = N'a''b', P = " + P},
		{"secret is a prefix of another secret", "A = N'a', B = N'a'''", []string{"a", "a'"}, "A = " + P + ", B = " + P},
		{"empty secret ignored", "A = N'', B = N'x'", []string{""}, "A = N'', B = N'x'"},
		{"no secret", "DROP LOGIN [pw]", []string{"pw"}, "DROP LOGIN [pw]"},
		// A quote inside a bracketed or double-quoted name opens nothing.
		{"quote in bracketed name", "ALTER LOGIN [o'b] WITH PASSWORD = N'pw'", []string{"pw"}, "ALTER LOGIN [o'b] WITH PASSWORD = " + P},
		{"escaped bracket", "ALTER LOGIN [a]]'b] WITH PASSWORD = N'pw'", []string{"pw"}, "ALTER LOGIN [a]]'b] WITH PASSWORD = " + P},
		{"quote in double-quoted name", `ALTER LOGIN "o'b" WITH PASSWORD = N'pw'`, []string{"pw"}, `ALTER LOGIN "o'b" WITH PASSWORD = ` + P},
		{"quote in line comment", "-- it's\nPASSWORD = N'pw'", []string{"pw"}, "-- it's\nPASSWORD = " + P},
		{"quote in block comment", "/* it's /* nested' */ */ PASSWORD = N'pw'", []string{"pw"}, "/* it's /* nested' */ */ PASSWORD = " + P},
		// Only a lone N is a prefix; a word ending in N is a word.
		{"word ending in N", "IN'pw'", []string{"pw"}, "IN" + P},
		{"unterminated literal", "PASSWORD = N'pw", []string{"pw"}, "PASSWORD = " + P},
	}
	for _, c := range cases {
		if got := redactSecrets(c.stmt, "<p>", c.secrets...); got != c.want {
			t.Errorf("%s: redactSecrets(%q) = %q; want %q", c.name, c.stmt, got, c.want)
		}
	}
}
