package gosmo

import (
	"context"
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
			write: func(ctx context.Context, d *Database) error { return d.DropViewContext(ctx, "Sales", "vCustomer") },
			want:  "DROP VIEW [Sales].[vCustomer]",
		},
		{
			// An empty schema means dbo, not an unqualified name — an
			// unqualified DROP resolves against the caller's default schema,
			// which is not necessarily the object's.
			name:  "DropView defaults the schema",
			write: func(ctx context.Context, d *Database) error { return d.DropViewContext(ctx, "", "vCustomer") },
			want:  "DROP VIEW [dbo].[vCustomer]",
		},
		{
			name:  "DropFunction",
			write: func(ctx context.Context, d *Database) error { return d.DropFunctionContext(ctx, "dbo", "fnAge") },
			want:  "DROP FUNCTION [dbo].[fnAge]",
		},
		{
			name:  "DropTrigger",
			write: func(ctx context.Context, d *Database) error { return d.DropTriggerContext(ctx, "dbo", "trAudit") },
			want:  "DROP TRIGGER [dbo].[trAudit]",
		},
		{
			name:  "DropDatabaseRole",
			write: func(ctx context.Context, d *Database) error { return d.DropDatabaseRoleContext(ctx, "app_reader") },
			want:  "DROP ROLE [app_reader]",
		},
		{
			name: "Table.DropConstraint",
			write: func(ctx context.Context, d *Database) error {
				return (&Table{db: d, Schema: "dbo", Name: "Orders"}).DropConstraintContext(ctx, "PK_Orders")
			},
			want: "ALTER TABLE [dbo].[Orders] DROP CONSTRAINT [PK_Orders]",
		},
		{
			name: "Table.DropColumn",
			write: func(ctx context.Context, d *Database) error {
				return (&Table{db: d, Schema: "sa]les", Name: "Or'ders"}).DropColumnContext(ctx, "a]b")
			},
			want: "ALTER TABLE [sa]]les].[Or'ders] DROP COLUMN [a]]b]",
		},
		{
			name: "DatabaseRole.Drop delegates to the database",
			write: func(ctx context.Context, d *Database) error {
				return (&DatabaseRole{db: d, Name: "app_reader"}).DropContext(ctx)
			},
			want: "DROP ROLE [app_reader]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Database{server: &Server{}, name: "AppDB"}
			ctx, script := WithScript(context.Background())
			if err := tc.write(ctx, d); err != nil {
				t.Fatalf("%s under WithScript: %v", tc.name, err)
			}
			all := strings.Join(script.Statements, "\n")
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
				return d.RenameObjectContext(ctx, "Sales", "vOld", "vNew")
			},
			want: []string{"EXEC sp_rename", "N'[Sales].[vOld]'", "N'vNew'", "N'OBJECT'"},
		},
		{
			// The counterpart to a rename: sp_rename cannot cross schemas, so
			// the move is its own statement and its own method.
			name: "TransferObject",
			write: func(ctx context.Context, d *Database) error {
				return d.TransferObjectContext(ctx, "arch]ive", "sa]les", "Or'ders")
			},
			want: []string{"ALTER SCHEMA [arch]]ive] TRANSFER [sa]]les].[Or'ders]"},
		},
		{
			name: "TransferObject defaults the source schema",
			write: func(ctx context.Context, d *Database) error {
				return d.TransferObjectContext(ctx, "archive", "", "Orders")
			},
			want: []string{"ALTER SCHEMA [archive] TRANSFER [dbo].[Orders]"},
		},
		{
			name: "Statistic.Rename",
			write: func(ctx context.Context, d *Database) error {
				st := &Statistic{table: &Table{db: d, Schema: "dbo", Name: "Orders"}, Name: "st_old"}
				return st.RenameContext(ctx, "st_new")
			},
			want: []string{"EXEC sp_rename", "N'[dbo].[Orders].[st_old]'", "N'st_new'", "N'STATISTICS'"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Database{server: &Server{}, name: "AppDB"}
			ctx, script := WithScript(context.Background())
			if err := tc.write(ctx, d); err != nil {
				t.Fatalf("%s under WithScript: %v", tc.name, err)
			}
			all := strings.Join(script.Statements, "\n")
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
// Server.execContext rather than Database.exec.
func TestServerLevelDropAndRenameStatements(t *testing.T) {
	cases := []struct {
		name  string
		write func(context.Context, *Server) error
		want  string
	}{
		{
			name:  "DropServerRole",
			write: func(ctx context.Context, s *Server) error { return s.DropServerRoleContext(ctx, "auditors") },
			want:  "DROP SERVER ROLE [auditors]",
		},
		{
			name: "RenameDatabase",
			write: func(ctx context.Context, s *Server) error {
				return s.RenameDatabaseContext(ctx, "AppDB", "AppDB2", false)
			},
			want: "ALTER DATABASE [AppDB] MODIFY NAME = [AppDB2]",
		},
		{
			// Forced: single-user before, multi-user after — and the release
			// names the database by whatever it is called by then.
			name: "RenameDatabase force",
			write: func(ctx context.Context, s *Server) error {
				return s.RenameDatabaseContext(ctx, "AppDB", "AppDB2", true)
			},
			want: "ALTER DATABASE [AppDB] SET SINGLE_USER WITH ROLLBACK IMMEDIATE",
		},
		{
			name: "RenameDatabase force releases under the new name",
			write: func(ctx context.Context, s *Server) error {
				return s.RenameDatabaseContext(ctx, "AppDB", "AppDB2", true)
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
			all := strings.Join(script.Statements, "\n")
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
	d := &Database{server: &Server{}, name: "AppDB"}
	ctx, script := WithScript(context.Background())

	drops := []struct {
		name  string
		write func() error
	}{
		{"view", func() error { return d.DropViewContext(ctx, "dbo", "v") }},
		{"function", func() error { return d.DropFunctionContext(ctx, "dbo", "f") }},
		{"procedure", func() error { return d.DropStoredProcedureContext(ctx, "dbo", "p") }},
		{"trigger", func() error { return d.DropTriggerContext(ctx, "dbo", "tr") }},
		{"synonym", func() error { return d.DropSynonymContext(ctx, "dbo", "syn") }},
		{"sequence", func() error { return d.DropSequenceContext(ctx, "dbo", "seq") }},
		{"table", func() error { return d.DropTableContext(ctx, "dbo", "t", false) }},
		{"database role", func() error { return d.DropDatabaseRoleContext(ctx, "r") }},
		{"schema", func() error { return d.DropSchemaContext(ctx, "s") }},
		{"user", func() error { return d.DropUserContext(ctx, "u") }},
	}
	for _, dr := range drops {
		if err := dr.write(); err != nil {
			t.Fatalf("drop %s under WithScript: %v", dr.name, err)
		}
	}
	for _, stmt := range script.Statements {
		if strings.Contains(stmt, "IF EXISTS") {
			t.Errorf("a Drop* write method emitted IF EXISTS:\n%s", stmt)
		}
	}
	if len(script.Statements) != len(drops) {
		t.Fatalf("captured %d statements, want one per drop (%d)", len(script.Statements), len(drops))
	}
}

// A name is bracket-quoted, and an embedded "]" doubled, everywhere in this
// family — a name that isn't would either fail to parse or, worse, resolve
// to a different object.
func TestDropRenameQuotesAwkwardNames(t *testing.T) {
	d := &Database{server: &Server{}, name: "AppDB"}
	ctx, script := WithScript(context.Background())
	if err := d.DropViewContext(ctx, "we]ird", "v]iew"); err != nil {
		t.Fatalf("DropViewContext: %v", err)
	}
	if err := (&Table{db: d, Schema: "dbo", Name: "Ord]ers"}).DropConstraintContext(ctx, "CK]1"); err != nil {
		t.Fatalf("DropConstraintContext: %v", err)
	}
	all := strings.Join(script.Statements, "\n")
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
		{"same schema, different case", "SALES", "sales", "already in schema"},
		{"same schema by default", "dbo", "", "already in schema"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := &Database{server: &Server{}, name: "AppDB"}
			ctx, script := WithScript(context.Background())
			err := d.TransferObjectContext(ctx, c.target, c.schema, "Orders")
			if err == nil {
				t.Fatalf("no error; statements: %v", script.Statements)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to mention %q", err, c.want)
			}
			if len(script.Statements) != 0 {
				t.Errorf("emitted %q, want nothing", script.Statements)
			}
		})
	}
}

// TestDropColumnRefusesAnEmptyName is the same guard DropColumn needs:
// quoteIdent("") is "[]", which the server rejects naming a column the caller
// never asked for.
func TestDropColumnRefusesAnEmptyName(t *testing.T) {
	d := &Database{server: &Server{}, name: "AppDB"}
	ctx, script := WithScript(context.Background())
	err := (&Table{db: d, Schema: "dbo", Name: "Orders"}).DropColumnContext(ctx, "")
	if err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Errorf("error = %v, want it to name the missing column", err)
	}
	if len(script.Statements) != 0 {
		t.Errorf("emitted %q, want nothing", script.Statements)
	}
}
