package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
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

// readRetryDelay is the backoff before the nth retry (1-based).
func readRetryDelay(attempt int) time.Duration {
	return time.Duration(attempt) * 50 * time.Millisecond
}

// withRetry runs fn, retrying on transient (IsRetryable) failures up to
// readRetryAttempts times with a short backoff between tries. It is meant only
// for idempotent operations — a single read that can be re-run safely. A
// cancelled ctx stops the retry loop and returns ctx's error.
func withRetry[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var (
		v   T
		err error
	)
	for attempt := 1; ; attempt++ {
		v, err = fn()
		if err == nil || attempt >= readRetryAttempts || !IsRetryable(err) {
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
// a non-retryable error (see IsRetryable) or a cancelled ctx.
func AcquireConn(ctx context.Context, db *sql.DB, database string) (*sql.Conn, error) {
	prologue := "SELECT 1"
	if database != "" {
		prologue = "USE " + QuoteName(database)
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
			if ctx.Err() != nil || attempt >= readRetryAttempts || !IsRetryable(err) {
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
