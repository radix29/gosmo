//go:build livedb

// Live verification of the per-database batch's CATCH (perDatabaseExec): a
// database that cannot be entered is skipped, and any other error — a fault
// in the query itself above all — fails the read instead of passing as
// "no rows in that database".
//
//	go test -tags livedb . -run TestLivePerDatabase -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway databases and login; touches nothing
// else. Skipped entirely without -livedb.
package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

func TestLivePerDatabaseBatchSkipsOnlyUnenterable(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 3*time.Minute)
	defer done()

	const login, password = "gosmo_pdb_login", "Pdb!Probe#2026"
	ok, dropOK := liveScratchDB(t, db, ctx, "gosmo_pdb_ok")
	defer dropOK()
	noAccess, dropNoAccess := liveScratchDB(t, db, ctx, "gosmo_pdb_noaccess")
	defer dropNoAccess()
	off, dropOff := liveScratchDB(t, db, ctx, "gosmo_pdb_off")
	defer dropOff()
	held, dropHeld := liveScratchDB(t, db, ctx, "gosmo_pdb_held")
	defer dropHeld()
	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_pdb_src")
	defer dropSrc()
	srv := ok.Server()

	db.ExecContext(ctx, "IF SUSER_ID(N'"+login+"') IS NOT NULL DROP LOGIN ["+login+"]")
	if _, err := db.ExecContext(ctx, "CREATE LOGIN ["+login+"] WITH PASSWORD = N'"+password+"', CHECK_POLICY = OFF"); err != nil {
		t.Fatalf("CREATE LOGIN: %v", err)
	}
	defer db.ExecContext(context.Background(), "DROP LOGIN ["+login+"]")
	liveExecIn(t, ok, ctx, "CREATE USER [pdb_user] FOR LOGIN ["+login+"]")

	// RESTORING: a copy of gosmo_pdb_src restored WITH NORECOVERY.
	var dir string
	if err := db.QueryRowContext(ctx, "SELECT CAST(SERVERPROPERTY('InstanceDefaultDataPath') AS nvarchar(400))").Scan(&dir); err != nil {
		t.Fatalf("data path: %v", err)
	}
	const restoring = "gosmo_pdb_restoring"
	device := dir + "gosmo_pdb_src.bak"
	defer func() {
		c := context.Background()
		db.ExecContext(c, "IF DB_ID(N'"+restoring+"') IS NOT NULL DROP DATABASE ["+restoring+"]")
		db.ExecContext(c, "EXEC master.dbo.xp_delete_file 0, N'"+device+"'")
		db.ExecContext(c, "EXEC msdb.dbo.sp_delete_database_backuphistory @database_name = N'"+src.Name+"'")
		db.ExecContext(c, "EXEC msdb.dbo.sp_delete_database_backuphistory @database_name = N'"+restoring+"'")
	}()
	var logical, logicalLog string
	if err := db.QueryRowContext(ctx, `SELECT MAX(CASE WHEN type = 0 THEN name END), MAX(CASE WHEN type = 1 THEN name END)
		FROM sys.master_files WHERE database_id = DB_ID(N'`+src.Name+`')`).Scan(&logical, &logicalLog); err != nil {
		t.Fatalf("logical names: %v", err)
	}
	for _, stmt := range []string{
		"BACKUP DATABASE [" + src.Name + "] TO DISK = N'" + device + "' WITH INIT, COPY_ONLY",
		"RESTORE DATABASE [" + restoring + "] FROM DISK = N'" + device + "' WITH NORECOVERY," +
			" MOVE N'" + logical + "' TO N'" + dir + restoring + ".mdf'," +
			" MOVE N'" + logicalLog + "' TO N'" + dir + restoring + "_log.ldf'",
		"ALTER DATABASE [" + off.Name + "] SET OFFLINE WITH ROLLBACK IMMEDIATE",
		"ALTER DATABASE [" + held.Name + "] SET SINGLE_USER WITH ROLLBACK IMMEDIATE",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%.40s: %v", stmt, err)
		}
	}
	defer db.ExecContext(context.Background(), "ALTER DATABASE ["+off.Name+"] SET ONLINE")
	// Deferred before the holder's Close, so it runs after it.
	defer db.ExecContext(context.Background(), "ALTER DATABASE ["+held.Name+"] SET MULTI_USER")
	holder, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer holder.Close()
	if _, err := holder.ExecContext(ctx, "USE ["+held.Name+"]"); err != nil {
		t.Fatalf("hold %s: %v", held.Name, err)
	}

	// The handles are only names to perDatabaseBatch, so the missing
	// database (Msg 911: dropped or renamed since it was listed) is a Ref.
	dbs := []*Database{ok, noAccess, off, held, srv.DatabaseRef(restoring), srv.DatabaseRef("gosmo_pdb_missing")}
	batch := func(s *Server, q string) ([]string, error) {
		return perDatabaseBatch(ctx, s, "probe", dbs, q,
			func(kind string, scan func(...any) error) (string, error) {
				var one int
				return kind, scan(&one)
			},
			func(string, *Database) {})
	}
	names := func(s *Server) []string {
		t.Helper()
		var got []string
		_, err := perDatabaseBatch(ctx, s, "probe", dbs, `SELECT DB_NAME() AS probe, 1`,
			func(_ string, scan func(...any) error) (string, error) {
				var one int
				return "", scan(&one)
			},
			func(_ string, d *Database) { got = append(got, d.Name) })
		if err != nil {
			t.Fatalf("probe batch: %v", err)
		}
		return got
	}

	// Every can't-enter state is skipped: 942, 924, 927, 911 as sa ...
	if got := names(srv); !slices.Equal(got, []string{ok.Name, noAccess.Name}) {
		t.Errorf("as sa, entered %q; want only the two ONLINE databases", got)
	}
	// ... and 916 as a login with no user in gosmo_pdb_noaccess.
	u, err := url.Parse(*liveDSN)
	if err != nil {
		t.Fatalf("parse -livedb: %v", err)
	}
	u.User = url.UserPassword(login, password)
	ldb, err := sql.Open("sqlserver", u.String())
	if err != nil {
		t.Fatalf("open as %s: %v", login, err)
	}
	defer ldb.Close()
	lsrv, err := NewServer(ctx, ldb)
	if err != nil {
		t.Fatalf("NewServer as %s: %v", login, err)
	}
	if got := names(lsrv); !slices.Equal(got, []string{ok.Name}) {
		t.Errorf("as %s, entered %q; want only %s", login, got, ok.Name)
	}

	// A fault in the query is the read's failure, not a skipped database:
	// a missing column (207), a missing object (208), a collation conflict
	// (468), all at compile; and a divide by zero (8134) at run time in the
	// second database only, after the first has sent its row, which must not
	// come back as a short success.
	for _, c := range []struct {
		q    string
		want int32
	}{
		{`SELECT DB_NAME() AS probe, no_such_column FROM sys.objects`, 207},
		{`SELECT DB_NAME() AS probe, 1 FROM dbo.no_such_table`, 208},
		{`SELECT DB_NAME() AS probe, 1 FROM sys.objects a JOIN sys.objects b
    ON a.name COLLATE Latin1_General_CS_AS = b.name COLLATE Latin1_General_CI_AS`, 468},
		{`SELECT DB_NAME() AS probe, 1 / CASE DB_NAME() WHEN N'gosmo_pdb_noaccess' THEN 0 ELSE 1 END`, 8134},
	} {
		got, err := batch(srv, c.q)
		if me, isMs := errors.AsType[mssql.Error](err); !isMs || me.Number != c.want {
			t.Errorf("batch %.50q = %q, %v; want Msg %d", c.q, got, err, c.want)
		}
	}

	// Login.UserMappings is the user-visible case: a broken read showed the
	// login as unmapped everywhere.
	l, err := srv.LoginByName(ctx, login)
	if err != nil {
		t.Fatalf("LoginByName: %v", err)
	}
	ms, err := l.UserMappingsIn(ctx, dbs[:2])
	if err != nil || len(ms) != 1 || ms[0].Database != ok.Name {
		t.Errorf("UserMappingsIn = %v, %v; want the one mapping in %s", ms, err, ok.Name)
	}
	broken := strings.Replace(userMappingsQuery, "dp.principal_id,", "dp.no_such_column,", 1)
	if broken == userMappingsQuery {
		t.Fatal("userMappingsQuery no longer selects dp.principal_id; break it another way")
	}
	sid, _, err := l.identity(ctx, "sid")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	ms, err = l.userMappingsBatch(ctx, broken, sid, dbs[:2])
	if me, isMs := errors.AsType[mssql.Error](err); !isMs || me.Number != 207 {
		t.Errorf("UserMappings with a broken query = %v, %v; want Msg 207", ms, err)
	}
}
