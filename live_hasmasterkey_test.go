//go:build livedb

// Live verification that HasMasterKey sees a master key its caller holds no
// right on.
//
//	go test -tags livedb . -run TestLiveHasMasterKey -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database and login; touches nothing else.
package gosmo

import (
	"context"
	"database/sql"
	"testing"
)

// TestLiveHasMasterKeySeenByALowPrivilegeUser pins the reason HasMasterKey
// reads sys.databases as well as sys.symmetric_keys: a CREATE CERTIFICATE-only
// user has no row for the master key in the second, and a New Certificate
// dialog asking it then offered to create a master key the database already
// had (found driving gossms, 2026-09-22). Also pins the one case the flag
// cannot answer for — a master key with its service-master-key encryption
// dropped — so a change in either direction is noticed.
func TestLiveHasMasterKeySeenByALowPrivilegeUser(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const login = "gosmo_live_hasdmk"
	const pass = "P@ssw0rd_gosmo_live"

	d, drop := liveScratchDB(t, db, ctx, "gosmo_hasdmk_live")
	defer drop()

	db.ExecContext(ctx, "IF SUSER_ID('"+login+"') IS NOT NULL DROP LOGIN ["+login+"]")
	if _, err := db.ExecContext(ctx, "CREATE LOGIN ["+login+"] WITH PASSWORD = '"+pass+"', CHECK_POLICY = OFF"); err != nil {
		t.Fatalf("create login: %v", err)
	}
	defer db.ExecContext(context.Background(), "DROP LOGIN ["+login+"]")
	u := "[" + login + "]"
	liveExecIn(t, d, ctx, `CREATE USER `+u+` FOR LOGIN `+u, `GRANT CREATE CERTIFICATE TO `+u)

	pool, err := sql.Open("sqlserver", liveRestrictedDSN(t, login, pass))
	if err != nil {
		t.Fatalf("open as %s: %v", login, err)
	}
	defer pool.Close()
	has := func(s *Server) bool {
		t.Helper()
		ok, err := s.DatabaseRef(d.Name).HasMasterKeyContext(ctx)
		if err != nil {
			t.Fatalf("HasMasterKeyContext: %v", err)
		}
		return ok
	}
	admin, low := &Server{db: db}, &Server{db: pool}

	if has(admin) || has(low) {
		t.Fatal("a database with no master key reads as having one")
	}
	liveExecIn(t, d, ctx, `CREATE MASTER KEY ENCRYPTION BY PASSWORD = 'Dmk_P@ssw0rd_live'`)
	if !has(admin) {
		t.Error("sysadmin: the new master key is not seen")
	}
	if !has(low) {
		t.Error("CREATE CERTIFICATE-only user: the master key is not seen")
	}

	liveExecIn(t, d, ctx, `ALTER MASTER KEY DROP ENCRYPTION BY SERVICE MASTER KEY`)
	if !has(admin) {
		t.Error("sysadmin: a master key without service-master-key encryption is not seen")
	}
	if has(low) {
		t.Log("the low-privilege user now sees a master key without service-master-key encryption — the documented limit no longer holds; update HasMasterKey's comment")
	}
}
