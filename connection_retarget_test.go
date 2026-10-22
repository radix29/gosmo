package gosmo

import (
	"context"
	"testing"
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

// A Server gosmo did not open the pool for has no login database to report.
func TestInitialDatabaseOfNewServerPool(t *testing.T) {
	if got := captureServer(t, 17).InitialDatabase(); got != "" {
		t.Errorf("InitialDatabase = %q, want empty", got)
	}
	if got := (*Server)(nil).InitialDatabase(); got != "" {
		t.Errorf("nil Server: InitialDatabase = %q, want empty", got)
	}
}
