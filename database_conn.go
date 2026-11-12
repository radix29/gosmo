package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

// -- Connection helpers --------------------------------------------------------
// A pooled call never leaves its session in d. A read (procRead) and a write
// (execIn) are RPC calls of d's own sp_executesql, and ExecProc names its
// procedure with d's name; only what needs a session switched to d for more
// than one call (capturePlan, a bulk copy) switches it, through withSessionIn,
// and switches it back. A session left in d went back to the pool holding a
// shared lock on d until the next checkout reset it, and someone else's
// RESTORE, SET OFFLINE or SET SINGLE_USER failed "Exclusive access could not
// be obtained" meanwhile (live_proc_read_test.go, live_proc_write_test.go).

// withConn hands fn a connection to run a statement on that names d itself —
// a procRead call, or ExecProc's three-part procedure name. Acquiring is
// idempotent and safe to retry against a fresh connection on a transient
// failure (a dropped pooled connection, etc.), same as query/queryRow's own
// acquire step below; fn is not retried: fn is the caller's actual write, and
// blindly re-running it on a fresh connection after a partial failure could
// re-apply side effects that already took hold.
//
// Inside InTransaction it is the transaction's session, switched to d (USE),
// and nothing is retried: a fresh connection is outside the transaction.
func (d *Database) withConn(ctx context.Context, fn func(context.Context, sqlConn) error) error {
	ctx, release := d.server.bound(ctx)
	defer release()
	if t := txFrom(ctx, d.server); t != nil {
		if !t.in(d.Name) {
			if err := d.use(ctx, t.tx); err != nil {
				t.lost()
				return err
			}
			t.at(d.Name)
		}
		err := withAllMessages(fn(ctx, t.tx))
		if err != nil {
			t.lost()
		}
		return err
	}
	conn, err := withRetry(ctx, func() (*sql.Conn, error) {
		conn, err := d.server.db.Conn(ctx)
		if err != nil {
			return nil, fmt.Errorf("gosmo: acquire connection: %w", err)
		}
		return conn, nil
	})
	if err != nil {
		return err
	}
	defer conn.Close()
	return withAllMessages(fn(ctx, conn))
}

// execIn runs one statement q in d, never retried: fn gets the connection and
// the form of q to send there with its arguments. On a pooled connection that
// is procRead's RPC call of [d].sys.sp_executesql, so the session stays in its
// login database, and a failure recheckEntry finds was d refusing the call is
// reported as the EntryError it is. Inside InTransaction it is q
// itself, on the transaction's session switched to d.
//
// The statement is a batch of its own inside sp_executesql, so a CREATE VIEW,
// PROCEDURE, FUNCTION, TRIGGER or SCHEMA still opens its batch, and a SET in
// it ends with the call. A USE in it would not (see queryOn) — no write sends
// one.
func (d *Database) execIn(ctx context.Context, q string, args []any, fn func(ctx context.Context, c sqlConn, q string, args []any) error) error {
	if txFrom(ctx, d.server) != nil {
		return d.withConn(ctx, func(ctx context.Context, c sqlConn) error { return fn(ctx, c, q, args) })
	}
	call, cargs, err := d.procRead(q, args)
	if err != nil {
		return err
	}
	return d.withConn(ctx, func(ctx context.Context, c sqlConn) error {
		err := fn(ctx, c, call, cargs)
		if err != nil {
			if uerr := d.recheckEntry(ctx, c, err); uerr != nil {
				return uerr
			}
		}
		return err
	})
}

// withSessionIn is withConn for fn that needs its session in d across several
// calls — SET SHOWPLAN_XML around a batch, a bulk copy. A pooled session is
// switched to d first (the switch retried with the acquire, as AcquireConn's
// is) and back to the database it came from afterwards, so it goes back to
// the pool where it started. A session that cannot be switched back is
// discarded rather than pooled. It refuses inside InTransaction, whose
// session is already switched: a caller that may run there uses withConn
// then (capturePlan).
func (d *Database) withSessionIn(ctx context.Context, fn func(context.Context, *sql.Conn) error) error {
	if txFrom(ctx, d.server) != nil {
		return unsupportedf("gosmo: %s: a call that holds its own session cannot run inside InTransaction", d.Name)
	}
	ctx, release := d.server.bound(ctx)
	defer release()
	var home string
	conn, err := withRetry(ctx, func() (*sql.Conn, error) {
		conn, err := d.server.db.Conn(ctx)
		if err != nil {
			return nil, fmt.Errorf("gosmo: acquire connection: %w", err)
		}
		if err := conn.QueryRowContext(ctx, "DECLARE @home sysname = DB_NAME(); "+d.useBatch("SELECT @home")).Scan(&home); err != nil {
			conn.Close()
			return nil, &EntryError{Database: d.Name, Err: err}
		}
		return conn, nil
	})
	if err != nil {
		return err
	}
	defer conn.Close()
	defer func() {
		// A background context: ctx may be why fn returned, and a session
		// left in d must not reach the pool either way.
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), switchBackTimeout)
		defer cancel()
		if _, err := conn.ExecContext(bg, "USE "+quoteIdent(home)); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	return withAllMessages(fn(ctx, conn))
}

// switchBackTimeout bounds withSessionIn's USE back to the login database.
const switchBackTimeout = 10 * time.Second

// scriptResult is the sql.Result stand-in returned to callers of exec when
// a ScriptCollector is capturing statements instead of running them — its
// value is never inspected by exec's own callers, which either discard the
// result or (as with the extended-property writers) issue a single
// unambiguous statement per call rather than branching on RowsAffected.
type scriptResult struct{}

func (scriptResult) LastInsertId() (int64, error) { return 0, nil }
func (scriptResult) RowsAffected() (int64, error) { return 0, nil }

func (d *Database) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return d.execShown(ctx, q, q, args...)
}

// execSecret is Server.execSecret for a database-scoped statement: stmt runs,
// shown is what an observer sees and a WithScript collector captures.
func (d *Database) execSecret(ctx context.Context, stmt, shown string) (sql.Result, error) {
	return d.execShown(ctx, stmt, shown)
}

// execPasswords is execSecret for a statement whose secrets are all
// passwords.
func (d *Database) execPasswords(ctx context.Context, stmt string, passwords ...string) (sql.Result, error) {
	return d.execSecret(ctx, stmt, redactSecrets(stmt, PasswordPlaceholder, passwords...))
}

func (d *Database) execShown(ctx context.Context, q, shown string, args ...any) (sql.Result, error) {
	if c, ok := scriptFrom(ctx); ok {
		// Parameters are substituted into the text, not dropped: a captured
		// statement is run by hand in a query editor, where nothing binds
		// @p1 — see bindScriptArgs.
		bound, err := bindScriptArgs(capturedForm(ctx, q, shown), args)
		if err != nil {
			return nil, err
		}
		// The real path below always runs q in d (execIn) — captured
		// statements need that made explicit, since the script may be
		// handed to a session scoped to a different database (or none).
		// The entry records the database rather than prefixing a USE:
		// ScriptCollector renders it, with the GO a statement that must
		// open its batch needs after it.
		c.append(ScriptEntry{Server: scriptServerName(ctx, d.server), Database: d.Name, SQL: bound})
		return scriptResult{}, nil
	}
	var res sql.Result
	err := d.execIn(ctx, q, args, func(ctx context.Context, c sqlConn, q string, args []any) error {
		var e error
		res, e = c.ExecContext(ctx, q, args...)
		return e
	})
	if err == nil && observerFrom(ctx) != nil {
		sent, berr := bindScriptArgs(shown, args)
		if berr != nil {
			sent = shown
		}
		observe(ctx, d.server, ScriptEntry{Server: scriptServerName(ctx, d.server), Database: d.Name, SQL: sent})
	}
	return res, err
}

// dbRows is a read's *sql.Rows plus what closing them must also let go of:
// the *sql.Conn pinned for the read (see Database.query), which *sql.Rows.Close
// alone leaves checked out of the pool, and the read's link to the Server's
// lifetime (see Server.bound). Either may be nil; a server-scoped read off the
// pool pins no connection.
type dbRows struct {
	*sql.Rows
	conn    *sql.Conn
	release func()
}

func (r *dbRows) Close() error {
	err := r.Rows.Close()
	if r.conn != nil {
		if cerr := r.conn.Close(); err == nil {
			err = cerr
		}
	}
	if r.release != nil {
		r.release()
		r.release = nil
	}
	return err
}

// use switches conn to d's database on its own, reporting a database it
// cannot enter as an EntryError, as every database-scoped call does.
func (d *Database) use(ctx context.Context, conn sqlConn) error {
	if _, err := conn.ExecContext(ctx, "USE "+quoteIdent(d.Name)); err != nil {
		return &EntryError{Database: d.Name, Err: err}
	}
	return nil
}

// useBatch is q preceded by the switch to d's database, as one batch — a read
// inside InTransaction then costs one round trip rather than two, which is
// 41 ms per read against an Azure SQL Managed Instance and 1.3 ms on a LAN.
// A pooled read goes through procRead instead, which never leaves the
// session in d; a transaction's session is meant to (serverTx.at).
//
// The guard is what makes one batch safe. A USE that fails (a missing,
// offline or inaccessible database: 911, 942, 916) raises its error and the
// batch goes on, so without it q would run in whatever database the pooled
// session was in and return that database's rows as this one's. @@ERROR
// rather than a DB_NAME() comparison, which would depend on the collation of
// the database compared in and on how the caller cased the name.
//
// The prefix shares q's first line, so every line number an error in q
// reports is the one q alone reported. Statements after a USE are compiled in
// the database it switched to; verified on 2016, 2017, 2025 and Managed
// Instance, parameterised and not. The parameters are the exception: they
// take the collation of the database the batch started in, so where that
// differs from d's, a parameter compared with a built-in's result
// (SCHEMA_NAME(x) = @p1, both collation-coercible) fails Msg 468 in the batch
// and works alone. So such a comparison carries COLLATE DATABASE_DEFAULT on
// the built-in's side (a catalog column, implicit, needs nothing), which
// TestNoNameBuiltinComparesWithAParameterUncollated enforces; recheckUse is
// the safety net for one it misses, at three round trips.
func (d *Database) useBatch(q string) string {
	return "USE " + quoteIdent(d.Name) + "; IF @@ERROR <> 0 RETURN; " + q
}

// msgCollationConflict is Msg 468, "Cannot resolve the collation conflict".
const msgCollationConflict = 468

// recheckUse tells the two halves of a useBatch read that failed with err
// apart. A USE failure is reported as one, so the USE runs again alone: if it
// fails, useErr is its error and is the read's. If it succeeds, the query
// half failed and err is already the query's own, with its own line numbers
// (see useBatch) — the query is not run a second time, since a lock timeout
// or a late overflow would cost twice, and only a failing read pays the one
// extra round trip.
//
// The exception is rerun: a collation conflict may be the batch's own doing
// (see useBatch), and the session is now in d, so q must run again alone —
// what every failing read did before 2026-10-04, which hid it. Every
// comparison gosmo's own queries make is collated (see useBatch), so this is
// only the net for one written otherwise.
func (d *Database) recheckUse(ctx context.Context, conn sqlConn, err error) (useErr error, rerun bool) {
	if ctx.Err() != nil {
		return nil, false
	}
	if uerr := d.use(ctx, conn); uerr != nil {
		// Number 0 is the driver's own report that the failure already ended
		// the transaction the read ran in (go-mssqldb's
		// checkServerAbortedTransaction): the USE never reached the server,
		// so it tells the halves apart no more, and err — the server's own
		// 911, 942 or 916 — is the better report (TestLiveUseBatch).
		if me, ok := errors.AsType[mssql.Error](uerr); ok && me.Number == 0 {
			return nil, false
		}
		return uerr, false
	}
	me, ok := errors.AsType[mssql.Error](err)
	return nil, ok && me.Number == msgCollationConflict
}

func (d *Database) query(ctx context.Context, q string, args ...any) (*dbRows, error) {
	// For queries that return rows we cannot use withConn (the conn would be
	// released before the caller finishes iterating). Instead we acquire a
	// dedicated conn, run the query in d through procRead, and return the rows
	// wrapped with that conn — the caller's defer rows.Close() releases both.
	// A single read is idempotent, so a transient failure (dropped pooled
	// connection, etc.) is retried on a fresh connection.
	ctx, release := d.server.bound(ctx)
	if t := txFrom(ctx, d.server); t != nil {
		rows, err := d.queryOn(ctx, t, q, args...)
		if err != nil {
			release()
			return nil, err
		}
		return &dbRows{Rows: rows, release: release}, nil
	}
	query, qargs, err := d.procRead(q, args)
	if err != nil {
		release()
		return nil, err
	}
	rows, err := withRetry(ctx, func() (*dbRows, error) {
		conn, err := d.server.db.Conn(ctx)
		if err != nil {
			return nil, fmt.Errorf("gosmo: acquire connection: %w", err)
		}
		rows, err := conn.QueryContext(ctx, query, qargs...)
		if err != nil {
			if uerr := d.recheckEntry(ctx, conn, err); uerr != nil {
				err = uerr
			}
			conn.Close()
			return nil, err
		}
		return &dbRows{Rows: rows, conn: conn, release: release}, nil
	})
	if err != nil {
		release()
	}
	return rows, err
}

// queryOn is query inside InTransaction: on the transaction's session, with
// the same error report (recheckUse), and never retried. The batch's USE is
// free, so it is sent even when the session is already in d. A read with
// arguments goes as an RPC call of sp_executesql, and a USE in a client's RPC
// call persists after it (unlike one in EXEC sp_executesql from T-SQL), so the
// session is in d afterwards either way; TestLiveInTransactionParameterisedReadKeepsDatabase
// pins it.
func (d *Database) queryOn(ctx context.Context, t *serverTx, q string, args ...any) (*sql.Rows, error) {
	rows, err := t.tx.QueryContext(ctx, d.useBatch(q), args...)
	if err != nil {
		t.lost()
		uerr, rerun := d.recheckUse(ctx, t.tx, err)
		if uerr != nil {
			return nil, withAllMessages(uerr)
		}
		if !rerun {
			return nil, withAllMessages(err)
		}
		if rows, err = t.tx.QueryContext(ctx, q, args...); err != nil {
			return nil, withAllMessages(err)
		}
	}
	t.at(d.Name)
	return rows, nil
}

// queryRow acquires a connection, runs q in d's database (procRead), and
// hands the resulting row to scan — retrying the whole acquire+query+scan
// sequence as one unit on a transient connection failure, same as
// Server.queryRow and for the same reason: QueryRowContext itself never
// returns an error, only Scan does, so scan has to run inside the retried
// closure to be covered by it at all. Handing the caller a live *sql.Row
// to scan later would let withRetry see a nil error and return before the
// failure that only surfaces at Scan time, silently skipping the retry.
//
// A failed call surfaces in Row.Err before scan sees the row, and that is
// where recheckEntry (or, inside InTransaction, recheckUse) tells d's refusal
// from q's own failure, as query does — scan never sees a USE failure, which
// it could otherwise wrap or map to something else, and sees the call's own
// failed row for a failure in q.
func (d *Database) queryRow(ctx context.Context, scan func(*sql.Row) error, q string, args ...any) error {
	ctx, release := d.server.bound(ctx)
	defer release()
	if t := txFrom(ctx, d.server); t != nil {
		row := t.tx.QueryRowContext(ctx, d.useBatch(q), args...)
		if err := row.Err(); err != nil {
			t.lost()
			uerr, rerun := d.recheckUse(ctx, t.tx, err)
			if uerr != nil {
				return uerr
			}
			if rerun {
				row = t.tx.QueryRowContext(ctx, q, args...)
			}
		}
		if row.Err() == nil {
			t.at(d.Name)
		}
		return withAllMessages(scan(row))
	}
	query, qargs, err := d.procRead(q, args)
	if err != nil {
		return err
	}
	_, err = withRetry(ctx, func() (struct{}, error) {
		conn, err := d.server.db.Conn(ctx)
		if err != nil {
			return struct{}{}, fmt.Errorf("gosmo: acquire connection: %w", err)
		}
		defer conn.Close()
		row := conn.QueryRowContext(ctx, query, qargs...)
		if err := row.Err(); err != nil {
			if uerr := d.recheckEntry(ctx, conn, err); uerr != nil {
				return struct{}{}, uerr
			}
		}
		return struct{}{}, scan(row)
	})
	return err
}
