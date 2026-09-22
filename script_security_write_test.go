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
	serverRole := func() *ServerRole { return &ServerRole{server: &Server{}, Name: "ro'le"} }
	securityPolicy := func() *SecurityPolicy {
		return &SecurityPolicy{db: scriptTestDB(), Schema: "Se]c", Name: "sp'1"}
	}

	runScriptCases(t, []scriptCase{
		// --- database principals
		{"CreateSchema", func(c context.Context) error {
			return scriptTestDB().CreateSchema(c, "sa]les", "o'brien")
		}, scriptUsePrefix + "CREATE SCHEMA [sa]]les] AUTHORIZATION [o'brien]"},
		{"CreateSchema without an owner", func(c context.Context) error {
			return scriptTestDB().CreateSchema(c, "sa]les", "")
		}, scriptUsePrefix + "CREATE SCHEMA [sa]]les]"},
		{"CreateUser", func(c context.Context) error {
			return scriptTestDB().CreateUser(c, "o'brien", `DOM\o]b`, "sa]les")
		}, scriptUsePrefix + `CREATE USER [o'brien] FOR LOGIN [DOM\o]]b] WITH DEFAULT_SCHEMA = [sa]]les]`},
		{"CreateUser without a default schema", func(c context.Context) error {
			return scriptTestDB().CreateUser(c, "o'brien", "app_login", "")
		}, scriptUsePrefix + "CREATE USER [o'brien] FOR LOGIN [app_login]"},
		{"AddRoleMember", func(c context.Context) error {
			return scriptTestDB().AddRoleMember(c, "db_own]er", "o'brien")
		}, scriptUsePrefix + "ALTER ROLE [db_own]]er] ADD MEMBER [o'brien]"},
		{"RemoveRoleMember", func(c context.Context) error {
			return scriptTestDB().RemoveRoleMember(c, "db_own]er", "o'brien")
		}, scriptUsePrefix + "ALTER ROLE [db_own]]er] DROP MEMBER [o'brien]"},
		{"SetOwner", func(c context.Context) error {
			return scriptTestDB().SetOwner(c, "o'brien")
		}, "ALTER AUTHORIZATION ON DATABASE::[App'DB] TO [o'brien]"},

		// --- User
		{"User AddToRole", func(c context.Context) error {
			return user().AddToRole(c, "db_datar]eader")
		}, scriptUsePrefix + "ALTER ROLE [db_datar]]eader] ADD MEMBER [o'brien]"},
		{"User RemoveFromRole", func(c context.Context) error {
			return user().RemoveFromRole(c, "db_datar]eader")
		}, scriptUsePrefix + "ALTER ROLE [db_datar]]eader] DROP MEMBER [o'brien]"},
		{"User SetDefaultSchema", func(c context.Context) error {
			return user().SetDefaultSchema(c, "sa]les")
		}, scriptUsePrefix + "ALTER USER [o'brien] WITH DEFAULT_SCHEMA = [sa]]les]"},
		{"User SetLogin", func(c context.Context) error {
			return user().SetLogin(c, `DOM\o]b`)
		}, scriptUsePrefix + `ALTER USER [o'brien] WITH LOGIN = [DOM\o]]b]`},
		{"User Grant", func(c context.Context) error {
			return user().Grant(c, PermSelect, "dbo", "Sales.Archive")
		}, scriptUsePrefix + "GRANT SELECT ON [dbo].[Sales.Archive] TO [o'brien]"},
		{"User Deny", func(c context.Context) error {
			return user().Deny(c, PermSelect, "dbo", "Sales.Archive")
		}, scriptUsePrefix + "DENY SELECT ON [dbo].[Sales.Archive] TO [o'brien]"},
		{"User Revoke", func(c context.Context) error {
			return user().Revoke(c, PermSelect, "dbo", "Sales.Archive")
		}, scriptUsePrefix + "REVOKE SELECT ON [dbo].[Sales.Archive] FROM [o'brien]"},

		// --- Login
		{"Schema Drop", func(c context.Context) error {
			return (&Schema{db: scriptTestDB(), Name: "sa]les"}).Drop(c)
		}, scriptUsePrefix + "DROP SCHEMA [sa]]les]"},
		{"Schema ChangeOwner", func(c context.Context) error {
			return (&Schema{db: scriptTestDB(), Name: "sa]les", Owner: "dbo"}).ChangeOwner(c, "o'brien")
		}, scriptUsePrefix + "ALTER AUTHORIZATION ON SCHEMA::[sa]]les] TO [o'brien]"},
		{"User Drop", func(c context.Context) error {
			return user().Drop(c)
		}, scriptUsePrefix + "DROP USER [o'brien]"},
		{"User Rename", func(c context.Context) error {
			return user().Rename(c, "o]b")
		}, scriptUsePrefix + "ALTER USER [o'brien] WITH NAME = [o]]b]"},
		{"Login Enable", func(c context.Context) error {
			return login().Enable(c)
		}, "ALTER LOGIN [o'brien] ENABLE"},
		{"Login Disable", func(c context.Context) error {
			return login().Disable(c)
		}, "ALTER LOGIN [o'brien] DISABLE"},
		{"Login AddServerRoleMember", func(c context.Context) error {
			return login().AddServerRoleMember(c, "sys]admin")
		}, "ALTER SERVER ROLE [sys]]admin] ADD MEMBER [o'brien]"},
		{"Login RemoveServerRoleMember", func(c context.Context) error {
			return login().RemoveServerRoleMember(c, "sys]admin")
		}, "ALTER SERVER ROLE [sys]]admin] DROP MEMBER [o'brien]"},
		{"Login SetDefaultDatabase", func(c context.Context) error {
			return login().SetDefaultDatabase(c, "App'DB")
		}, "ALTER LOGIN [o'brien] WITH DEFAULT_DATABASE = [App'DB]"},
		{"Login SetDefaultLanguage", func(c context.Context) error {
			return login().SetDefaultLanguage(c, "us_english")
		}, "ALTER LOGIN [o'brien] WITH DEFAULT_LANGUAGE = [us_english]"},
		{"Login SetPasswordPolicy", func(c context.Context) error {
			return login().SetPasswordPolicy(c, true, false)
		}, "ALTER LOGIN [o'brien] WITH CHECK_POLICY = ON, CHECK_EXPIRATION = OFF"},
		{"Login Rename", func(c context.Context) error {
			return login().Rename(c, "o]b")
		}, "ALTER LOGIN [o'brien] WITH NAME = [o]]b]"},
		{
			// The password is a literal, not an identifier: an apostrophe in
			// it must double, and HASHED is deliberately never emitted — it
			// would tell the server the value is one of its own hash formats
			// rather than cleartext.
			"Login ChangePassword", func(c context.Context) error {
				return login().ChangePassword(c, "p'wd")
			}, "ALTER LOGIN [o'brien] WITH PASSWORD = N'p''wd'"},
		{
			// MUST_CHANGE and UNLOCK follow the password space-separated —
			// they are password-clause modifiers, not comma-separated set
			// options — and MUST_CHANGE drags CHECK_EXPIRATION = ON in after
			// the comma.
			"Login ChangePasswordWithOptions", func(c context.Context) error {
				return login().ChangePasswordWithOptions(c, "p'wd", true, true)
			}, "ALTER LOGIN [o'brien] WITH PASSWORD = N'p''wd' MUST_CHANGE UNLOCK, CHECK_EXPIRATION = ON"},
		{"Login MapCredential", func(c context.Context) error {
			return login().MapCredential(c, "cred]1")
		}, "ALTER LOGIN [o'brien] ADD CREDENTIAL [cred]]1]"},
		{"Login UnmapCredential", func(c context.Context) error {
			return login().UnmapCredential(c, "cred]1")
		}, "ALTER LOGIN [o'brien] DROP CREDENTIAL [cred]]1]"},
		{"Login Drop", func(c context.Context) error {
			return login().Drop(c)
		}, "DROP LOGIN [o'brien]"},
		{"ServerRole Rename", func(c context.Context) error {
			return serverRole().Rename(c, "r]2")
		}, "ALTER SERVER ROLE [ro'le] WITH NAME = [r]]2]"},
		{"ServerRole ChangeOwner", func(c context.Context) error {
			return serverRole().ChangeOwner(c, "o'brien")
		}, "ALTER AUTHORIZATION ON SERVER ROLE::[ro'le] TO [o'brien]"},
		{"ServerRole Drop", func(c context.Context) error {
			return serverRole().Drop(c)
		}, "DROP SERVER ROLE [ro'le]"},
		{"SecurityPolicy Enable", func(c context.Context) error {
			return securityPolicy().Enable(c)
		}, scriptUsePrefix + "ALTER SECURITY POLICY [Se]]c].[sp'1] WITH (STATE = ON)"},
		{"SecurityPolicy Disable", func(c context.Context) error {
			return securityPolicy().Disable(c)
		}, scriptUsePrefix + "ALTER SECURITY POLICY [Se]]c].[sp'1] WITH (STATE = OFF)"},
		{"Server DropLogin", func(c context.Context) error {
			return (&Server{}).DropLogin(c, "o'brien")
		}, "DROP LOGIN [o'brien]"},

		// --- keys and certificates
		{"CreateCertificate", func(c context.Context) error {
			return scriptTestDB().CreateCertificate(c, CertificateSpec{
				Name: "Cert]1", Authorization: "o'brien", Subject: "gossms o'brien",
			})
		}, scriptUsePrefix + "CREATE CERTIFICATE [Cert]]1] AUTHORIZATION [o'brien] WITH SUBJECT = N'gossms o''brien'"},
		{"CreateMasterKey", func(c context.Context) error {
			return scriptTestDB().CreateMasterKey(c, "p'wd")
		}, scriptUsePrefix + "CREATE MASTER KEY ENCRYPTION BY PASSWORD = N'p''wd'"},
		{"CreateColumnMasterKeyWithSignature", func(c context.Context) error {
			return scriptTestDB().CreateColumnMasterKeyWithSignature(c, "CMK]1", "MSSQL_CERTIFICATE_STORE", "CurrentUser/my/a'b", []byte{0x0a, 0xff})
		}, scriptUsePrefix + `
CREATE COLUMN MASTER KEY [CMK]]1]
WITH (
    KEY_STORE_PROVIDER_NAME = N'MSSQL_CERTIFICATE_STORE',
    KEY_PATH = N'CurrentUser/my/a''b',
    ENCLAVE_COMPUTATIONS (SIGNATURE = 0x0AFF)
)`},
		{"CreateColumnMasterKey without enclave computations", func(c context.Context) error {
			return scriptTestDB().CreateColumnMasterKey(c, "CMK]1", "MSSQL_CERTIFICATE_STORE", "CurrentUser/my/a'b", false)
		}, scriptUsePrefix + `
CREATE COLUMN MASTER KEY [CMK]]1]
WITH (
    KEY_STORE_PROVIDER_NAME = N'MSSQL_CERTIFICATE_STORE',
    KEY_PATH = N'CurrentUser/my/a''b'
)`},
		{"CreateColumnEncryptionKey", func(c context.Context) error {
			return scriptTestDB().CreateColumnEncryptionKey(c, "CEK]1", []ColumnEncryptionKeyValue{
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
			return scriptTestDB().CreateColumnEncryptionKey(c, "CEK1", []ColumnEncryptionKeyValue{
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
		// Rotation: the second value is added by ALTER, and the retired one
		// dropped by naming its master key alone — the ciphertext is not
		// restated on the way out.
		{"ColumnEncryptionKey.AddValue", func(c context.Context) error {
			return scriptTestCEK().AddValue(c, ColumnEncryptionKeyValue{
				MasterKeyName: "CMK]2", EncryptionAlgorithm: "RSA_OAEP", EncryptedValue: []byte{0x0a, 0xff}})
		}, scriptUsePrefix + `ALTER COLUMN ENCRYPTION KEY [CEK]]1]
ADD VALUE
(
    COLUMN_MASTER_KEY = [CMK]]2],
    ALGORITHM = 'RSA_OAEP',
    ENCRYPTED_VALUE = 0x0AFF
)`},
		{"ColumnEncryptionKey.DropValue", func(c context.Context) error {
			return scriptTestCEK().DropValue(c, "CMK]1")
		}, scriptUsePrefix + `ALTER COLUMN ENCRYPTION KEY [CEK]]1]
DROP VALUE
(
    COLUMN_MASTER_KEY = [CMK]]1]
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
			return scriptTestDB().GrantSchemaPermissionWithOptions(c, "sa]les", PermSelect, "o'brien", withGrant)
		}, scriptUsePrefix + "GRANT SELECT ON SCHEMA::[sa]]les] TO [o'brien] WITH GRANT OPTION"},
		{"RevokeSchemaPermissionWithOptions", func(c context.Context) error {
			return scriptTestDB().RevokeSchemaPermissionWithOptions(c, "sa]les", PermSelect, "o'brien", revokeGrantOption)
		}, scriptUsePrefix + "REVOKE GRANT OPTION FOR SELECT ON SCHEMA::[sa]]les] FROM [o'brien] CASCADE"},
		{"DenyDatabasePermissionWithOptions", func(c context.Context) error {
			return scriptTestDB().DenyDatabasePermissionWithOptions(c, "CREATE TABLE", "o'brien", cascadeOnly)
		}, scriptUsePrefix + "DENY CREATE TABLE TO [o'brien] CASCADE"},
		{"RevokeDatabasePermissionWithOptions", func(c context.Context) error {
			return scriptTestDB().RevokeDatabasePermissionWithOptions(c, "CREATE TABLE", "o'brien", revokeGrantOption)
		}, scriptUsePrefix + "REVOKE GRANT OPTION FOR CREATE TABLE FROM [o'brien] CASCADE"},
		{"DenyServerPermissionWithOptions", func(c context.Context) error {
			return (&Server{}).DenyServerPermissionWithOptions(c, "VIEW SERVER STATE", "o'brien", cascadeOnly)
		}, "USE master; DENY VIEW SERVER STATE TO [o'brien] CASCADE"},
		{"DenyColumnPermission", func(c context.Context) error {
			return scriptTestDB().DenyColumnPermission(c, "dbo", "Sales.Archive", PermSelect, []string{"a]b", "c'd"}, "o'brien")
		}, scriptUsePrefix + "DENY SELECT ([a]]b], [c'd]) ON [dbo].[Sales.Archive] TO [o'brien]"},
		{"DenyColumnPermissionWithOptions", func(c context.Context) error {
			return scriptTestDB().DenyColumnPermissionWithOptions(c, "dbo", "Sales.Archive", PermSelect, []string{"a]b"}, "o'brien", cascadeOnly)
		}, scriptUsePrefix + "DENY SELECT ([a]]b]) ON [dbo].[Sales.Archive] TO [o'brien] CASCADE"},
		{"RevokeColumnPermissionWithOptions", func(c context.Context) error {
			return scriptTestDB().RevokeColumnPermissionWithOptions(c, "dbo", "Sales.Archive", PermSelect, []string{"a]b"}, "o'brien", revokeGrantOption)
		}, scriptUsePrefix + "REVOKE GRANT OPTION FOR SELECT ([a]]b]) ON [dbo].[Sales.Archive] FROM [o'brien] CASCADE"},
	})
}

// TestCreateUserRefusesAnEmptyLogin pins the guard on the one parameter
// CreateUser cannot quote its way out of. quoteIdent("") is "[]", so
// an empty login produced "CREATE USER [x] FOR LOGIN []" — syntactically
// valid to gosmo and rejected by the server with a message naming an empty
// login the caller never typed. A user with no login is CREATE USER ...
// WITHOUT LOGIN, a different statement; refusing here rather than guessing
// which was meant keeps that an explicit choice.
func TestCreateUserRefusesAnEmptyLogin(t *testing.T) {
	ctx, script := WithScript(context.Background())
	err := scriptTestDB().CreateUser(ctx, "o'brien", "", "dbo")
	if err == nil {
		t.Fatalf("CreateUser with an empty login returned nil, want an error")
	}
	if !strings.Contains(err.Error(), "login name") {
		t.Errorf("error = %v, want it to name the missing login", err)
	}
	if len(script.Statements()) != 0 {
		t.Errorf("Statements = %q, want none", script.Statements())
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
			return scriptTestDB().CreateColumnMasterKey(c, "CMK1", "MSSQL_CERTIFICATE_STORE", "CurrentUser/my/ab", true)
		}, "CreateColumnMasterKeyWithSignature"},
		{"signature form with an empty signature", func(c context.Context) error {
			return scriptTestDB().CreateColumnMasterKeyWithSignature(c, "CMK1", "MSSQL_CERTIFICATE_STORE", "CurrentUser/my/ab", nil)
		}, "signature is empty"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, script := WithScript(context.Background())
			err := c.call(ctx)
			if err == nil {
				t.Fatalf("no error; statements: %v", script.Statements())
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to mention %q", err, c.want)
			}
			if len(script.Statements()) != 0 {
				t.Errorf("emitted %d statement(s), want none:\n%s",
					len(script.Statements()), strings.Join(script.Statements(), "\n---\n"))
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
			err := scriptTestDB().CreateColumnEncryptionKey(ctx, "CEK1", c.values)
			if err == nil {
				t.Fatalf("no error; statements: %v", script.Statements())
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to mention %q", err, c.want)
			}
			if len(script.Statements()) != 0 {
				t.Errorf("emitted %d statement(s), want none:\n%s",
					len(script.Statements()), strings.Join(script.Statements(), "\n---\n"))
			}
		})
	}
}

// scriptTestCEK is a column encryption key handle bound to scriptTestDB, for
// the two ALTER cases. Its name carries a bracket for the same reason every
// other name in these files does.
func scriptTestCEK() *ColumnEncryptionKey {
	return &ColumnEncryptionKey{db: scriptTestDB(), Name: "CEK]1",
		MasterKeyName: "CMK]1", EncryptionAlgorithm: "RSA_OAEP",
		Values: []*ColumnEncryptionKeyValue{
			{MasterKeyName: "CMK]1", EncryptionAlgorithm: "RSA_OAEP", EncryptedValue: []byte{0x01}},
		}}
}

// TestColumnEncryptionKeyValueGuards pins the same completeness check on the
// ALTER path that CREATE has, plus DropValue's, and that neither emits a
// statement when it refuses.
func TestColumnEncryptionKeyValueGuards(t *testing.T) {
	good := ColumnEncryptionKeyValue{MasterKeyName: "CMK2", EncryptionAlgorithm: "RSA_OAEP", EncryptedValue: []byte{0x02}}
	add := func(edit func(*ColumnEncryptionKeyValue)) func(context.Context) error {
		v := good
		edit(&v)
		return func(c context.Context) error { return scriptTestCEK().AddValue(c, v) }
	}
	for _, c := range []struct {
		name string
		call func(context.Context) error
		want string
	}{
		{"add with no master key", add(func(v *ColumnEncryptionKeyValue) { v.MasterKeyName = "" }), "no column master key"},
		{"add with no algorithm", add(func(v *ColumnEncryptionKeyValue) { v.EncryptionAlgorithm = "" }), "no encryption algorithm"},
		{"add with no encrypted value", add(func(v *ColumnEncryptionKeyValue) { v.EncryptedValue = nil }), "no encrypted value"},
		{"drop with no master key", func(c context.Context) error { return scriptTestCEK().DropValue(c, "") },
			"column master key name is required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, script := WithScript(context.Background())
			err := c.call(ctx)
			if err == nil {
				t.Fatalf("no error; statements: %v", script.Statements())
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to mention %q", err, c.want)
			}
			if len(script.Statements()) != 0 {
				t.Errorf("emitted %d statement(s), want none:\n%s",
					len(script.Statements()), strings.Join(script.Statements(), "\n---\n"))
			}
		})
	}
}

// TestColumnEncryptionKeyScriptingLeavesTheHandle pins the other half of the
// mirroring in column_encryption_key_write_test.go: under WithScript nothing
// reaches the server, so Values and the summary must keep describing the
// catalog. A handle mutated here would defeat a caller's own pre-flight checks
// on the next pass — gossms's Script Changes re-runs a dirty page's apply, and
// its rotation guard counts len(Values).
func TestColumnEncryptionKeyScriptingLeavesTheHandle(t *testing.T) {
	ctx, script := WithScript(context.Background())
	cek := scriptTestCEK()

	if err := cek.AddValue(ctx, ColumnEncryptionKeyValue{
		MasterKeyName: "CMK]2", EncryptionAlgorithm: "RSA_OAEP_256", EncryptedValue: []byte{0x02}}); err != nil {
		t.Fatalf("AddValue: %v", err)
	}
	if err := cek.DropValue(ctx, "CMK]1"); err != nil {
		t.Fatalf("DropValue: %v", err)
	}
	if len(script.Statements()) != 2 {
		t.Fatalf("captured %d statement(s), want 2", len(script.Statements()))
	}
	if len(cek.Values) != 1 || cek.Values[0].MasterKeyName != "CMK]1" {
		t.Errorf("Values = %+v, want the unchanged CMK]1 alone", cek.Values)
	}
	if cek.MasterKeyName != "CMK]1" || cek.EncryptionAlgorithm != "RSA_OAEP" {
		t.Errorf("summary = %q/%q, want the unchanged CMK]1/RSA_OAEP",
			cek.MasterKeyName, cek.EncryptionAlgorithm)
	}
}
