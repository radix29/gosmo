package gosmo

import (
	"context"
	"database/sql/driver"
	"errors"
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
func IsRetryable(err error) bool {
	if err == nil {
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
			return v, err
		}
		select {
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		case <-time.After(readRetryDelay(attempt)):
		}
	}
}
