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
			want:  "DROP VIEW IF EXISTS [Sales].[vCustomer]",
		},
		{
			// An empty schema means dbo, not an unqualified name — an
			// unqualified DROP resolves against the caller's default schema,
			// which is not necessarily the object's.
			name:  "DropView defaults the schema",
			write: func(ctx context.Context, d *Database) error { return d.DropViewContext(ctx, "", "vCustomer") },
			want:  "DROP VIEW IF EXISTS [dbo].[vCustomer]",
		},
		{
			name:  "DropFunction",
			write: func(ctx context.Context, d *Database) error { return d.DropFunctionContext(ctx, "dbo", "fnAge") },
			want:  "DROP FUNCTION IF EXISTS [dbo].[fnAge]",
		},
		{
			name:  "DropTrigger",
			write: func(ctx context.Context, d *Database) error { return d.DropTriggerContext(ctx, "dbo", "trAudit") },
			want:  "DROP TRIGGER IF EXISTS [dbo].[trAudit]",
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
