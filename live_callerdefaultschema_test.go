//go:build livedb

// Live verification of CallerDefaultSchema.
//
//	go test -tags livedb . -run TestLiveCallerDefaultSchema -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database and login; touches nothing else.
package gosmo

import (
	"context"
	"database/sql"
	"testing"
)

// TestLiveCallerDefaultSchema pins that the read answers for the caller in
// the named database, not the connection's current one: a user whose
// DEFAULT_SCHEMA is not dbo reads it there and guest's schema in master,
// while sysadmin reads dbo in both.
func TestLiveCallerDefaultSchema(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const login = "gosmo_live_defschema"
	const pass = "P@ssw0rd_gosmo_live"

	d, drop := liveScratchDB(t, db, ctx, "gosmo_defschema_live")
	defer drop()

	db.ExecContext(ctx, "IF SUSER_ID('"+login+"') IS NOT NULL DROP LOGIN ["+login+"]")
	if _, err := db.ExecContext(ctx, "CREATE LOGIN ["+login+"] WITH PASSWORD = '"+pass+"', CHECK_POLICY = OFF"); err != nil {
		t.Fatalf("create login: %v", err)
	}
	defer db.ExecContext(context.Background(), "DROP LOGIN ["+login+"]")
	u := "[" + login + "]"
	liveExecIn(t, d, ctx, `CREATE SCHEMA sales`, `CREATE USER `+u+` FOR LOGIN `+u+` WITH DEFAULT_SCHEMA = sales`)

	pool, err := sql.Open("sqlserver", liveRestrictedDSN(t, login, pass))
	if err != nil {
		t.Fatalf("open as %s: %v", login, err)
	}
	defer pool.Close()
	admin, low := &Server{db: db}, &Server{db: pool}

	for _, c := range []struct {
		who      string
		s        *Server
		database string
		want     string
	}{
		{"sysadmin", admin, d.Name, "dbo"},
		{"sysadmin", admin, "master", "dbo"},
		{login, low, d.Name, "sales"},
		{login, low, "master", "guest"},
	} {
		got, err := c.s.DatabaseRef(c.database).CallerDefaultSchema(ctx)
		if err != nil {
			t.Errorf("%s in %s: %v", c.who, c.database, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s in %s: CallerDefaultSchema = %q, want %q", c.who, c.database, got, c.want)
		}
	}
}
