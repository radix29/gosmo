//go:build livedb

// Live verification that a forced Database.Drop deletes a database whatever
// state it is in. The forced drop used to lead with SET SINGLE_USER WITH
// ROLLBACK IMMEDIATE and drop only if that succeeded; a RESTORING database
// refuses the ALTER (Msg 5052), so one left by an interrupted restore could
// not be deleted at all. Which states take the ALTER is known only by asking
// a server.
//
//	go test -tags livedb . -run TestLiveForcedDrop -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own databases and one backup file beside their data
// files; touches nothing else.
package gosmo

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestLiveForcedDropDeletesADatabaseInAnyState stages a database RESTORING
// (WITH NORECOVERY), in STANDBY with a reader connected, and OFFLINE,
// force-drops each through Database.Drop, and checks it is gone. An OFFLINE
// database's files stay on disk, as Drop says; they are attached back and
// dropped so the next run can create them again. EMERGENCY, which takes the
// ALTER as ONLINE does, is not staged: its log file outlives the drop, and
// nothing portable deletes it, so a second run could not create it again.
func TestLiveForcedDropDeletesADatabaseInAnyState(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 3*time.Minute)
	defer done()
	srv := &Server{db: db}

	const src = "gossms_live_dropstate_src"
	drop := liveSingleUserDB(t, srv, ctx, src)
	defer drop()

	var dataFile, logFile string
	if err := db.QueryRowContext(ctx,
		"SELECT MAX(CASE WHEN type = 0 THEN physical_name END), MAX(CASE WHEN type = 1 THEN physical_name END) "+
			"FROM sys.master_files WHERE database_id = DB_ID(@p1)", src).Scan(&dataFile, &logFile); err != nil {
		t.Fatalf("locate the files: %v", err)
	}
	dir := dataFile[:strings.LastIndexAny(dataFile, `\/`)+1]
	bak := dir + src + ".bak"
	if _, err := srv.exec(ctx, "BACKUP DATABASE "+quoteIdent(src)+" TO DISK = N'"+escapeSingle(bak)+"' WITH INIT, COPY_ONLY"); err != nil {
		t.Fatalf("back up: %v", err)
	}
	defer liveRemoveFile(t, db, bak)

	gone := func(name string) bool {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sys.databases WHERE name = @p1", name).Scan(&n); err != nil {
			t.Fatalf("read sys.databases: %v", err)
		}
		return n == 0
	}
	state := func(name string) string {
		var s string
		db.QueryRowContext(ctx, "SELECT state_desc + CASE is_in_standby WHEN 1 THEN N' STANDBY' ELSE N'' END FROM sys.databases WHERE name = @p1", name).Scan(&s)
		return s
	}
	restore := func(name, with string) error {
		_, err := srv.exec(ctx, "RESTORE DATABASE "+quoteIdent(name)+" FROM DISK = N'"+escapeSingle(bak)+"' WITH "+with+
			", MOVE N'"+escapeSingle(src)+"' TO N'"+escapeSingle(dir+name+".mdf")+"'"+
			", MOVE N'"+escapeSingle(src+"_log")+"' TO N'"+escapeSingle(dir+name+"_log.ldf")+"'")
		return err
	}
	cleanup := func(name string) {
		srv.exec(context.Background(), "IF DB_ID('"+name+"') IS NOT NULL DROP DATABASE "+quoteIdent(name))
	}

	cases := []struct {
		name, want string
		stage      func(t *testing.T, name string)
	}{
		{"gossms_live_dropstate_restoring", "RESTORING", func(t *testing.T, name string) {
			if err := restore(name, "NORECOVERY"); err != nil {
				t.Fatalf("restore with norecovery: %v", err)
			}
		}},
		{"gossms_live_dropstate_standby", "ONLINE STANDBY", func(t *testing.T, name string) {
			t.Cleanup(func() { liveRemoveFile(t, db, dir+name+".tuf") })
			if err := restore(name, "STANDBY = N'"+escapeSingle(dir+name+".tuf")+"'"); err != nil {
				t.Fatalf("restore with standby: %v", err)
			}
			// A reader, as on a log-shipping secondary: the drop must get it out.
			conn, err := db.Conn(ctx)
			if err != nil {
				t.Fatalf("reader connection: %v", err)
			}
			t.Cleanup(func() { conn.Close() })
			if _, err := conn.ExecContext(ctx, "USE "+quoteIdent(name)); err != nil {
				t.Fatalf("reader into the standby database: %v", err)
			}
		}},
		{"gossms_live_dropstate_offline", "OFFLINE", func(t *testing.T, name string) {
			if _, err := srv.exec(ctx, "CREATE DATABASE "+quoteIdent(name)+
				" ON (NAME = N'"+name+"', FILENAME = N'"+escapeSingle(dir+name+".mdf")+"')"+
				" LOG ON (NAME = N'"+name+"_log', FILENAME = N'"+escapeSingle(dir+name+"_log.ldf")+"')"+
				"; ALTER DATABASE "+quoteIdent(name)+" SET OFFLINE"); err != nil {
				t.Fatalf("create and set offline: %v", err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			cleanup(c.name)
			defer cleanup(c.name)
			c.stage(t, c.name)
			if got := state(c.name); got != c.want {
				t.Fatalf("staged state %q, want %q — the test would prove nothing", got, c.want)
			}
			if err := srv.DatabaseRef(c.name).Drop(ctx, TerminationRollbackImmediate); err != nil {
				t.Fatalf("forced drop of a %s database: %v", c.want, err)
			}
			if !gone(c.name) {
				t.Errorf("after the forced drop the %s database is still there, %s", c.want, state(c.name))
			}
			if c.want == "OFFLINE" {
				// The files outlive the drop; attach them back to delete them.
				if _, err := srv.exec(ctx, "CREATE DATABASE "+quoteIdent(c.name)+
					" ON (FILENAME = N'"+escapeSingle(dir+c.name+".mdf")+"'), (FILENAME = N'"+escapeSingle(dir+c.name+"_log.ldf")+"') FOR ATTACH"); err != nil {
					t.Errorf("the OFFLINE database's files did not survive the drop to be attached back: %v", err)
				}
			}
		})
	}
}
