package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Under WithScript InTransaction only runs fn: the statements are collected,
// with no BEGIN TRANSACTION or COMMIT around them, and no connection is used —
// the Server here has none.
func TestInTransactionUnderScriptOnlyRunsFn(t *testing.T) {
	s := &Server{}
	ctx, col := WithScript(context.Background())
	err := s.InTransaction(ctx, func(ctx context.Context) error {
		_, err := s.CreateResourcePool(ctx, CreateResourcePoolRequest{Name: "p"})
		return err
	})
	if err != nil {
		t.Fatalf("InTransaction: %v", err)
	}
	if got, want := col.Statements(), []string{"CREATE RESOURCE POOL [p]"}; !slices.Equal(got, want) {
		t.Errorf("captured %q, want %q", got, want)
	}
}

// One context carries one transaction, so a second Server's InTransaction
// inside the first refuses, and joining the same Server's does not begin
// another.
func TestInTransactionNesting(t *testing.T) {
	a, b := &Server{}, &Server{}
	ctx := context.WithValue(context.Background(), txCtxKey{}, &serverTx{server: a})

	ran := false
	if err := a.InTransaction(ctx, func(context.Context) error { ran = true; return nil }); err != nil || !ran {
		t.Errorf("nested InTransaction on the same Server: ran %v, err %v — want it to join", ran, err)
	}
	if err := b.InTransaction(ctx, func(context.Context) error { return nil }); !errors.Is(err, ErrUnsupported) {
		t.Errorf("InTransaction on another Server inside one: %v, want ErrUnsupported", err)
	}
	if txFrom(ctx, b) != nil {
		t.Error("txFrom matched a transaction on another Server")
	}
}

// The methods that need a session of their own refuse inside a transaction
// before touching the server.
func TestInTransactionRefusesOwnSessionMethods(t *testing.T) {
	s := &Server{}
	ctx := context.WithValue(context.Background(), txCtxKey{}, &serverTx{server: s})
	d := s.DatabaseRef("db")
	if _, err := d.BulkInsert(ctx, BulkCopy{Schema: "dbo", Table: "t", Columns: []string{"a"}}, nil); !errors.Is(err, ErrUnsupported) {
		t.Errorf("BulkInsert: %v, want ErrUnsupported", err)
	}
	if _, err := s.EffectiveServerPermissions(ctx, "l"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("EffectiveServerPermissions: %v, want ErrUnsupported", err)
	}
	if _, err := d.EffectivePermissions(ctx, "u"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("EffectivePermissions: %v, want ErrUnsupported", err)
	}
	progress := func(int, string) {}
	if err := s.Backup(ctx, BackupOptions{Database: "db", Devices: []BackupTarget{DiskTarget("x.bak")}, Progress: progress}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Backup with progress: %v, want ErrUnsupported", err)
	}
}

// txRecDriver records every statement, BEGIN, COMMIT and ROLLBACK it is
// handed, answers any query with one row "master", and fails a statement
// containing "FAIL".
type txRecDriver struct {
	mu  sync.Mutex
	log []string
}

func (d *txRecDriver) add(s string) {
	d.mu.Lock()
	d.log = append(d.log, s)
	d.mu.Unlock()
}

func (d *txRecDriver) Open(string) (driver.Conn, error)             { return txRecConn{d}, nil }
func (d *txRecDriver) Connect(context.Context) (driver.Conn, error) { return txRecConn{d}, nil }
func (d *txRecDriver) Driver() driver.Driver                        { return d }

type txRecConn struct{ d *txRecDriver }

func (c txRecConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c txRecConn) Close() error                        { return nil }
func (c txRecConn) Begin() (driver.Tx, error)           { c.d.add("BEGIN"); return txRecTx(c), nil }
func (c txRecConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	c.d.add(q)
	if strings.Contains(q, "FAIL") {
		return nil, errors.New("refused")
	}
	return driver.ResultNoRows, nil
}
func (c txRecConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	c.d.add(q)
	return &captureRows{rows: [][]driver.Value{{"master"}}}, nil
}

type txRecTx txRecConn

func (t txRecTx) Commit() error   { t.d.add("COMMIT"); return nil }
func (t txRecTx) Rollback() error { t.d.add("ROLLBACK"); return nil }

// execAtomic sends atomicBatch on its own and under WithScript — inside a
// transaction's context too, since a script has no outer transaction — and
// atomicTxBatch, with no transaction statements of its own, inside
// InTransaction.
func TestExecAtomicForm(t *testing.T) {
	stmts := []string{"EXEC dbo.one", "EXEC dbo.two"}
	rec := &txRecDriver{}
	s := &Server{db: sql.OpenDB(rec)}
	defer s.db.Close()
	ctx := context.Background()

	if err := s.execAtomic(ctx, stmts); err != nil {
		t.Fatal(err)
	}
	if err := s.InTransaction(ctx, func(ctx context.Context) error {
		sctx, col := WithScript(ctx)
		if err := s.execAtomic(sctx, stmts); err != nil {
			return err
		}
		if got := col.Statements(); !slices.Equal(got, []string{atomicBatch(stmts)}) {
			t.Errorf("captured inside a transaction %q, want atomicBatch", got)
		}
		return s.execAtomic(ctx, stmts)
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{atomicBatch(stmts), "BEGIN", "SELECT DB_NAME()", atomicTxBatch(stmts), "COMMIT"}
	if !slices.Equal(rec.log, want) {
		t.Errorf("sent:\n%q\nwant:\n%q", rec.log, want)
	}
	for _, bad := range []string{"TRANSACTION", "XACT_ABORT"} {
		if strings.Contains(atomicTxBatch(stmts), bad) {
			t.Errorf("atomicTxBatch holds %s:\n%s", bad, atomicTxBatch(stmts))
		}
	}
	if !strings.HasPrefix(atomicTxBatch(stmts), "BEGIN TRY\nEXEC dbo.one;\nEXEC dbo.two;\n") ||
		!strings.HasSuffix(atomicTxBatch(stmts), "BEGIN CATCH\nTHROW;\nEND CATCH;") {
		t.Errorf("atomicTxBatch:\n%s", atomicTxBatch(stmts))
	}
}

// An atomic write that fails inside InTransaction refuses the COMMIT even
// when fn tolerates the error: the statements before the failing one are in
// the transaction, and committing them is the half-done write.
func TestInTransactionRefusesCommitAfterAtomicFailure(t *testing.T) {
	rec := &txRecDriver{}
	s := &Server{db: sql.OpenDB(rec)}
	defer s.db.Close()
	var inner error
	err := s.InTransaction(context.Background(), func(ctx context.Context) error {
		inner = s.execAtomic(ctx, []string{"EXEC dbo.one", "FAIL"})
		return nil
	})
	if inner == nil || err == nil || !errors.Is(err, inner) {
		t.Fatalf("InTransaction = %v after the write failed with %v, want that error", err, inner)
	}
	if slices.Contains(rec.log, "COMMIT") || !slices.Contains(rec.log, "ROLLBACK") {
		t.Errorf("sent %q, want a ROLLBACK and no COMMIT", rec.log)
	}
}

// Inside a transaction the session's database is tracked, so a database-scoped
// write sends its USE only when the session is not already there: once for
// many writes in one database, again after a server-scoped statement took it
// home, after a failure, and after a server permission's USE master.
func TestInTransactionSendsUSEOnlyWhenMoving(t *testing.T) {
	cases := []struct {
		name string
		fn   func(ctx context.Context, s *Server) error
		want []string
	}{
		{"same database", func(ctx context.Context, s *Server) error {
			d := s.DatabaseRef("app")
			for _, w := range []string{"W1", "W2", "W3"} {
				if _, err := d.exec(ctx, w); err != nil {
					return err
				}
			}
			return nil
		}, []string{"USE [app]", "W1", "W2", "W3"}},
		{"home database", func(ctx context.Context, s *Server) error {
			_, err := s.DatabaseRef("master").exec(ctx, "W1")
			return err
		}, []string{"W1"}},
		{"alternating", func(ctx context.Context, s *Server) error {
			d := s.DatabaseRef("app")
			_, _ = d.exec(ctx, "W1")
			_ = s.exec(ctx, "S1")
			_ = s.exec(ctx, "S2")
			_, err := d.exec(ctx, "W2")
			return err
		}, []string{"USE [app]", "W1", "USE [master]", "S1", "S2", "USE [app]", "W2"}},
		{"failure then write", func(ctx context.Context, s *Server) error {
			d := s.DatabaseRef("app")
			_, _ = d.exec(ctx, "W1")
			_, _ = d.exec(ctx, "FAIL")
			_, err := d.exec(ctx, "W2")
			return err
		}, []string{"USE [app]", "W1", "FAIL", "USE [app]", "W2"}},
		{"read then write", func(ctx context.Context, s *Server) error {
			d := s.DatabaseRef("app")
			rows, err := d.query(ctx, "R1")
			if err != nil {
				return err
			}
			rows.Close()
			_, err = d.exec(ctx, "W1")
			return err
		}, []string{"USE [app]; IF @@ERROR <> 0 RETURN; R1", "W1"}},
		{"server permission", func(ctx context.Context, s *Server) error {
			if err := s.GrantServerPermission(ctx, "CONNECT SQL", "l", PermissionOptions{}); err != nil {
				return err
			}
			_, err := s.DatabaseRef("master").exec(ctx, "W1")
			return err
		}, []string{"USE master; GRANT CONNECT SQL TO [l]", "USE [master]", "W1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &txRecDriver{}
			s := &Server{db: sql.OpenDB(rec)}
			defer s.db.Close()
			if err := s.InTransaction(context.Background(), func(ctx context.Context) error {
				return tc.fn(ctx, s)
			}); err != nil {
				t.Fatal(err)
			}
			want := append(append([]string{"BEGIN", "SELECT DB_NAME()"}, tc.want...), "COMMIT")
			if !slices.Equal(rec.log, want) {
				t.Errorf("sent:\n%q\nwant:\n%q", rec.log, want)
			}
		})
	}
}
