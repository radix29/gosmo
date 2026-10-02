package gosmo

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestBuildChangePasswordStatementQuotesLiteralByDefault(t *testing.T) {
	got := buildChangePasswordStatement("app_login", "hunter2", false, false)
	want := "ALTER LOGIN [app_login] WITH PASSWORD = " + QuoteLiteral("hunter2")
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
	want := "ALTER LOGIN [app_login] WITH PASSWORD = " + QuoteLiteral("hunter2") + " MUST_CHANGE, CHECK_EXPIRATION = ON"
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
	want := "ALTER LOGIN [app_login] WITH PASSWORD = " + QuoteLiteral("hunter2") + " UNLOCK"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestBuildChangePasswordStatementMustChangeAndUnlock(t *testing.T) {
	got := buildChangePasswordStatement("app_login", "hunter2", true, true)
	want := "ALTER LOGIN [app_login] WITH PASSWORD = " + QuoteLiteral("hunter2") + " MUST_CHANGE UNLOCK, CHECK_EXPIRATION = ON"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestBuildChangePasswordStatementEscapesQuotes(t *testing.T) {
	got := buildChangePasswordStatement("app_login", "it's a secret", true, false)
	want := "ALTER LOGIN [app_login] WITH PASSWORD = " + QuoteLiteral("it's a secret") + " MUST_CHANGE, CHECK_EXPIRATION = ON"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// -- CREATE LOGIN sources -----------------------------------------------------

// createLoginStatementFor is the test's view of the statement builder: it
// resolves LoginSourceAuto the way CreateLogin does, so a case can be
// written the way a caller writes it.
func createLoginStatementFor(t *testing.T, name, password string, opts *CreateLoginRequest) (string, bool) {
	t.Helper()
	src := opts.Source
	if src == LoginSourceAuto {
		if password == "" && opts.PasswordHash == nil {
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
	stmt, alter := createLoginStatementFor(t, "app", "hunter2", &CreateLoginRequest{DefaultDatabase: "master"})
	want := "CREATE LOGIN [app] WITH PASSWORD = " + QuoteLiteral("hunter2") + ", DEFAULT_DATABASE = [master]"
	if stmt != want {
		t.Errorf("got:\n%s\nwant:\n%s", stmt, want)
	}
	if alter {
		t.Error("a SQL login carries DEFAULT_DATABASE in CREATE; no ALTER is needed")
	}

	stmt, alter = createLoginStatementFor(t, `CONTOSO\svc`, "", &CreateLoginRequest{DefaultDatabase: "master"})
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
	stmt, alter := createLoginStatementFor(t, "user@contoso.com", "", &CreateLoginRequest{
		Source: LoginSourceExternalProvider,
	})
	if stmt != "CREATE LOGIN [user@contoso.com] FROM EXTERNAL PROVIDER" {
		t.Errorf("external provider statement wrong: %s", stmt)
	}
	if alter {
		t.Error("no DefaultDatabase was asked for, so no ALTER should follow")
	}
}

// OBJECT_ID names the Entra principal explicitly, for a display name the
// directory cannot resolve on its own. SQL Server 2022 and later.
func TestCreateLoginExternalProviderWithObjectID(t *testing.T) {
	stmt, alter := createLoginStatementFor(t, "sales team", "", &CreateLoginRequest{
		Source:   LoginSourceExternalProvider,
		ObjectID: "3f2504e0-4f89-11d3-9a0c-0305e82c3301",
	})
	want := "CREATE LOGIN [sales team] FROM EXTERNAL PROVIDER WITH OBJECT_ID = " +
		QuoteLiteral("3f2504e0-4f89-11d3-9a0c-0305e82c3301")
	if stmt != want {
		t.Errorf("got:\n%s\nwant:\n%s", stmt, want)
	}
	if alter {
		t.Error("no DefaultDatabase was asked for, so no ALTER should follow")
	}
}

// The object id is a string literal, so it is escaped like every other one
// rather than concatenated raw.
func TestCreateLoginObjectIDIsQuotedAsALiteral(t *testing.T) {
	stmt, _ := createLoginStatementFor(t, "x", "", &CreateLoginRequest{
		Source:   LoginSourceExternalProvider,
		ObjectID: "a'b",
	})
	if !strings.Contains(stmt, QuoteLiteral("a'b")) {
		t.Errorf("object id was not quoted as a literal: %s", stmt)
	}
}

// DEFAULT_DATABASE still cannot ride along with OBJECT_ID — it is not part of
// the option list FROM EXTERNAL PROVIDER accepts, and stays on the ALTER.
func TestCreateLoginObjectIDDoesNotPullDefaultDatabaseIntoTheCreate(t *testing.T) {
	stmt, alter := createLoginStatementFor(t, "x", "", &CreateLoginRequest{
		Source:          LoginSourceExternalProvider,
		ObjectID:        "3f2504e0-4f89-11d3-9a0c-0305e82c3301",
		DefaultDatabase: "sales",
	})
	if strings.Contains(stmt, "DEFAULT_DATABASE") {
		t.Errorf("CREATE LOGIN cannot carry DEFAULT_DATABASE here: %s", stmt)
	}
	if !alter {
		t.Error("DefaultDatabase was dropped instead of moved to an ALTER LOGIN")
	}
}

// A misplaced ObjectID is refused rather than silently ignored: a caller that
// sets it on a SQL or Windows login has asked for a login the server would
// create as something else entirely.
func TestCreateLoginObjectIDOnAnotherSourceIsRefused(t *testing.T) {
	for _, src := range []LoginSource{LoginSourceSQL, LoginSourceWindows, LoginSourceCertificate, LoginSourceAsymmetricKey} {
		opts := CreateLoginRequest{Source: src, ObjectID: "3f2504e0-4f89-11d3-9a0c-0305e82c3301",
			CertificateName: "c", AsymmetricKeyName: "k"}
		password := ""
		if src == LoginSourceSQL {
			password = "hunter2"
		}
		if _, _, err := createLoginStatement("x", password, src, &opts); err == nil {
			t.Errorf("%s login: want an error for a stray ObjectID, got none", src)
		}
	}
}

// FROM EXTERNAL PROVIDER takes no WITH option list, so DEFAULT_DATABASE has
// to be applied by a following ALTER LOGIN rather than named in the CREATE,
// which would not parse.
func TestCreateLoginDefaultDatabaseMovesToAnAlterForAnExternalLogin(t *testing.T) {
	stmt, alter := createLoginStatementFor(t, "x", "", &CreateLoginRequest{
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
	for _, opts := range []CreateLoginRequest{
		{Source: LoginSourceCertificate, CertificateName: "sig_cert", DefaultDatabase: "sales"},
		{Source: LoginSourceAsymmetricKey, AsymmetricKeyName: "sig_key", DefaultDatabase: "sales"},
	} {
		if _, _, err := createLoginStatement("x", "", opts.Source, &opts); err == nil {
			t.Errorf("%s login with a default database: want an error, got none", opts.Source)
		}
	}

	// Without one, both build normally.
	stmt, alter := createLoginStatementFor(t, "x", "", &CreateLoginRequest{
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
		opts     CreateLoginRequest
	}{
		{"password on a Windows login", "hunter2", CreateLoginRequest{Source: LoginSourceWindows}},
		{"password on an external login", "hunter2", CreateLoginRequest{Source: LoginSourceExternalProvider}},
		{"SQL login with no password", "", CreateLoginRequest{Source: LoginSourceSQL}},
		{"certificate with no name", "", CreateLoginRequest{Source: LoginSourceCertificate}},
		{"asymmetric key with no name", "", CreateLoginRequest{Source: LoginSourceAsymmetricKey}},
		{"MustChange on a Windows login", "", CreateLoginRequest{Source: LoginSourceWindows, MustChange: true}},
	} {
		if _, _, err := createLoginStatement("x", tc.password, tc.opts.Source, &tc.opts); err == nil {
			t.Errorf("%s: want an error, got none", tc.name)
		}
	}
}

// T4: every SQL-login option rides in the CREATE's one WITH list, so a weak
// password with the policy off is accepted by the CREATE itself rather than
// refused (Msg 15118) before a follow-up ALTER could turn the policy off.
func TestCreateLoginSQLOptionsGoInOneWithList(t *testing.T) {
	stmt, alter := createLoginStatementFor(t, "app", "a", &CreateLoginRequest{
		DefaultDatabase: "sales",
		DefaultLanguage: "us_english",
		CheckPolicy:     new(false),
		CheckExpiration: new(false),
		SID:             []byte{0x01, 0xab},
		Credential:      "cred]x",
	})
	want := "CREATE LOGIN [app] WITH PASSWORD = " + QuoteLiteral("a") +
		", SID = 0x01AB, DEFAULT_DATABASE = [sales], DEFAULT_LANGUAGE = [us_english]" +
		", CHECK_EXPIRATION = OFF, CHECK_POLICY = OFF, CREDENTIAL = [cred]]x]"
	if stmt != want {
		t.Errorf("got:\n%s\nwant:\n%s", stmt, want)
	}
	if alter {
		t.Error("a SQL login carries every option in CREATE; no ALTER is needed")
	}
}

// Nil policy pointers leave the options out, so the server defaults apply.
func TestCreateLoginNilPolicyIsOmitted(t *testing.T) {
	stmt, _ := createLoginStatementFor(t, "app", "a", &CreateLoginRequest{})
	if stmt != "CREATE LOGIN [app] WITH PASSWORD = "+QuoteLiteral("a") {
		t.Errorf("statement = %q", stmt)
	}
	stmt, _ = createLoginStatementFor(t, "app", "a", &CreateLoginRequest{CheckPolicy: new(true), CheckExpiration: new(true)})
	if !strings.HasSuffix(stmt, ", CHECK_EXPIRATION = ON, CHECK_POLICY = ON") {
		t.Errorf("statement = %q", stmt)
	}
}

// MUST_CHANGE still brings CHECK_EXPIRATION = ON, once, whether or not the
// caller also asked for it.
func TestCreateLoginMustChangeImpliesCheckExpiration(t *testing.T) {
	for _, exp := range []*bool{nil, new(true)} {
		stmt, _ := createLoginStatementFor(t, "app", "a", &CreateLoginRequest{MustChange: true, CheckExpiration: exp})
		want := "CREATE LOGIN [app] WITH PASSWORD = " + QuoteLiteral("a") + " MUST_CHANGE, CHECK_EXPIRATION = ON"
		if stmt != want {
			t.Errorf("CheckExpiration %v: got\n%s\nwant\n%s", exp, stmt, want)
		}
	}
}

// A Windows login takes DEFAULT_LANGUAGE in its CREATE too.
func TestCreateLoginWindowsDefaultLanguage(t *testing.T) {
	stmt, _ := createLoginStatementFor(t, `CONTOSO\svc`, "", &CreateLoginRequest{DefaultLanguage: "Deutsch"})
	if stmt != `CREATE LOGIN [CONTOSO\svc] FROM WINDOWS WITH DEFAULT_LANGUAGE = [Deutsch]` {
		t.Errorf("statement = %q", stmt)
	}
	stmt, _ = createLoginStatementFor(t, `CONTOSO\svc`, "", &CreateLoginRequest{DefaultDatabase: "sales", DefaultLanguage: "Deutsch"})
	if stmt != `CREATE LOGIN [CONTOSO\svc] FROM WINDOWS WITH DEFAULT_DATABASE = [sales], DEFAULT_LANGUAGE = [Deutsch]` {
		t.Errorf("statement = %q", stmt)
	}
}

// PasswordHash is emitted as a binary literal under HASHED, and alone implies
// a SQL login.
func TestCreateLoginPasswordHash(t *testing.T) {
	stmt, _ := createLoginStatementFor(t, "app", "", &CreateLoginRequest{
		PasswordHash: []byte{0x02, 0x00, 0xfe}, SID: []byte{0x10}, CheckPolicy: new(false),
	})
	want := "CREATE LOGIN [app] WITH PASSWORD = 0x0200FE HASHED, SID = 0x10, CHECK_POLICY = OFF"
	if stmt != want {
		t.Errorf("got:\n%s\nwant:\n%s", stmt, want)
	}
}

// The combinations SQL Server refuses are refused before anything runs.
func TestCreateLoginRefusesWhatTheServerRefuses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		password string
		opts     CreateLoginRequest
	}{
		{"expiration on, policy off (15122)", "a", CreateLoginRequest{Source: LoginSourceSQL, CheckPolicy: new(false), CheckExpiration: new(true)}},
		{"MustChange, policy off (15122)", "a", CreateLoginRequest{Source: LoginSourceSQL, MustChange: true, CheckPolicy: new(false)}},
		{"MustChange, expiration off", "a", CreateLoginRequest{Source: LoginSourceSQL, MustChange: true, CheckExpiration: new(false)}},
		{"MustChange with HASHED (33010)", "", CreateLoginRequest{Source: LoginSourceSQL, MustChange: true, PasswordHash: []byte{1}}},
		{"Password and PasswordHash", "a", CreateLoginRequest{Source: LoginSourceSQL, PasswordHash: []byte{1}}},
		{"PasswordHash on a Windows login", "", CreateLoginRequest{Source: LoginSourceWindows, PasswordHash: []byte{1}}},
		{"SID on a Windows login", "", CreateLoginRequest{Source: LoginSourceWindows, SID: []byte{1}}},
		{"Credential on a Windows login", "", CreateLoginRequest{Source: LoginSourceWindows, Credential: "c"}},
		{"CheckPolicy on a Windows login", "", CreateLoginRequest{Source: LoginSourceWindows, CheckPolicy: new(true)}},
		{"CheckExpiration on an external login", "", CreateLoginRequest{Source: LoginSourceExternalProvider, CheckExpiration: new(false)}},
		{"language on a certificate login", "", CreateLoginRequest{Source: LoginSourceCertificate, CertificateName: "c", DefaultLanguage: "us_english"}},
		{"language on an asymmetric key login", "", CreateLoginRequest{Source: LoginSourceAsymmetricKey, AsymmetricKeyName: "k", DefaultLanguage: "us_english"}},
	} {
		if _, _, err := createLoginStatement("x", tc.password, tc.opts.Source, &tc.opts); err == nil {
			t.Errorf("%s: want an error, got none", tc.name)
		}
	}
}

// An external-provider login's language joins its default database on the one
// follow-up ALTER.
func TestCreateLoginContextScriptsExternalDefaultsInOneAlter(t *testing.T) {
	s := &Server{}
	ctx, script := WithScript(context.Background())
	if _, err := s.CreateLogin(ctx, CreateLoginRequest{
		Name: "u@contoso.com", Source: LoginSourceExternalProvider, DefaultDatabase: "sales", DefaultLanguage: "Deutsch",
	}); err != nil {
		t.Fatalf("CreateLogin under WithScript: %v", err)
	}
	want := []string{atomicBatch([]string{
		"CREATE LOGIN [u@contoso.com] FROM EXTERNAL PROVIDER",
		"ALTER LOGIN [u@contoso.com] WITH DEFAULT_DATABASE = [sales], DEFAULT_LANGUAGE = [Deutsch]",
	})}
	if !slices.Equal(script.Statements(), want) {
		t.Errorf("Statements = %q, want %q", script.Statements(), want)
	}
	ctx, script = WithScript(context.Background())
	if _, err := s.CreateLogin(ctx, CreateLoginRequest{
		Name: "u@contoso.com", Source: LoginSourceExternalProvider, DefaultLanguage: "Deutsch",
	}); err != nil {
		t.Fatalf("CreateLogin under WithScript: %v", err)
	}
	if got := script.Statements(); len(got) != 1 || !strings.Contains(got[0], "\nALTER LOGIN [u@contoso.com] WITH DEFAULT_LANGUAGE = [Deutsch];\n") {
		t.Errorf("Statements = %q", got)
	}
}

// The identifiers reach the statement bracket-quoted, like every other name
// gosmo emits.
func TestCreateLoginQuotesMappedObjectNames(t *testing.T) {
	stmt, _ := createLoginStatementFor(t, "x", "", &CreateLoginRequest{
		Source: LoginSourceCertificate, CertificateName: "cert]name",
	})
	if stmt != "CREATE LOGIN [x] FROM CERTIFICATE [cert]]name]" {
		t.Errorf("certificate name not quoted: %s", stmt)
	}
}

// CreateLogin issues both halves under WithScript: the CREATE, then the
// ALTER that carries a DefaultDatabase the CREATE form cannot — as one
// atomicBatch (T30), so a refused ALTER leaves no login behind. Azure SQL
// Database wants CREATE LOGIN alone in its batch, so there they stay two.
func TestCreateLoginContextScriptsCreateThenAlter(t *testing.T) {
	req := CreateLoginRequest{
		Name:            "user@contoso.com",
		Source:          LoginSourceExternalProvider,
		DefaultDatabase: "sales",
	}
	halves := []string{
		"CREATE LOGIN [user@contoso.com] FROM EXTERNAL PROVIDER",
		"ALTER LOGIN [user@contoso.com] WITH DEFAULT_DATABASE = [sales]",
	}
	for _, c := range []struct {
		name string
		s    *Server
		want []string
	}{
		{"one atomic batch", &Server{}, []string{atomicBatch(halves)}},
		{"Azure SQL Database", &Server{info: &ServerInfo{EngineEdition: int(EngineAzureSQLDatabase)}}, halves},
	} {
		ctx, script := WithScript(context.Background())
		if _, err := c.s.CreateLogin(ctx, req); err != nil {
			t.Fatalf("%s: CreateLogin under WithScript: %v", c.name, err)
		}
		if !slices.Equal(script.Statements(), c.want) {
			t.Errorf("%s: Statements = %q, want %q", c.name, script.Statements(), c.want)
		}
	}
}

// A SQL login is still one statement — the ALTER exists only for the sources
// whose CREATE has no WITH list.
func TestCreateLoginContextScriptsASQLLoginAsOneStatement(t *testing.T) {
	s := &Server{}
	ctx, script := WithScript(context.Background())

	if _, err := s.CreateLogin(ctx, CreateLoginRequest{Name: "app", Password: "hunter2", DefaultDatabase: "sales"}); err != nil {
		t.Fatalf("CreateLogin under WithScript: %v", err)
	}
	if len(script.Statements()) != 1 {
		t.Fatalf("Statements = %q, want one statement", script.Statements())
	}
}
