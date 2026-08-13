//go:build livedb

// Live verification that ErrNotFound actually classifies what the server
// does, rather than what the code reads as though it does.
//
// The unit tests in errors_test.go pin the sentinel's plumbing — Unwrap
// reaches it, the messages are unchanged. Only the server can say whether a
// real missing login produces sql.ErrNoRows at all (and so reaches notFoundf),
// and whether a *visible* failure — the case the sentinel exists to separate —
// stays out of it. Both are the difference between the endpoint dialog
// reporting the real fault and reporting a misleading CREATE LOGIN failure.
//
//	go test -tags livedb . -run TestLiveNotFound -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway login; touches nothing else.
// Skipped entirely without -livedb.
package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"testing"
)

// openAs dials the same server the -livedb DSN names, as a different login.
func openAs(t *testing.T, user, pass string) (*sql.DB, error) {
	t.Helper()
	u, err := url.Parse(*liveDSN)
	if err != nil {
		return nil, err
	}
	u.User = url.UserPassword(user, pass)
	db, err := sql.Open("sqlserver", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, db.Ping()
}

func TestLiveNotFoundClassifiesAMissingLogin(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	srv := &Server{db: db}

	const missing = "zz_gossms_notfound_probe_absent"
	if _, err := srv.LoginByNameContext(ctx, missing); err == nil {
		t.Fatalf("login %q unexpectedly exists on this server", missing)
	} else {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("a genuinely missing login must satisfy ErrNotFound, got %v", err)
		}
		if want := `gosmo: login "` + missing + `" not found`; err.Error() != want {
			t.Errorf("message = %q, want %q", err.Error(), want)
		}
	}
}

func TestLiveNotFoundDoesNotSwallowAVisibleLogin(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	srv := &Server{db: db}

	// A login that does exist must come back cleanly — the other half of the
	// endpoint dialog's branch, which skips creation on this answer.
	const name = "zz_gossms_notfound_probe"
	if _, err := db.ExecContext(ctx,
		`IF SUSER_ID(@p1) IS NOT NULL DROP LOGIN `+quoteIdent(name), name); err != nil {
		t.Fatalf("pre-clean: %v", err)
	}
	if err := srv.CreateLoginContext(ctx, name, "inSecure123!Probe", nil); err != nil {
		t.Fatalf("create throwaway login: %v", err)
	}
	defer func() {
		if _, err := db.ExecContext(ctx, `DROP LOGIN `+quoteIdent(name)); err != nil {
			t.Errorf("drop throwaway login %s: %v", name, err)
		}
	}()

	got, err := srv.LoginByNameContext(ctx, name)
	if err != nil {
		t.Fatalf("existing login reported an error: %v", err)
	}
	if got.Name != name {
		t.Errorf("Name = %q, want %q", got.Name, name)
	}
}

func TestLiveNotFoundKeepsARealFailureDistinct(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	srv := &Server{db: db}

	// A cancelled context is the cheapest way to make the lookup itself fail
	// against a live server without breaking anything on it. This is exactly
	// the shape the endpoint dialog used to misread as "the login is absent",
	// so it must not satisfy ErrNotFound.
	dead, cancel := context.WithCancel(ctx)
	cancel()

	_, err := srv.LoginByNameContext(dead, "sa")
	if err == nil {
		t.Fatal("a cancelled context should have failed the lookup")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("a failed lookup must not read as not-found, got %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the underlying cause should stay reachable, got %v", err)
	}
	if strings.Contains(err.Error(), "not found") {
		t.Errorf("a failed lookup must not say %q: %v", "not found", err)
	}
}

// TestLiveNotFoundCannotSeePastMetadataVisibility pins the limit of the
// sentinel, which is a SQL Server behaviour rather than a gosmo one: metadata
// visibility hides a principal the caller lacks VIEW ANY DEFINITION on by
// returning *no rows*, not an error. An existing login therefore reads as
// ErrNotFound, and nothing in gosmo can tell that from a real absence because
// the server does not distinguish them either.
//
// This is why the endpoint dialog tolerates "already exists" from CREATE LOGIN
// instead of trusting the lookup. If this test ever fails — if the server
// starts erroring instead of filtering — that tolerance can be reconsidered.
func TestLiveNotFoundCannotSeePastMetadataVisibility(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const name = "zz_gossms_visibility_probe"
	const pass = "inSecure123!Probe"
	if _, err := db.ExecContext(ctx,
		`IF SUSER_ID(@p1) IS NOT NULL DROP LOGIN `+quoteIdent(name), name); err != nil {
		t.Fatalf("pre-clean: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE LOGIN `+quoteIdent(name)+
		` WITH PASSWORD = `+QuoteLiteral(pass)+`, CHECK_POLICY = OFF`); err != nil {
		t.Fatalf("create throwaway login: %v", err)
	}
	defer func() {
		if _, err := db.ExecContext(ctx, `DROP LOGIN `+quoteIdent(name)); err != nil {
			t.Errorf("drop throwaway login %s: %v", name, err)
		}
	}()
	if _, err := db.ExecContext(ctx, `DENY VIEW ANY DEFINITION TO `+quoteIdent(name)); err != nil {
		t.Fatalf("deny: %v", err)
	}

	self, err := openAs(t, name, pass)
	if err != nil {
		t.Fatalf("connect as throwaway login: %v", err)
	}
	defer self.Close()

	// The login is asking about itself, and it exists — yet it is invisible.
	_, err = (&Server{db: self}).LoginByNameContext(ctx, name)
	if err == nil {
		t.Skip("this server does not filter the login's own row; nothing to pin")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected the invisible-but-existing login to read as ErrNotFound, got %v", err)
	}
	t.Logf("confirmed: existing login %q reads as ErrNotFound when hidden by "+
		"metadata visibility — absence and invisibility are indistinguishable", name)
}

// TestLiveCertificateNotFoundIsNilNil pins the one by-name lookup that reports
// absence as a value rather than an error. Nothing covered it before, and the
// endpoint pipeline branches on it twice: a nil certificate means "create one",
// so a CertificateByName that started erroring on absence would stop the
// pipeline instead of running it.
//
// It is also the contract that made certificate.go's `err == sql.ErrNoRows`
// worth changing to errors.Is — the bare comparison silently converts to the
// wrong branch if anything in Database.queryRow ever wraps.
func TestLiveCertificateNotFoundIsNilNil(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	master := (&Server{db: db}).Database("master")

	cert, err := master.CertificateByNameContext(ctx, "zz_gossms_no_such_certificate")
	if err != nil {
		t.Fatalf("a missing certificate must not be an error, got %v", err)
	}
	if cert != nil {
		t.Fatalf("a missing certificate must be nil, got %+v", cert)
	}

	// The other half: a certificate that does exist comes back populated, so
	// "nil" really does mean absent rather than "always nil".
	const name = "zz_gossms_cert_probe"
	if _, err := db.ExecContext(ctx,
		`USE master; IF EXISTS (SELECT 1 FROM sys.certificates WHERE name = @p1) DROP CERTIFICATE `+
			quoteIdent(name), name); err != nil {
		t.Fatalf("pre-clean: %v", err)
	}
	// ENCRYPTION BY PASSWORD, not the database master key: master has no DMK
	// on a stock instance, and creating one there would be a real and lasting
	// change to the server for the sake of a test.
	if _, err := db.ExecContext(ctx, `USE master; CREATE CERTIFICATE `+quoteIdent(name)+
		` ENCRYPTION BY PASSWORD = `+QuoteLiteral("inSecure123!Cert")+
		` WITH SUBJECT = `+QuoteLiteral("gossms not-found probe")); err != nil {
		t.Fatalf("create throwaway certificate: %v", err)
	}
	defer func() {
		if _, err := db.ExecContext(ctx, `USE master; DROP CERTIFICATE `+quoteIdent(name)); err != nil {
			t.Errorf("drop throwaway certificate %s: %v", name, err)
		}
	}()

	got, err := master.CertificateByNameContext(ctx, name)
	if err != nil {
		t.Fatalf("existing certificate reported an error: %v", err)
	}
	if got == nil {
		t.Fatal("existing certificate came back nil — the nil answer would then mean nothing")
	}
	if got.Name != name {
		t.Errorf("Name = %q, want %q", got.Name, name)
	}
}
