package gosmo

import (
	"context"
	"strings"
	"testing"
)

// TestScriptSecurityWrites pins the statements behind the principal,
// permission and key/certificate writes. See script_write_common_test.go for
// what these assert and why.
func TestScriptSecurityWrites(t *testing.T) {
	user := func() *User { return &User{db: scriptTestDB(), Name: "o'brien"} }
	login := func() *Login { return &Login{server: &Server{}, Name: "o'brien"} }

	runScriptCases(t, []scriptCase{
		// --- database principals
		{"CreateSchema", func(c context.Context) error {
			return scriptTestDB().CreateSchemaContext(c, "sa]les", "o'brien")
		}, scriptUsePrefix + "CREATE SCHEMA [sa]]les] AUTHORIZATION [o'brien]"},
		{"CreateSchema without an owner", func(c context.Context) error {
			return scriptTestDB().CreateSchemaContext(c, "sa]les", "")
		}, scriptUsePrefix + "CREATE SCHEMA [sa]]les]"},
		{"CreateUser", func(c context.Context) error {
			return scriptTestDB().CreateUserContext(c, "o'brien", `DOM\o]b`, "sa]les")
		}, scriptUsePrefix + `CREATE USER [o'brien] FOR LOGIN [DOM\o]]b] WITH DEFAULT_SCHEMA = [sa]]les]`},
		{"CreateUser without a default schema", func(c context.Context) error {
			return scriptTestDB().CreateUserContext(c, "o'brien", "app_login", "")
		}, scriptUsePrefix + "CREATE USER [o'brien] FOR LOGIN [app_login]"},
		{"AddRoleMember", func(c context.Context) error {
			return scriptTestDB().AddRoleMemberContext(c, "db_own]er", "o'brien")
		}, scriptUsePrefix + "ALTER ROLE [db_own]]er] ADD MEMBER [o'brien]"},
		{"RemoveRoleMember", func(c context.Context) error {
			return scriptTestDB().RemoveRoleMemberContext(c, "db_own]er", "o'brien")
		}, scriptUsePrefix + "ALTER ROLE [db_own]]er] DROP MEMBER [o'brien]"},
		{"SetOwner", func(c context.Context) error {
			return scriptTestDB().SetOwnerContext(c, "o'brien")
		}, "ALTER AUTHORIZATION ON DATABASE::[App'DB] TO [o'brien]"},

		// --- User
		{"User AddToRole", func(c context.Context) error {
			return user().AddToRoleContext(c, "db_datar]eader")
		}, scriptUsePrefix + "ALTER ROLE [db_datar]]eader] ADD MEMBER [o'brien]"},
		{"User RemoveFromRole", func(c context.Context) error {
			return user().RemoveFromRoleContext(c, "db_datar]eader")
		}, scriptUsePrefix + "ALTER ROLE [db_datar]]eader] DROP MEMBER [o'brien]"},
		{"User SetDefaultSchema", func(c context.Context) error {
			return user().SetDefaultSchemaContext(c, "sa]les")
		}, scriptUsePrefix + "ALTER USER [o'brien] WITH DEFAULT_SCHEMA = [sa]]les]"},
		{"User SetLogin", func(c context.Context) error {
			return user().SetLoginContext(c, `DOM\o]b`)
		}, scriptUsePrefix + `ALTER USER [o'brien] WITH LOGIN = [DOM\o]]b]`},
		{"User Grant", func(c context.Context) error {
			return user().GrantContext(c, PermSelect, "dbo", "Sales.Archive")
		}, scriptUsePrefix + "GRANT SELECT ON [dbo].[Sales.Archive] TO [o'brien]"},
		{"User Deny", func(c context.Context) error {
			return user().DenyContext(c, PermSelect, "dbo", "Sales.Archive")
		}, scriptUsePrefix + "DENY SELECT ON [dbo].[Sales.Archive] TO [o'brien]"},
		{"User Revoke", func(c context.Context) error {
			return user().RevokeContext(c, PermSelect, "dbo", "Sales.Archive")
		}, scriptUsePrefix + "REVOKE SELECT ON [dbo].[Sales.Archive] FROM [o'brien]"},

		// --- Login
		{"Login Enable", func(c context.Context) error {
			return login().EnableContext(c)
		}, "ALTER LOGIN [o'brien] ENABLE"},
		{"Login Disable", func(c context.Context) error {
			return login().DisableContext(c)
		}, "ALTER LOGIN [o'brien] DISABLE"},
		{"Login AddServerRoleMember", func(c context.Context) error {
			return login().AddServerRoleMemberContext(c, "sys]admin")
		}, "ALTER SERVER ROLE [sys]]admin] ADD MEMBER [o'brien]"},
		{"Login RemoveServerRoleMember", func(c context.Context) error {
			return login().RemoveServerRoleMemberContext(c, "sys]admin")
		}, "ALTER SERVER ROLE [sys]]admin] DROP MEMBER [o'brien]"},
		{"Login SetDefaultDatabase", func(c context.Context) error {
			return login().SetDefaultDatabaseContext(c, "App'DB")
		}, "ALTER LOGIN [o'brien] WITH DEFAULT_DATABASE = [App'DB]"},
		{"Login SetDefaultLanguage", func(c context.Context) error {
			return login().SetDefaultLanguageContext(c, "us_english")
		}, "ALTER LOGIN [o'brien] WITH DEFAULT_LANGUAGE = [us_english]"},
		{"Login SetPasswordPolicy", func(c context.Context) error {
			return login().SetPasswordPolicyContext(c, true, false)
		}, "ALTER LOGIN [o'brien] WITH CHECK_POLICY = ON, CHECK_EXPIRATION = OFF"},
		{"Server DropLogin", func(c context.Context) error {
			return (&Server{}).DropLoginContext(c, "o'brien")
		}, "DROP LOGIN [o'brien]"},

		// --- keys and certificates
		{"CreateCertificate", func(c context.Context) error {
			return scriptTestDB().CreateCertificateContext(c, CertificateSpec{
				Name: "Cert]1", Authorization: "o'brien", Subject: "gossms o'brien",
			})
		}, scriptUsePrefix + "CREATE CERTIFICATE [Cert]]1] AUTHORIZATION [o'brien] WITH SUBJECT = N'gossms o''brien'"},
		{"CreateMasterKey", func(c context.Context) error {
			return scriptTestDB().CreateMasterKeyContext(c, "p'wd")
		}, scriptUsePrefix + "CREATE MASTER KEY ENCRYPTION BY PASSWORD = N'p''wd'"},
		{"CreateColumnMasterKeyWithSignature", func(c context.Context) error {
			return scriptTestDB().CreateColumnMasterKeyWithSignatureContext(c, "CMK]1", "MSSQL_CERTIFICATE_STORE", "CurrentUser/my/a'b", []byte{0x0a, 0xff})
		}, scriptUsePrefix + `
CREATE COLUMN MASTER KEY [CMK]]1]
WITH (
    KEY_STORE_PROVIDER_NAME = N'MSSQL_CERTIFICATE_STORE',
    KEY_PATH = N'CurrentUser/my/a''b',
    ENCLAVE_COMPUTATIONS (SIGNATURE = 0x0AFF)
)`},
		{"CreateColumnMasterKey without enclave computations", func(c context.Context) error {
			return scriptTestDB().CreateColumnMasterKeyContext(c, "CMK]1", "MSSQL_CERTIFICATE_STORE", "CurrentUser/my/a'b", false)
		}, scriptUsePrefix + `
CREATE COLUMN MASTER KEY [CMK]]1]
WITH (
    KEY_STORE_PROVIDER_NAME = N'MSSQL_CERTIFICATE_STORE',
    KEY_PATH = N'CurrentUser/my/a''b'
)`},
		{"CreateColumnEncryptionKey", func(c context.Context) error {
			return scriptTestDB().CreateColumnEncryptionKeyContext(c, "CEK]1", []ColumnEncryptionKeyValue{
				{MasterKeyName: "CMK]1", EncryptionAlgorithm: "RSA_OAEP", EncryptedValue: []byte{0x0a, 0xff}},
			})
		}, scriptUsePrefix + `CREATE COLUMN ENCRYPTION KEY [CEK]]1]
WITH VALUES
(
    COLUMN_MASTER_KEY = [CMK]]1],
    ALGORITHM = 'RSA_OAEP',
    ENCRYPTED_VALUE = 0x0AFF
)`},
		// A key mid-rotation is encrypted under two master keys and CREATE has
		// to restate both — one comma, and every value repeated in full.
		{"CreateColumnEncryptionKey mid-rotation", func(c context.Context) error {
			return scriptTestDB().CreateColumnEncryptionKeyContext(c, "CEK1", []ColumnEncryptionKeyValue{
				{MasterKeyName: "CMK1", EncryptionAlgorithm: "RSA_OAEP", EncryptedValue: []byte{0x01}},
				{MasterKeyName: "CMK2", EncryptionAlgorithm: "RSA_OAEP", EncryptedValue: []byte{0x02}},
			})
		}, scriptUsePrefix + `CREATE COLUMN ENCRYPTION KEY [CEK1]
WITH VALUES
(
    COLUMN_MASTER_KEY = [CMK1],
    ALGORITHM = 'RSA_OAEP',
    ENCRYPTED_VALUE = 0x01
),
(
    COLUMN_MASTER_KEY = [CMK2],
    ALGORITHM = 'RSA_OAEP',
    ENCRYPTED_VALUE = 0x02
)`},
	})
}

// TestScriptPermissionOptionWrites pins the WITH GRANT OPTION / CASCADE /
// GRANT OPTION FOR forms, which differ from the plain Grant/Deny/Revoke only
// in the clause each option appends — and which are where a wrong clause is
// hardest to notice, since the statement still runs and still changes
// permissions, just not the ones asked for.
func TestScriptPermissionOptionWrites(t *testing.T) {
	withGrant := PermissionOptions{WithGrantOption: true}
	cascadeOnly := PermissionOptions{Cascade: true}
	revokeGrantOption := PermissionOptions{Cascade: true, GrantOptionOnly: true}

	runScriptCases(t, []scriptCase{
		{"GrantSchemaPermissionWithOptions", func(c context.Context) error {
			return scriptTestDB().GrantSchemaPermissionWithOptionsContext(c, "sa]les", PermSelect, "o'brien", withGrant)
		}, scriptUsePrefix + "GRANT SELECT ON SCHEMA::[sa]]les] TO [o'brien] WITH GRANT OPTION"},
		{"RevokeSchemaPermissionWithOptions", func(c context.Context) error {
			return scriptTestDB().RevokeSchemaPermissionWithOptionsContext(c, "sa]les", PermSelect, "o'brien", revokeGrantOption)
		}, scriptUsePrefix + "REVOKE GRANT OPTION FOR SELECT ON SCHEMA::[sa]]les] FROM [o'brien] CASCADE"},
		{"DenyDatabasePermissionWithOptions", func(c context.Context) error {
			return scriptTestDB().DenyDatabasePermissionWithOptionsContext(c, "CREATE TABLE", "o'brien", cascadeOnly)
		}, scriptUsePrefix + "DENY CREATE TABLE TO [o'brien] CASCADE"},
		{"RevokeDatabasePermissionWithOptions", func(c context.Context) error {
			return scriptTestDB().RevokeDatabasePermissionWithOptionsContext(c, "CREATE TABLE", "o'brien", revokeGrantOption)
		}, scriptUsePrefix + "REVOKE GRANT OPTION FOR CREATE TABLE FROM [o'brien] CASCADE"},
		{"DenyServerPermissionWithOptions", func(c context.Context) error {
			return (&Server{}).DenyServerPermissionWithOptionsContext(c, "VIEW SERVER STATE", "o'brien", cascadeOnly)
		}, "USE master; DENY VIEW SERVER STATE TO [o'brien] CASCADE"},
		{"DenyColumnPermission", func(c context.Context) error {
			return scriptTestDB().DenyColumnPermissionContext(c, "dbo", "Sales.Archive", PermSelect, []string{"a]b", "c'd"}, "o'brien")
		}, scriptUsePrefix + "DENY SELECT ([a]]b], [c'd]) ON [dbo].[Sales.Archive] TO [o'brien]"},
		{"DenyColumnPermissionWithOptions", func(c context.Context) error {
			return scriptTestDB().DenyColumnPermissionWithOptionsContext(c, "dbo", "Sales.Archive", PermSelect, []string{"a]b"}, "o'brien", cascadeOnly)
		}, scriptUsePrefix + "DENY SELECT ([a]]b]) ON [dbo].[Sales.Archive] TO [o'brien] CASCADE"},
		{"RevokeColumnPermissionWithOptions", func(c context.Context) error {
			return scriptTestDB().RevokeColumnPermissionWithOptionsContext(c, "dbo", "Sales.Archive", PermSelect, []string{"a]b"}, "o'brien", revokeGrantOption)
		}, scriptUsePrefix + "REVOKE GRANT OPTION FOR SELECT ([a]]b]) ON [dbo].[Sales.Archive] FROM [o'brien] CASCADE"},
	})
}

// TestCreateUserRefusesAnEmptyLogin pins the guard on the one parameter
// CreateUserContext cannot quote its way out of. quoteIdent("") is "[]", so
// an empty login produced "CREATE USER [x] FOR LOGIN []" — syntactically
// valid to gosmo and rejected by the server with a message naming an empty
// login the caller never typed. A user with no login is CREATE USER ...
// WITHOUT LOGIN, a different statement; refusing here rather than guessing
// which was meant keeps that an explicit choice.
func TestCreateUserRefusesAnEmptyLogin(t *testing.T) {
	ctx, script := WithScript(context.Background())
	err := scriptTestDB().CreateUserContext(ctx, "o'brien", "", "dbo")
	if err == nil {
		t.Fatalf("CreateUserContext with an empty login returned nil, want an error")
	}
	if !strings.Contains(err.Error(), "login name") {
		t.Errorf("error = %v, want it to name the missing login", err)
	}
	if len(script.Statements) != 0 {
		t.Errorf("Statements = %q, want none", script.Statements)
	}
}

// TestCreateColumnMasterKeyRefusesEnclaveComputationsWithoutASignature pins the
// two refusals rather than the statements. ENCLAVE_COMPUTATIONS takes a
// signature the client computes from the master key's private key — the boolean
// spelling this package emitted until 2026-08-21 (ENCLAVE_COMPUTATIONS = YES)
// is not syntax SQL Server accepts, so a caller asking for one has to be sent
// to CreateColumnMasterKeyWithSignature instead of shipped a statement that
// fails at the server.
func TestCreateColumnMasterKeyRefusesEnclaveComputationsWithoutASignature(t *testing.T) {
	for _, c := range []struct {
		name string
		call func(context.Context) error
		want string
	}{
		{"bool form asking for enclave computations", func(c context.Context) error {
			return scriptTestDB().CreateColumnMasterKeyContext(c, "CMK1", "MSSQL_CERTIFICATE_STORE", "CurrentUser/my/ab", true)
		}, "CreateColumnMasterKeyWithSignature"},
		{"signature form with an empty signature", func(c context.Context) error {
			return scriptTestDB().CreateColumnMasterKeyWithSignatureContext(c, "CMK1", "MSSQL_CERTIFICATE_STORE", "CurrentUser/my/ab", nil)
		}, "signature is empty"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, script := WithScript(context.Background())
			err := c.call(ctx)
			if err == nil {
				t.Fatalf("no error; statements: %v", script.Statements)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to mention %q", err, c.want)
			}
			if len(script.Statements) != 0 {
				t.Errorf("emitted %d statement(s), want none:\n%s",
					len(script.Statements), strings.Join(script.Statements, "\n---\n"))
			}
		})
	}
}

// TestCreateColumnEncryptionKeyRefusesAnIncompleteValue pins the guards rather
// than a statement. Every part of a WITH VALUES entry is required by the
// server, and an empty one quotes into syntax it rejects only when the key is
// first used to decrypt a column — long after the create appeared to succeed.
func TestCreateColumnEncryptionKeyRefusesAnIncompleteValue(t *testing.T) {
	good := ColumnEncryptionKeyValue{MasterKeyName: "CMK1", EncryptionAlgorithm: "RSA_OAEP", EncryptedValue: []byte{0x01}}
	blank := func(edit func(*ColumnEncryptionKeyValue)) []ColumnEncryptionKeyValue {
		v := good
		edit(&v)
		return []ColumnEncryptionKeyValue{v}
	}
	for _, c := range []struct {
		name   string
		values []ColumnEncryptionKeyValue
		want   string
	}{
		{"no values at all", nil, "at least one encrypted value"},
		{"no master key", blank(func(v *ColumnEncryptionKeyValue) { v.MasterKeyName = "" }), "no column master key"},
		{"no algorithm", blank(func(v *ColumnEncryptionKeyValue) { v.EncryptionAlgorithm = "" }), "no encryption algorithm"},
		{"no encrypted value", blank(func(v *ColumnEncryptionKeyValue) { v.EncryptedValue = nil }), "no encrypted value"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, script := WithScript(context.Background())
			err := scriptTestDB().CreateColumnEncryptionKeyContext(ctx, "CEK1", c.values)
			if err == nil {
				t.Fatalf("no error; statements: %v", script.Statements)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to mention %q", err, c.want)
			}
			if len(script.Statements) != 0 {
				t.Errorf("emitted %d statement(s), want none:\n%s",
					len(script.Statements), strings.Join(script.Statements, "\n---\n"))
			}
		})
	}
}
