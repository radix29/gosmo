//go:build livedb

// The replication reads against the fixture of
// live_replication_fixture_test.go, and against an instance with no
// replication configured (TestLiveReplicationNotConfigured), where every
// read must come back empty rather than fail on a missing table.
//
//	go test -tags livedb . -run TestLiveReplication -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
package gosmo

import (
	"slices"
	"strings"
	"testing"
)

func TestLiveReplicationInfo(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	liveReplicationFixture(t, db, ctx)
	srv := liveServer(t, db, ctx)

	info, err := srv.ReplicationInfo(ctx)
	if err != nil {
		t.Fatalf("ReplicationInfo: %v", err)
	}
	if !info.DistributorConfigured || !info.IsDistributor || !info.IsPublisher {
		t.Errorf("configured/distributor/publisher = %v/%v/%v, want all true",
			info.DistributorConfigured, info.IsDistributor, info.IsPublisher)
	}
	var self string
	if err := db.QueryRowContext(ctx, `SELECT @@SERVERNAME`).Scan(&self); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(info.Distributor, self) {
		t.Errorf("Distributor = %q, want this instance, %q", info.Distributor, self)
	}
	if !slices.ContainsFunc(info.DistributionDatabases, func(d DistributionDatabase) bool {
		return d.Name == "distribution" && d.MaxRetentionHours > 0
	}) {
		t.Errorf("DistributionDatabases = %+v, want distribution", info.DistributionDatabases)
	}
	if len(info.Publishers) == 0 || info.Publishers[0].DistributionDatabase != "distribution" ||
		!info.Publishers[0].WindowsAuth || !info.Publishers[0].Active {
		t.Errorf("Publishers = %+v", info.Publishers)
	}
	flags := map[string]ReplicationDatabase{}
	for _, d := range info.Databases {
		flags[d.Name] = d
	}
	if !flags[replFixtureTranDB].Published || !flags[replFixtureMergeDB].MergePublished || !flags["distribution"].Distribution {
		t.Errorf("Databases = %+v", info.Databases)
	}
	if info.DistributorDetailsHidden {
		t.Error("DistributorDetailsHidden for sysadmin")
	}
	if !flags[replFixtureTranDB].Readable || !flags[replFixtureMergeDB].Readable {
		t.Errorf("published databases not Readable to sysadmin: %+v", info.Databases)
	}
	if _, ok := flags[replFixtureSubDB]; ok {
		t.Errorf("subscriber-only %s listed among replication databases", replFixtureSubDB)
	}
}

func TestLiveReplicationPublications(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	liveReplicationFixture(t, db, ctx)
	srv := liveServer(t, db, ctx)

	all, err := srv.LocalPublications(ctx)
	if err != nil {
		t.Fatalf("LocalPublications: %v", err)
	}
	var names []string
	for _, p := range all {
		names = append(names, p.Database().Name+":"+p.Name)
	}
	for _, want := range []string{replFixtureMergeDB + ":" + replFixtureMergePub, replFixtureTranDB + ":" + replFixtureTranPub} {
		if !slices.Contains(names, want) {
			t.Errorf("LocalPublications = %v, missing %s", names, want)
		}
	}

	// Transactional.
	tran, err := srv.DatabaseRef(replFixtureTranDB).PublicationByName(ctx, replFixtureTranPub)
	if err != nil {
		t.Fatalf("PublicationByName(tran): %v", err)
	}
	if tran.Type != PublicationTransactional || !tran.Active || tran.SyncMethod != "native" ||
		tran.RetentionUnit != "hour" || !tran.AllowPush || !tran.AllowPull || tran.AllowAnonymous ||
		!tran.IndependentAgent || tran.ImmediateSync || tran.Description != "gossms fixture: transactional" {
		t.Errorf("transactional publication = %+v", tran)
	}
	arts, err := tran.Articles(ctx)
	if err != nil {
		t.Fatalf("Articles(tran): %v", err)
	}
	byName := map[string]*Article{}
	for _, a := range arts {
		byName[a.Name] = a
		if a.Publication() != tran {
			t.Errorf("article %s: Publication() is not its publication", a.Name)
		}
	}
	if len(arts) != 3 {
		t.Errorf("transactional articles = %d, want 3", len(arts))
	}
	if c := byName["Customer"]; c == nil || c.Filter != "Region = N'EU'" || c.SchemaOnly ||
		c.SourceSchema != "dbo" || c.SourceObject != "Customer" || c.DestinationObject != "Customer" ||
		!strings.HasPrefix(c.Type, "logbased") || c.PreCreationCommand != "drop" ||
		c.SchemaOption&0x01 == 0 || len(c.SchemaOption.Options()) == 0 {
		t.Errorf("Customer article = %+v", c)
	}
	if o := byName["OrderLine"]; o == nil || o.Filter != "" {
		t.Errorf("OrderLine article = %+v", o)
	}
	if g := byName["GetCustomers"]; g == nil || !g.SchemaOnly || g.Type != "proc schema only" || g.DestinationObject != "GetCustomers" {
		t.Errorf("GetCustomers article = %+v", g)
	}
	subs, err := tran.Subscriptions(ctx)
	if err != nil {
		t.Fatalf("Subscriptions(tran): %v", err)
	}
	if len(subs) != 1 || subs[0].SubscriberDB != replFixtureSubDB || subs[0].Type != SubscriptionPull ||
		subs[0].Status != "active" || subs[0].SyncType != "automatic" {
		t.Errorf("transactional subscriptions = %+v, want one active pull into %s", subs, replFixtureSubDB)
	}

	// Merge.
	merge, err := srv.DatabaseRef(replFixtureMergeDB).PublicationByName(ctx, replFixtureMergePub)
	if err != nil {
		t.Fatalf("PublicationByName(merge): %v", err)
	}
	if merge.Type != PublicationMerge || !merge.Active || merge.Retention != 14 || merge.RetentionUnit != "day" ||
		merge.SyncMethod != "native" || merge.CompatibilityLevel == 0 || merge.Description != "gossms fixture: merge" {
		t.Errorf("merge publication = %+v", merge)
	}
	marts, err := merge.Articles(ctx)
	if err != nil {
		t.Fatalf("Articles(merge): %v", err)
	}
	if len(marts) != 1 || marts[0].Name != "Item" || marts[0].Type != "table" || marts[0].Filter != "Category = N'A'" ||
		marts[0].SourceObject != "Item" {
		t.Errorf("merge articles = %+v", marts)
	}
	msubs, err := merge.Subscriptions(ctx)
	if err != nil {
		t.Fatalf("Subscriptions(merge): %v", err)
	}
	if len(msubs) != 1 || msubs[0].SubscriberDB != replFixtureSubDB || msubs[0].Type != SubscriptionPull ||
		msubs[0].SubscriberType != "client" || msubs[0].Status != "active" {
		t.Errorf("merge subscriptions = %+v, want one active client pull into %s", msubs, replFixtureSubDB)
	}

	// The subscriber's copy of the merge publication is not one it publishes.
	if pubs, err := srv.DatabaseRef(replFixtureSubDB).Publications(ctx); err != nil || len(pubs) != 0 {
		t.Errorf("Publications(%s) = %d, %v; want none", replFixtureSubDB, len(pubs), err)
	}
}

func TestLiveReplicationLocalSubscriptions(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	liveReplicationFixture(t, db, ctx)
	srv := liveServer(t, db, ctx)

	all, err := srv.LocalSubscriptions(ctx)
	if err != nil {
		t.Fatalf("LocalSubscriptions: %v", err)
	}
	var tran, merge *LocalSubscription
	for _, l := range all {
		if l.Database().Name != replFixtureSubDB {
			continue
		}
		switch l.Publication {
		case replFixtureTranPub:
			tran = l
		case replFixtureMergePub:
			merge = l
		}
	}
	if tran == nil || merge == nil {
		t.Fatalf("LocalSubscriptions = %+v, want both fixture subscriptions in %s", all, replFixtureSubDB)
	}
	for _, l := range all {
		if l.Database().Name == replFixtureMergeDB {
			t.Errorf("merge publisher %s listed as a subscriber: %+v", replFixtureMergeDB, l)
		}
	}
	if tran.PublisherDB != replFixtureTranDB || tran.PublicationType != PublicationTransactional ||
		tran.Type != SubscriptionPull || tran.Distributor == "" || tran.AgentJob == "" ||
		!tran.IndependentAgent || tran.LastSyncTime.IsZero() {
		t.Errorf("transactional local subscription = %+v", tran)
	}
	if merge.PublisherDB != replFixtureMergeDB || merge.PublicationType != PublicationMerge ||
		merge.Type != SubscriptionPull || merge.SubscriberType != "client" || merge.Distributor == "" ||
		merge.LastSyncTime.IsZero() || merge.LastSyncStatus != 2 {
		t.Errorf("merge local subscription = %+v", merge)
	}
}

// TestLiveReplicationNotConfigured runs where the fixture is not: every read
// must find nothing, without an error, on an instance with no distributor
// and databases with no replication tables.
func TestLiveReplicationNotConfigured(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	var configured int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.servers WHERE is_distributor = 1`).Scan(&configured); err != nil {
		t.Fatal(err)
	}
	if configured != 0 {
		t.Skip("replication is configured on this instance")
	}
	srv := liveServer(t, db, ctx)

	info, err := srv.ReplicationInfo(ctx)
	if err != nil {
		t.Fatalf("ReplicationInfo: %v", err)
	}
	if info.DistributorConfigured || info.IsDistributor || info.IsPublisher ||
		len(info.DistributionDatabases)+len(info.Publishers)+len(info.Databases) != 0 {
		t.Errorf("ReplicationInfo = %+v, want zero", info)
	}
	if pubs, err := srv.LocalPublications(ctx); err != nil || len(pubs) != 0 {
		t.Errorf("LocalPublications = %d, %v; want none", len(pubs), err)
	}
	if subs, err := srv.LocalSubscriptions(ctx); err != nil || len(subs) != 0 {
		t.Errorf("LocalSubscriptions = %d, %v; want none", len(subs), err)
	}
	master := srv.DatabaseRef("master")
	if pubs, err := master.Publications(ctx); err != nil || len(pubs) != 0 {
		t.Errorf("master.Publications = %d, %v; want none", len(pubs), err)
	}
	if subs, err := master.LocalSubscriptions(ctx); err != nil || len(subs) != 0 {
		t.Errorf("master.LocalSubscriptions = %d, %v; want none", len(subs), err)
	}
}
