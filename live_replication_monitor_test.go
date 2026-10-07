//go:build livedb

// The Replication Monitor reads against the fixture of
// live_replication_fixture_test.go, whose instance is its own distributor.
//
//	go test -tags livedb . -run TestLiveReplicationMonitor -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
package gosmo

import (
	"database/sql"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// fixtureDistribution is the fixture's distribution database, read the way
// a caller gets it.
func fixtureDistribution(t *testing.T, srv *Server) *DistributionDatabase {
	t.Helper()
	info, err := srv.ReplicationInfo(t.Context())
	if err != nil {
		t.Fatalf("ReplicationInfo: %v", err)
	}
	for i := range info.DistributionDatabases {
		if info.DistributionDatabases[i].Name == "distribution" {
			dd := &info.DistributionDatabases[i]
			if dd.Server() != srv {
				t.Fatalf("DistributionDatabase.Server() is not the server read from")
			}
			return dd
		}
	}
	t.Fatalf("no distribution database in %+v", info.DistributionDatabases)
	return nil
}

func TestLiveReplicationMonitor(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	liveReplicationFixture(t, db, ctx)
	srv := liveServer(t, db, ctx)
	dd := fixtureDistribution(t, srv)

	if ok, err := dd.CanMonitor(ctx); err != nil || !ok {
		t.Fatalf("CanMonitor as sysadmin = %v, %v; want true", ok, err)
	}

	pubrs, err := srv.MonitorPublishers(ctx)
	if err != nil {
		t.Fatalf("MonitorPublishers: %v", err)
	}
	if len(pubrs) == 0 || pubrs[0].DistributionDB != "distribution" || pubrs[0].PublicationCount < 2 {
		t.Errorf("MonitorPublishers = %+v", pubrs)
	}

	pubs, err := dd.MonitorPublications(ctx)
	if err != nil {
		t.Fatalf("MonitorPublications: %v", err)
	}
	byPub := map[string]MonitorPublication{}
	for _, p := range pubs {
		byPub[p.Publication] = p
	}
	tran, merge := byPub[replFixtureTranPub], byPub[replFixtureMergePub]
	if tran.Type != PublicationTransactional || tran.PublisherDB != replFixtureTranDB ||
		tran.SnapshotAgent == "" || tran.LogReaderAgent == "" || tran.SubscriptionCount != 1 ||
		tran.RetentionUnit != "hour" || tran.Status == ReplNeverRun {
		t.Errorf("transactional publication = %+v", tran)
	}
	if merge.Type != PublicationMerge || merge.SnapshotAgent == "" || merge.LogReaderAgent != "" ||
		merge.RetentionUnit == "" || merge.LastDistSync.IsZero() {
		t.Errorf("merge publication = %+v", merge)
	}

	subs, err := dd.MonitorSubscriptions(ctx)
	if err != nil {
		t.Fatalf("MonitorSubscriptions: %v", err)
	}
	bySub := map[string]MonitorSubscription{}
	for _, s := range subs {
		bySub[s.Publication] = s
	}
	tsub, msub := bySub[replFixtureTranPub], bySub[replFixtureMergePub]
	if tsub.SubscriberDB != replFixtureSubDB || tsub.Type != SubscriptionPull || tsub.DistributionAgent == "" ||
		tsub.MergeAgent != "" || tsub.LogReaderAgent != tran.LogReaderAgent || tsub.PublicationType != PublicationTransactional {
		t.Errorf("transactional subscription = %+v", tsub)
	}
	if msub.SubscriberDB != replFixtureSubDB || msub.MergeAgent == "" || msub.DistributionAgent != "" ||
		msub.PublicationType != PublicationMerge || msub.MergeRunDuration == nil {
		t.Errorf("merge subscription = %+v", msub)
	}

	agents, err := dd.Agents(ctx)
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	byName := map[string]*ReplicationAgent{}
	for _, a := range agents {
		byName[a.Name] = a
		if a.DistributionDatabase() != dd {
			t.Errorf("agent %q: DistributionDatabase() is not dd", a.Name)
		}
	}
	// Each agent the procedures name is among Agents, of the right kind,
	// and has run: setup ran every one.
	for name, kind := range map[string]ReplAgentKind{
		tran.SnapshotAgent:     ReplSnapshotAgent,
		tran.LogReaderAgent:    ReplLogReaderAgent,
		merge.SnapshotAgent:    ReplSnapshotAgent,
		tsub.DistributionAgent: ReplDistributionAgent,
		msub.MergeAgent:        ReplMergeAgent,
	} {
		a := byName[name]
		if a == nil {
			t.Errorf("agent %q not among Agents", name)
			continue
		}
		if a.Kind != kind || a.Status == ReplNeverRun || a.LastTime.IsZero() || a.LastAction == "" {
			t.Errorf("agent %q = %+v, want a %v with history", name, a, kind)
		}
		if !strings.EqualFold(a.Publisher, tran.Publisher) {
			t.Errorf("agent %q Publisher = %q, want %q", name, a.Publisher, tran.Publisher)
		}
		sess, err := a.Sessions(ctx, 0, false)
		if err != nil {
			t.Errorf("Sessions(%q): %v", name, err)
			continue
		}
		if len(sess) == 0 || sess[0].Start.IsZero() || sess[0].LastAction == "" || sess[0].Actions == 0 {
			t.Errorf("Sessions(%q) = %+v", name, sess)
			continue
		}
		for i := 1; i < len(sess); i++ {
			if sess[i].Start.After(sess[i-1].Start) {
				t.Errorf("Sessions(%q) not newest first: %v after %v", name, sess[i].Start, sess[i-1].Start)
			}
		}
		// The newest session is the agent's last run.
		if !sess[0].Start.Equal(a.LastStart) && a.Kind != ReplMergeAgent {
			t.Errorf("Sessions(%q)[0].Start = %v, agent LastStart %v", name, sess[0].Start, a.LastStart)
		}
		acts, err := a.SessionActions(ctx, sess[0])
		if err != nil {
			t.Errorf("SessionActions(%q): %v", name, err)
			continue
		}
		if len(acts) == 0 || acts[0].Message == "" || acts[0].Time.IsZero() {
			t.Errorf("SessionActions(%q) = %+v", name, acts)
		}
		// A merge session's actions all carry the session's end time.
		if a.Kind == ReplMergeAgent && len(acts) > 0 && !acts[0].Time.Equal(sess[0].End) {
			t.Errorf("SessionActions(%q) of the newest session read another: %v, want %v", name, acts[0].Time, sess[0].End)
		}
	}
	if a := byName[tsub.DistributionAgent]; a != nil &&
		(a.SubscriberDB != replFixtureSubDB || a.Subscriber == "" || a.Publication != replFixtureTranPub) {
		t.Errorf("distribution agent = %+v", a)
	}
	if a := byName[msub.MergeAgent]; a != nil && a.SubscriberDB != replFixtureSubDB {
		t.Errorf("merge agent SubscriberDB = %q, want %q", a.SubscriberDB, replFixtureSubDB)
	}
	if a := byName[tran.LogReaderAgent]; a != nil && (a.Publication != "" || a.LatencyMs == nil) {
		t.Errorf("log reader = %+v, want no publication and a latency", a)
	}

	// No error has id 0.
	if errs, err := dd.Errors(ctx, 0); err != nil || len(errs) != 0 {
		t.Errorf("Errors(0) = %+v, %v", errs, err)
	}
}

// TestLiveReplicationMonitorErrors reads the detail behind every error a
// failed session names. It needs an agent to have failed; drive one
// by renaming a subscriber table and running its Distribution Agent.
func TestLiveReplicationMonitorErrors(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	liveReplicationFixture(t, db, ctx)
	srv := liveServer(t, db, ctx)
	dd := fixtureDistribution(t, srv)

	agents, err := dd.Agents(ctx)
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	found := 0
	for _, a := range agents {
		sess, err := a.Sessions(ctx, 0, true)
		if err != nil {
			t.Fatalf("Sessions(%q, errors only): %v", a.Name, err)
		}
		for _, s := range sess {
			if s.Status != ReplFailed {
				t.Errorf("errors-only session of %q has status %v", a.Name, s.Status)
			}
			acts, err := a.SessionActions(ctx, s)
			if err != nil {
				t.Fatalf("SessionActions(%q): %v", a.Name, err)
			}
			for _, act := range acts {
				if act.ErrorID == 0 {
					continue
				}
				errs, err := dd.Errors(ctx, act.ErrorID)
				if err != nil {
					t.Fatalf("Errors(%d): %v", act.ErrorID, err)
				}
				for _, e := range errs {
					if e.Text == "" || e.Time.IsZero() {
						t.Errorf("Errors(%d) row = %+v", act.ErrorID, e)
					}
					t.Logf("%s error %d: [%s] %s: %s (seqno %s, cmd %d)", a.Name, act.ErrorID, e.Code, e.SourceName, e.Text, e.XactSeqno, e.CommandID)
				}
				found += len(errs)
			}
		}
	}
	if found == 0 {
		t.Skip("no agent history carries an error: inject one to exercise Errors")
	}
	t.Logf("%d error rows read", found)
}

// A login that is not sysadmin can monitor only as db_owner or replmonitor
// in the distribution database; without a user there it is told no, not an
// error.
func TestLiveReplicationMonitorAccess(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done) // registered first, so it runs after the drops below
	liveReplicationFixture(t, db, ctx)

	const login, pass = "gosmo_repl_monitor_probe", "Pr0be!pass-9172"
	if _, err := db.ExecContext(ctx, "CREATE LOGIN "+quoteIdent(login)+" WITH PASSWORD = N'"+pass+"', CHECK_POLICY = OFF"); err != nil {
		t.Fatalf("create login: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(ctx, "USE distribution; DROP USER IF EXISTS "+quoteIdent(login)); err != nil {
			t.Errorf("drop user: %v", err)
		}
		if _, err := db.ExecContext(ctx, "DROP LOGIN "+quoteIdent(login)); err != nil {
			t.Errorf("drop login: %v", err)
		}
	})

	u, err := url.Parse(*liveDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(login, pass)
	other, err := sql.Open("sqlserver", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	srv, err := NewServer(ctx, other)
	if err != nil {
		t.Fatalf("connect as %s: %v", login, err)
	}
	dd := srv.DistributionDatabaseRef("distribution")

	if ok, err := dd.CanMonitor(ctx); err != nil || ok {
		t.Errorf("CanMonitor with no user = %v, %v; want false", ok, err)
	}
	if _, err := db.ExecContext(ctx, "USE distribution; CREATE USER "+quoteIdent(login)+"; ALTER ROLE replmonitor ADD MEMBER "+quoteIdent(login)); err != nil {
		t.Fatalf("add to replmonitor: %v", err)
	}
	if ok, err := dd.CanMonitor(ctx); err != nil || !ok {
		t.Errorf("CanMonitor as replmonitor = %v, %v; want true", ok, err)
	}
	// msdb's distributor tables are sysadmin's alone: ReplicationInfo
	// skips them rather than failing, and still names the distribution
	// database.
	info, err := srv.ReplicationInfo(ctx)
	if err != nil {
		t.Fatalf("ReplicationInfo as replmonitor: %v", err)
	}
	if !info.IsDistributor || !info.DistributorDetailsHidden || len(info.DistributionDatabases) != 0 ||
		!slices.ContainsFunc(info.Databases, func(d ReplicationDatabase) bool { return d.Name == "distribution" && d.Distribution }) {
		t.Errorf("ReplicationInfo as replmonitor = %+v", info)
	}
	if !info.IsPublisher {
		t.Error("IsPublisher false with MSdistpublishers hidden; want it from the published databases")
	}
	if _, err := dd.MonitorPublications(ctx); err != nil {
		t.Errorf("MonitorPublications as replmonitor: %v", err)
	}
	if _, err := dd.Agents(ctx); err != nil {
		t.Errorf("Agents as replmonitor: %v", err)
	}
}
