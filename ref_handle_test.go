package gosmo

import (
	"context"
	"testing"
)

// useAppDB (database_credential_write_test.go) is the USE prefix every
// statement a *Database issues is batched behind — see Database.useBatch — so
// a database-scoped handle's scripted statement carries it and a
// server-scoped one does not.
//
// The Ref handles added for the server-role, user, statistic, configuration,
// certificate, asymmetric-key, partition and Always Encrypted key families,
// and the external-resource ones, carry only a name. These pin that every write those
// families expose builds the same statement from a handle as it would from a
// ByName-populated object — the whole point of the handle is that the write
// never reads a cached field — and that each one works under WithScript,
// where the ByName lookup has nothing to find.
func TestRefHandleWriteStatements(t *testing.T) {
	cases := []struct {
		name string
		act  func(context.Context, *Server) error
		want string
	}{
		{"server role rename", func(ctx context.Context, s *Server) error {
			return s.ServerRoleRef("auditors").Rename(ctx, "readers")
		}, "ALTER SERVER ROLE [auditors] WITH NAME = [readers]"},
		{"server role change owner", func(ctx context.Context, s *Server) error {
			return s.ServerRoleRef("auditors").ChangeOwner(ctx, "sa")
		}, "ALTER AUTHORIZATION ON SERVER ROLE::[auditors] TO [sa]"},
		{"server role drop", func(ctx context.Context, s *Server) error {
			return s.ServerRoleRef("auditors").Drop(ctx)
		}, "DROP SERVER ROLE [auditors]"},

		{"user rename", func(ctx context.Context, s *Server) error {
			return s.DatabaseRef("AppDB").UserRef("app").Rename(ctx, "app2")
		}, useAppDB + "ALTER USER [app] WITH NAME = [app2]"},
		{"user set default schema", func(ctx context.Context, s *Server) error {
			return s.DatabaseRef("AppDB").UserRef("app").SetDefaultSchema(ctx, "sales")
		}, useAppDB + "ALTER USER [app] WITH DEFAULT_SCHEMA = [sales]"},
		{"user set login", func(ctx context.Context, s *Server) error {
			return s.DatabaseRef("AppDB").UserRef("app").SetLogin(ctx, "applogin")
		}, useAppDB + "ALTER USER [app] WITH LOGIN = [applogin]"},
		{"user drop", func(ctx context.Context, s *Server) error {
			return s.DatabaseRef("AppDB").UserRef("app").Drop(ctx)
		}, useAppDB + "DROP USER [app]"},

		{"statistic update", func(ctx context.Context, s *Server) error {
			return s.DatabaseRef("AppDB").TableRef("dbo", "Orders").StatisticRef("ix_o").Update(ctx, 0)
		}, useAppDB + "UPDATE STATISTICS [dbo].[Orders] [ix_o] WITH FULLSCAN"},
		{"statistic drop", func(ctx context.Context, s *Server) error {
			return s.DatabaseRef("AppDB").TableRef("dbo", "Orders").StatisticRef("ix_o").Drop(ctx)
		}, useAppDB + "DROP STATISTICS [dbo].[Orders].[ix_o]"},

		{"certificate drop", func(ctx context.Context, s *Server) error {
			return s.DatabaseRef("AppDB").CertificateRef("app_cert").Drop(ctx)
		}, useAppDB + "DROP CERTIFICATE [app_cert]"},
		{"asymmetric key drop", func(ctx context.Context, s *Server) error {
			return s.DatabaseRef("AppDB").AsymmetricKeyRef("app_key").Drop(ctx)
		}, useAppDB + "DROP ASYMMETRIC KEY [app_key]"},

		{"partition function drop", func(ctx context.Context, s *Server) error {
			return s.DatabaseRef("AppDB").PartitionFunctionRef("pf]1").Drop(ctx)
		}, useAppDB + "DROP PARTITION FUNCTION [pf]]1]"},
		{"partition scheme drop", func(ctx context.Context, s *Server) error {
			return s.DatabaseRef("AppDB").PartitionSchemeRef("ps]1").Drop(ctx)
		}, useAppDB + "DROP PARTITION SCHEME [ps]]1]"},
		{"column master key drop", func(ctx context.Context, s *Server) error {
			return s.DatabaseRef("AppDB").ColumnMasterKeyRef("CMK]1").Drop(ctx)
		}, useAppDB + "DROP COLUMN MASTER KEY [CMK]]1]"},
		{"column encryption key drop", func(ctx context.Context, s *Server) error {
			return s.DatabaseRef("AppDB").ColumnEncryptionKeyRef("CEK]1").Drop(ctx)
		}, useAppDB + "DROP COLUMN ENCRYPTION KEY [CEK]]1]"},
		{"external file format drop", func(ctx context.Context, s *Server) error {
			return s.DatabaseRef("AppDB").ExternalFileFormatRef("ff]1").Drop(ctx)
		}, useAppDB + "DROP EXTERNAL FILE FORMAT [ff]]1]"},
		{"external library drop", func(ctx context.Context, s *Server) error {
			return s.DatabaseRef("AppDB").ExternalLibraryRef("lib]1").Drop(ctx)
		}, useAppDB + "DROP EXTERNAL LIBRARY [lib]]1]"},

		{"configuration set value", func(ctx context.Context, s *Server) error {
			return s.ConfigurationRef("max degree of parallelism").SetValue(ctx, 4)
		}, "EXEC sp_configure N'max degree of parallelism', 4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, col := WithScript(context.Background())
			if err := tc.act(ctx, &Server{}); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if len(col.Statements()) != 1 {
				t.Fatalf("got %d statements, want 1: %v", len(col.Statements()), col.Statements())
			}
			if col.Statements()[0] != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", col.Statements()[0], tc.want)
			}
		})
	}
}

// A handle's name reaches the statement as an identifier, so a bracket in it
// must be doubled — otherwise the statement addresses a different object, or
// none.
func TestRefHandleNamesAreQuoted(t *testing.T) {
	ctx, col := WithScript(context.Background())
	s := &Server{}
	if err := s.ServerRoleRef("odd]role").Drop(ctx); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if err := s.DatabaseRef("AppDB").UserRef("odd]user").Rename(ctx, "plain"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	want := []string{
		"DROP SERVER ROLE [odd]]role]",
		useAppDB + "ALTER USER [odd]]user] WITH NAME = [plain]",
	}
	if len(col.Statements()) != len(want) {
		t.Fatalf("got %d statements, want %d: %v", len(col.Statements()), len(want), col.Statements())
	}
	for i, w := range want {
		if col.Statements()[i] != w {
			t.Errorf("statement %d: got %q, want %q", i, col.Statements()[i], w)
		}
	}
}

// Under WithScript nothing ran, so a write that mirrors its change back onto
// the receiver must leave a handle alone — a handle claiming state the server
// was never told about is what the next call would build from.
func TestRefHandleIsNotMutatedWhileScripting(t *testing.T) {
	ctx, _ := WithScript(context.Background())
	u := (&Server{}).DatabaseRef("AppDB").UserRef("app")
	if err := u.Rename(ctx, "app2"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if u.Name != "app" {
		t.Errorf("Name = %q after a scripted rename, want %q", u.Name, "app")
	}
	if err := u.SetDefaultSchema(ctx, "sales"); err != nil {
		t.Fatalf("SetDefaultSchema: %v", err)
	}
	if u.DefaultSchema != "" {
		t.Errorf("DefaultSchema = %q after a scripted change, want empty", u.DefaultSchema)
	}

	c := (&Server{}).ConfigurationRef("max degree of parallelism")
	if err := c.SetValue(ctx, 4); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	if c.Value != 0 {
		t.Errorf("Value = %d after a scripted sp_configure, want 0", c.Value)
	}
}
