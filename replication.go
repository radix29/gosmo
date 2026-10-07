package gosmo

import (
	"context"
	"database/sql"
	"fmt"
)

// ============================================================
// Replication (read-only)
// ============================================================
//
// What SSMS's Object Explorer shows of replication: whether the instance has
// a distributor and is one, the publications each published database holds
// with their articles and subscriptions, and the subscriptions each database
// on this instance holds to publications elsewhere. Nothing here configures
// replication. The subscription reads, at the publisher and at the
// subscriber, are replication_subscription.go's; the monitor reads (agent
// status and history, errors) are replication_monitor.go's.
//
// Every read here is of replication's own tables — sys.servers and
// sys.databases at the server, the dbo.sys*publications / sys*articles /
// sys*subscriptions tables a publishing database is given, the
// MSreplication_subscriptions / sysmergesubscriptions tables a subscribing
// one is given, and msdb's MSdistributiondbs/MSdistpublishers at a
// distributor. Those tables exist only once replication made them, so each
// read asks OBJECT_ID first and a database (or instance) without them reads
// as having nothing — never as "invalid object name".
//
// Which flag to trust: sys.databases.is_published and is_merge_published
// are set by sp_replicationdboption and are what ReplicationPublications
// lists by. is_subscribed is not — it stays 0 on a database holding a pull
// subscription (seen on 2025) — so local subscriptions are found from the
// subscriber-side tables themselves.
//
// Names of servers compare case-insensitively throughout: replication stores
// whatever spelling @@SERVERNAME had when each row was written, and win10cli
// holds "WIN10CLI" in some tables and "win10cli" in others for one instance.
//
// No version gate: every column read here exists on 2016, gosmo's floor.
// Linux instances publish snapshot and transactional only (2017 CU18 and
// later), so a merge read there finds no tables and returns none. Azure SQL
// Database has no replication tables to read and is refused outright; a
// Managed Instance can publish and distribute and reads like any other.
//
// Rights: the server reads need none beyond VIEW ANY DATABASE; the msdb
// distributor tables are readable by sysadmin only (not replmonitor, seen on
// 2025), so ReplicationInfo skips them for anyone else
// (DistributorDetailsHidden); a database's replication tables are readable by
// any user of that database. A read refused for a permission returns the
// server's error.

// refuseAzureReplication refuses what on Azure SQL Database, which has no
// replication catalog: it can only be a push subscriber, and keeps no
// subscriber-side tables a login could read.
func (s *Server) refuseAzureReplication(what string) error {
	if s.info != nil && EngineEdition(s.info.EngineEdition) == EngineAzureSQLDatabase {
		return unsupportedVersionf("gosmo: %s: Azure SQL Database has no replication catalog", what)
	}
	return nil
}

// -- Server: distributor and publisher configuration --------------------------

// ReplicationInfo is the instance's replication configuration, as the
// Replication folder of SSMS's Object Explorer and its Distributor
// Properties read it.
type ReplicationInfo struct {
	// DistributorConfigured is true once sp_adddistributor has named a
	// distributor for this instance — itself or a remote one. Without it the
	// instance can neither publish nor distribute, and every other field is
	// empty but Databases (which a subscriber still has).
	DistributorConfigured bool
	// Distributor is the distributor's server name, as sys.servers holds it
	// for the repl_distributor entry.
	Distributor string
	// IsDistributor is true when this instance is its own distributor: it
	// hosts a distribution database. DistributionDatabases and Publishers are
	// read only then.
	IsDistributor bool
	// IsPublisher is true when the local distributor has this instance
	// registered as a publisher (sp_adddistpublisher), or, with a remote
	// distributor or msdb's MSdistpublishers unreadable, when a database
	// here is published — which of the remote distributor's publishers this
	// instance is cannot be read from here.
	IsPublisher bool

	// DistributionDatabases and Publishers are msdb's MSdistributiondbs and
	// MSdistpublishers, which only sysadmin may read (replmonitor may not:
	// seen on 2025). DistributorDetailsHidden says they were not readable
	// and are empty for that reason; the distribution databases' names are
	// still in Databases (Distribution), which DistributionDatabaseRef
	// turns into handles for the monitor reads.
	DistributionDatabases    []DistributionDatabase
	Publishers               []DistributionPublisher
	DistributorDetailsHidden bool

	// Databases lists the databases with a replication role, in name order:
	// published for snapshot/transactional, for merge, or a distribution
	// database. A subscriber-only database is not among them — see
	// Server.LocalSubscriptions.
	Databases []ReplicationDatabase
}

// DistributionDatabase is one distribution database at this distributor,
// from msdb.dbo.MSdistributiondbs. Its Replication Monitor reads are
// replication_monitor.go's.
type DistributionDatabase struct {
	srv *Server

	Name string
	// The transaction retention window in hours: commands are kept at least
	// MinRetentionHours, and deleted once older than MaxRetentionHours even
	// if a subscription has not read them (it is then marked inactive).
	MinRetentionHours int
	MaxRetentionHours int
	// HistoryRetentionHours is how long agent history is kept.
	HistoryRetentionHours int
}

// DistributionDatabaseRef is a handle on the distribution database name at
// this instance, for the monitor reads (replication_monitor.go). It issues no
// query: its retention fields are zero. ReplicationInfo's
// DistributionDatabases are the populated form, but only sysadmin can read
// them; the names are in ReplicationInfo.Databases for anyone.
func (s *Server) DistributionDatabaseRef(name string) *DistributionDatabase {
	return &DistributionDatabase{srv: s, Name: name}
}

// DistributionPublisher is one publisher registered at this distributor,
// from msdb.dbo.MSdistpublishers.
type DistributionPublisher struct {
	Name                 string
	DistributionDatabase string
	// WorkingDirectory is the default snapshot folder.
	WorkingDirectory string
	// WindowsAuth is true when the distributor's agents connect to the
	// publisher with Windows authentication (security_mode 1), false for a
	// SQL Server login.
	WindowsAuth bool
	Active      bool
	// PublisherType is MSSQLSERVER for a SQL Server publisher, ORACLE or
	// ORACLE GATEWAY for a heterogeneous one.
	PublisherType string
}

// ReplicationDatabase is one database's replication flags from sys.databases.
type ReplicationDatabase struct {
	Name           string
	Published      bool // snapshot or transactional publishing enabled
	MergePublished bool
	Distribution   bool // a distribution database
	// Readable is whether this login can read the database's replication
	// tables: it is online and HAS_DBACCESS says yes. LocalPublications skips
	// a published database that is not, so this is how a caller tells "no
	// publications" from "publications it cannot see".
	Readable bool
}

// ReplicationInfo reads the instance's distributor and publisher
// configuration. An instance with no replication configured returns a zero
// ReplicationInfo and no error.
func (s *Server) ReplicationInfo(ctx context.Context) (*ReplicationInfo, error) {
	const what = "read replication configuration"
	if err := s.refuseAzureReplication(what); err != nil {
		return nil, err
	}
	info := &ReplicationInfo{}
	var distributor sql.NullString
	var hasDistDBs, hasDistPublishers, canReadDistDBs, canReadDistPublishers bool
	// sp_get_distributor would answer the first two, but the shape of its
	// result set differs between versions; sys.servers is what it reads.
	// The msdb tables are asked about twice: whether they exist, and whether
	// this login may read them — a refused SELECT would fail the whole read.
	err := s.queryRowScan(ctx, `
SELECT (SELECT TOP (1) data_source FROM sys.servers WHERE is_distributor = 1),
       CAST(CASE WHEN EXISTS (SELECT 1 FROM sys.databases WHERE is_distributor = 1) THEN 1 ELSE 0 END AS bit),
       CAST(CASE WHEN OBJECT_ID(N'msdb.dbo.MSdistributiondbs', N'U') IS NULL THEN 0 ELSE 1 END AS bit),
       CAST(CASE WHEN OBJECT_ID(N'msdb.dbo.MSdistpublishers', N'U') IS NULL THEN 0 ELSE 1 END AS bit),
       CAST(ISNULL(HAS_PERMS_BY_NAME(N'msdb.dbo.MSdistributiondbs', N'OBJECT', N'SELECT'), 0) AS bit),
       CAST(ISNULL(HAS_PERMS_BY_NAME(N'msdb.dbo.MSdistpublishers', N'OBJECT', N'SELECT'), 0) AS bit)`, nil,
		&distributor, &info.IsDistributor, &hasDistDBs, &hasDistPublishers, &canReadDistDBs, &canReadDistPublishers)
	if err != nil {
		return nil, fmt.Errorf("gosmo: %s: %w", what, err)
	}
	info.DistributorConfigured = distributor.Valid
	info.Distributor = distributor.String
	// To a login that may not read them the tables are invisible too, so
	// OBJECT_ID is NULL: at a distributor, where they always exist, that is
	// the same answer as a refused SELECT.
	if info.IsDistributor && (!canReadDistDBs || !canReadDistPublishers) {
		info.DistributorDetailsHidden = true
	}
	hasDistDBs = hasDistDBs && canReadDistDBs
	hasDistPublishers = hasDistPublishers && canReadDistPublishers

	if info.IsDistributor && hasDistDBs {
		rows, err := s.query(ctx, `
SELECT name, min_distretention, max_distretention, history_retention
FROM   msdb.dbo.MSdistributiondbs
ORDER  BY name`)
		info.DistributionDatabases, err = scanRows(rows, err, "read distribution databases", func(scan func(...any) error) (DistributionDatabase, error) {
			d := DistributionDatabase{srv: s}
			err := scan(&d.Name, &d.MinRetentionHours, &d.MaxRetentionHours, &d.HistoryRetentionHours)
			return d, err
		})
		if err != nil {
			return nil, err
		}
	}
	if info.IsDistributor && hasDistPublishers {
		rows, err := s.query(ctx, `
SELECT name, distribution_db, ISNULL(working_directory, N''), security_mode, active,
       ISNULL(publisher_type, N''),
       CAST(CASE WHEN UPPER(name) = UPPER(@@SERVERNAME) THEN 1 ELSE 0 END AS bit)
FROM   msdb.dbo.MSdistpublishers
ORDER  BY name`)
		info.Publishers, err = scanRows(rows, err, "read distribution publishers", func(scan func(...any) error) (DistributionPublisher, error) {
			var p DistributionPublisher
			var mode int
			var self bool
			err := scan(&p.Name, &p.DistributionDatabase, &p.WorkingDirectory, &mode, &p.Active, &p.PublisherType, &self)
			p.WindowsAuth = mode == 1
			if self {
				info.IsPublisher = true
			}
			return p, err
		})
		if err != nil {
			return nil, err
		}
	}

	rows, err := s.query(ctx, `
SELECT name, is_published, is_merge_published, is_distributor,
       CAST(CASE WHEN state = 0 AND HAS_DBACCESS(name) = 1 THEN 1 ELSE 0 END AS bit)
FROM   sys.databases
WHERE  is_published = 1 OR is_merge_published = 1 OR is_distributor = 1
ORDER  BY name`)
	info.Databases, err = scanRows(rows, err, "read replication databases", func(scan func(...any) error) (ReplicationDatabase, error) {
		var d ReplicationDatabase
		err := scan(&d.Name, &d.Published, &d.MergePublished, &d.Distribution, &d.Readable)
		return d, err
	})
	if err != nil {
		return nil, err
	}
	if info.DistributorConfigured && (!info.IsDistributor || !hasDistPublishers) {
		for _, d := range info.Databases {
			if d.Published || d.MergePublished {
				info.IsPublisher = true
			}
		}
	}
	return info, nil
}

// -- Publications --------------------------------------------------------------

// PublicationType is the kind of a publication.
type PublicationType int

const (
	PublicationTransactional PublicationType = iota
	PublicationSnapshot
	// PublicationPeerToPeer is a transactional publication enabled for
	// peer-to-peer replication.
	PublicationPeerToPeer
	PublicationMerge
)

func (t PublicationType) String() string {
	switch t {
	case PublicationTransactional:
		return "Transactional"
	case PublicationSnapshot:
		return "Snapshot"
	case PublicationPeerToPeer:
		return "Peer-to-peer"
	case PublicationMerge:
		return "Merge"
	}
	return fmt.Sprintf("PublicationType(%d)", int(t))
}

// Publication is one publication of a published database: a row of
// dbo.syspublications (snapshot, transactional, peer-to-peer) or of
// dbo.sysmergepublications (merge).
type Publication struct {
	db *Database

	// pubID is syspublications.pubid; mergeID is sysmergepublications.pubid.
	// Exactly one is set, by Type.
	pubID   int
	mergeID string

	Name        string
	Description string
	Type        PublicationType
	Active      bool

	// Retention is how long a subscription may go without synchronizing
	// before it expires, in RetentionUnit: "hour" for a transactional or
	// snapshot publication, "day", "week", "month" or "year" for merge. 0
	// means a subscription never expires.
	Retention     int
	RetentionUnit string

	// SyncMethod is the snapshot format, by sp_addpublication's
	// @sync_method name: native, character, concurrent, concurrent_c,
	// database snapshot, database snapshot character. A merge publication
	// has native or character only.
	SyncMethod string

	AllowPush      bool
	AllowPull      bool
	AllowAnonymous bool
	// AllowSubscriptionCopy permits a subscription database to be copied to
	// make another subscriber.
	AllowSubscriptionCopy bool
	// ReplicateDDL propagates schema changes on published objects.
	ReplicateDDL bool

	// Snapshot location and scripts.
	SnapshotInDefaultFolder bool
	AltSnapshotFolder       string
	CompressSnapshot        bool
	PreSnapshotScript       string
	PostSnapshotScript      string

	// Transactional and snapshot only (false for merge).

	// ImmediateSync keeps a snapshot generated for every run of the Snapshot
	// Agent, so a new subscription can initialize at once.
	ImmediateSync bool
	// IndependentAgent gives each subscription its own Distribution Agent,
	// rather than one shared by every subscription to the database.
	IndependentAgent          bool
	AllowInitializeFromBackup bool
	// AllowQueuedUpdates and AllowImmediateUpdates are updatable
	// subscriptions — queued and immediate updating.
	AllowQueuedUpdates    bool
	AllowImmediateUpdates bool

	// Merge only (zero otherwise).

	// CompatibilityLevel is sysmergepublications.backward_comp_level, the
	// oldest subscriber version the publication supports (90 = 2005, 100 =
	// 2008, …).
	CompatibilityLevel int
	AllowWebSync       bool
	// UsePartitionGroups is precomputed partitions.
	UsePartitionGroups bool
	// CentralizedConflicts keeps conflict rows at the publisher rather than
	// at the subscriber that lost.
	CentralizedConflicts bool
	// ConflictRetention is how many days conflict rows are kept.
	ConflictRetention int
}

// Database returns the published database the publication belongs to.
func (p *Publication) Database() *Database { return p.db }

// syncMethodNames maps syspublications.sync_method to sp_addpublication's
// @sync_method; merge's sync_mode uses 0 and 1 the same way.
var syncMethodNames = map[int]string{
	0: "native",
	1: "character",
	3: "concurrent",
	4: "concurrent_c",
	5: "database snapshot",
	6: "database snapshot character",
}

// mergeRetentionUnits maps sysmergepublications.retention_period_unit.
var mergeRetentionUnits = map[int]string{0: "day", 1: "week", 2: "month", 3: "year"}

// replTables reports which of names exist as tables in d, in order — the
// replication tables are created only when replication needs them.
func (d *Database) replTables(ctx context.Context, what string, names ...string) ([]bool, error) {
	q := "SELECT "
	for i := range names {
		if i > 0 {
			q += ", "
		}
		q += fmt.Sprintf("CAST(CASE WHEN OBJECT_ID(@p%d, N'U') IS NULL THEN 0 ELSE 1 END AS bit)", i+1)
	}
	args := make([]any, len(names))
	have := make([]bool, len(names))
	dest := make([]any, len(names))
	for i, n := range names {
		args[i] = "dbo." + n
		dest[i] = &have[i]
	}
	err := d.queryRow(ctx, func(row *sql.Row) error { return row.Scan(dest...) }, q, args...)
	if err != nil {
		return nil, fmt.Errorf("gosmo: %s: %w", what, err)
	}
	return have, nil
}

// Publications returns the publications d publishes, transactional and
// snapshot first, then merge, each in name order. A database that publishes
// nothing returns none and no error.
//
// A merge subscriber's sysmergepublications also holds a row for each
// publication it subscribes to; those are the publisher's, not d's, and are
// left out.
func (d *Database) Publications(ctx context.Context) ([]*Publication, error) {
	what := fmt.Sprintf("read publications of database %q", d.Name)
	if err := d.server.refuseAzureReplication(what); err != nil {
		return nil, err
	}
	have, err := d.replTables(ctx, what, "syspublications", "sysmergepublications")
	if err != nil {
		return nil, err
	}
	var out []*Publication
	if have[0] {
		rows, err := d.query(ctx, `
SELECT pubid, name, ISNULL(description, N''), repl_freq, options, status, retention, sync_method,
       ISNULL(allow_push, 0), ISNULL(allow_pull, 0), ISNULL(allow_anonymous, 0),
       ISNULL(allow_subscription_copy, 0), CAST(ISNULL(replicate_ddl, 0) AS int),
       ISNULL(snapshot_in_defaultfolder, 1), ISNULL(alt_snapshot_folder, N''), ISNULL(compress_snapshot, 0),
       ISNULL(pre_snapshot_script, N''), ISNULL(post_snapshot_script, N''),
       ISNULL(immediate_sync, 0), ISNULL(independent_agent, 0), ISNULL(allow_initialize_from_backup, 0),
       ISNULL(allow_queued_tran, 0), ISNULL(allow_sync_tran, 0)
FROM   dbo.syspublications
ORDER  BY name`)
		pubs, err := scanRows(rows, err, what, func(scan func(...any) error) (*Publication, error) {
			p := &Publication{db: d, RetentionUnit: "hour"}
			var freq, options, status, method, replicateDDL int
			if err := scan(&p.pubID, &p.Name, &p.Description, &freq, &options, &status, &p.Retention, &method,
				&p.AllowPush, &p.AllowPull, &p.AllowAnonymous, &p.AllowSubscriptionCopy, &replicateDDL,
				&p.SnapshotInDefaultFolder, &p.AltSnapshotFolder, &p.CompressSnapshot,
				&p.PreSnapshotScript, &p.PostSnapshotScript,
				&p.ImmediateSync, &p.IndependentAgent, &p.AllowInitializeFromBackup,
				&p.AllowQueuedUpdates, &p.AllowImmediateUpdates); err != nil {
				return nil, err
			}
			switch {
			case freq == 1:
				p.Type = PublicationSnapshot
			case options&0x1 != 0:
				p.Type = PublicationPeerToPeer
			default:
				p.Type = PublicationTransactional
			}
			p.Active = status == 1
			p.SyncMethod = syncMethodNames[method]
			p.ReplicateDDL = replicateDDL != 0
			return p, nil
		})
		if err != nil {
			return nil, err
		}
		out = append(out, pubs...)
	}
	if have[1] {
		rows, err := d.query(ctx, `
SELECT CONVERT(nchar(36), pubid), name, ISNULL(description, N''), status, ISNULL(retention, 0),
       ISNULL(retention_period_unit, 0), ISNULL(sync_mode, 0),
       ISNULL(allow_push, 0), ISNULL(allow_pull, 0), ISNULL(allow_anonymous, 0),
       ISNULL(allow_subscription_copy, 0), CAST(ISNULL(replicate_ddl, 0) AS int),
       ISNULL(snapshot_in_defaultfolder, 1), ISNULL(alt_snapshot_folder, N''), ISNULL(compress_snapshot, 0),
       ISNULL(pre_snapshot_script, N''), ISNULL(post_snapshot_script, N''),
       ISNULL(backward_comp_level, 0), CAST(ISNULL(allow_web_synchronization, 0) AS int),
       CAST(ISNULL(use_partition_groups, 0) AS int), ISNULL(centralized_conflicts, 0), ISNULL(conflict_retention, 0)
FROM   dbo.sysmergepublications
WHERE  publisher_db = DB_NAME() AND UPPER(publisher) = UPPER(@@SERVERNAME)
ORDER  BY name`)
		pubs, err := scanRows(rows, err, what, func(scan func(...any) error) (*Publication, error) {
			p := &Publication{db: d, Type: PublicationMerge}
			var status, unit, mode int
			var replicateDDL, webSync, partitionGroups int
			if err := scan(&p.mergeID, &p.Name, &p.Description, &status, &p.Retention,
				&unit, &mode,
				&p.AllowPush, &p.AllowPull, &p.AllowAnonymous, &p.AllowSubscriptionCopy, &replicateDDL,
				&p.SnapshotInDefaultFolder, &p.AltSnapshotFolder, &p.CompressSnapshot,
				&p.PreSnapshotScript, &p.PostSnapshotScript,
				&p.CompatibilityLevel, &webSync, &partitionGroups,
				&p.CentralizedConflicts, &p.ConflictRetention); err != nil {
				return nil, err
			}
			p.Active = status == 1
			p.RetentionUnit = mergeRetentionUnits[unit]
			p.SyncMethod = syncMethodNames[mode]
			p.ReplicateDDL = replicateDDL != 0
			p.AllowWebSync = webSync != 0
			// use_partition_groups is NULL until the snapshot decides it,
			// and -1 where it is not possible.
			p.UsePartitionGroups = partitionGroups > 0
			return p, nil
		})
		if err != nil {
			return nil, err
		}
		out = append(out, pubs...)
	}
	return out, nil
}

// PublicationByName returns d's publication called name, of either kind.
func (d *Database) PublicationByName(ctx context.Context, name string) (*Publication, error) {
	pubs, err := d.Publications(ctx)
	if err != nil {
		return nil, err
	}
	p, err := matchName(pubs, name, func(p *Publication) string { return p.Name })
	if err != nil {
		return nil, fmt.Errorf("gosmo: publication %q in database %q: %w", name, d.Name, err)
	}
	return p, nil
}

// LocalPublications returns the publications of every published database
// on the instance — SSMS's Replication › Local Publications — in database
// order, then as Database.Publications orders them. A published database
// that cannot be read (offline, or the login has no access) is skipped.
func (s *Server) LocalPublications(ctx context.Context) ([]*Publication, error) {
	const what = "list local publications"
	if err := s.refuseAzureReplication(what); err != nil {
		return nil, err
	}
	rows, err := s.query(ctx, s.databaseSelect()+`
	WHERE (is_published = 1 OR is_merge_published = 1)
	  AND state = 0 AND HAS_DBACCESS(name) = 1
	ORDER BY name`)
	dbs, err := scanRows(rows, err, what, func(scan func(...any) error) (*Database, error) {
		return scanDatabase(s, scan)
	})
	if err != nil {
		return nil, err
	}
	var out []*Publication
	for _, d := range dbs {
		pubs, err := d.Publications(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, pubs...)
	}
	return out, nil
}

// -- Articles ------------------------------------------------------------------

// Article is one published object of a publication: a row of
// dbo.sysarticles (a table or an indexed view, or a stored procedure whose
// execution is replicated), dbo.sysschemaarticles (an object whose schema
// only is published) or their merge counterparts.
type Article struct {
	pub *Publication

	Name string
	// Type is the article type by sp_addarticle's @type name: logbased,
	// logbased manualfilter, logbased manualview, logbased manualboth,
	// proc exec, serializable proc exec, proc schema only, view schema only,
	// func schema only, indexed view logbased, … For merge: table, proc
	// schema only, view schema only, func schema only, synonym schema only.
	// TypeCode is the raw value it was decoded from.
	Type     string
	TypeCode int
	// SchemaOnly is true for an article that publishes an object's
	// definition and none of its data.
	SchemaOnly bool

	// The published object, in the publication database. SourceSchema is
	// empty when the object no longer resolves.
	SourceSchema string
	SourceObject string
	// Where the subscriber puts it.
	DestinationOwner  string
	DestinationObject string

	// Filter is the row filter's WHERE clause (merge: the subset filter),
	// empty for an unfiltered article.
	Filter string
	// PreCreationCommand is what the snapshot does to an existing
	// destination object: none, drop, delete or truncate.
	PreCreationCommand string
	SchemaOption       SchemaOption
	Description        string
}

// Publication returns the publication the article belongs to.
func (a *Article) Publication() *Publication { return a.pub }

// articleTypeNames maps sysarticles.type and sysschemaarticles.type to
// sp_addarticle's @type names.
var articleTypeNames = map[int]string{
	1:   "logbased",
	3:   "logbased manualfilter",
	5:   "logbased manualview",
	7:   "logbased manualboth",
	8:   "proc exec",
	24:  "serializable proc exec",
	32:  "proc schema only",
	64:  "view schema only",
	128: "func schema only",
	257: "indexed view logbased",
	259: "indexed view logbased manualfilter",
	261: "indexed view logbased manualview",
	263: "indexed view logbased manualboth",
}

// mergeArticleTypeNames maps sysmergearticles.type and
// sysmergeschemaarticles.type to sp_addmergearticle's @type names.
var mergeArticleTypeNames = map[int]string{
	10:  "table",
	32:  "proc schema only",
	64:  "view schema only",
	128: "func schema only",
	160: "synonym schema only",
}

// preCreationCommands maps pre_creation_cmd (merge: pre_creation_command).
var preCreationCommands = map[int]string{0: "none", 1: "drop", 2: "delete", 3: "truncate"}

// typeName is names[code], or the code itself for one the map lacks.
func typeName(names map[int]string, code int) string {
	if n, ok := names[code]; ok {
		return n
	}
	return fmt.Sprintf("type %d", code)
}

// Articles returns the publication's articles in name order.
func (p *Publication) Articles(ctx context.Context) ([]*Article, error) {
	what := fmt.Sprintf("read articles of publication %q in database %q", p.Name, p.db.Name)
	if p.Type == PublicationMerge {
		return p.mergeArticles(ctx, what)
	}
	// sysarticles and sysschemaarticles always come together; the
	// publication was read from syspublications, so both exist.
	rows, err := p.db.query(ctx, `
SELECT a.name, a.type, CAST(0 AS bit),
       ISNULL(OBJECT_SCHEMA_NAME(a.objid), N''), ISNULL(OBJECT_NAME(a.objid), N''),
       ISNULL(a.dest_owner, N''), ISNULL(a.dest_table, N''),
       ISNULL(CONVERT(nvarchar(max), a.filter_clause), N''), a.pre_creation_cmd,
       CAST(a.schema_option AS bigint), ISNULL(a.description, N'')
FROM   dbo.sysarticles AS a
WHERE  a.pubid = @p1
UNION ALL
SELECT a.name, a.type, CAST(1 AS bit),
       ISNULL(OBJECT_SCHEMA_NAME(a.objid), N''), ISNULL(OBJECT_NAME(a.objid), N''),
       ISNULL(a.dest_owner, N''), ISNULL(a.dest_object, N''),
       N'', a.pre_creation_cmd,
       CAST(a.schema_option AS bigint), ISNULL(a.description, N'')
FROM   dbo.sysschemaarticles AS a
WHERE  a.pubid = @p1
ORDER  BY 1`, p.pubID)
	return scanRows(rows, err, what, func(scan func(...any) error) (*Article, error) {
		a := &Article{pub: p}
		var pre int
		var opt int64
		if err := scan(&a.Name, &a.TypeCode, &a.SchemaOnly, &a.SourceSchema, &a.SourceObject,
			&a.DestinationOwner, &a.DestinationObject, &a.Filter, &pre, &opt, &a.Description); err != nil {
			return nil, err
		}
		a.Type = typeName(articleTypeNames, a.TypeCode)
		a.PreCreationCommand = preCreationCommands[pre]
		a.SchemaOption = SchemaOption(uint64(opt))
		return a, nil
	})
}

func (p *Publication) mergeArticles(ctx context.Context, what string) ([]*Article, error) {
	rows, err := p.db.query(ctx, `
SELECT a.name, a.type, CAST(0 AS bit),
       ISNULL(OBJECT_SCHEMA_NAME(a.objid), N''), ISNULL(OBJECT_NAME(a.objid), N''),
       ISNULL(a.destination_owner, N''), ISNULL(a.destination_object, N''),
       ISNULL(a.subset_filterclause, N''), a.pre_creation_command,
       CAST(a.schema_option AS bigint), ISNULL(a.description, N'')
FROM   dbo.sysmergearticles AS a
WHERE  a.pubid = CONVERT(uniqueidentifier, @p1)
UNION ALL
SELECT a.name, a.type, CAST(1 AS bit),
       ISNULL(OBJECT_SCHEMA_NAME(a.objid), N''), ISNULL(OBJECT_NAME(a.objid), N''),
       ISNULL(a.destination_owner, N''), ISNULL(a.destination_object, N''),
       N'', a.pre_creation_command,
       CAST(a.schema_option AS bigint), ISNULL(a.description, N'')
FROM   dbo.sysmergeschemaarticles AS a
WHERE  a.pubid = CONVERT(uniqueidentifier, @p1)
ORDER  BY 1`, p.mergeID)
	return scanRows(rows, err, what, func(scan func(...any) error) (*Article, error) {
		a := &Article{pub: p}
		var pre int
		var opt int64
		if err := scan(&a.Name, &a.TypeCode, &a.SchemaOnly, &a.SourceSchema, &a.SourceObject,
			&a.DestinationOwner, &a.DestinationObject, &a.Filter, &pre, &opt, &a.Description); err != nil {
			return nil, err
		}
		a.Type = typeName(mergeArticleTypeNames, a.TypeCode)
		a.PreCreationCommand = preCreationCommands[pre]
		a.SchemaOption = SchemaOption(uint64(opt))
		return a, nil
	})
}

// SchemaOption is an article's schema_option bitmask: what the snapshot
// creates at the subscriber besides the object itself. The bits are
// sp_addarticle's and sp_addmergearticle's @schema_option values.
type SchemaOption uint64

// schemaOptionBits lists the documented bits in order, each with what
// setting it does.
var schemaOptionBits = []struct {
	bit  SchemaOption
	desc string
}{
	{0x01, "Create the object (CREATE TABLE, CREATE PROCEDURE, …)"},
	{0x02, "Generate the change-propagation stored procedures"},
	{0x04, "Script identity columns with the IDENTITY property"},
	{0x08, "Replicate timestamp columns (else converted to binary)"},
	{0x10, "Copy the clustered index"},
	{0x20, "Convert user-defined types to base types"},
	{0x40, "Copy nonclustered indexes"},
	{0x80, "Copy the primary key constraint"},
	{0x100, "Copy user triggers"},
	{0x200, "Copy foreign key constraints"},
	{0x400, "Copy check constraints"},
	{0x800, "Copy defaults"},
	{0x1000, "Copy column-level collation"},
	{0x2000, "Copy extended properties"},
	{0x4000, "Copy unique constraints"},
	{0x8000, "(reserved, not valid on 2005 and later publishers)"},
	{0x10000, "Copy check constraints as NOT FOR REPLICATION"},
	{0x20000, "Copy foreign key constraints as NOT FOR REPLICATION"},
	{0x40000, "Copy filegroups of a partitioned table or index"},
	{0x80000, "Copy the partition scheme of a partitioned table"},
	{0x100000, "Copy the partition scheme of a partitioned index"},
	{0x200000, "Copy table statistics"},
	{0x400000, "Copy default bindings"},
	{0x800000, "Copy rule bindings"},
	{0x1000000, "Copy the full-text index"},
	{0x2000000, "Do not copy XML schema collections bound to xml columns"},
	{0x4000000, "Copy indexes on xml columns"},
	{0x8000000, "Create schemas missing at the subscriber"},
	{0x10000000, "Convert xml columns to ntext"},
	{0x20000000, "Convert (n)varchar(max) and varbinary(max) to text types"},
	{0x40000000, "Copy permissions"},
	{0x80000000, "Drop dependencies on objects outside the publication"},
	{0x100000000, "Copy the FILESTREAM attribute"},
	{0x200000000, "Convert date and time types to earlier types"},
	{0x400000000, "Copy data and index compression"},
	{0x800000000, "Store FILESTREAM data on its own filegroup"},
	{0x1000000000, "Convert CLR types over 8000 bytes to varbinary(max)"},
	{0x2000000000, "Convert hierarchyid to varbinary(max)"},
	{0x4000000000, "Copy filtered indexes"},
	{0x8000000000, "Convert geography and geometry to varbinary(max)"},
	{0x10000000000, "Copy indexes on geography and geometry columns"},
	{0x20000000000, "Copy the SPARSE attribute of columns"},
	{0x40000000000, "Create memory-optimized tables at the subscriber"},
	{0x80000000000, "Convert clustered indexes of memory-optimized articles to nonclustered"},
	{0x400000000000, "Copy nonclustered columnstore indexes"},
	{0x800000000000, "Copy filtered nonclustered columnstore indexes"},
}

// Options describes each bit set in o, lowest bit first. A set bit this
// list does not document reads as its hexadecimal value.
func (o SchemaOption) Options() []string {
	var out []string
	known := SchemaOption(0)
	for _, b := range schemaOptionBits {
		known |= b.bit
		if o&b.bit != 0 {
			out = append(out, b.desc)
		}
	}
	for rest := o &^ known; rest != 0; rest &= rest - 1 {
		out = append(out, fmt.Sprintf("0x%X", uint64(rest&-rest)))
	}
	return out
}
