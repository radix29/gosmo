package gosmo_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/radix29/gosmo"
	"github.com/radix29/gosmo/gosmotest"
)

// -- what this file pins ------------------------------------------------------
//
// The scripters gossms's Script As reaches for a login, a database user, a
// certificate, the two key kinds and a table's index, foreign key, check
// constraint and statistic ran only under -tags livedb, so a regression in
// the read each makes, the column order it scans or the builder it feeds
// showed only on a live run. Each case serves the method's reads from rules
// keyed on that read's own text and arguments — no catch-all, so a read
// renamed, re-keyed or added fails as an unanswered query — and compares the
// whole script. Row values are chosen asymmetric (policy on, expiration off;
// one descending key column) so a scan out of column order changes the text.

// read matches a query containing text whose bound arguments are args.
func read(text string, args ...any) func(gosmotest.Query) bool {
	return func(q gosmotest.Query) bool {
		if !strings.Contains(q.Text, text) || len(q.Args) != len(args) {
			return false
		}
		for i, a := range args {
			if !reflect.DeepEqual(q.Arg(i), a) {
				return false
			}
		}
		return true
	}
}

// golden fails unless got is want, printing both whole.
func golden(t *testing.T, what, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s =\n%s\nwant\n%s", what, got, want)
	}
}

// scriptCase is one scripter's golden case: rules adds the reads it makes to
// a fresh Fake, run calls it on that Fake's server, want is the whole script.
type scriptCase struct {
	name  string
	rules func(f *gosmotest.Fake)
	run   func(ctx context.Context, srv *gosmo.Server) (string, error)
	want  string
}

// runScriptCases runs each case on its own Fake, so one case's rules cannot
// answer another's reads.
func runScriptCases(t *testing.T, cases []scriptCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := gosmotest.New(t)
			tc.rules(f)
			got, err := tc.run(context.Background(), f.Server())
			if err != nil {
				t.Fatal(err)
			}
			golden(t, tc.name, got, tc.want)
		})
	}
}

// noProps answers the extended-property read of one securable — class and
// major id — with none, so a case pins which securable the scripter reads
// them for.
func noProps(f *gosmotest.Fake, class, majorID int64) {
	f.OnFunc(read("FROM   sys.extended_properties ep\nWHERE  ep.class = @p1", class, majorID, int64(0)), gosmotest.Rows())
}

// app is the database every database-scoped case scripts in.
func app(srv *gosmo.Server) *gosmo.Scripter {
	return gosmo.NewScripter(srv.DatabaseRef("app"), gosmo.DefaultScriptOptions())
}

// onServer is the ServerScripter every server-scoped case scripts with.
func onServer(srv *gosmo.Server) *gosmo.ServerScripter {
	return gosmo.NewServerScripter(srv, gosmo.DefaultScriptOptions())
}

// loginRow is a loginSelect row: name, sid, type, disabled, default
// database and language, the policy and expiration flags.
func loginRow(name string, sid []byte, typ gosmo.PrincipalType, disabled bool, defDB, defLang any, policy, expiration bool) gosmotest.Responder {
	return gosmotest.Rows([]any{name, sid, string(typ), disabled, defDB, scriptWhen, scriptWhen, defLang, policy, expiration})
}

const loginRoles = "FROM   sys.server_role_members rm"

func TestScriptLoginGolden(t *testing.T) {
	f := gosmotest.New(t)
	f.OnFunc(read("FROM sys.server_principals sp", "app_login"),
		loginRow("app_login", []byte{0xAB, 0x01}, gosmo.PrincipalSQLLogin, true, "appdb", "us_english", true, false))
	// public is listed only to prove it is skipped: ADD MEMBER to it is refused.
	f.OnFunc(read(loginRoles, "app_login"), gosmotest.Rows([]any{"dbcreator"}, []any{"public"}, []any{"sysadmin"}))
	sc := gosmo.NewServerScripter(f.Server(), gosmo.DefaultScriptOptions())

	got, err := sc.ScriptLogin(context.Background(), "app_login")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "ScriptLogin", got, "IF SUSER_ID(N'app_login') IS NULL\n"+
		"CREATE LOGIN [app_login] WITH PASSWORD = N'<password, sysname, >', SID = 0xAB01, "+
		"DEFAULT_DATABASE = [appdb], DEFAULT_LANGUAGE = [us_english], CHECK_EXPIRATION = OFF, CHECK_POLICY = ON;\nGO\n"+
		"ALTER LOGIN [app_login] DISABLE;\nGO\n"+
		"ALTER SERVER ROLE [dbcreator] ADD MEMBER [app_login];\nGO\n"+
		"ALTER SERVER ROLE [sysadmin] ADD MEMBER [app_login];\nGO\n")
}

// A certificate-mapped login names its certificate from master, read by the
// login's SID; it takes no WITH options, so its default database is dropped.
func TestScriptLoginResolvesACertificateMapping(t *testing.T) {
	sid := []byte{0x01, 0x06, 0xFF}
	f := gosmotest.New(t)
	f.OnFunc(read("FROM sys.server_principals sp", "signer"),
		loginRow("signer", sid, gosmo.PrincipalCertificateMappedLogin, false, "master", nil, false, false))
	f.OnFunc(read("FROM master.sys.certificates    WHERE sid = @p1", sid), gosmotest.Rows([]any{"cert_sign"}))
	f.OnFunc(read(loginRoles, "signer"), gosmotest.Rows())
	sc := gosmo.NewServerScripter(f.Server(), gosmo.DefaultScriptOptions())

	got, err := sc.ScriptLogin(context.Background(), "signer")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "ScriptLogin", got, "IF SUSER_ID(N'signer') IS NULL\n"+
		"CREATE LOGIN [signer] FROM CERTIFICATE [cert_sign];\nGO\n")
}

// A DROP restores no memberships, so it reads none.
func TestScriptLoginDropReadsNoRoles(t *testing.T) {
	f := gosmotest.New(t)
	f.OnFunc(read("FROM sys.server_principals sp", "app_login"),
		loginRow("app_login", []byte{0xAB}, gosmo.PrincipalSQLLogin, false, "", "", true, true))
	f.On(loginRoles, gosmotest.Fail(errors.New("must not be read")))
	opts := gosmo.DefaultScriptOptions()
	opts.Verb = gosmo.ScriptDrop

	got, err := gosmo.NewServerScripter(f.Server(), opts).ScriptLogin(context.Background(), "app_login")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "ScriptLogin DROP", got, "IF SUSER_ID(N'app_login') IS NOT NULL\n    DROP LOGIN [app_login];\nGO\n")
}

// The user's extended properties are read at its own class and id (4, the
// principal_id) and restated at USER level.
func TestScriptUserGolden(t *testing.T) {
	f := gosmotest.New(t)
	f.OnFunc(read("FROM   sys.database_principals dp", "app_user"), gosmotest.Rows(
		[]any{"app_user", int64(5), "SQL_USER", "sales", scriptWhen, scriptWhen, "INSTANCE", []byte{0xAB, 0x01}, "app_login", false, nil}))
	f.OnFunc(read("FROM   sys.extended_properties ep\nWHERE  ep.class = @p1", int64(4), int64(5), int64(0)), gosmotest.Rows(
		[]any{"MS_Description", "nvarchar", int64(0), int64(0), int64(200), "Order entry"}))
	sc := gosmo.NewScripter(f.Server().DatabaseRef("app"), gosmo.DefaultScriptOptions())

	got, err := sc.ScriptUser(context.Background(), "app_user")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "ScriptUser", got, "IF DATABASE_PRINCIPAL_ID(N'app_user') IS NULL\n"+
		"CREATE USER [app_user] FOR LOGIN [app_login] WITH DEFAULT_SCHEMA = [sales];\nGO\n"+
		"IF NOT EXISTS (SELECT 1 FROM sys.fn_listextendedproperty(N'MS_Description', N'USER', N'app_user', NULL, NULL, NULL, NULL))\n"+
		"    EXEC sys.sp_addextendedproperty @name = N'MS_Description', @value = N'Order entry', "+
		"@level0type = N'USER', @level0name = N'app_user';\nGO\n")
}

func TestScriptCertificateGolden(t *testing.T) {
	f := gosmotest.New(t)
	f.OnFunc(read("FROM   sys.certificates c", "cert_sign"), gosmotest.Rows(
		[]any{"cert_sign", int64(256), int64(1), "CN=sign", "ENCRYPTED_BY_MASTER_KEY", scriptWhen, scriptWhen,
			[]byte{1, 2}, "dbo", "CN=sign", "1a 2b", int64(2048), false, nil, ""}))
	f.OnFunc(read("SELECT CERTENCODED(CERT_ID(@p1))", "cert_sign"), gosmotest.Rows([]any{[]byte{0x30, 0x82, 0x01}}))
	sc := gosmo.NewScripter(f.Server().DatabaseRef("app"), gosmo.DefaultScriptOptions())

	got, err := sc.ScriptCertificate(context.Background(), "cert_sign")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "ScriptCertificate", got, "/* The certificate's private key cannot be read from the server, so it is\n"+
		"   not scripted: this recreates the public certificate only, which can\n"+
		"   verify signatures and encrypt, but not sign or decrypt. */\n"+
		"IF NOT EXISTS (SELECT 1 FROM sys.certificates WHERE name = N'cert_sign')\n"+
		"CREATE CERTIFICATE [cert_sign] AUTHORIZATION [dbo]\n"+
		"    FROM BINARY = 0x308201\n"+
		"    ACTIVE FOR BEGIN_DIALOG = OFF;\nGO\n")
}

func TestScriptAsymmetricKeyGolden(t *testing.T) {
	f := gosmotest.New(t)
	f.OnFunc(read("FROM   sys.asymmetric_keys k", "ak_pub"), gosmotest.Rows(
		[]any{"ak_pub", int64(257), int64(1), "RSA_2048", int64(2048), "NO_PRIVATE_KEY", []byte{3, 4}, "dbo", "", ""}))
	sc := gosmo.NewScripter(f.Server().DatabaseRef("app"), gosmo.DefaultScriptOptions())

	got, err := sc.ScriptAsymmetricKey(context.Background(), "ak_pub")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "ScriptAsymmetricKey", got, "/* An asymmetric key cannot be recreated from what the server exposes, so\n"+
		"   this creates a NEW key pair with the same algorithm, not this key: what\n"+
		"   the original signed or encrypted will not verify or decrypt with it.\n"+
		"   The original holds only a public key; the result has a private key too. */\n"+
		"IF NOT EXISTS (SELECT 1 FROM sys.asymmetric_keys WHERE name = N'ak_pub')\n"+
		"CREATE ASYMMETRIC KEY [ak_pub] AUTHORIZATION [dbo]\n"+
		"    WITH ALGORITHM = RSA_2048;\nGO\n")
}

// A symmetric key's encryptions arrive one row each and are grouped onto the
// key; the script lists them in CREATE SYMMETRIC KEY's grammar order, not
// the rows'.
func TestScriptSymmetricKeyGolden(t *testing.T) {
	key := []any{"sk_data", int64(258), int64(1), "dbo", "AES_256", int64(256),
		"8f0c2a4e-0000-0000-0000-000000000002", scriptWhen, scriptWhen, ""}
	f := gosmotest.New(t)
	f.OnFunc(read("FROM   sys.symmetric_keys k", "sk_data"), gosmotest.Rows(
		append(key[:len(key):len(key)], "ENCRYPTION BY PASSWORD V2", nil, nil),
		append(key[:len(key):len(key)], "ENCRYPTION BY CERTIFICATE", []byte{1, 2}, "cert_sign")))
	sc := gosmo.NewScripter(f.Server().DatabaseRef("app"), gosmo.DefaultScriptOptions())

	got, err := sc.ScriptSymmetricKey(context.Background(), "sk_data")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "ScriptSymmetricKey", got, "/* A symmetric key's material cannot be read from the server, so this\n"+
		"   creates a NEW key with the same algorithm and encryptions, not this key:\n"+
		"   data encrypted with the original will not decrypt with it. Only a key\n"+
		"   created with KEY_SOURCE and IDENTITY_VALUE can be recreated, by adding\n"+
		"   both to the WITH clause with their original values. */\n"+
		"IF NOT EXISTS (SELECT 1 FROM sys.symmetric_keys WHERE name = N'sk_data')\n"+
		"CREATE SYMMETRIC KEY [sk_data] AUTHORIZATION [dbo]\n"+
		"    WITH ALGORITHM = AES_256\n"+
		"    ENCRYPTION BY CERTIFICATE [cert_sign],\n"+
		"        PASSWORD = N'"+gosmo.PasswordPlaceholder+"';\nGO\n")
}

// ordersID is the object_id of the table ordersFake serves.
const ordersID = int64(901)

// ordersFake serves [sales].[Orders] (object 901): the table row, and the
// rows props for its extended properties (none when omitted). Each test adds
// the read of the child it scripts.
func ordersFake(t *testing.T, props ...[]any) *gosmotest.Fake {
	f := gosmotest.New(t)
	f.OnFunc(read("FROM   sys.tables t", "sales", "Orders"), gosmotest.Rows(
		[]any{ordersID, "sales", "Orders", scriptWhen, scriptWhen, false, false, false, false, false, false, false}))
	f.OnFunc(read("FROM   sys.extended_properties ep", ordersID, true), gosmotest.Rows(props...))
	return f
}

// indexRow is an indexListSelect row of a rowstore index on the default
// filegroup; the arguments are the columns that vary here.
func indexRow(name string, id int64, typ string, unique, pk bool, fill int64, filter, compression string) []any {
	return []any{name, id, typ, unique, pk, false, false, false, fill, filter,
		false, false, true, true, `[{"n":1,"c":"` + compression + `","x":false}]`, "PRIMARY", false, true, "",
		false, false, false, int64(0), int64(0), false, "", "", false, "", "", nil, nil, nil, nil, "", "", "", "", int64(0)}
}

// indexFake is ordersFake with a clustered primary key and a filtered,
// compressed nonclustered index with a descending key and an included
// column, every index's columns read in one query.
func indexFake(t *testing.T, props ...[]any) *gosmotest.Fake {
	f := ordersFake(t, props...)
	f.OnFunc(read("FROM   sys.indexes i\nLEFT   JOIN sys.hash_indexes", ordersID), gosmotest.Rows(
		indexRow("PK_Orders", 1, "CLUSTERED", true, true, 0, "", "NONE"),
		indexRow("IX_Orders_Cust", 2, "NONCLUSTERED", false, false, 90, "[Status]=(1)", "PAGE")))
	f.OnFunc(read("FROM   sys.index_columns ic", ordersID), gosmotest.Rows(
		[]any{int64(1), "OrderID", false, false, int64(0)},
		[]any{int64(2), "CustomerID", false, false, int64(0)},
		[]any{int64(2), "OrderDate", true, false, int64(0)},
		[]any{int64(2), "Total", false, true, int64(0)}))
	f.OnFunc(read("AND i.index_id IN (0, 1)", ordersID), gosmotest.Rows([]any{"PRIMARY", false, true, ""}))
	return f
}

func TestScriptIndexGolden(t *testing.T) {
	sc := gosmo.NewScripter(indexFake(t).Server().DatabaseRef("app"), gosmo.DefaultScriptOptions())

	got, err := sc.ScriptIndex(context.Background(), "sales", "Orders", "IX_Orders_Cust")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "ScriptIndex", got, "IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'IX_Orders_Cust' AND object_id = OBJECT_ID(N'[sales].[Orders]'))\n"+
		"CREATE NONCLUSTERED INDEX [IX_Orders_Cust]\n"+
		"    ON [sales].[Orders] ([CustomerID] ASC, [OrderDate] DESC)\n"+
		"    INCLUDE ([Total])\n"+
		"    WHERE [Status]=(1)\n"+
		"    WITH (FILLFACTOR = 90, DATA_COMPRESSION = PAGE);\nGO\n\n")
}

// A primary key's index is scripted as the constraint it is, with the
// properties on it at INDEX or CONSTRAINT level — not another child's.
func TestScriptIndexOfAPrimaryKeyIsItsConstraint(t *testing.T) {
	prop := func(l2type, l2name, name string) []any {
		return []any{"sales", "TABLE", "Orders", l2type, l2name, name, "nvarchar", int64(0), int64(0), int64(100), "v"}
	}
	f := indexFake(t, prop("CONSTRAINT", "PK_Orders", "on_pk"), prop("INDEX", "IX_Orders_Cust", "on_ix"))
	sc := gosmo.NewScripter(f.Server().DatabaseRef("app"), gosmo.DefaultScriptOptions())

	got, err := sc.ScriptIndex(context.Background(), "sales", "Orders", "PK_Orders")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "ScriptIndex PK", got, "IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE name = N'PK_Orders' AND parent_object_id = OBJECT_ID(N'[sales].[Orders]'))\n"+
		"ALTER TABLE [sales].[Orders]\n"+
		"    ADD CONSTRAINT [PK_Orders] PRIMARY KEY CLUSTERED ([OrderID] ASC);\nGO\n\n"+
		"IF NOT EXISTS (SELECT 1 FROM sys.fn_listextendedproperty(N'on_pk', N'SCHEMA', N'sales', N'TABLE', N'Orders', N'CONSTRAINT', N'PK_Orders'))\n"+
		"    EXEC sys.sp_addextendedproperty @name = N'on_pk', @value = N'v', @level0type = N'SCHEMA', @level0name = N'sales', "+
		"@level1type = N'TABLE', @level1name = N'Orders', @level2type = N'CONSTRAINT', @level2name = N'PK_Orders';\nGO\n")
}

func TestScriptForeignKeyGolden(t *testing.T) {
	f := ordersFake(t)
	f.OnFunc(read("FROM   sys.foreign_keys fk", ordersID), gosmotest.Rows(
		[]any{"FK_Orders_Customers", false, true, false, "CASCADE", "NO_ACTION", "sales", "Customers",
			`[{"v":"CustomerID"}]`, `[{"v":"ID"}]`}))
	sc := gosmo.NewScripter(f.Server().DatabaseRef("app"), gosmo.DefaultScriptOptions())

	got, err := sc.ScriptForeignKey(context.Background(), "sales", "Orders", "FK_Orders_Customers")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "ScriptForeignKey", got, "IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE name = N'FK_Orders_Customers' AND parent_object_id = OBJECT_ID(N'[sales].[Orders]'))\n"+
		"ALTER TABLE [sales].[Orders] WITH NOCHECK\n"+
		"    ADD CONSTRAINT [FK_Orders_Customers]\n"+
		"    FOREIGN KEY ([CustomerID])\n"+
		"    REFERENCES [sales].[Customers] ([ID])\n"+
		"    ON DELETE CASCADE;\nGO\n\n")
}

// A disabled check is added WITH NOCHECK, then switched off.
func TestScriptCheckConstraintGolden(t *testing.T) {
	f := ordersFake(t)
	f.OnFunc(read("FROM   sys.check_constraints cc", ordersID), gosmotest.Rows(
		[]any{"CK_Orders_Total", "([Total]>=(0))", true, "Total", true, false}))
	sc := gosmo.NewScripter(f.Server().DatabaseRef("app"), gosmo.DefaultScriptOptions())

	got, err := sc.ScriptCheckConstraint(context.Background(), "sales", "Orders", "CK_Orders_Total")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "ScriptCheckConstraint", got, "IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE name = N'CK_Orders_Total' AND parent_object_id = OBJECT_ID(N'[sales].[Orders]'))\n"+
		"ALTER TABLE [sales].[Orders] WITH NOCHECK\n"+
		"    ADD CONSTRAINT [CK_Orders_Total] CHECK ([Total]>=(0));\nGO\n"+
		"ALTER TABLE [sales].[Orders] NOCHECK CONSTRAINT [CK_Orders_Total];\nGO\n\n")
}

// statFake is ordersFake with one filtered NORECOMPUTE statistic over two
// columns in stat-column order, auto- and user-created as given.
func statFake(t *testing.T, autoCreated, userCreated bool) *gosmotest.Fake {
	f := ordersFake(t)
	f.OnFunc(read("FROM   sys.stats s\nOUTER  APPLY", ordersID, "st_Orders_Date"), gosmotest.Rows(
		[]any{"st_Orders_Date", int64(3), autoCreated, userCreated, true, "([Status]=(1))",
			scriptWhen, int64(10), int64(10), int64(2), int64(10), true, false, int64(0)}))
	f.OnFunc(read("JOIN   sys.stats_columns sc", "st_Orders_Date", "[sales].[Orders]"), gosmotest.Rows(
		[]any{"OrderDate"}, []any{"CustomerID"}))
	return f
}

func TestScriptStatisticGolden(t *testing.T) {
	sc := gosmo.NewScripter(statFake(t, false, true).Server().DatabaseRef("app"), gosmo.DefaultScriptOptions())

	got, err := sc.ScriptStatistic(context.Background(), "sales", "Orders", "st_Orders_Date")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "ScriptStatistic", got, "IF NOT EXISTS (SELECT 1 FROM sys.stats WHERE name = N'st_Orders_Date' AND object_id = OBJECT_ID(N'[sales].[Orders]'))\n"+
		"CREATE STATISTICS [st_Orders_Date] ON [sales].[Orders] ([OrderDate], [CustomerID]) WHERE ([Status]=(1)) WITH NORECOMPUTE;\nGO\n")
}

// An index's own statistic is neither auto- nor user-created: refused,
// before its columns are read.
func TestScriptStatisticRefusesAnIndexStatistic(t *testing.T) {
	f := statFake(t, false, false)
	sc := gosmo.NewScripter(f.Server().DatabaseRef("app"), gosmo.DefaultScriptOptions())

	if got, err := sc.ScriptStatistic(context.Background(), "sales", "Orders", "st_Orders_Date"); !errors.Is(err, gosmo.ErrInvalidRequest) {
		t.Errorf("ScriptStatistic = %q, %v; want ErrInvalidRequest", got, err)
	}
	if n := countQueries(f, "sys.stats_columns"); n != 0 {
		t.Errorf("columns read %d times, want none", n)
	}
}
