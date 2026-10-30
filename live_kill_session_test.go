//go:build livedb

// Live verification of SessionByID and KillSession, the pair behind gossms's
// Kill Process: the read finds a second connection of this process with its
// client process id and host, KILL ends it, and the read then reports it
// gone.
//
//	go test -tags livedb . -run TestLiveKillSession -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Kills only the session it opened; touches nothing else.
package gosmo

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"testing"
)

func TestLiveKillSession(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// The victim reports its own program name, so the read is seen to return
	// the session asked for rather than any of this process's.
	u, err := url.Parse(*liveDSN)
	if err != nil {
		t.Fatalf("parse -livedb: %v", err)
	}
	q := u.Query()
	q.Set("app name", "gosmo-kill-victim")
	u.RawQuery = q.Encode()
	vdb, err := sql.Open("sqlserver", u.String())
	if err != nil {
		t.Fatalf("open victim: %v", err)
	}
	defer vdb.Close()
	victim, err := vdb.Conn(ctx)
	if err != nil {
		t.Fatalf("victim conn: %v", err)
	}
	defer victim.Close()
	var spid int
	if err := victim.QueryRowContext(ctx, "SELECT @@SPID").Scan(&spid); err != nil {
		t.Fatalf("victim @@SPID: %v", err)
	}

	as, err := srv.SessionByID(ctx, spid)
	if err != nil {
		t.Fatalf("SessionByID(%d): %v", spid, err)
	}
	if as.SessionID != spid || as.ProgramName != "gosmo-kill-victim" || as.LoginName == "" ||
		as.HostProcessID != os.Getpid() || !as.FromCallerHost || !as.IsUserProcess || as.DatabaseName == "" {
		t.Errorf("SessionByID(%d) = %+v; want this process's victim on this host", spid, *as)
	}

	// The reader's own session is found too, unlike ActiveSessions.
	rc, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("reader conn: %v", err)
	}
	var own int
	if err := rc.QueryRowContext(ctx, "SELECT @@SPID").Scan(&own); err != nil {
		t.Fatalf("reader @@SPID: %v", err)
	}
	rc.Close()
	if _, err := srv.SessionByID(ctx, own); err != nil {
		t.Errorf("SessionByID of a session of the reading pool: %v", err)
	}

	if err := srv.KillSession(ctx, spid); err != nil {
		t.Fatalf("KillSession(%d): %v", spid, err)
	}
	if _, err := srv.SessionByID(ctx, spid); !errors.Is(err, ErrNotFound) {
		t.Errorf("SessionByID after the KILL: %v, want ErrNotFound", err)
	}
	if err := victim.PingContext(ctx); err == nil {
		t.Error("the killed connection still answers")
	}
	if err := srv.KillSession(ctx, 0); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("KillSession(0): %v, want ErrInvalidRequest", err)
	}
}
