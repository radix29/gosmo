package gosmo

import (
	"context"
	"strings"
	"testing"
)

func TestBuildChangePasswordStatementQuotesLiteralByDefault(t *testing.T) {
	got := buildChangePasswordStatement("app_login", "hunter2", false, false)
	want := "ALTER LOGIN [app_login] WITH PASSWORD = " + nStringLiteral("hunter2")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, "HASHED") {
		t.Error("statement used HASHED, which tells SQL Server the value is already a password hash, not cleartext")
	}
}

func TestBuildChangePasswordStatementMustChangeAddsCheckExpiration(t *testing.T) {
	// MUST_CHANGE requires CHECK_EXPIRATION = ON or SQL Server rejects it.
	got := buildChangePasswordStatement("app_login", "hunter2", true, false)
	want := "ALTER LOGIN [app_login] WITH PASSWORD = " + nStringLiteral("hunter2") + " MUST_CHANGE, CHECK_EXPIRATION = ON"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestBuildChangePasswordStatementUnlock(t *testing.T) {
	// UNLOCK is a password-clause modifier, space-separated after
	// PASSWORD = '...', not a comma-separated <set_option>:
	// "ALTER LOGIN ... WITH PASSWORD = '...', UNLOCK" is rejected with
	// "Incorrect syntax near 'UNLOCK'".
	got := buildChangePasswordStatement("app_login", "hunter2", false, true)
	want := "ALTER LOGIN [app_login] WITH PASSWORD = " + nStringLiteral("hunter2") + " UNLOCK"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestBuildChangePasswordStatementMustChangeAndUnlock(t *testing.T) {
	got := buildChangePasswordStatement("app_login", "hunter2", true, true)
	want := "ALTER LOGIN [app_login] WITH PASSWORD = " + nStringLiteral("hunter2") + " MUST_CHANGE UNLOCK, CHECK_EXPIRATION = ON"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestBuildChangePasswordStatementEscapesQuotes(t *testing.T) {
	got := buildChangePasswordStatement("app_login", "it's a secret", true, false)
	want := "ALTER LOGIN [app_login] WITH PASSWORD = " + nStringLiteral("it's a secret") + " MUST_CHANGE, CHECK_EXPIRATION = ON"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// -- CREATE LOGIN sources -----------------------------------------------------

// createLoginStatementFor is the test's view of the statement builder: it
// resolves LoginSourceAuto the way CreateLoginContext does, so a case can be
// written the way a caller writes it.
func createLoginStatementFor(t *testing.T, name, password string, opts *CreateLoginOptions) (string, bool) {
	t.Helper()
	src := opts.Source
	if src == LoginSourceAuto {
		if password == "" {
			src = LoginSourceWindows
		} else {
			src = LoginSourceSQL
		}
	}
	stmt, alter, err := createLoginStatement(name, password, src, opts)
	if err != nil {
		t.Fatalf("createLoginStatement: %v", err)
	}
	return stmt, alter
}

// The zero Source keeps CreateLogin's original password-decides-the-type
// rule, which every existing caller relies on.
func TestCreateLoginAutoSourceFollowsThePassword(t *testing.T) {
	stmt, alter := createLoginStatementFor(t, "app", "hunter2", &CreateLoginOptions{DefaultDatabase: "master"})
	want := "CREATE LOGIN [app] WITH PASSWORD = " + nStringLiteral("hunter2") + ", DEFAULT_DATABASE = [master]"
	if stmt != want {
		t.Errorf("got:\n%s\nwant:\n%s", stmt, want)
	}
	if alter {
		t.Error("a SQL login carries DEFAULT_DATABASE in CREATE; no ALTER is needed")
	}

	stmt, alter = createLoginStatementFor(t, `CONTOSO\svc`, "", &CreateLoginOptions{DefaultDatabase: "master"})
	if stmt != `CREATE LOGIN [CONTOSO\svc] FROM WINDOWS WITH DEFAULT_DATABASE = [master]` {
		t.Errorf("Windows login statement wrong: %s", stmt)
	}
	if alter {
		t.Error("a Windows login carries DEFAULT_DATABASE in CREATE; no ALTER is needed")
	}
}

// The capability the round trip was missing: gosmo could read and script an
// Entra login but not create one.
func TestCreateLoginExternalProvider(t *testing.T) {
	stmt, alter := createLoginStatementFor(t, "user@contoso.com", "", &CreateLoginOptions{
		Source: LoginSourceExternalProvider,
	})
	if stmt != "CREATE LOGIN [user@contoso.com] FROM EXTERNAL PROVIDER" {
		t.Errorf("external provider statement wrong: %s", stmt)
	}
	if alter {
		t.Error("no DefaultDatabase was asked for, so no ALTER should follow")
	}
}

// FROM EXTERNAL PROVIDER takes no WITH option list, so DEFAULT_DATABASE has
// to be applied by a following ALTER LOGIN rather than named in the CREATE,
// which would not parse.
func TestCreateLoginDefaultDatabaseMovesToAnAlterForAnExternalLogin(t *testing.T) {
	stmt, alter := createLoginStatementFor(t, "x", "", &CreateLoginOptions{
		Source: LoginSourceExternalProvider, DefaultDatabase: "sales",
	})
	if stmt != "CREATE LOGIN [x] FROM EXTERNAL PROVIDER" {
		t.Errorf("statement = %q", stmt)
	}
	if !alter {
		t.Error("DefaultDatabase was dropped instead of moved to an ALTER LOGIN")
	}
	if strings.Contains(stmt, "DEFAULT_DATABASE") {
		t.Errorf("CREATE LOGIN cannot carry DEFAULT_DATABASE here: %s", stmt)
	}
}

// A mapped login has no default database in either statement: SQL Server
// answers "Cannot use the parameter DEFAULT_DATABASE for a certificate or
// asymmetric key login" to the CREATE and to the ALTER alike (verified live
// on win10cli), so the option is refused rather than sent.
func TestCreateLoginRefusesADefaultDatabaseForAMappedLogin(t *testing.T) {
	for _, opts := range []CreateLoginOptions{
		{Source: LoginSourceCertificate, CertificateName: "sig_cert", DefaultDatabase: "sales"},
		{Source: LoginSourceAsymmetricKey, AsymmetricKeyName: "sig_key", DefaultDatabase: "sales"},
	} {
		if _, _, err := createLoginStatement("x", "", opts.Source, &opts); err == nil {
			t.Errorf("%s login with a default database: want an error, got none", opts.Source)
		}
	}

	// Without one, both build normally.
	stmt, alter := createLoginStatementFor(t, "x", "", &CreateLoginOptions{
		Source: LoginSourceCertificate, CertificateName: "sig_cert",
	})
	if stmt != "CREATE LOGIN [x] FROM CERTIFICATE [sig_cert]" || alter {
		t.Errorf("statement = %q, alter = %v", stmt, alter)
	}
}

func TestCreateLoginRejectsMismatchedOptions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		password string
		opts     CreateLoginOptions
	}{
		{"password on a Windows login", "hunter2", CreateLoginOptions{Source: LoginSourceWindows}},
		{"password on an external login", "hunter2", CreateLoginOptions{Source: LoginSourceExternalProvider}},
		{"SQL login with no password", "", CreateLoginOptions{Source: LoginSourceSQL}},
		{"certificate with no name", "", CreateLoginOptions{Source: LoginSourceCertificate}},
		{"asymmetric key with no name", "", CreateLoginOptions{Source: LoginSourceAsymmetricKey}},
		{"MustChange on a Windows login", "", CreateLoginOptions{Source: LoginSourceWindows, MustChange: true}},
	} {
		if _, _, err := createLoginStatement("x", tc.password, tc.opts.Source, &tc.opts); err == nil {
			t.Errorf("%s: want an error, got none", tc.name)
		}
	}
}

// The identifiers reach the statement bracket-quoted, like every other name
// gosmo emits.
func TestCreateLoginQuotesMappedObjectNames(t *testing.T) {
	stmt, _ := createLoginStatementFor(t, "x", "", &CreateLoginOptions{
		Source: LoginSourceCertificate, CertificateName: "cert]name",
	})
	if stmt != "CREATE LOGIN [x] FROM CERTIFICATE [cert]]name]" {
		t.Errorf("certificate name not quoted: %s", stmt)
	}
}

// CreateLoginContext issues both halves under WithScript: the CREATE, then the
// ALTER that carries a DefaultDatabase the CREATE form cannot.
func TestCreateLoginContextScriptsCreateThenAlter(t *testing.T) {
	s := &Server{}
	ctx, script := WithScript(context.Background())

	err := s.CreateLoginContext(ctx, "user@contoso.com", "", &CreateLoginOptions{
		Source:          LoginSourceExternalProvider,
		DefaultDatabase: "sales",
	})
	if err != nil {
		t.Fatalf("CreateLoginContext under WithScript: %v", err)
	}
	want := []string{
		"CREATE LOGIN [user@contoso.com] FROM EXTERNAL PROVIDER",
		"ALTER LOGIN [user@contoso.com] WITH DEFAULT_DATABASE = [sales]",
	}
	if len(script.Statements) != len(want) {
		t.Fatalf("Statements = %q, want %q", script.Statements, want)
	}
	for i := range want {
		if script.Statements[i] != want[i] {
			t.Errorf("Statements[%d] = %q, want %q", i, script.Statements[i], want[i])
		}
	}
}

// A SQL login is still one statement — the ALTER exists only for the sources
// whose CREATE has no WITH list.
func TestCreateLoginContextScriptsASQLLoginAsOneStatement(t *testing.T) {
	s := &Server{}
	ctx, script := WithScript(context.Background())

	if err := s.CreateLoginContext(ctx, "app", "hunter2", &CreateLoginOptions{DefaultDatabase: "sales"}); err != nil {
		t.Fatalf("CreateLoginContext under WithScript: %v", err)
	}
	if len(script.Statements) != 1 {
		t.Fatalf("Statements = %q, want one statement", script.Statements)
	}
}
