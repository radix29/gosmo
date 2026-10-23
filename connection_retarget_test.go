package gosmo

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
)

// loginServer is captureServer with a Connect-style connector whose login
// database is database, so leftLoginDatabase has a pool to move.
func loginServer(t *testing.T, database string) *Server {
	t.Helper()
	s := captureServer(t, 17)
	opts := ConnectionOptions{Server: "h", User: "u", Password: "p", Database: database}
	applyDefaults(&opts)
	login, err := newRetargetingConnector(opts)
	if err != nil {
		t.Fatalf("newRetargetingConnector: %v", err)
	}
	s.login = login
	return s
}

// A write that leaves the pool's login database unopenable moves the pool, or
// every connection dialled afterwards is refused at login (Msg 4060) until the
// caller reconnects (gossms B27).
func TestWriteOnLoginDatabaseMovesThePool(t *testing.T) {
	cases := []struct {
		name  string
		write func(context.Context, *Server) error
		want  string
	}{
		{"Drop", func(ctx context.Context, s *Server) error {
			return s.DatabaseRef("Sales").Drop(ctx, TerminationNone)
		}, "master"},
		{"Detach", func(ctx context.Context, s *Server) error {
			return s.DatabaseRef("Sales").Detach(ctx, DetachOptions{})
		}, "master"},
		{"SetOffline", func(ctx context.Context, s *Server) error {
			return s.DatabaseRef("Sales").SetOffline(ctx)
		}, "master"},
		{"Rename", func(ctx context.Context, s *Server) error {
			return s.DatabaseRef("Sales").Rename(ctx, "Sales2", TerminationNone)
		}, "Sales2"},
		{"DatabaseSnapshot.Drop", func(ctx context.Context, s *Server) error {
			return s.DatabaseSnapshotRef("Sales").Drop(ctx)
		}, "master"},
		{"Restore NORECOVERY", func(ctx context.Context, s *Server) error {
			return s.Restore(ctx, RestoreOptions{Database: "Sales",
				Devices: []BackupTarget{DiskTarget("x.bak")}, Recovery: RestoreWithNoRecovery})
		}, "master"},
		{"SetUserAccess SINGLE_USER", func(ctx context.Context, s *Server) error {
			return s.DatabaseRef("Sales").SetUserAccess(ctx, UserAccessSingle)
		}, "master"},
		{"UnjoinDatabase", func(ctx context.Context, s *Server) error {
			return s.AvailabilityGroupRef("ag").UnjoinDatabase(ctx, "Sales")
		}, "master"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The pool's database matched ignoring case, as SQL Server's
			// default collation does.
			s := loginServer(t, "sales")
			if err := tc.write(t.Context(), s); err != nil {
				t.Fatalf("write: %v", err)
			}
			if got := s.InitialDatabase(); got != tc.want {
				t.Errorf("InitialDatabase = %q, want %q", got, tc.want)
			}

			// Another database's write leaves the pool alone.
			s = loginServer(t, "Other")
			if err := tc.write(t.Context(), s); err != nil {
				t.Fatalf("write: %v", err)
			}
			if got := s.InitialDatabase(); got != "Other" {
				t.Errorf("another database's write: InitialDatabase = %q, want Other", got)
			}

			// A captured script wrote nothing, so nothing moves.
			s = loginServer(t, "Sales")
			ctx, _ := WithScript(t.Context())
			if err := tc.write(ctx, s); err != nil {
				t.Fatalf("write under WithScript: %v", err)
			}
			if got := s.InitialDatabase(); got != "Sales" {
				t.Errorf("under WithScript: InitialDatabase = %q, want Sales", got)
			}
		})
	}
}

// The writes that leave the login database openable leave the pool where it is.
func TestWriteThatKeepsLoginDatabaseOpenable(t *testing.T) {
	writes := map[string]func(context.Context, *Server) error{
		"Restore RECOVERY": func(ctx context.Context, s *Server) error {
			return s.Restore(ctx, RestoreOptions{Database: "Sales",
				Devices: []BackupTarget{DiskTarget("x.bak")}, Recovery: RestoreWithRecovery})
		},
		"Restore STANDBY": func(ctx context.Context, s *Server) error {
			return s.Restore(ctx, RestoreOptions{Database: "Sales",
				Devices: []BackupTarget{DiskTarget("x.bak")}, Recovery: RestoreWithStandBy, StandByFile: "x.tuf"})
		},
		"SetUserAccess MULTI_USER": func(ctx context.Context, s *Server) error {
			return s.DatabaseRef("Sales").SetUserAccess(ctx, UserAccessMulti)
		},
		"SetUserAccess RESTRICTED_USER": func(ctx context.Context, s *Server) error {
			return s.DatabaseRef("Sales").SetUserAccess(ctx, UserAccessRestricted)
		},
	}
	for name, write := range writes {
		t.Run(name, func(t *testing.T) {
			s := loginServer(t, "Sales")
			if err := write(t.Context(), s); err != nil {
				t.Fatalf("write: %v", err)
			}
			if got := s.InitialDatabase(); got != "Sales" {
				t.Errorf("InitialDatabase = %q, want Sales", got)
			}
		})
	}
}

// fakeLogin is a connector whose dial the login database refuses with err
// unless it is master. dials records each dial's database.
type fakeLogin struct {
	database string
	err      error
	dials    *[]string
}

func (f *fakeLogin) Connect(context.Context) (driver.Conn, error) {
	*f.dials = append(*f.dials, f.database)
	if f.database != "master" && f.err != nil {
		return nil, f.err
	}
	return nil, nil
}

func (f *fakeLogin) Driver() driver.Driver { return nil }

// fakeLoginConnector is a retargetingConnector on fakeLogin, logging in to
// database, with fallBack as given.
func fakeLoginConnector(t *testing.T, database string, refusal error, fallBack bool) (*retargetingConnector, *[]string) {
	t.Helper()
	var dials []string
	c, err := newRetargetingConnectorWith(ConnectionOptions{Database: database},
		func(opts ConnectionOptions) (driver.Connector, error) {
			return &fakeLogin{database: opts.Database, err: refusal, dials: &dials}, nil
		})
	if err != nil {
		t.Fatalf("newRetargetingConnectorWith: %v", err)
	}
	c.fallBack = fallBack
	return c, &dials
}

// A dial the login database refuses — another session took it offline,
// restored it WITH NORECOVERY or holds it SINGLE_USER — moves the pool to
// master and dials once more, or every read fails until the caller reconnects.
func TestRefusedDialMovesThePoolToMaster(t *testing.T) {
	// The login's own failure comes last; the refusal is in All.
	login := func(n int32) error {
		e := mssql.Error{Number: 18456, Message: "Login failed"}
		e.All = []mssql.Error{{Number: n}, e}
		return e
	}
	for _, n := range loginRefusalNumbers {
		c, dials := fakeLoginConnector(t, "Sales", login(n), true)
		if _, err := c.Connect(t.Context()); err != nil {
			t.Errorf("Msg %d: Connect = %v, want the retry on master to succeed", n, err)
		}
		if got := *dials; len(got) != 2 || got[0] != "Sales" || got[1] != "master" {
			t.Errorf("Msg %d: dials = %v, want [Sales master]", n, got)
		}
		if got := c.database(); got != "master" {
			t.Errorf("Msg %d: database = %q, want master", n, got)
		}
		// It stays there.
		*dials = nil
		c.Connect(t.Context())
		if got := *dials; len(got) != 1 || got[0] != "master" {
			t.Errorf("Msg %d: next dials = %v, want [master]", n, got)
		}
	}
	// Wrapped, as the driver's errors reach here through database/sql.
	c, _ := fakeLoginConnector(t, "Sales", fmt.Errorf("dial: %w", mssql.Error{Number: 4060}), true)
	if _, err := c.Connect(t.Context()); err != nil || c.database() != "master" {
		t.Errorf("wrapped 4060: Connect = %v, database = %q; want nil, master", err, c.database())
	}
}

// What the fallback leaves alone: a refusal before Connect returned (a
// misspelled database must fail Connect), any other dial failure, a pool
// already on master, and Azure SQL Database (fallBack never set).
func TestRefusedDialThatStaysPut(t *testing.T) {
	cases := []struct {
		name     string
		database string
		err      error
		fallBack bool
		dials    int
	}{
		{"before Connect returned", "Sales", mssql.Error{Number: 4060}, false, 1},
		{"bad password", "Sales", mssql.Error{Number: 18456}, true, 1},
		{"network", "Sales", errors.New("dial tcp: connection refused"), true, 1},
		{"already master", "master", mssql.Error{Number: 4060}, true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, dials := fakeLoginConnector(t, tc.database, tc.err, tc.fallBack)
			if tc.database == "master" {
				// fakeLogin lets master in; make it refuse to see the error
				// come back unretried.
				c.cur.(*fakeLogin).database = "master!"
			}
			if _, err := c.Connect(t.Context()); err == nil {
				t.Fatal("Connect = nil, want the dial's error")
			}
			if len(*dials) != tc.dials {
				t.Errorf("dials = %v, want %d", *dials, tc.dials)
			}
			if got := c.database(); got != tc.database {
				t.Errorf("database = %q, want %q", got, tc.database)
			}
		})
	}
}

// A Server gosmo did not open the pool for has no login database to report.
func TestInitialDatabaseOfNewServerPool(t *testing.T) {
	if got := captureServer(t, 17).InitialDatabase(); got != "" {
		t.Errorf("InitialDatabase = %q, want empty", got)
	}
	if got := (*Server)(nil).InitialDatabase(); got != "" {
		t.Errorf("nil Server: InitialDatabase = %q, want empty", got)
	}
}
