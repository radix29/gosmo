//go:build livedb

// A database master key in master, for the live tests that cannot run without
// one: CREATE CERTIFICATE and CREATE ASYMMETRIC KEY both encrypt the private
// key with it, and fail with Msg 15581 where there is none.
//
// Two of the three instances in the house have no DMK in master, which is why
// those tests failed there for reasons that had nothing to do with gosmo. The
// helper creates one only when master has none, and drops only the one it
// created — a pre-existing key protects real certificates and is never touched.
package gosmo

import (
	"context"
	"testing"
)

// masterKeyPassword is the throwaway DMK's password. It protects a key that
// exists for the length of one test and encrypts nothing the test does not
// also drop.
const masterKeyPassword = "gosmo-live-test-DMK-1"

// ensureMasterKey guarantees master has a database master key and returns a
// function that undoes whatever it did. On an instance that already has one it
// creates nothing and the returned function is a no-op.
func ensureMasterKey(t *testing.T, s *Server, ctx context.Context) func() {
	t.Helper()
	const existsQ = `SELECT COUNT(*) FROM master.sys.symmetric_keys WHERE name = '##MS_DatabaseMasterKey##'`
	var n int
	if err := s.db.QueryRowContext(ctx, existsQ).Scan(&n); err != nil {
		t.Fatalf("read master's symmetric keys: %v", err)
	}
	if n > 0 {
		return func() {}
	}
	if err := s.execContext(ctx, "USE [master]; CREATE MASTER KEY ENCRYPTION BY PASSWORD = '"+masterKeyPassword+"'"); err != nil {
		t.Fatalf("create master key in master: %v", err)
	}
	t.Logf("master had no database master key; created a throwaway one for this test")
	return func() {
		// Background context: the test's may already be cancelled, and the
		// key must not outlive the run whatever happened to it.
		if err := s.execContext(context.Background(), "USE [master]; DROP MASTER KEY"); err != nil {
			t.Errorf("dropping the throwaway master key in master: %v — drop it by hand", err)
		}
	}
}
