package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"testing"
)

// The Delete/Rename family is a set of one-statement writes whose whole
// behaviour is the statement they produce, so each is pinned through
// WithScript — the only way to see the exact T-SQL without a server.

func TestDropStatements(t *testing.T) {
	cases := []struct {
		name  string
		write func(context.Context, *Database) error
		want  string
	}{
		{
			name:  "DropView",
			write: func(ctx context.Context, d *Database) error { return d.DropView(ctx, "Sales", "vCustomer") },
			want:  "DROP VIEW [Sales].[vCustomer]",
		},
		{
			// An empty schema means dbo, not an unqualified name — an
			// unqualified DROP resolves against the caller's default schema,
			// which is not necessarily the object's.
			name:  "DropView defaults the schema",
			write: func(ctx context.Context, d *Database) error { return d.DropView(ctx, "", "vCustomer") },
			want:  "DROP VIEW [dbo].[vCustomer]",
		},
		{
			name:  "DropFunction",
			write: func(ctx context.Context, d *Database) error { return d.DropFunction(ctx, "dbo", "fnAge") },
			want:  "DROP FUNCTION [dbo].[fnAge]",
		},
		{
			name:  "DropTrigger",
			write: func(ctx context.Context, d *Database) error { return d.DropTrigger(ctx, "dbo", "trAudit") },
			want:  "DROP TRIGGER [dbo].[trAudit]",
		},
		{
			name:  "DropDatabaseRole",
			write: func(ctx context.Context, d *Database) error { return d.DropDatabaseRole(ctx, "app_reader") },
			want:  "DROP ROLE [app_reader]",
		},
		{
			name: "Table.DropConstraint",
			write: func(ctx context.Context, d *Database) error {
				return (&Table{db: d, Schema: "dbo", Name: "Orders"}).DropConstraint(ctx, "PK_Orders")
			},
			want: "ALTER TABLE [dbo].[Orders] DROP CONSTRAINT [PK_Orders]",
		},
		{
			name: "Table.DropColumn",
			write: func(ctx context.Context, d *Database) error {
				return (&Table{db: d, Schema: "sa]les", Name: "Or'ders"}).DropColumn(ctx, "a]b")
			},
			want: "ALTER TABLE [sa]]les].[Or'ders] DROP COLUMN [a]]b]",
		},
		{
			// All three type families funnel into DROP TYPE — nothing in the
			// statement distinguishes an alias, table or CLR type.
			name: "UserDefinedDataType.Drop",
			write: func(ctx context.Context, d *Database) error {
				return (&UserDefinedDataType{db: d, Schema: "sa]les", Name: "Pho'ne"}).Drop(ctx)
			},
			want: "DROP TYPE [sa]]les].[Pho'ne]",
		},
		{
			name: "UserDefinedTableType.Drop",
			write: func(ctx context.Context, d *Database) error {
				return (&UserDefinedTableType{db: d, Schema: "Sales", Name: "OrderLines"}).Drop(ctx)
			},
			want: "DROP TYPE [Sales].[OrderLines]",
		},
		{
			name: "ClrType.Drop",
			write: func(ctx context.Context, d *Database) error {
				return (&ClrType{db: d, Schema: "Sales", Name: "Geo"}).Drop(ctx)
			},
			want: "DROP TYPE [Sales].[Geo]",
		},
		{
			// The same dbo default as DropView, reached through the handle:
			// an unqualified DROP TYPE resolves against the caller's default
			// schema, not the type's.
			name: "UserDefinedDataType.Drop defaults the schema",
			write: func(ctx context.Context, d *Database) error {
				return (&UserDefinedDataType{db: d, Name: "Phone"}).Drop(ctx)
			},
			want: "DROP TYPE [dbo].[Phone]",
		},
		{
			name:  "DropType",
			write: func(ctx context.Context, d *Database) error { return d.DropType(ctx, "Sales", "Phone") },
			want:  "DROP TYPE [Sales].[Phone]",
		},
		{
			name:  "DropType defaults the schema",
			write: func(ctx context.Context, d *Database) error { return d.DropType(ctx, "", "Phone") },
			want:  "DROP TYPE [dbo].[Phone]",
		},
		{
			name: "XMLSchemaCollection.Drop",
			write: func(ctx context.Context, d *Database) error {
				return (&XMLSchemaCollection{db: d, Schema: "arch]ive", Name: "Order'Schema"}).Drop(ctx)
			},
			want: "DROP XML SCHEMA COLLECTION [arch]]ive].[Order'Schema]",
		},
		{
			name: "XMLSchemaCollection.Drop defaults the schema",
			write: func(ctx context.Context, d *Database) error {
				return (&XMLSchemaCollection{db: d, Name: "OrderSchema"}).Drop(ctx)
			},
			want: "DROP XML SCHEMA COLLECTION [dbo].[OrderSchema]",
		},
		{
			name: "DropXMLSchemaCollection",
			write: func(ctx context.Context, d *Database) error {
				return d.DropXMLSchemaCollection(ctx, "archive", "OrderSchema")
			},
			want: "DROP XML SCHEMA COLLECTION [archive].[OrderSchema]",
		},
		{
			name: "DropXMLSchemaCollection defaults the schema",
			write: func(ctx context.Context, d *Database) error {
				return d.DropXMLSchemaCollection(ctx, "", "OrderSchema")
			},
			want: "DROP XML SCHEMA COLLECTION [dbo].[OrderSchema]",
		},
		{
			name:  "DropRule",
			write: func(ctx context.Context, d *Database) error { return d.DropRule(ctx, "Sales", "ru'le") },
			want:  "DROP RULE [Sales].[ru'le]",
		},
		{
			name: "Rule.Drop defaults the schema",
			write: func(ctx context.Context, d *Database) error {
				return (&Rule{db: d, Name: "ru]le"}).Drop(ctx)
			},
			want: "DROP RULE [dbo].[ru]]le]",
		},
		{
			name: "DropDefault",
			write: func(ctx context.Context, d *Database) error {
				return d.DropDefault(ctx, "Sales", "df'1")
			},
			want: "DROP DEFAULT [Sales].[df'1]",
		},
		{
			name: "Default.Drop defaults the schema",
			write: func(ctx context.Context, d *Database) error {
				return (&Default{db: d, Name: "df]1"}).Drop(ctx)
			},
			want: "DROP DEFAULT [dbo].[df]]1]",
		},
		{
			name: "Sequence.Drop",
			write: func(ctx context.Context, d *Database) error {
				return (&Sequence{db: d, Schema: "Sales", Name: "seq'1"}).Drop(ctx)
			},
			want: "DROP SEQUENCE [Sales].[seq'1]",
		},
		{
			name: "Sequence.Drop defaults the schema",
			write: func(ctx context.Context, d *Database) error {
				return (&Sequence{db: d, Name: "seq]1"}).Drop(ctx)
			},
			want: "DROP SEQUENCE [dbo].[seq]]1]",
		},
		{
			name: "Synonym.Drop",
			write: func(ctx context.Context, d *Database) error {
				return (&Synonym{db: d, Schema: "Sales", Name: "syn'1"}).Drop(ctx)
			},
			want: "DROP SYNONYM [Sales].[syn'1]",
		},
		{
			name: "Synonym.Drop defaults the schema",
			write: func(ctx context.Context, d *Database) error {
				return (&Synonym{db: d, Name: "syn]1"}).Drop(ctx)
			},
			want: "DROP SYNONYM [dbo].[syn]]1]",
		},
		{
			// A partition function and scheme are database-scoped and have no
			// schema of their own, so there is no default to pin here.
			name: "PartitionFunction.Drop",
			write: func(ctx context.Context, d *Database) error {
				return (&PartitionFunction{db: d, Name: "pf]1"}).Drop(ctx)
			},
			want: "DROP PARTITION FUNCTION [pf]]1]",
		},
		{
			name: "PartitionScheme.Drop",
			write: func(ctx context.Context, d *Database) error {
				return (&PartitionScheme{db: d, Name: "ps'1"}).Drop(ctx)
			},
			want: "DROP PARTITION SCHEME [ps'1]",
		},
		{
			name: "DropExternalDataSource",
			write: func(ctx context.Context, d *Database) error {
				return d.DropExternalDataSource(ctx, "eds]1")
			},
			want: "DROP EXTERNAL DATA SOURCE [eds]]1]",
		},
		{
			name: "ExternalDataSource.Drop",
			write: func(ctx context.Context, d *Database) error {
				return (&ExternalDataSource{db: d, Name: "eds'1"}).Drop(ctx)
			},
			want: "DROP EXTERNAL DATA SOURCE [eds'1]",
		},
		{
			name: "ExternalFileFormat.Drop",
			write: func(ctx context.Context, d *Database) error {
				return (&ExternalFileFormat{db: d, Name: "eff'1"}).Drop(ctx)
			},
			want: "DROP EXTERNAL FILE FORMAT [eff'1]",
		},
		{
			// The version gate in front of this one answers "supported" for a
			// server whose version is unknown, which is what a scripted
			// database has — the statement still has to be the right one.
			name: "ExternalLibrary.Drop",
			write: func(ctx context.Context, d *Database) error {
				return (&ExternalLibrary{db: d, Name: "lib]1"}).Drop(ctx)
			},
			want: "DROP EXTERNAL LIBRARY [lib]]1]",
		},
		{
			name: "Schema.Drop",
			write: func(ctx context.Context, d *Database) error {
				return (&Schema{db: d, Name: "sa]les"}).Drop(ctx)
			},
			want: "DROP SCHEMA [sa]]les]",
		},
		{
			name: "User.Drop",
			write: func(ctx context.Context, d *Database) error {
				return (&User{db: d, Name: "o'brien"}).Drop(ctx)
			},
			want: "DROP USER [o'brien]",
		},
		{
			name: "SecurityPolicy.Drop",
			write: func(ctx context.Context, d *Database) error {
				return (&SecurityPolicy{db: d, Schema: "Sec", Name: "sp'1"}).Drop(ctx)
			},
			want: "DROP SECURITY POLICY [Sec].[sp'1]",
		},
		{
			name: "DropAssembly",
			write: func(ctx context.Context, d *Database) error {
				return d.DropAssembly(ctx, "asm]1")
			},
			want: "DROP ASSEMBLY [asm]]1]",
		},
		{
			name: "Assembly.Drop",
			write: func(ctx context.Context, d *Database) error {
				return (&Assembly{db: d, Name: "asm'1"}).Drop(ctx)
			},
			want: "DROP ASSEMBLY [asm'1]",
		},
		{
			name: "Certificate.Drop",
			write: func(ctx context.Context, d *Database) error {
				return (&Certificate{db: d, Name: "cert'1"}).Drop(ctx)
			},
			want: "DROP CERTIFICATE [cert'1]",
		},
		{
			name: "ColumnMasterKey.Drop",
			write: func(ctx context.Context, d *Database) error {
				return (&ColumnMasterKey{db: d, Name: "CMK]1"}).Drop(ctx)
			},
			want: "DROP COLUMN MASTER KEY [CMK]]1]",
		},
		{
			name: "ColumnEncryptionKey.Drop",
			write: func(ctx context.Context, d *Database) error {
				return (&ColumnEncryptionKey{db: d, Name: "CEK'1"}).Drop(ctx)
			},
			want: "DROP COLUMN ENCRYPTION KEY [CEK'1]",
		},
		{
			// DROP STATISTICS takes a three-part name whose parts are quoted
			// separately — the whole thing is not one identifier, so a
			// FullName() here would produce a name the parser reads as two
			// parts and one dotted string.
			name: "Statistic.Drop",
			write: func(ctx context.Context, d *Database) error {
				return (&Statistic{table: &Table{db: d, Schema: "sa]les", Name: "Or'ders"}, Name: "st]1"}).Drop(ctx)
			},
			want: "DROP STATISTICS [sa]]les].[Or'ders].[st]]1]",
		},
		{
			name: "DatabaseSnapshot.Drop is a DROP DATABASE",
			write: func(ctx context.Context, d *Database) error {
				return (&DatabaseSnapshot{server: d.server, Name: "AppDB_snap]1"}).Drop(ctx)
			},
			want: "DROP DATABASE [AppDB_snap]]1]",
		},
		{
			name: "DatabaseRole.Drop delegates to the database",
			write: func(ctx context.Context, d *Database) error {
				return (&DatabaseRole{db: d, Name: "app_reader"}).Drop(ctx)
			},
			want: "DROP ROLE [app_reader]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Database{server: &Server{}, Name: "AppDB"}
			ctx, script := WithScript(context.Background())
			if err := tc.write(ctx, d); err != nil {
				t.Fatalf("%s under WithScript: %v", tc.name, err)
			}
			all := strings.Join(script.Statements(), "\n")
			if !strings.Contains(all, tc.want) {
				t.Errorf("captured script missing %q:\n%s", tc.want, all)
			}
		})
	}
}

func TestRenameStatements(t *testing.T) {
	cases := []struct {
		name  string
		write func(context.Context, *Database) error
		want  []string
	}{
		{
			name: "RenameObject",
			write: func(ctx context.Context, d *Database) error {
				return d.RenameObject(ctx, "Sales", "vOld", "vNew")
			},
			want: []string{"EXEC sp_rename", "N'[Sales].[vOld]'", "N'vNew'", "N'OBJECT'"},
		},
		{
			// The counterpart to a rename: sp_rename cannot cross schemas, so
			// the move is its own statement and its own method.
			name: "TransferObject",
			write: func(ctx context.Context, d *Database) error {
				return d.TransferObject(ctx, "arch]ive", "sa]les", "Or'ders")
			},
			want: []string{"ALTER SCHEMA [arch]]ive] TRANSFER [sa]]les].[Or'ders]"},
		},
		{
			name: "DatabaseRole.Rename",
			write: func(ctx context.Context, d *Database) error {
				return (&DatabaseRole{db: d, Name: "app]reader"}).Rename(ctx, "app'reader")
			},
			want: []string{"ALTER ROLE [app]]reader] WITH NAME = [app'reader]"},
		},
		{
			// sp_rename's USERDATATYPE class is the only one that reaches
			// sys.types, and it reaches alias types only.
			name: "RenameUserDefinedDataType",
			write: func(ctx context.Context, d *Database) error {
				return d.RenameUserDefinedDataType(ctx, "", "Phone", "PhoneNo")
			},
			want: []string{"EXEC sp_rename", "N'[dbo].[Phone]'", "N'PhoneNo'", "N'USERDATATYPE'"},
		},
		{
			// A type is not in sys.objects, so TRANSFER needs its class
			// prefix; without it the server refuses the transfer naming an
			// object that does not exist.
			name: "TransferType",
			write: func(ctx context.Context, d *Database) error {
				return d.TransferType(ctx, "archive", "sales", "Phone")
			},
			want: []string{"ALTER SCHEMA [archive] TRANSFER TYPE::[sales].[Phone]"},
		},
		{
			name: "TransferXMLSchemaCollection",
			write: func(ctx context.Context, d *Database) error {
				return d.TransferXMLSchemaCollection(ctx, "archive", "", "OrderSchema")
			},
			want: []string{"ALTER SCHEMA [archive] TRANSFER XML SCHEMA COLLECTION::[dbo].[OrderSchema]"},
		},
		{
			name: "TransferObject defaults the source schema",
			write: func(ctx context.Context, d *Database) error {
				return d.TransferObject(ctx, "archive", "", "Orders")
			},
			want: []string{"ALTER SCHEMA [archive] TRANSFER [dbo].[Orders]"},
		},
		{
			// sp_rename's COLUMN class takes the three-part table.column form
			// in @objname and a bare @newname. A quoted identifier in either
			// half is escaped, since the parameter is a string, not an
			// identifier the parser sees.
			name: "Table.RenameColumn",
			write: func(ctx context.Context, d *Database) error {
				t := &Table{db: d, Schema: "dbo", Name: "Or]ders"}
				return t.RenameColumn(ctx, "no'te", "note")
			},
			want: []string{"EXEC sp_rename", "N'[dbo].[Or]]ders].[no''te]'", "N'note'", "N'COLUMN'"},
		},
		{
			name: "Statistic.Rename",
			write: func(ctx context.Context, d *Database) error {
				st := &Statistic{table: &Table{db: d, Schema: "dbo", Name: "Orders"}, Name: "st_old"}
				return st.Rename(ctx, "st_new")
			},
			want: []string{"EXEC sp_rename", "N'[dbo].[Orders].[st_old]'", "N'st_new'", "N'STATISTICS'"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Database{server: &Server{}, Name: "AppDB"}
			ctx, script := WithScript(context.Background())
			if err := tc.write(ctx, d); err != nil {
				t.Fatalf("%s under WithScript: %v", tc.name, err)
			}
			all := strings.Join(script.Statements(), "\n")
			for _, want := range tc.want {
				if !strings.Contains(all, want) {
					t.Errorf("captured script missing %q:\n%s", want, all)
				}
			}
			if strings.Contains(all, "@p1") || strings.Contains(all, "@p2") {
				t.Errorf("captured script still has an unbound placeholder:\n%s", all)
			}
		})
	}
}

// The two server-level writes take no Database, so they capture through
// Server.exec rather than Database.exec.
func TestServerLevelDropAndRenameStatements(t *testing.T) {
	cases := []struct {
		name  string
		write func(context.Context, *Server) error
		want  string
	}{
		{
			name:  "DropServerRole",
			write: func(ctx context.Context, s *Server) error { return s.DropServerRole(ctx, "auditors") },
			want:  "DROP SERVER ROLE [auditors]",
		},
		{
			name: "RenameDatabase",
			write: func(ctx context.Context, s *Server) error {
				return s.RenameDatabase(ctx, "AppDB", "AppDB2", false)
			},
			want: "ALTER DATABASE [AppDB] MODIFY NAME = [AppDB2]",
		},
		{
			// Forced: single-user before, multi-user after — and the release
			// names the database by whatever it is called by then.
			name: "RenameDatabase force",
			write: func(ctx context.Context, s *Server) error {
				return s.RenameDatabase(ctx, "AppDB", "AppDB2", true)
			},
			want: "ALTER DATABASE [AppDB] SET SINGLE_USER WITH ROLLBACK IMMEDIATE",
		},
		{
			name: "RenameDatabase force releases under the new name",
			write: func(ctx context.Context, s *Server) error {
				return s.RenameDatabase(ctx, "AppDB", "AppDB2", true)
			},
			want: "ALTER DATABASE [AppDB2] SET MULTI_USER",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{}
			ctx, script := WithScript(context.Background())
			if err := tc.write(ctx, s); err != nil {
				t.Fatalf("%s under WithScript: %v", tc.name, err)
			}
			all := strings.Join(script.Statements(), "\n")
			if !strings.Contains(all, tc.want) {
				t.Errorf("captured script missing %q:\n%s", tc.want, all)
			}
		})
	}
}

// No Drop* write method carries IF EXISTS: dropping something that isn't
// there has to reach the caller as the server's error, or a UI built on this
// reports "deleted" for an object it never touched. Half the family used to
// carry it and half did not, so the same gesture answered two different ways
// depending on the object type. See the note on Database.DropTable.
//
// Asserted over the statements themselves rather than by grepping the source,
// so a new Drop* that reintroduces IF EXISTS is caught only if it is listed
// here — which is the point: adding one to this list is how a new drop gets
// its statement pinned at all.
func TestDropStatementsAreNotIdempotent(t *testing.T) {
	d := &Database{server: &Server{}, Name: "AppDB"}
	ctx, script := WithScript(context.Background())

	drops := []struct {
		name  string
		write func() error
	}{
		{"view", func() error { return d.DropView(ctx, "dbo", "v") }},
		{"function", func() error { return d.DropFunction(ctx, "dbo", "f") }},
		{"procedure", func() error { return d.DropStoredProcedure(ctx, "dbo", "p") }},
		{"trigger", func() error { return d.DropTrigger(ctx, "dbo", "tr") }},
		{"database trigger", func() error { return d.DatabaseTriggerRef("ddl_tr").Drop(ctx) }},
		{"synonym", func() error { return d.DropSynonym(ctx, "dbo", "syn") }},
		{"sequence", func() error { return d.DropSequence(ctx, "dbo", "seq") }},
		{"table", func() error { return d.DropTable(ctx, "dbo", "t", false) }},
		{"database scoped credential", func() error { return d.DatabaseScopedCredentialRef("cred").Drop(ctx) }},
		{"certificate", func() error { return d.CertificateRef("cert").Drop(ctx) }},
		{"asymmetric key", func() error { return d.AsymmetricKeyRef("key").Drop(ctx) }},
		{"symmetric key", func() error { return d.SymmetricKeyRef("key").Drop(ctx) }},
		{"database role", func() error { return d.DropDatabaseRole(ctx, "r") }},
		{"schema", func() error { return d.DropSchema(ctx, "s") }},
		{"user", func() error { return d.DropUser(ctx, "u") }},
	}
	for _, dr := range drops {
		if err := dr.write(); err != nil {
			t.Fatalf("drop %s under WithScript: %v", dr.name, err)
		}
	}
	for _, stmt := range script.Statements() {
		if strings.Contains(stmt, "IF EXISTS") {
			t.Errorf("a Drop* write method emitted IF EXISTS:\n%s", stmt)
		}
	}
	if len(script.Statements()) != len(drops) {
		t.Fatalf("captured %d statements, want one per drop (%d)", len(script.Statements()), len(drops))
	}
}

// A name is bracket-quoted, and an embedded "]" doubled, everywhere in this
// family — a name that isn't would either fail to parse or, worse, resolve
// to a different object.
func TestDropRenameQuotesAwkwardNames(t *testing.T) {
	d := &Database{server: &Server{}, Name: "AppDB"}
	ctx, script := WithScript(context.Background())
	if err := d.DropView(ctx, "we]ird", "v]iew"); err != nil {
		t.Fatalf("DropView: %v", err)
	}
	if err := (&Table{db: d, Schema: "dbo", Name: "Ord]ers"}).DropConstraint(ctx, "CK]1"); err != nil {
		t.Fatalf("DropConstraint: %v", err)
	}
	all := strings.Join(script.Statements(), "\n")
	for _, want := range []string{"[we]]ird].[v]]iew]", "[dbo].[Ord]]ers] DROP CONSTRAINT [CK]]1]"} {
		if !strings.Contains(all, want) {
			t.Errorf("captured script missing %q:\n%s", want, all)
		}
	}
}

// TestTransferObjectRefusals pins the two cases where no statement should
// reach the server. A same-schema transfer is not a no-op at the server — it
// still drops the permissions granted directly on the object — so it is
// refused rather than sent, and an empty target would quote into
// "ALTER SCHEMA [] TRANSFER", which fails naming a schema the caller never
// typed.
func TestTransferObjectRefusals(t *testing.T) {
	for _, c := range []struct {
		name           string
		target, schema string
		want           string
	}{
		{"empty target", "", "sales", "target schema is required"},
		{"same schema", "sales", "sales", "already in schema"},
		{"same schema by default", "dbo", "", "already in schema"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := &Database{server: &Server{}, Name: "AppDB"}
			ctx, script := WithScript(context.Background())
			err := d.TransferObject(ctx, c.target, c.schema, "Orders")
			if err == nil {
				t.Fatalf("no error; statements: %v", script.Statements())
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to mention %q", err, c.want)
			}
			if len(script.Statements()) != 0 {
				t.Errorf("emitted %q, want nothing", script.Statements())
			}
		})
	}
}

// TestTransferObjectCaseOnlyDifferenceAsksTheServer pins T13: target and
// source names that differ only in case are one schema under a
// case-insensitive collation and two under a case-sensitive one. The server's
// SCHEMA_ID comparison decides, rather than a case-blind compare in Go that
// refused a legitimate [sales] → [Sales] transfer in a _CS_ database.
func TestTransferObjectCaseOnlyDifferenceAsksTheServer(t *testing.T) {
	db, err := sql.Open("capture", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	d := &Database{server: &Server{db: db}, Name: "AppDB"}
	ctx := context.Background()
	for _, c := range []struct {
		name     string
		sameID   bool
		wantSent bool
	}{
		{"case-insensitive collation: same schema", true, false},
		{"case-sensitive collation: two schemas", false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			captured.reset(cannedRow{match: "SCHEMA_ID(@p1) = SCHEMA_ID(@p2)",
				cols: []string{"same"}, row: []driver.Value{c.sameID}})
			err := d.TransferObject(ctx, "SALES", "sales", "Orders")
			sent := captured.find("ALTER SCHEMA [SALES] TRANSFER [sales].[Orders]") != ""
			if sent != c.wantSent {
				t.Errorf("ALTER SCHEMA sent = %v, want %v (err %v)", sent, c.wantSent, err)
			}
			if c.wantSent && err != nil {
				t.Errorf("err = %v, want nil", err)
			}
			if !c.wantSent && (err == nil || !strings.Contains(err.Error(), "already in schema")) {
				t.Errorf("err = %v, want an already-in-schema refusal", err)
			}
		})
	}
	// Names that differ by more than case never ask.
	captured.reset()
	if err := d.TransferObject(ctx, "hr", "sales", "Orders"); err != nil {
		t.Fatalf("TransferObject: %v", err)
	}
	if captured.find("SCHEMA_ID(") != "" {
		t.Error("a transfer between differently-spelled schemas queried SCHEMA_ID")
	}
}

// TestDropColumnRefusesAnEmptyName is the same guard DropColumn needs:
// quoteIdent("") is "[]", which the server rejects naming a column the caller
// never asked for.
func TestDropColumnRefusesAnEmptyName(t *testing.T) {
	d := &Database{server: &Server{}, Name: "AppDB"}
	ctx, script := WithScript(context.Background())
	err := (&Table{db: d, Schema: "dbo", Name: "Orders"}).DropColumn(ctx, "")
	if err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Errorf("error = %v, want it to name the missing column", err)
	}
	if len(script.Statements()) != 0 {
		t.Errorf("emitted %q, want nothing", script.Statements())
	}
}

// -- SINGLE_USER is never left behind ----------------------------------------
//
// Every write below opens by taking exclusive access with SET SINGLE_USER WITH
// ROLLBACK IMMEDIATE, which locks the database to one login. If the write that
// follows fails, nothing else puts it back — so the repair is part of the
// method's contract, and these pin it. See Server.restoreMultiUser.

// TestAFailedForcedDropIsPutBackToMultiUser. A DROP can genuinely fail after
// the alter succeeded — another session takes the single-user slot, the
// database is in an availability group, the login may set state but not drop —
// and leaving it there locks everyone else out of a database that still exists.
func TestAFailedForcedDropIsPutBackToMultiUser(t *testing.T) {
	s := detServer(t)
	detFailOn("DROP DATABASE")

	if err := s.DropDatabase(context.Background(), "appdb", true); err == nil {
		t.Fatal("a failing drop returned no error")
	}
	stmts := detLog.statements()
	if len(stmts) != 1 {
		t.Fatalf("statements %v, want only the batch, which repairs itself", stmts)
	}
	assertExclusiveBatch(t, stmts[0], "[appdb]", "DROP DATABASE [appdb]")
}

// TestAFailedForcedDropIsPutBackToMultiUserEvenWhenTheContextIsGone. The
// repair has to outlive the caller's context, because the deadline expiring
// during the drop is one of the ways the drop fails — and the one where a
// repair on the same context cannot reach the server at all.
func TestAFailedForcedDropIsPutBackToMultiUserEvenWhenTheContextIsGone(t *testing.T) {
	s := detServer(t)
	ctx := detCancelOn(t, "DROP DATABASE")

	if err := s.DropDatabase(ctx, "appdb", true); err == nil {
		t.Fatal("a drop whose context expired returned no error")
	}
	stmts := detLog.statements()
	last := stmts[len(stmts)-1]
	if !strings.Contains(last, "[appdb] SET MULTI_USER") {
		t.Errorf("last statement after a cancelled forced drop is %q, want the database put back to MULTI_USER: %v", last, stmts)
	}
}

// TestAFailedDropWithoutForceLeavesTheAccessModeAlone. Without force nothing
// here set the access mode, so a MULTI_USER on the way out would silently undo
// a RESTRICTED_USER or SINGLE_USER the database was deliberately left in.
func TestAFailedDropWithoutForceLeavesTheAccessModeAlone(t *testing.T) {
	s := detServer(t)
	detLog.mu.Lock()
	detLog.failOn = "DROP DATABASE" // a plain error: cut short, the worst case
	detLog.mu.Unlock()

	if err := s.DropDatabase(context.Background(), "appdb", false); err == nil {
		t.Fatal("a failing drop returned no error")
	}
	for _, stmt := range detLog.statements() {
		if strings.Contains(stmt, "MULTI_USER") {
			t.Errorf("an unforced drop issued %q, changing an access mode it never set", stmt)
		}
	}
}

// TestASuccessfulDropDoesNotTryToAlterTheDatabaseAfterwards. The database is
// gone by then, so the alter would fail with "not found" — and the repair is
// best-effort, so that failure would be swallowed rather than reported, which
// is worse than not issuing it. The batch's own repair is guarded on DB_ID.
func TestASuccessfulDropDoesNotTryToAlterTheDatabaseAfterwards(t *testing.T) {
	s := detServer(t)
	if err := s.DropDatabase(context.Background(), "appdb", true); err != nil {
		t.Fatalf("DropDatabase: %v", err)
	}
	stmts := detLog.statements()
	if len(stmts) != 1 {
		t.Fatalf("statements %v, want only the batch", stmts)
	}
	assertExclusiveBatch(t, stmts[0], "[appdb]", "DROP DATABASE [appdb]")
}

// TestAForcedRenameReleasesMultiUserEvenWhenTheContextIsGone. Unlike the drop
// and the detach, the rename's release runs on success too — but only the
// failure path is at risk, and it is the one where the database still exists
// to be stranded. MODIFY NAME needs exclusive access and waits for it, so the
// deadline expiring there is the expected failure, not an exotic one.
func TestAForcedRenameReleasesMultiUserEvenWhenTheContextIsGone(t *testing.T) {
	s := detServer(t)
	ctx := detCancelOn(t, "MODIFY NAME")

	if err := s.RenameDatabase(ctx, "AppDB", "AppDB2", true); err == nil {
		t.Fatal("a rename whose context expired returned no error")
	}
	stmts := detLog.statements()
	last := stmts[len(stmts)-1]
	// Nothing says whether the rename happened, so the repair asks: the old
	// name first, the new one only if the old is gone.
	if len(stmts) != 2 || !strings.HasPrefix(last, "IF DB_ID(N'AppDB') IS NOT NULL ALTER DATABASE [AppDB] SET MULTI_USER") ||
		!strings.Contains(last, "ELSE IF DB_ID(N'AppDB2') IS NOT NULL ALTER DATABASE [AppDB2] SET MULTI_USER") {
		t.Errorf("last statement after a cancelled rename is %q, want the repair under whichever name exists: %v", last, stmts)
	}
}

// TestAForcedRenameIsOneBatchThatReleasesWhicheverNameTheDatabaseHas. The
// release runs on success too — under the new name — and on failure under
// the old one; a refused rename to a name another database already has must
// not touch that other database.
func TestAForcedRenameIsOneBatchThatReleasesWhicheverNameTheDatabaseHas(t *testing.T) {
	for _, fail := range []bool{false, true} {
		s := detServer(t)
		if fail {
			detFailOn("MODIFY NAME")
		}
		err := s.RenameDatabase(context.Background(), "AppDB", "AppDB2", true)
		if (err != nil) != fail {
			t.Fatalf("fail=%v: RenameDatabase returned %v", fail, err)
		}
		stmts := detLog.statements()
		if len(stmts) != 1 {
			t.Fatalf("fail=%v: statements %v, want one batch", fail, stmts)
		}
		b := stmts[0]
		single := strings.Index(b, "ALTER DATABASE [AppDB] SET SINGLE_USER WITH ROLLBACK IMMEDIATE")
		rename := strings.Index(b, "ALTER DATABASE [AppDB] MODIFY NAME = [AppDB2]")
		if single < 0 || rename < single || !strings.Contains(b, "IF @closed = 1\nBEGIN\n    BEGIN TRY\n        ALTER DATABASE [AppDB] MODIFY NAME") {
			t.Errorf("fail=%v: batch does not gate MODIFY NAME on SINGLE_USER:\n%s", fail, b)
		}
		if !strings.Contains(b, "BEGIN CATCH\n        ALTER DATABASE [AppDB] SET MULTI_USER;\n        THROW;\n    END CATCH;\n    ALTER DATABASE [AppDB2] SET MULTI_USER;\nEND;") {
			t.Errorf("fail=%v: batch does not release under whichever name the database has:\n%s", fail, b)
		}
	}
}

// TestAForcedDropOnAManagedInstanceKillsSessionsInsteadOfSingleUser. A Managed
// Instance refuses SET SINGLE_USER (Msg 5008), and a forced drop that led with
// it failed before reaching the DROP — so there the connections are closed by
// KILL, and no access-mode statement is issued on either path.
func TestAForcedDropOnAManagedInstanceKillsSessionsInsteadOfSingleUser(t *testing.T) {
	for _, fail := range []bool{false, true} {
		s := detServer(t)
		s.info = &ServerInfo{EngineEdition: int(EngineAzureManagedInst)}
		if fail {
			detLog.mu.Lock()
			detLog.failOn = "DROP DATABASE"
			detLog.mu.Unlock()
		}
		err := s.DropDatabase(context.Background(), "appdb", true)
		if (err != nil) != fail {
			t.Fatalf("fail=%v: DropDatabase returned %v", fail, err)
		}
		// One batch: a session reconnecting between the KILLs and the DROP
		// fails the DROP (S8).
		stmts := detLog.statements()
		if len(stmts) != 1 || !strings.Contains(stmts[0], "DB_ID(N'appdb')") ||
			!strings.HasSuffix(stmts[0], "END;\nDROP DATABASE [appdb];") ||
			strings.Index(stmts[0], "KILL") > strings.Index(stmts[0], "DROP DATABASE") {
			t.Errorf("fail=%v: statements %v, want the session KILL batch then DROP DATABASE, as one", fail, stmts)
		}
		for _, stmt := range stmts {
			if strings.Contains(stmt, "SINGLE_USER") || strings.Contains(stmt, "MULTI_USER") {
				t.Errorf("fail=%v: a Managed Instance drop issued %q, which it refuses", fail, stmt)
			}
		}
	}
}

// TestAForcedRenameOnAManagedInstanceKillsSessionsInsteadOfSingleUser — the
// rename's half of the same refusal.
func TestAForcedRenameOnAManagedInstanceKillsSessionsInsteadOfSingleUser(t *testing.T) {
	s := detServer(t)
	s.info = &ServerInfo{EngineEdition: int(EngineAzureManagedInst)}
	if err := s.RenameDatabase(context.Background(), "AppDB", "AppDB2", true); err != nil {
		t.Fatalf("RenameDatabase: %v", err)
	}
	stmts := detLog.statements()
	if len(stmts) != 1 || !strings.Contains(stmts[0], "KILL") ||
		!strings.HasSuffix(stmts[0], "END;\nALTER DATABASE [AppDB] MODIFY NAME = [AppDB2];") {
		t.Errorf("statements %v, want the session KILL batch then MODIFY NAME, as one", stmts)
	}
}

// TestAForcedDropOnPremStillUsesSingleUser: the Managed Instance path is keyed
// on the edition, and every other one keeps ROLLBACK IMMEDIATE.
func TestAForcedDropOnPremStillUsesSingleUser(t *testing.T) {
	s := detServer(t)
	s.info = &ServerInfo{EngineEdition: int(EngineEnterprise)}
	if err := s.DropDatabase(context.Background(), "appdb", true); err != nil {
		t.Fatalf("DropDatabase: %v", err)
	}
	stmts := detLog.statements()
	if len(stmts) != 1 {
		t.Fatalf("statements %v, want one batch", stmts)
	}
	assertExclusiveBatch(t, stmts[0], "[appdb]", "DROP DATABASE [appdb]")
}

// TestWithScriptForcedExclusiveWritesAreOneStatement. A captured forced drop,
// rename or detach is one statement, so a script run by hand has no gap
// between freeing the single-user slot and using it either (S8).
func TestWithScriptForcedExclusiveWritesAreOneStatement(t *testing.T) {
	cases := map[string]func(context.Context, *Server) error{
		"drop":   func(ctx context.Context, s *Server) error { return s.DropDatabase(ctx, "appdb", true) },
		"rename": func(ctx context.Context, s *Server) error { return s.RenameDatabase(ctx, "appdb", "appdb2", true) },
		"detach": func(ctx context.Context, s *Server) error {
			return s.DetachDatabase(ctx, "appdb", DetachOptions{DropConnections: true})
		},
	}
	for name, write := range cases {
		for _, mi := range []bool{false, true} {
			if mi && name == "detach" {
				continue // a Managed Instance cannot detach at all
			}
			s := &Server{}
			if mi {
				s.info = &ServerInfo{EngineEdition: int(EngineAzureManagedInst)}
			}
			ctx, script := WithScript(context.Background())
			if err := write(ctx, s); err != nil {
				t.Fatalf("%s (mi=%v) under WithScript: %v", name, mi, err)
			}
			if got := script.Statements(); len(got) != 1 {
				t.Errorf("%s (mi=%v): captured %d statements, want 1: %v", name, mi, len(got), got)
			}
		}
	}
}
