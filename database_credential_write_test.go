package gosmo

import (
	"context"
	"strings"
	"testing"
)

// scriptDB is the database handle these tests write through: no server behind
// it, so every write has to be collected by WithScript rather than executed.
// Database.exec prefixes each collected statement with its own USE, so the
// wanted text below carries it too — asserting on the statement without it
// would not notice a write that landed in the wrong database.
func scriptDB() *Database { return (&Server{}).Database("AppDB") }

const useAppDB = "USE [AppDB];\n"

func TestCreateDatabaseScopedCredentialStatementShape(t *testing.T) {
	cases := []struct {
		name string
		spec DatabaseScopedCredentialSpec
		want string
	}{
		{
			name: "identity only",
			spec: DatabaseScopedCredentialSpec{Name: "app_cred", Identity: "Managed Identity"},
			want: `CREATE DATABASE SCOPED CREDENTIAL [app_cred] WITH IDENTITY = N'Managed Identity'`,
		},
		{
			name: "identity and secret",
			spec: DatabaseScopedCredentialSpec{Name: "app_cred", Identity: "SHARED ACCESS SIGNATURE", Secret: "sv=2019"},
			want: `CREATE DATABASE SCOPED CREDENTIAL [app_cred] WITH IDENTITY = N'SHARED ACCESS SIGNATURE', SECRET = N'sv=2019'`,
		},
		{
			name: "quotes in the literals are escaped",
			spec: DatabaseScopedCredentialSpec{Name: "o'brien", Identity: "it's me", Secret: "don't"},
			want: `CREATE DATABASE SCOPED CREDENTIAL [o'brien] WITH IDENTITY = N'it''s me', SECRET = N'don''t'`,
		},
		{
			name: "a bracket in the name is doubled",
			spec: DatabaseScopedCredentialSpec{Name: "we[i]rd", Identity: "x"},
			want: `CREATE DATABASE SCOPED CREDENTIAL [we[i]]rd] WITH IDENTITY = N'x'`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.spec.createDatabaseScopedCredentialStatement()
			if err != nil {
				t.Fatalf("createDatabaseScopedCredentialStatement: %v", err)
			}
			if got != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

func TestCreateDatabaseScopedCredentialRequiresNameAndIdentity(t *testing.T) {
	if _, err := (DatabaseScopedCredentialSpec{Identity: "x"}).createDatabaseScopedCredentialStatement(); err == nil {
		t.Error("a credential with no name was accepted")
	}
	if _, err := (DatabaseScopedCredentialSpec{Name: "c"}).createDatabaseScopedCredentialStatement(); err == nil {
		t.Error("a credential with no identity was accepted")
	}
}

// A nil secret must emit no SECRET clause — and that clears the stored secret
// rather than preserving it, which is the whole reason AlterContext takes a
// pointer, exactly as the server-level half does.
func TestAlterDatabaseScopedCredentialSecretClause(t *testing.T) {
	secret := "sv=2019"
	cases := []struct {
		name   string
		secret *string
		want   string
	}{
		{
			name:   "nil clears the secret",
			secret: nil,
			want:   useAppDB + `ALTER DATABASE SCOPED CREDENTIAL [app_cred] WITH IDENTITY = N'Managed Identity'`,
		},
		{
			name:   "a set secret is written",
			secret: &secret,
			want:   useAppDB + `ALTER DATABASE SCOPED CREDENTIAL [app_cred] WITH IDENTITY = N'Managed Identity', SECRET = N'sv=2019'`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, col := WithScript(context.Background())
			c := scriptDB().DatabaseScopedCredential("app_cred")
			if err := c.AlterContext(ctx, "Managed Identity", tc.secret); err != nil {
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

func TestAlterDatabaseScopedCredentialRequiresIdentity(t *testing.T) {
	ctx, col := WithScript(context.Background())
	if err := scriptDB().DatabaseScopedCredential("app_cred").AlterContext(ctx, "", nil); err == nil {
		t.Error("an empty identity was accepted")
	}
	if len(col.Statements) != 0 {
		t.Errorf("a statement was built anyway: %v", col.Statements)
	}
}

// Under WithScript the CREATE was only collected, so reading the credential
// back would find nothing. The name-only handle is what a caller gets instead.
func TestCreateDatabaseScopedCredentialUnderScriptReturnsAHandle(t *testing.T) {
	ctx, col := WithScript(context.Background())
	c, err := scriptDB().CreateDatabaseScopedCredentialContext(ctx,
		DatabaseScopedCredentialSpec{Name: "app_cred", Identity: "x"})
	if err != nil {
		t.Fatalf("CreateDatabaseScopedCredentialContext: %v", err)
	}
	if c == nil || c.Name != "app_cred" {
		t.Fatalf("got %#v, want a handle named app_cred", c)
	}
	if len(col.Statements) != 1 ||
		!strings.HasPrefix(col.Statements[0], useAppDB+"CREATE DATABASE SCOPED CREDENTIAL [app_cred]") {
		t.Errorf("collected statements: %v", col.Statements)
	}
	// The handle must still be usable for a follow-up write.
	if err := c.DropContext(ctx); err != nil {
		t.Fatalf("DropContext on the returned handle: %v", err)
	}
}

func TestDropDatabaseScopedCredentialStatement(t *testing.T) {
	ctx, col := WithScript(context.Background())
	if err := scriptDB().DatabaseScopedCredential("app_cred").DropContext(ctx); err != nil {
		t.Fatalf("DropContext: %v", err)
	}
	want := useAppDB + "DROP DATABASE SCOPED CREDENTIAL [app_cred]"
	if len(col.Statements) != 1 || col.Statements[0] != want {
		t.Errorf("got %v, want [%s]", col.Statements, want)
	}
	// No Drop* in this package carries IF EXISTS — dropping something that
	// isn't there has to reach the caller as the server's error.
	if strings.Contains(col.Statements[0], "IF EXISTS") {
		t.Errorf("DROP DATABASE SCOPED CREDENTIAL used IF EXISTS: %s", col.Statements[0])
	}
}

// AlterContext mirrors the new identity onto the receiver, so it must not do
// so under WithScript, where the server still holds the old one.
func TestAlterDatabaseScopedCredentialDoesNotMirrorUnderScript(t *testing.T) {
	ctx, _ := WithScript(context.Background())
	c := scriptDB().DatabaseScopedCredential("app_cred")
	c.Identity = "old"
	if err := c.AlterContext(ctx, "new", nil); err != nil {
		t.Fatalf("AlterContext: %v", err)
	}
	if c.Identity != "old" {
		t.Errorf("Identity became %q under WithScript; nothing ran, so it must stay %q", c.Identity, "old")
	}
}

// -- ScriptDatabaseScopedCredential ----------------------------------------

func TestBuildDatabaseScopedCredentialScriptCarriesASecretPlaceholder(t *testing.T) {
	c := &DatabaseScopedCredential{Name: "app_cred", Identity: "SHARED ACCESS SIGNATURE"}
	got := buildDatabaseScopedCredentialScript(c, ScriptOptions{})

	// Emitting no SECRET clause would produce a script that silently creates
	// the credential without one.
	if !strings.Contains(got, "SECRET = N'"+credentialSecretPlaceholder+"'") {
		t.Errorf("script has no secret placeholder:\n%s", got)
	}
	if !strings.Contains(got, "cannot be read from the server") {
		t.Errorf("script does not say the placeholder is a placeholder:\n%s", got)
	}
	if !strings.Contains(got, `CREATE DATABASE SCOPED CREDENTIAL [app_cred] WITH IDENTITY = N'SHARED ACCESS SIGNATURE'`) {
		t.Errorf("script does not create the credential:\n%s", got)
	}
}

func TestBuildDatabaseScopedCredentialScriptDropGuard(t *testing.T) {
	c := &DatabaseScopedCredential{Name: "o'brien", Identity: "x"}
	got := buildDatabaseScopedCredentialScript(c, ScriptOptions{Verb: ScriptDrop})

	want := "IF EXISTS (SELECT 1 FROM sys.database_scoped_credentials WHERE name = N'o''brien')"
	if !strings.Contains(got, want) {
		t.Errorf("script does not guard the drop with %s:\n%s", want, got)
	}
	if !strings.Contains(got, "DROP DATABASE SCOPED CREDENTIAL [o'brien]") {
		t.Errorf("script does not drop the credential:\n%s", got)
	}
	if strings.Contains(got, "CREATE DATABASE SCOPED CREDENTIAL") {
		t.Errorf("ScriptDrop emitted a CREATE:\n%s", got)
	}
}
