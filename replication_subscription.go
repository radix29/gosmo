package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Replication subscriptions, read from both ends: as the publisher records
// them (Publication.Subscriptions) and as a subscribing database here holds
// them (LocalSubscriptions). replication.go's header covers both files.

// -- Subscriptions at the publisher -------------------------------------------

// SubscriptionType is how a subscription's agent is run.
type SubscriptionType int

const (
	// SubscriptionPush runs the agent at the distributor (merge: the
	// publisher's distributor).
	SubscriptionPush SubscriptionType = iota
	// SubscriptionPull runs the agent at the subscriber.
	SubscriptionPull
	SubscriptionAnonymous
)

func (t SubscriptionType) String() string {
	switch t {
	case SubscriptionPush:
		return "Push"
	case SubscriptionPull:
		return "Pull"
	case SubscriptionAnonymous:
		return "Anonymous"
	}
	return fmt.Sprintf("SubscriptionType(%d)", int(t))
}

// Subscription is one subscription to a publication, as the publisher
// records it: dbo.syssubscriptions (one row per article, folded here into
// one per subscriber database) or dbo.sysmergesubscriptions.
type Subscription struct {
	pub *Publication

	Subscriber   string
	SubscriberDB string
	Type         SubscriptionType
	// Status is inactive, subscribed (registered but not yet initialized —
	// transactional only), active, or deleted (merge only).
	Status string
	// SyncType is how the subscription is initialized: automatic (from a
	// snapshot) or none (the subscriber already has the schema and data).
	SyncType string
	// Description is merge's; a transactional subscription has none.
	Description string

	// Merge only (zero otherwise).

	// SubscriberType is server (a global subscription, which can republish
	// and has its own conflict priority) or client (a local one).
	SubscriberType string
	Priority       float64
	// The publisher's record of the last synchronization: LastSyncStatus is
	// the agent run status (1 started, 2 succeeded, 3 in progress, 4 idle,
	// 5 retrying, 6 failed), 0 before the first.
	LastSyncTime    time.Time
	LastSyncStatus  int
	LastSyncSummary string
}

// Publication returns the publication subscribed to.
func (s *Subscription) Publication() *Publication { return s.pub }

// mergeSubscriberTypes maps sysmergesubscriptions.subscriber_type.
var mergeSubscriberTypes = map[int]string{1: "server", 2: "client", 3: "anonymous"}

// Subscriptions returns the subscriptions to the publication registered at
// the publisher, in subscriber and database order. An anonymous merge
// subscription is not registered there and so is not among them; nor is a
// transactional publication's "virtual" subscription, the placeholder
// immediate_sync keeps for snapshots.
func (p *Publication) Subscriptions(ctx context.Context) ([]*Subscription, error) {
	what := fmt.Sprintf("read subscriptions of publication %q in database %q", p.Name, p.db.Name)
	if p.Type == PublicationMerge {
		return p.mergeSubscriptions(ctx, what)
	}
	// A subscription has a syssubscriptions row per article; status and sync
	// type are the same on each but for one article added since, whose row
	// is behind — MIN reports the subscription by its least advanced.
	rows, err := p.db.query(ctx, `
SELECT ISNULL(s.srvname, N''), s.dest_db, MIN(s.subscription_type), MIN(s.status), MIN(s.sync_type)
FROM   dbo.syssubscriptions AS s
JOIN   (SELECT artid, pubid FROM dbo.sysarticles
        UNION ALL
        SELECT artid, pubid FROM dbo.sysschemaarticles) AS a ON a.artid = s.artid
WHERE  a.pubid = @p1 AND s.srvid >= 0 AND s.dest_db <> N'virtual'
GROUP  BY s.srvname, s.dest_db
ORDER  BY s.srvname, s.dest_db`, p.pubID)
	return scanRows(rows, err, what, func(scan func(...any) error) (*Subscription, error) {
		s := &Subscription{pub: p}
		var subType, status, syncType int
		if err := scan(&s.Subscriber, &s.SubscriberDB, &subType, &status, &syncType); err != nil {
			return nil, err
		}
		s.Type = SubscriptionType(subType)
		s.Status = map[int]string{0: "inactive", 1: "subscribed", 2: "active"}[status]
		s.SyncType = map[int]string{1: "automatic", 2: "none"}[syncType]
		return s, nil
	})
}

func (p *Publication) mergeSubscriptions(ctx context.Context, what string) ([]*Subscription, error) {
	// The publisher's own row (subid = pubid) is the publication's, not a
	// subscription.
	rows, err := p.db.query(ctx, `
SELECT subscriber_server, db_name, subscription_type, status, sync_type, ISNULL(description, N''),
       subscriber_type, CAST(ISNULL(priority, 0) AS float),
       last_sync_date, ISNULL(last_sync_status, 0), ISNULL(last_sync_summary, N'')
FROM   dbo.sysmergesubscriptions
WHERE  pubid = CONVERT(uniqueidentifier, @p1) AND subid <> pubid
ORDER  BY subscriber_server, db_name`, p.mergeID)
	return scanRows(rows, err, what, func(scan func(...any) error) (*Subscription, error) {
		s := &Subscription{pub: p}
		var subType, status, syncType, subscriberType int
		var last sql.NullTime
		if err := scan(&s.Subscriber, &s.SubscriberDB, &subType, &status, &syncType, &s.Description,
			&subscriberType, &s.Priority, &last, &s.LastSyncStatus, &s.LastSyncSummary); err != nil {
			return nil, err
		}
		s.Type = SubscriptionType(subType)
		s.Status = map[int]string{0: "inactive", 1: "active", 2: "deleted"}[status]
		s.SyncType = map[int]string{1: "automatic", 2: "none"}[syncType]
		s.SubscriberType = mergeSubscriberTypes[subscriberType]
		s.LastSyncTime = last.Time
		return s, nil
	})
}

// -- Subscriptions at the subscriber ------------------------------------------

// LocalSubscription is a subscription a database on this instance holds to
// a publication: SSMS's Replication › Local Subscriptions. It is read from
// the subscriber's own tables — dbo.MSreplication_subscriptions for
// snapshot and transactional, dbo.sysmergesubscriptions for merge — so it
// is there whether the publisher is this instance or another.
type LocalSubscription struct {
	db *Database

	Publisher   string
	PublisherDB string
	Publication string
	// PublicationType is what the subscription is to. A push subscription
	// to a snapshot publication reads as transactional: the subscriber
	// records the difference only for a pull subscription.
	PublicationType PublicationType
	Type            SubscriptionType
	// Distributor is the distributor the pull agent connects to, empty for
	// a push subscription to a transactional publication (its agent runs
	// at the distributor, which the subscriber does not record).
	Distributor string
	Description string

	// LastSyncTime is when the subscription last synchronized: the last
	// transaction applied (transactional) or the last merge.
	LastSyncTime time.Time

	// Transactional and snapshot only (zero for merge).

	// AgentJob is the Distribution Agent's job name — at the subscriber
	// for a pull subscription, at the distributor for push.
	AgentJob         string
	IndependentAgent bool
	// UpdateMode is 0 read-only, 1 immediate updating, 2 queued updating,
	// 3 immediate with queued failover, 4 queued with immediate failover.
	UpdateMode int

	// Merge only (zero otherwise): the subscriber's record of its last
	// merge, as Subscription's.
	SubscriberType  string
	LastSyncStatus  int
	LastSyncSummary string
}

// Database returns the subscription database.
func (l *LocalSubscription) Database() *Database { return l.db }

// LocalSubscriptions returns the subscriptions d holds, snapshot and
// transactional first, then merge, each in publisher, database and
// publication order. A database that subscribes to nothing returns none and
// no error.
func (d *Database) LocalSubscriptions(ctx context.Context) ([]*LocalSubscription, error) {
	what := fmt.Sprintf("read local subscriptions of database %q", d.Name)
	if err := d.server.refuseAzureReplication(what); err != nil {
		return nil, err
	}
	have, err := d.replTables(ctx, what, "MSreplication_subscriptions", "MSsubscription_properties",
		"sysmergesubscriptions", "sysmergepublications")
	if err != nil {
		return nil, err
	}
	var out []*LocalSubscription
	if have[0] {
		// MSsubscription_properties holds a pull subscription's settings;
		// without one (push only) the join has nothing to find.
		props := `CAST(NULL AS int), CAST(NULL AS sysname)`
		join := ``
		if have[1] {
			props = `p.publication_type, p.distributor`
			join = `
LEFT   JOIN dbo.MSsubscription_properties AS p
         ON p.publisher = s.publisher AND p.publisher_db = s.publisher_db AND p.publication = s.publication`
		}
		rows, err := d.query(ctx, `
SELECT s.publisher, s.publisher_db, s.publication, s.subscription_type,
       ISNULL(s.description, N''), s.time, ISNULL(s.distribution_agent, N''),
       s.independent_agent, s.update_mode, `+props+`
FROM   dbo.MSreplication_subscriptions AS s`+join+`
ORDER  BY s.publisher, s.publisher_db, s.publication`)
		subs, err := scanRows(rows, err, what, func(scan func(...any) error) (*LocalSubscription, error) {
			l := &LocalSubscription{db: d}
			var subType int
			var last sql.NullTime
			var pubType sql.NullInt32
			var distributor sql.NullString
			if err := scan(&l.Publisher, &l.PublisherDB, &l.Publication, &subType,
				&l.Description, &last, &l.AgentJob,
				&l.IndependentAgent, &l.UpdateMode, &pubType, &distributor); err != nil {
				return nil, err
			}
			l.Type = SubscriptionType(subType)
			l.LastSyncTime = last.Time
			l.Distributor = distributor.String
			if pubType.Valid && pubType.Int32 == 1 {
				l.PublicationType = PublicationSnapshot
			}
			return l, nil
		})
		if err != nil {
			return nil, err
		}
		out = append(out, subs...)
	}
	if have[2] && have[3] {
		// d's own row is the one naming d; the publisher's row beside it
		// has subid = pubid. A publication d itself publishes (republishing)
		// is d's, not a subscription.
		rows, err := d.query(ctx, `
SELECT p.publisher, p.publisher_db, p.name, s.subscription_type, ISNULL(s.description, N''),
       s.last_sync_date, ISNULL(p.distributor, N''), s.subscriber_type,
       ISNULL(s.last_sync_status, 0), ISNULL(s.last_sync_summary, N'')
FROM   dbo.sysmergesubscriptions AS s
JOIN   dbo.sysmergepublications AS p ON p.pubid = s.pubid
WHERE  s.subid <> s.pubid
  AND  s.db_name = DB_NAME() AND UPPER(s.subscriber_server) = UPPER(@@SERVERNAME)
  AND  NOT (p.publisher_db = DB_NAME() AND UPPER(p.publisher) = UPPER(@@SERVERNAME))
ORDER  BY p.publisher, p.publisher_db, p.name`)
		subs, err := scanRows(rows, err, what, func(scan func(...any) error) (*LocalSubscription, error) {
			l := &LocalSubscription{db: d, PublicationType: PublicationMerge}
			var subType, subscriberType int
			var last sql.NullTime
			if err := scan(&l.Publisher, &l.PublisherDB, &l.Publication, &subType, &l.Description,
				&last, &l.Distributor, &subscriberType,
				&l.LastSyncStatus, &l.LastSyncSummary); err != nil {
				return nil, err
			}
			l.Type = SubscriptionType(subType)
			l.LastSyncTime = last.Time
			l.SubscriberType = mergeSubscriberTypes[subscriberType]
			return l, nil
		})
		if err != nil {
			return nil, err
		}
		out = append(out, subs...)
	}
	return out, nil
}

// LocalSubscriptions returns the subscriptions held by every database on
// the instance, in database order, then as Database.LocalSubscriptions
// orders them. A database that cannot be read (offline, or the login has no
// access) is skipped.
//
// sys.databases.is_subscribed does not say which databases to look in (see
// the top of this file), so the candidates are those holding a subscriber
// table at all.
func (s *Server) LocalSubscriptions(ctx context.Context) ([]*LocalSubscription, error) {
	const what = "list local subscriptions"
	if err := s.refuseAzureReplication(what); err != nil {
		return nil, err
	}
	rows, err := s.query(ctx, s.databaseSelect()+`
	WHERE state = 0 AND HAS_DBACCESS(name) = 1
	  AND (OBJECT_ID(QUOTENAME(name) + N'.dbo.MSreplication_subscriptions', N'U') IS NOT NULL
	    OR OBJECT_ID(QUOTENAME(name) + N'.dbo.sysmergesubscriptions', N'U') IS NOT NULL)
	ORDER BY name`)
	dbs, err := scanRows(rows, err, what, func(scan func(...any) error) (*Database, error) {
		return scanDatabase(s, scan)
	})
	if err != nil {
		return nil, err
	}
	var out []*LocalSubscription
	for _, d := range dbs {
		subs, err := d.LocalSubscriptions(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, subs...)
	}
	return out, nil
}
