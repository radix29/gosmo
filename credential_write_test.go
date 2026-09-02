package gosmo

import (
	"context"
	"strings"
	"testing"
)

func TestCreateCredentialStatementShape(t *testing.T) {
	cases := []struct {
		name string
		spec CredentialSpec
		want string
	}{
		{
			name: "identity only",
			spec: CredentialSpec{Name: "app_cred", Identity: `DOMAIN\svc`},
			want: `CREATE CREDENTIAL [app_cred] WITH IDENTITY = N'DOMAIN\svc'`,
		},
		{
			name: "identity and secret",
			spec: CredentialSpec{Name: "app_cred", Identity: `DOMAIN\svc`, Secret: "hunter2"},
			want: `CREATE CREDENTIAL [app_cred] WITH IDENTITY = N'DOMAIN\svc', SECRET = N'hunter2'`,
		},
		{
			// FOR CRYPTOGRAPHIC PROVIDER follows the whole WITH clause. As
			// another comma-separated option inside it, SQL Server rejects it.
			name: "cryptographic provider",
			spec: CredentialSpec{Name: "ekm_cred", Identity: "ekm_user", Secret: "s", CryptographicProvider: "MyEKM"},
			want: `CREATE CREDENTIAL [ekm_cred] WITH IDENTITY = N'ekm_user', SECRET = N's' FOR CRYPTOGRAPHIC PROVIDER [MyEKM]`,
		},
		{
			name: "quotes in the literals are escaped",
			spec: CredentialSpec{Name: "o'brien", Identity: "it's me", Secret: "don't"},
			want: `CREATE CREDENTIAL [o'brien] WITH IDENTITY = N'it''s me', SECRET = N'don''t'`,
		},
		{
			name: "a bracket in the name is doubled",
			spec: CredentialSpec{Name: "we[i]rd", Identity: "x"},
			want: `CREATE CREDENTIAL [we[i]]rd] WITH IDENTITY = N'x'`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.spec.createCredentialStatement()
			if err != nil {
				t.Fatalf("createCredentialStatement: %v", err)
			}
			if got != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

func TestCreateCredentialStatementRequiresNameAndIdentity(t *testing.T) {
	if _, err := (CredentialSpec{Identity: "x"}).createCredentialStatement(); err == nil {
		t.Error("a credential with no name was accepted")
	}
	// CREATE CREDENTIAL has no form without IDENTITY; the server's own error
	// is a syntax error naming nothing useful.
	if _, err := (CredentialSpec{Name: "c"}).createCredentialStatement(); err == nil {
		t.Error("a credential with no identity was accepted")
	}
}

// A nil secret must emit no SECRET clause — and that clears the stored secret
// rather than preserving it, which is the whole reason AlterContext takes a
// pointer. Documented under ALTER CREDENTIAL: "If the optional SECRET argument
// is not specified, the value of the stored secret will be set to NULL."
func TestAlterCredentialSecretClause(t *testing.T) {
	secret := "hunter2"
	cases := []struct {
		name   string
		secret *string
		want   string
	}{
		{
			name:   "nil clears the secret",
			secret: nil,
			want:   `ALTER CREDENTIAL [app_cred] WITH IDENTITY = N'DOMAIN\svc'`,
		},
		{
			name:   "a set secret is written",
			secret: &secret,
			want:   `ALTER CREDENTIAL [app_cred] WITH IDENTITY = N'DOMAIN\svc', SECRET = N'hunter2'`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, col := WithScript(context.Background())
			c := (&Server{}).Credential("app_cred")
			if err := c.AlterContext(ctx, `DOMAIN\svc`, tc.secret); err != nil {
				t.Fatalf("AlterContext: %v", err)
			}
			if len(col.Statements) != 1 {
				t.Fatalf("got %d statements, want 1: %v", len(col.Statements), col.Statements)
			}
			if col.Statements[0] != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", col.Statements[0], tc.want)
			}
		})
	}
}

func TestAlterCredentialRequiresIdentity(t *testing.T) {
	ctx, col := WithScript(context.Background())
	if err := (&Server{}).Credential("app_cred").AlterContext(ctx, "", nil); err == nil {
		t.Error("an empty identity was accepted")
	}
	if len(col.Statements) != 0 {
		t.Errorf("a statement was built anyway: %v", col.Statements)
	}
}

// Under WithScript the CREATE was only collected, so reading the credential
// back would find nothing. The name-only handle is what a caller gets instead.
func TestCreateCredentialUnderScriptReturnsAHandle(t *testing.T) {
	ctx, col := WithScript(context.Background())
	c, err := (&Server{}).CreateCredentialContext(ctx, CredentialSpec{Name: "app_cred", Identity: "x"})
	if err != nil {
		t.Fatalf("CreateCredentialContext: %v", err)
	}
	if c == nil || c.Name != "app_cred" {
		t.Fatalf("got %#v, want a handle named app_cred", c)
	}
	if len(col.Statements) != 1 || !strings.HasPrefix(col.Statements[0], "CREATE CREDENTIAL [app_cred]") {
		t.Errorf("collected statements: %v", col.Statements)
	}
	// The handle must still be usable for a follow-up write.
	if err := c.DropContext(ctx); err != nil {
		t.Fatalf("DropContext on the returned handle: %v", err)
	}
}

func TestDropCredentialStatement(t *testing.T) {
	ctx, col := WithScript(context.Background())
	if err := (&Server{}).Credential("app_cred").DropContext(ctx); err != nil {
		t.Fatalf("DropContext: %v", err)
	}
	want := "DROP CREDENTIAL [app_cred]"
	if len(col.Statements) != 1 || col.Statements[0] != want {
		t.Errorf("got %v, want [%s]", col.Statements, want)
	}
	// DROP CREDENTIAL has no IF EXISTS form; emitting one is a syntax error.
	if strings.Contains(col.Statements[0], "IF EXISTS") {
		t.Errorf("DROP CREDENTIAL used IF EXISTS, which SQL Server does not accept: %s", col.Statements[0])
	}
}

// AlterContext mirrors the new identity onto the receiver, so it must not do
// so under WithScript, where the server still holds the old one.
func TestAlterCredentialDoesNotMirrorUnderScript(t *testing.T) {
	ctx, _ := WithScript(context.Background())
	c := (&Server{}).Credential("app_cred")
	c.Identity = "old"
	if err := c.AlterContext(ctx, "new", nil); err != nil {
		t.Fatalf("AlterContext: %v", err)
	}
	if c.Identity != "old" {
		t.Errorf("Identity became %q under WithScript; nothing ran, so it must stay %q", c.Identity, "old")
	}
}

// -- ScriptCredential ------------------------------------------------------

func TestBuildCredentialScriptCarriesASecretPlaceholder(t *testing.T) {
	c := &Credential{Name: "app_cred", Identity: `DOMAIN\svc`}
	got := buildCredentialScript(c, ScriptOptions{})

	// Emitting no SECRET clause would produce a script that silently creates
	// the credential without one.
	if !strings.Contains(got, "SECRET = N'"+credentialSecretPlaceholder+"'") {
		t.Errorf("script has no secret placeholder:\n%s", got)
	}
	if !strings.Contains(got, "cannot be read from the server") {
		t.Errorf("script does not say the placeholder is a placeholder:\n%s", got)
	}
	if !strings.Contains(got, `CREATE CREDENTIAL [app_cred] WITH IDENTITY = N'DOMAIN\svc'`) {
		t.Errorf("script does not create the credential:\n%s", got)
	}
}

func TestBuildCredentialScriptDropGuard(t *testing.T) {
	c := &Credential{Name: "o'brien", Identity: "x"}
	got := buildCredentialScript(c, ScriptOptions{Verb: ScriptDrop})

	want := "IF EXISTS (SELECT 1 FROM sys.credentials WHERE name = N'o''brien')"
	if !strings.Contains(got, want) {
		t.Errorf("script does not guard the drop with %s:\n%s", want, got)
	}
	if !strings.Contains(got, "DROP CREDENTIAL [o'brien]") {
		t.Errorf("script does not drop the credential:\n%s", got)
	}
	if strings.Contains(got, "CREATE CREDENTIAL") {
		t.Errorf("ScriptDrop emitted a CREATE:\n%s", got)
	}
}

func TestBuildCredentialScriptKeepsTheCryptographicProvider(t *testing.T) {
	c := &Credential{Name: "ekm_cred", Identity: "ekm_user", TargetType: "CRYPTOGRAPHIC PROVIDER", CryptographicProvider: "MyEKM"}
	got := buildCredentialScript(c, ScriptOptions{})
	if !strings.Contains(got, "FOR CRYPTOGRAPHIC PROVIDER [MyEKM]") {
		t.Errorf("script drops the provider binding, which recreates a different credential:\n%s", got)
	}
}
