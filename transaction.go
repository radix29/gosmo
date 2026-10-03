package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
)

// sqlConn is what a statement runs on: a pinned *sql.Conn, or the *sql.Tx of
// an InTransaction call.
type sqlConn interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// txCtxKey carries the *serverTx InTransaction installs.
type txCtxKey struct{}

// serverTx is one InTransaction call's transaction.
type serverTx struct {
	server *Server
	tx     *sql.Tx
	// home is the database the session was in when the transaction began.
	// A database-scoped statement switches the session with USE, and on a
	// pooled connection the next use resets it; on the one session a
	// transaction holds nothing does, so a server-scoped statement after one
	// switches back first (see serverConn).
	home string

	mu      sync.Mutex
	away    bool         // a database-scoped statement has switched the session from home
	pending []heldReport // statement-observer reports held until COMMIT
}

// heldReport is a report held back from a statement observer until the
// transaction commits, with the observer it was for.
type heldReport struct {
	fn func(ScriptEntry)
	e  ScriptEntry
}

// txFrom is ctx's transaction on s, or nil: none, or one on another Server,
// whose statements are not s's to run.
func txFrom(ctx context.Context, s *Server) *serverTx {
	t, _ := ctx.Value(txCtxKey{}).(*serverTx)
	if t == nil || t.server != s {
		return nil
	}
	return t
}

// InTransaction runs fn in one transaction on s: every statement a gosmo
// method issues through s with fn's context — writes and reads, server- and
// database-scoped — runs on that transaction's one session. fn returning nil
// commits; an error, a panic, or ctx (or s) ending first rolls back, and the
// error is returned. Pool and workload-group DDL, CREATE and ALTER of most
// securables and objects, and permission statements are transactional, so a
// multi-step change either lands whole or not at all.
//
// Reads go through the transaction too. They see its uncommitted writes, and
// they have to: a read on another session of a catalog row the transaction
// has changed would wait on the transaction's own locks.
//
// What it does not cover:
//   - Statements SQL Server refuses inside a user transaction fail as they
//     would by hand: ALTER RESOURCE GOVERNOR RECONFIGURE, DISABLE and RESET
//     STATISTICS (Msg 574), ALTER DATABASE (Msg 226), CREATE/DROP DATABASE,
//     BACKUP, RESTORE, and so on. Run those after InTransaction returns.
//   - Methods that need a session of their own refuse with an ErrUnsupported
//     error: Backup and Restore with progress, BulkInsert and the effective
//     permissions of another login.
//   - Statements issued through another Server, or run on DB() directly, are
//     not part of it.
//   - fn's statements run one at a time on one session, so fn must not call
//     into gosmo with its context from several goroutines at once.
//   - A method that mirrors its write onto its receiver (a Rename updating
//     Name) has done so before COMMIT, and a rollback does not undo it.
//     After a failed InTransaction, read the objects again.
//
// A statement observer (WithStatementObserver) on fn's context hears of each
// statement after COMMIT succeeds, in order, and of none on a rollback: it
// only ever reports what is in force.
//
// A call with a context already inside InTransaction on s joins that
// transaction: fn's error is returned, and the outer call decides whether to
// commit. Inside InTransaction on another Server it refuses, since one
// context carries one transaction. Under WithScript it runs fn and nothing
// else: the statements are collected, not run, and the script carries no
// BEGIN TRANSACTION.
func (s *Server) InTransaction(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	if Scripting(ctx) {
		return fn(ctx)
	}
	if outer, _ := ctx.Value(txCtxKey{}).(*serverTx); outer != nil {
		if outer.server == s {
			return fn(ctx)
		}
		return unsupportedf("gosmo: begin transaction: already inside a transaction on another server")
	}

	bctx, release := s.bound(ctx)
	defer release()
	tx, err := s.db.BeginTx(bctx, nil)
	if err != nil {
		return fmt.Errorf("gosmo: begin transaction: %w", withAllMessages(err))
	}
	t := &serverTx{server: s, tx: tx}
	if err := tx.QueryRowContext(bctx, "SELECT DB_NAME()").Scan(&t.home); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("gosmo: begin transaction: %w", withAllMessages(err))
	}

	committed := false
	defer func() {
		if !committed {
			// Rollback's error is not the caller's: the transaction is gone
			// either way, and a session that broke takes it with it.
			_ = tx.Rollback()
		}
	}()
	if err := fn(context.WithValue(ctx, txCtxKey{}, t)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("gosmo: commit transaction: %w", withAllMessages(err))
	}
	committed = true
	for _, o := range t.pending {
		o.fn(o.e)
	}
	return nil
}

// serverConn is what a server-scoped statement runs on inside t: its
// transaction, first switched back to the database it began in if a
// database-scoped statement left it elsewhere.
func (t *serverTx) serverConn(ctx context.Context) (sqlConn, error) {
	t.mu.Lock()
	away := t.away
	t.mu.Unlock()
	if away && t.home != "" {
		if _, err := t.tx.ExecContext(ctx, "USE "+quoteIdent(t.home)); err != nil {
			return nil, fmt.Errorf("gosmo: USE %s: %w", t.home, err)
		}
		t.mu.Lock()
		t.away = false
		t.mu.Unlock()
	}
	return t.tx, nil
}

// leftHome records that a database-scoped statement switched the session.
func (t *serverTx) leftHome() {
	t.mu.Lock()
	t.away = true
	t.mu.Unlock()
}

// hold keeps e for ctx's statement observer until the transaction commits.
func (t *serverTx) hold(ctx context.Context, e ScriptEntry) {
	if fn := observerFrom(ctx); fn != nil {
		t.mu.Lock()
		t.pending = append(t.pending, heldReport{fn: fn, e: e})
		t.mu.Unlock()
	}
}

// refuseInTx is the ErrUnsupported error of a method that needs a session of
// its own, when ctx is inside InTransaction on s; nil otherwise.
func refuseInTx(ctx context.Context, s *Server, op string) error {
	if txFrom(ctx, s) == nil {
		return nil
	}
	return unsupportedf("gosmo: %s: not possible inside InTransaction — it needs a session of its own", op)
}
