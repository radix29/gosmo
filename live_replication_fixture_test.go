//go:build livedb

// The replication fixture the live replication tests read: a distributor,
// a transactional and a merge publication, and a pull subscription to each,
// made by testdata/replication/setup.sql and removed by teardown.sql. Setting
// up replication is a server-level change and takes a minute, so the scripts
// are run by hand rather than per test, and a test needing the fixture skips
// when it is absent.
//
//	sqlcmd -S host -U sa -P PASS -C -b -i testdata/replication/setup.sql
//	go test -tags livedb . -run TestLiveReplicationFixture -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
package gosmo

import (
	"context"
	"database/sql"
	"testing"
)

// The fixture's names, as setup.sql creates them.
const (
	replFixtureTranDB   = "gossms_p5_repl_tran"
	replFixtureMergeDB  = "gossms_p5_repl_merge"
	replFixtureSubDB    = "gossms_p5_repl_sub"
	replFixtureTranPub  = "gossms_p5_tran_pub"
	replFixtureMergePub = "gossms_p5_merge_pub"
)

// liveReplicationFixture skips t unless setup.sql's fixture is in place on
// the -livedb instance: a local distributor and all three databases.
func liveReplicationFixture(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	var present int
	err := db.QueryRowContext(ctx, `SELECT CASE WHEN
		EXISTS (SELECT 1 FROM sys.servers WHERE is_distributor = 1)
		AND DB_ID(@p1) IS NOT NULL AND DB_ID(@p2) IS NOT NULL AND DB_ID(@p3) IS NOT NULL
		THEN 1 ELSE 0 END`, replFixtureTranDB, replFixtureMergeDB, replFixtureSubDB).Scan(&present)
	if err != nil {
		t.Fatalf("probe replication fixture: %v", err)
	}
	if present == 0 {
		t.Skip("replication fixture absent: run testdata/replication/setup.sql on the -livedb instance")
	}
}

// TestLiveReplicationFixture checks the fixture has the shape the
// replication tests assume, so a half-built one fails here, by name, rather
// than as a puzzling miss in a read under test.
func TestLiveReplicationFixture(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	liveReplicationFixture(t, db, ctx)

	count := func(what, query string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, query).Scan(&n); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		return n
	}
	checks := []struct {
		what, query string
		want        int
	}{
		{"publications at the distributor",
			`SELECT COUNT(*) FROM distribution.dbo.MSpublications
			 WHERE publisher_db IN (N'gossms_p5_repl_tran', N'gossms_p5_repl_merge')`, 2},
		{"transactional articles (two tables, one proc schema only)",
			`SELECT (SELECT COUNT(*) FROM gossms_p5_repl_tran.dbo.sysarticles)
			      + (SELECT COUNT(*) FROM gossms_p5_repl_tran.dbo.sysschemaarticles)`, 3},
		{"filtered transactional article",
			`SELECT COUNT(*) FROM gossms_p5_repl_tran.dbo.sysarticles WHERE filter <> 0`, 1},
		{"merge articles",
			`SELECT COUNT(*) FROM gossms_p5_repl_merge.dbo.sysmergearticles`, 1},
		{"pull subscription to the transactional publication",
			`SELECT COUNT(*) FROM gossms_p5_repl_sub.dbo.MSreplication_subscriptions
			 WHERE publication = N'gossms_p5_tran_pub'`, 1},
		{"merge pull subscription, subscriber side",
			`SELECT COUNT(*) FROM gossms_p5_repl_sub.dbo.sysmergesubscriptions AS s
			 JOIN gossms_p5_repl_sub.dbo.sysmergepublications AS p ON p.pubid = s.pubid
			 WHERE p.name = N'gossms_p5_merge_pub' AND s.subscriber_type = 2`, 1},
		{"rows delivered by the filtered transactional article (Region = EU)",
			`SELECT COUNT(*) FROM gossms_p5_repl_sub.dbo.Customer`, 2},
	}
	for _, c := range checks {
		if got := count(c.what, c.query); got != c.want {
			t.Errorf("%s: got %d, want %d — re-run teardown.sql then setup.sql", c.what, got, c.want)
		}
	}
}
