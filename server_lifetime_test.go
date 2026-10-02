package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"
)

// -- fake driver whose every read but loadInfo's waits for its context -------

type lifetimeDriver struct{ started chan string }

func (d lifetimeDriver) Open(string) (driver.Conn, error) { return &lifetimeConn{d.started}, nil }

type lifetimeConn struct{ started chan string }

func (c *lifetimeConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *lifetimeConn) Close() error                        { return nil }
func (c *lifetimeConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (c *lifetimeConn) QueryContext(ctx context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(q, "SERVERPROPERTY('ServerName')") || strings.Contains(q, "dm_os_sys_info") {
		return fakeInfoAnswer(q, nil)
	}
	c.started <- q
	<-ctx.Done()
	return nil, ctx.Err()
}

var lifetimeStarted = make(chan string, 8)

func init() { sql.Register("gosmolifetime", lifetimeDriver{lifetimeStarted}) }

// Close must stop a statement in flight even when the caller's own context
// never ends: closing the *sql.DB alone leaves it running, and with it the
// session on the server.
func TestCloseCancelsAReadInFlight(t *testing.T) {
	for name, read := range map[string]func(context.Context, *Server) error{
		"server row": func(ctx context.Context, s *Server) error {
			_, err := s.CurrentLogin(ctx)
			return err
		},
		"server rows": func(ctx context.Context, s *Server) error {
			_, err := s.Databases(ctx)
			return err
		},
		"database rows": func(ctx context.Context, s *Server) error {
			_, err := s.DatabaseRef("d").Tables(ctx)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			pool, err := sql.Open("gosmolifetime", "")
			if err != nil {
				t.Fatalf("sql.Open: %v", err)
			}
			s, err := NewServer(context.Background(), pool)
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}

			done := make(chan error, 1)
			go func() { done <- read(context.Background(), s) }()
			select {
			case <-lifetimeStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("the read never reached the driver")
			}
			s.Close()

			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("read after Close = %v, want context.Canceled", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Close did not stop the read")
			}
			if s.Context().Err() == nil {
				t.Error("Context() not done after Close")
			}
		})
	}
}

// A Server built without newServer — a test's bare literal — has no lifetime,
// nor has a nil one; Context must still be usable.
func TestContextOfABareServerIsNeverNil(t *testing.T) {
	if (&Server{}).Context() == nil {
		t.Fatal("Context() = nil")
	}
	if (*Server)(nil).Context() == nil {
		t.Fatal("nil Server: Context() = nil")
	}
}
