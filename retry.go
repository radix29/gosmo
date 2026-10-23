package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

// ============================================================
// Transient-failure retry for idempotent reads
// ============================================================

// readRetryAttempts is the total number of tries (initial + retries) gosmo's
// read helpers make when a call fails with a transient error.
const readRetryAttempts = 3

// IsRetryable reports whether err represents a transient failure worth
// retrying — the driver's RetryableError; a dropped pooled connection
// (driver.ErrBadConn), including when wrapped; or one of the raw
// connection-level failures the driver itself uses to flag a connection
// dead (see mssql.Conn.checkBadConn): a network error, a corrupted TDS byte
// stream (mssql.StreamError), a fatal server-side error that severs the
// connection (mssql.ServerError), or io.EOF. Those last few surface
// unwrapped rather than as RetryableError whenever the driver decided
// retrying the exact in-flight call wasn't safe (its own mayRetry=false) —
// that restriction is about automatically retrying the *same* call, not
// about whether the connection itself is salvageable, so a caller retrying
// its own idempotent operation on a fresh connection is still safe to do
// so. It is exported so callers that run their own statements (e.g. an
// ad-hoc query runner) can decide whether a failure is worth another
// attempt; note that only idempotent operations are safe to retry blindly.
//
// A context error is never retryable. context.DeadlineExceeded has to be
// rejected explicitly because it implements net.Error, so the net.Error test
// below would otherwise report a caller's own expired deadline as a
// transient failure and have them wait out three attempts on a deadline that
// has already passed. context.Canceled fails that test anyway and is named
// alongside it so the two can't drift apart.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if _, ok := errors.AsType[mssql.RetryableError](err); ok {
		return true
	}
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, io.EOF) {
		return true
	}
	if _, ok := errors.AsType[mssql.StreamError](err); ok {
		return true
	}
	if _, ok := errors.AsType[mssql.ServerError](err); ok {
		return true
	}
	_, ok := errors.AsType[net.Error](err)
	return ok
}

// transientServerErrorNumbers are the SQL Server errors that fail a statement
// without anything being wrong with it, so re-running it is the documented
// remedy:
//
//	1205  The transaction was deadlocked … and has been chosen as the
//	      deadlock victim. Rerun the transaction.
//	4221  Login to read-secondary failed due to long wait on
//	      'HADR_DATABASE_WAIT_FOR_TRANSITION_TO_VERSIONING'.
//	10928 Resource ID … The limit for the database is … and has been reached.
//	10929 Resource ID … The minimum guarantee is … Please retry the request later.
//	40197 The service has encountered an error processing your request.
//	40501 The service is currently busy. Retry the request after 10 seconds.
//	40613 Database '…' on server '…' is not currently available.
//	49918 Cannot process request. Not enough resources to process request.
//	49919 Cannot process create or update request. Too many operations in progress.
//	49920 Cannot process request. Too many operations in progress.
//
// All but 1205 are Azure SQL's documented transient errors, which a Managed
// Instance raises too. 1222 (lock request time out) is deliberately absent:
// it is the caller's own SET LOCK_TIMEOUT expiring, not a transient fault.
var transientServerErrorNumbers = []int32{1205, 4221, 10928, 10929, 40197, 40501, 40613, 49918, 49919, 49920}

// isTransientServerError reports whether err carries, in any message of the
// batch, a SQL Server error that transientServerErrorNumbers lists. It matches
// numbers only, as IsAlreadyExists does, so it holds in any session language.
func isTransientServerError(err error) bool {
	se, ok := AsSQLError(err)
	if !ok {
		return false
	}
	return slices.ContainsFunc(se.messages(), func(m SQLError) bool {
		return slices.Contains(transientServerErrorNumbers, m.Number)
	})
}

// IsTransient reports whether an idempotent operation that failed with err is
// worth running again: either the connection failed (IsRetryable) or the
// server refused the statement for a passing reason — a deadlock victim (Msg
// 1205) or one of Azure SQL's transient errors (see
// transientServerErrorNumbers). It is the rule gosmo's own reads and
// AcquireConn retry on, exported for callers running their own reads.
//
// Like IsRetryable it says nothing about safety: only an idempotent operation
// may be retried blindly. A 1205 inside a transaction has already rolled the
// transaction back, so re-running the one statement that failed is wrong
// there — gosmo never retries inside InTransaction.
func IsTransient(err error) bool {
	return IsRetryable(err) || isTransientServerError(err)
}

// readRetryDelay is the backoff before the nth retry (1-based).
func readRetryDelay(attempt int) time.Duration {
	return time.Duration(attempt) * 50 * time.Millisecond
}

// withRetry runs fn, retrying on transient (IsTransient) failures up to
// readRetryAttempts times with a short backoff between tries. It is meant only
// for idempotent operations — a single read that can be re-run safely — and
// never inside InTransaction. A cancelled ctx stops the retry loop and returns
// ctx's error.
//
// It covers what fn returns, nothing later: a deadlock or a dropped connection
// that surfaces while the caller is iterating rows it was already handed
// (rows.Next/Err) is the caller's to handle, since part of the result has been
// consumed.
func withRetry[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var (
		v   T
		err error
	)
	for attempt := 1; ; attempt++ {
		v, err = fn()
		if err == nil || attempt >= readRetryAttempts || !IsTransient(err) {
			return v, withAllMessages(err)
		}
		select {
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		case <-time.After(readRetryDelay(attempt)):
		}
	}
}

// ============================================================
// Pinned connections for callers running their own statements
// ============================================================

// AcquireConn returns a live pinned *sql.Conn from db, switched to database
// (USE) if non-empty, retrying on a fresh connection when the pool hands back
// a dead one. It is for callers that run a whole script or session on one
// connection: database/sql's own bad-connection retry covers only *sql.DB
// calls, not a pinned *sql.Conn, so a connection dropped while idle (NAT
// timeout, killed session, failover) fails the next statement the caller
// issues. gosmo's own reads get the same treatment through withRetry.
//
// Only the USE/SELECT-1 prologue is retried, never anything the caller goes
// on to run — the retry is safe precisely because the prologue is idempotent.
// A dead connection is closed rather than returned to the pool, which evicts
// it via driver.Validator.IsValid. Retries follow readRetryAttempts and
// readRetryDelay, the same budget as gosmo's read helpers, and stop early on
// a non-transient error (see IsTransient) or a cancelled ctx.
func AcquireConn(ctx context.Context, db *sql.DB, database string) (*sql.Conn, error) {
	prologue := "SELECT 1"
	if database != "" {
		prologue = "USE " + quoteIdent(database)
	}
	wrapErr := func(err error) error {
		if database != "" {
			return fmt.Errorf("gosmo: switch to database %s: %w", database, withAllMessages(err))
		}
		return withAllMessages(err)
	}

	// Bounded by the >= check below (== would spin forever at 0).
	for attempt := 1; ; attempt++ {
		conn, err := db.Conn(ctx)
		if err != nil {
			return nil, fmt.Errorf("gosmo: acquire connection: %w", err)
		}
		if _, err := conn.ExecContext(ctx, prologue); err != nil {
			conn.Close() // dead — evicted from the pool via driver.Validator.IsValid
			if ctx.Err() != nil || attempt >= readRetryAttempts || !IsTransient(err) {
				return nil, wrapErr(err)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(readRetryDelay(attempt)):
			}
			continue
		}
		return conn, nil
	}
}
