package gosmo

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// ApplyPermission at object, schema, database and server scope, each verb,
// called with the zero PermissionOptions. This pins the exact statement and
// the exact validation error each produces — the plain form every caller
// without a modifier gets. It began as the pin for the twelve
// Grant/Deny/Revoke methods these four classes had until 2026-10-08, and
// pins the same statements now.
func TestApplyPermissionRendersAndRejects(t *testing.T) {
	cases := []struct {
		name string
		// call issues the statement; badPermission issues the same call with
		// a permission name no allowlist contains.
		call          func(d *Database, s *Server, ctx context.Context) error
		badPermission func(d *Database, s *Server, ctx context.Context) error
		wantStmt      string
		wantErr       string
	}{
		{
			name: "object grant",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.ApplyPermission(ctx, VerbGrant, Securable{Class: SecurableTable, Schema: "dbo", Name: "Orders"}, PermSelect, "app_reader", PermissionOptions{})
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.ApplyPermission(ctx, VerbGrant, Securable{Class: SecurableTable, Schema: "dbo", Name: "Orders"}, ObjectPermission("NOPE"), "app_reader", PermissionOptions{})
			},
			wantStmt: "GRANT SELECT ON [dbo].[Orders] TO [app_reader]",
			wantErr:  `gosmo: grant permission: "NOPE" cannot be granted on a table`,
		},
		{
			name: "object deny",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.ApplyPermission(ctx, VerbDeny, Securable{Class: SecurableTable, Schema: "dbo", Name: "Orders"}, PermSelect, "app_reader", PermissionOptions{})
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.ApplyPermission(ctx, VerbDeny, Securable{Class: SecurableTable, Schema: "dbo", Name: "Orders"}, ObjectPermission("NOPE"), "app_reader", PermissionOptions{})
			},
			wantStmt: "DENY SELECT ON [dbo].[Orders] TO [app_reader]",
			wantErr:  `gosmo: deny permission: "NOPE" cannot be granted on a table`,
		},
		{
			name: "object revoke",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.ApplyPermission(ctx, VerbRevoke, Securable{Class: SecurableTable, Schema: "dbo", Name: "Orders"}, PermSelect, "app_reader", PermissionOptions{})
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.ApplyPermission(ctx, VerbRevoke, Securable{Class: SecurableTable, Schema: "dbo", Name: "Orders"}, ObjectPermission("NOPE"), "app_reader", PermissionOptions{})
			},
			wantStmt: "REVOKE SELECT ON [dbo].[Orders] FROM [app_reader]",
			wantErr:  `gosmo: revoke permission: "NOPE" cannot be granted on a table`,
		},
		{
			name: "schema grant",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.ApplyPermission(ctx, VerbGrant, Securable{Class: SecurableSchema, Name: "sales"}, PermSelect, "app_reader", PermissionOptions{})
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.ApplyPermission(ctx, VerbGrant, Securable{Class: SecurableSchema, Name: "sales"}, ObjectPermission("NOPE"), "app_reader", PermissionOptions{})
			},
			wantStmt: "GRANT SELECT ON SCHEMA::[sales] TO [app_reader]",
			wantErr:  `gosmo: grant permission: "NOPE" cannot be granted on a schema`,
		},
		{
			name: "schema deny",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.ApplyPermission(ctx, VerbDeny, Securable{Class: SecurableSchema, Name: "sales"}, PermUpdate, "app_reader", PermissionOptions{})
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.ApplyPermission(ctx, VerbDeny, Securable{Class: SecurableSchema, Name: "sales"}, ObjectPermission("NOPE"), "app_reader", PermissionOptions{})
			},
			wantStmt: "DENY UPDATE ON SCHEMA::[sales] TO [app_reader]",
			wantErr:  `gosmo: deny permission: "NOPE" cannot be granted on a schema`,
		},
		{
			name: "schema revoke",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.ApplyPermission(ctx, VerbRevoke, Securable{Class: SecurableSchema, Name: "sales"}, PermExecute, "app_reader", PermissionOptions{})
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.ApplyPermission(ctx, VerbRevoke, Securable{Class: SecurableSchema, Name: "sales"}, ObjectPermission("NOPE"), "app_reader", PermissionOptions{})
			},
			wantStmt: "REVOKE EXECUTE ON SCHEMA::[sales] FROM [app_reader]",
			wantErr:  `gosmo: revoke permission: "NOPE" cannot be granted on a schema`,
		},
		{
			name: "database grant",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.ApplyPermission(ctx, VerbGrant, Securable{Class: SecurableDatabase}, DatabasePermission("CREATE TABLE"), "app_reader", PermissionOptions{})
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.ApplyPermission(ctx, VerbGrant, Securable{Class: SecurableDatabase}, DatabasePermission("NOPE"), "app_reader", PermissionOptions{})
			},
			wantStmt: "GRANT CREATE TABLE TO [app_reader]",
			wantErr:  `gosmo: grant permission: "NOPE" cannot be granted on a database`,
		},
		{
			name: "database deny",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.ApplyPermission(ctx, VerbDeny, Securable{Class: SecurableDatabase}, DatabasePermission("CREATE TABLE"), "app_reader", PermissionOptions{})
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.ApplyPermission(ctx, VerbDeny, Securable{Class: SecurableDatabase}, DatabasePermission("NOPE"), "app_reader", PermissionOptions{})
			},
			wantStmt: "DENY CREATE TABLE TO [app_reader]",
			wantErr:  `gosmo: deny permission: "NOPE" cannot be granted on a database`,
		},
		{
			name: "database revoke",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.ApplyPermission(ctx, VerbRevoke, Securable{Class: SecurableDatabase}, DatabasePermission("CREATE TABLE"), "app_reader", PermissionOptions{})
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.ApplyPermission(ctx, VerbRevoke, Securable{Class: SecurableDatabase}, DatabasePermission("NOPE"), "app_reader", PermissionOptions{})
			},
			wantStmt: "REVOKE CREATE TABLE FROM [app_reader]",
			wantErr:  `gosmo: revoke permission: "NOPE" cannot be granted on a database`,
		},
		{
			name: "server grant",
			call: func(_ *Database, s *Server, ctx context.Context) error {
				return s.ApplyPermission(ctx, VerbGrant, Securable{Class: SecurableServer}, ServerPermission("VIEW SERVER STATE"), "app_login", PermissionOptions{})
			},
			badPermission: func(_ *Database, s *Server, ctx context.Context) error {
				return s.ApplyPermission(ctx, VerbGrant, Securable{Class: SecurableServer}, ServerPermission("NOPE"), "app_login", PermissionOptions{})
			},
			wantStmt: "USE master; GRANT VIEW SERVER STATE TO [app_login]",
			wantErr:  `gosmo: grant permission: "NOPE" cannot be granted on a server`,
		},
		{
			name: "server deny",
			call: func(_ *Database, s *Server, ctx context.Context) error {
				return s.ApplyPermission(ctx, VerbDeny, Securable{Class: SecurableServer}, ServerPermission("VIEW SERVER STATE"), "app_login", PermissionOptions{})
			},
			badPermission: func(_ *Database, s *Server, ctx context.Context) error {
				return s.ApplyPermission(ctx, VerbDeny, Securable{Class: SecurableServer}, ServerPermission("NOPE"), "app_login", PermissionOptions{})
			},
			wantStmt: "USE master; DENY VIEW SERVER STATE TO [app_login]",
			wantErr:  `gosmo: deny permission: "NOPE" cannot be granted on a server`,
		},
		{
			name: "server revoke",
			call: func(_ *Database, s *Server, ctx context.Context) error {
				return s.ApplyPermission(ctx, VerbRevoke, Securable{Class: SecurableServer}, ServerPermission("VIEW SERVER STATE"), "app_login", PermissionOptions{})
			},
			badPermission: func(_ *Database, s *Server, ctx context.Context) error {
				return s.ApplyPermission(ctx, VerbRevoke, Securable{Class: SecurableServer}, ServerPermission("NOPE"), "app_login", PermissionOptions{})
			},
			wantStmt: "USE master; REVOKE VIEW SERVER STATE FROM [app_login]",
			wantErr:  `gosmo: revoke permission: "NOPE" cannot be granted on a server`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, ctx, script := scriptedDB(t)
			if err := tc.call(d, d.server, ctx); err != nil {
				t.Fatalf("call: %v", err)
			}
			if got := onlyStatement(t, script); got != tc.wantStmt {
				t.Errorf("statement =\n%q\nwant\n%q", got, tc.wantStmt)
			}

			d, ctx, script = scriptedDB(t)
			err := tc.badPermission(d, d.server, ctx)
			if err == nil {
				t.Fatal("an unrecognized permission name was accepted")
			}
			if !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("error %v does not wrap ErrInvalidRequest", err)
			}
			if err.Error() != tc.wantErr {
				t.Errorf("error =\n%q\nwant\n%q", err.Error(), tc.wantErr)
			}
			if len(script.Statements()) != 0 {
				t.Errorf("a rejected call still issued %v", script.Statements())
			}
		})
	}
}

// Each class takes the permission names its own GRANT page lists and refuses
// the rest before anything is sent: EXECUTE is a procedure's and not a
// table's, SELECT a table-valued function's and not a scalar one's.
func TestApplyPermissionPerClassAllowlist(t *testing.T) {
	cases := []struct {
		sec  Securable
		perm ObjectPermission
		ok   bool
		want string // the statement, when ok
	}{
		{Securable{Class: SecurableProcedure, Schema: "dbo", Name: "p"}, PermExecute, true, "GRANT EXECUTE ON [dbo].[p] TO [x]"},
		{Securable{Class: SecurableProcedure, Schema: "dbo", Name: "p"}, PermSelect, false, ""},
		{Securable{Class: SecurableTable, Schema: "dbo", Name: "t"}, PermExecute, false, ""},
		{Securable{Class: SecurableView, Schema: "dbo", Name: "v"}, PermSelect, true, "GRANT SELECT ON [dbo].[v] TO [x]"},
		{Securable{Class: SecurableScalarFunction, Schema: "dbo", Name: "f"}, PermExecute, true, "GRANT EXECUTE ON [dbo].[f] TO [x]"},
		{Securable{Class: SecurableScalarFunction, Schema: "dbo", Name: "f"}, PermSelect, false, ""},
		{Securable{Class: SecurableTableFunction, Schema: "dbo", Name: "tf"}, PermSelect, true, "GRANT SELECT ON [dbo].[tf] TO [x]"},
		{Securable{Class: SecurableTableFunction, Schema: "dbo", Name: "tf"}, PermInsert, false, ""},
		{Securable{Class: SecurableInlineFunction, Schema: "dbo", Name: "itf"}, PermInsert, true, "GRANT INSERT ON [dbo].[itf] TO [x]"},
		{Securable{Class: SecurableProcedure, Schema: "dbo", Name: "p"}, PermReferences, true, "GRANT REFERENCES ON [dbo].[p] TO [x]"},
		{Securable{Class: SecurableView, Schema: "dbo", Name: "v"}, PermViewChangeTracking, false, ""},
		{Securable{Class: SecurableTable, Schema: "dbo", Name: "t"}, PermViewChangeTracking, true, "GRANT VIEW CHANGE TRACKING ON [dbo].[t] TO [x]"},
		{Securable{Class: SecurableTableFunction, Schema: "dbo", Name: "tf"}, PermExecute, false, ""},
		{Securable{Class: SecurableSequence, Schema: "dbo", Name: "s"}, PermUpdate, true, "GRANT UPDATE ON [dbo].[s] TO [x]"},
		{Securable{Class: SecurableSequence, Schema: "dbo", Name: "s"}, PermSelect, false, ""},
		{Securable{Class: SecurableSynonym, Schema: "dbo", Name: "syn"}, PermExecute, true, "GRANT EXECUTE ON [dbo].[syn] TO [x]"},
		{Securable{Class: SecurableSynonym, Schema: "dbo", Name: "syn"}, PermAlter, true, "GRANT ALTER ON [dbo].[syn] TO [x]"},
		{Securable{Class: SecurableSynonym, Schema: "dbo", Name: "syn"}, PermReferences, false, ""},
		{Securable{Class: SecurableUserType, Schema: "dbo", Name: "ty"}, PermExecute, true, "GRANT EXECUTE ON TYPE::[dbo].[ty] TO [x]"},
		{Securable{Class: SecurableUserType, Schema: "dbo", Name: "ty"}, PermSelect, false, ""},
		{Securable{Class: SecurableCertificate, Name: "c"}, PermControl, true, "GRANT CONTROL ON CERTIFICATE::[c] TO [x]"},
		{Securable{Class: SecurableCertificate, Name: "c"}, PermExecute, false, ""},
		{Securable{Class: SecurableSchema, Name: "s"}, PermExecute, true, "GRANT EXECUTE ON SCHEMA::[s] TO [x]"},
	}
	for _, tc := range cases {
		name := string(tc.sec.Class) + " " + string(tc.perm)
		t.Run(name, func(t *testing.T) {
			d, ctx, script := scriptedDB(t)
			err := d.ApplyPermission(ctx, VerbGrant, tc.sec, tc.perm, "x", PermissionOptions{})
			if !tc.ok {
				if !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("err = %v, want ErrInvalidRequest", err)
				}
				if n := len(script.Statements()); n != 0 {
					t.Errorf("a refused call still issued %v", script.Statements())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := onlyStatement(t, script); got != tc.want {
				t.Errorf("statement = %q, want %q", got, tc.want)
			}
			if !slices.Contains(tc.sec.Class.PermissionNames(), string(tc.perm)) {
				t.Errorf("PermissionNames(%s) lacks %s, which it accepts", tc.sec.Class, tc.perm)
			}
		})
	}
}

// A securable that is incomplete or in the wrong place is refused before
// anything is rendered.
func TestApplyPermissionRefusesMalformedSecurables(t *testing.T) {
	cases := []struct {
		name string
		call func(d *Database, s *Server, ctx context.Context) error
		is   error
	}{
		{"server class on a database", func(d *Database, _ *Server, ctx context.Context) error {
			return d.ApplyPermission(ctx, VerbGrant, Securable{Class: SecurableServer}, ServerPermission("VIEW SERVER STATE"), "x", PermissionOptions{})
		}, ErrInvalidRequest},
		{"table class on a server", func(_ *Database, s *Server, ctx context.Context) error {
			return s.ApplyPermission(ctx, VerbGrant, Securable{Class: SecurableTable, Schema: "dbo", Name: "t"}, PermSelect, "x", PermissionOptions{})
		}, ErrInvalidRequest},
		{"unknown class", func(d *Database, _ *Server, ctx context.Context) error {
			return d.ApplyPermission(ctx, VerbGrant, Securable{Class: "QUEUE", Schema: "dbo", Name: "q"}, PermAlter, "x", PermissionOptions{})
		}, ErrInvalidRequest},
		{"unknown verb", func(d *Database, _ *Server, ctx context.Context) error {
			return d.ApplyPermission(ctx, "ALLOW", Securable{Class: SecurableTable, Schema: "dbo", Name: "t"}, PermSelect, "x", PermissionOptions{})
		}, ErrInvalidRequest},
		{"no name", func(d *Database, _ *Server, ctx context.Context) error {
			return d.ApplyPermission(ctx, VerbGrant, Securable{Class: SecurableProcedure, Schema: "dbo"}, PermExecute, "x", PermissionOptions{})
		}, ErrInvalidRequest},
		{"no schema", func(d *Database, _ *Server, ctx context.Context) error {
			return d.ApplyPermission(ctx, VerbGrant, Securable{Class: SecurableProcedure, Name: "p"}, PermExecute, "x", PermissionOptions{})
		}, ErrSchemaRequired},
		{"schema on a certificate", func(d *Database, _ *Server, ctx context.Context) error {
			return d.ApplyPermission(ctx, VerbGrant, Securable{Class: SecurableCertificate, Schema: "dbo", Name: "c"}, PermControl, "x", PermissionOptions{})
		}, ErrInvalidRequest},
		{"name on a database permission", func(d *Database, _ *Server, ctx context.Context) error {
			return d.ApplyPermission(ctx, VerbGrant, Securable{Class: SecurableDatabase, Name: "appdb"}, DatabasePermission("CONNECT"), "x", PermissionOptions{})
		}, ErrInvalidRequest},
		{"columns on a procedure", func(d *Database, _ *Server, ctx context.Context) error {
			return d.ApplyPermission(ctx, VerbGrant, Securable{Class: SecurableProcedure, Schema: "dbo", Name: "p", Columns: []string{"c"}}, PermExecute, "x", PermissionOptions{})
		}, ErrInvalidRequest},
		{"nil permission", func(d *Database, _ *Server, ctx context.Context) error {
			return d.ApplyPermission(ctx, VerbGrant, Securable{Class: SecurableTable, Schema: "dbo", Name: "t"}, nil, "x", PermissionOptions{})
		}, ErrInvalidRequest},
		{"WITH GRANT OPTION on a deny", func(d *Database, _ *Server, ctx context.Context) error {
			return d.ApplyPermission(ctx, VerbDeny, Securable{Class: SecurableTable, Schema: "dbo", Name: "t"}, PermSelect, "x", PermissionOptions{WithGrantOption: true})
		}, ErrInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, ctx, script := scriptedDB(t)
			err := tc.call(d, d.server, ctx)
			if !errors.Is(err, tc.is) {
				t.Fatalf("err = %v, want %v", err, tc.is)
			}
			if n := len(script.Statements()); n != 0 {
				t.Errorf("a refused call still issued %v", script.Statements())
			}
		})
	}
}

func TestSecurableClassPermissionNamesMatchTheOlderCatalogs(t *testing.T) {
	for class, want := range map[SecurableClass][]string{
		SecurableServer:   ServerPermissionNames(),
		SecurableDatabase: DatabasePermissionNames(),
		SecurableSchema:   SchemaPermissionNames(),
		SecurableTable:    ObjectPermissionNames(),
	} {
		if got := class.PermissionNames(); !slices.Equal(got, want) {
			t.Errorf("%s.PermissionNames() = %v, want %v", class, got, want)
		}
	}
	if got := SecurableClass("QUEUE").PermissionNames(); len(got) != 0 {
		t.Errorf("an unknown class lists %v", got)
	}
}
