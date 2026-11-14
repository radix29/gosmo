//go:build livedb

// Connection helpers shared by the package's live tests (go test -tags
// livedb, -livedb DSN): the admin pool every test starts from, and pools on
// the same server as another session or another login. A test whose setup
// needs more (a database, a login, a procedure) keeps that helper beside it.
package gosmo

import (
	"context"
	"database/sql"
	"flag"
	"net/url"
	"testing"
	"time"
)

// liveDSN is the server every live test runs against; without it each skips.
var liveDSN = flag.String("livedb", "", "SQL Server DSN for the live tests")

// liveDB is an admin pool on -livedb, pinged, with a one-minute context and
// the func that closes both.
func liveDB(t *testing.T) (*sql.DB, context.Context, func()) {
	t.Helper()
	return liveDBTimeout(t, 60*time.Second)
}

// skipSlowInShort skips a live test that takes 15 s or more on a test
// instance (each waits on a server timer: a lock or exclusive-access
// deadline, a restore, a login pool's idle time, or many databases) when
// -short is given. A quick run is then `go test -tags livedb -short`; the
// full run and a -run that names the test leave out -short and keep it.
func skipSlowInShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("slow live test (15 s or more), skipped under -short")
	}
}

// liveDBTimeout is liveDB with a longer deadline, for a test whose setup
// alone outlasts the minute (creating databases, on Managed Instance above
// all). Taking it here, rather than shadowing liveDB's ctx with a fresh one,
// keeps a single context per test.
func liveDBTimeout(t *testing.T, timeout time.Duration) (*sql.DB, context.Context, func()) {
	t.Helper()
	if *liveDSN == "" {
		t.Skip("no -livedb DSN given")
	}
	db, err := sql.Open("sqlserver", *liveDSN)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	if err := db.PingContext(ctx); err != nil {
		cancel()
		db.Close()
		t.Fatalf("ping: %v", err)
	}
	return db, ctx, func() { db.Close(); cancel() }
}

// liveRestrictedDSN is the -livedb DSN with its credentials swapped for the
// throwaway login's. The probe has to run *as* that login: HAS_PERMS_BY_NAME
// answers for the session's own principal, so sa's connection reports sa's
// rights whatever database it is pointed at.
func liveRestrictedDSN(t *testing.T, user, pass string) string {
	t.Helper()
	u, err := url.Parse(*liveDSN)
	if err != nil {
		t.Fatalf("parse -livedb DSN: %v", err)
	}
	u.User = url.UserPassword(user, pass)
	return u.String()
}

// liveOpenAs is a pool on the live server as login, for a test that has to
// see what a lesser principal sees. The caller closes it, and before dropping
// the login: a pooled session still open makes DROP LOGIN fail, so a test
// that drops the login with defer closes the pool with defer too, and one
// that drops it in t.Cleanup may close it either way.
func liveOpenAs(t *testing.T, login, pass string) *sql.DB {
	t.Helper()
	pool, err := sql.Open("sqlserver", liveRestrictedDSN(t, login, pass))
	if err != nil {
		t.Fatalf("open as %s: %v", login, err)
	}
	return pool
}

// liveSecondDB is a second pool on the live DSN: another session, which
// neither shares nor resets gosmo's pooled connections.
func liveSecondDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlserver", *liveDSN)
	if err != nil {
		t.Fatalf("open second pool: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
