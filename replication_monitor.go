package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ============================================================
// Replication Monitor (read-only)
// ============================================================
//
// What SSMS's Replication Monitor shows, read at a distributor: the
// publishers it serves, their publications and subscriptions with status,
// warnings, latency and performance, every replication agent with its last
// action, an agent's history, and the error detail a failed action left in
// MSrepl_errors. Nothing here starts, stops or reinitializes anything.
//
// The status/warning/latency figures are the monitor procedures' own
// (sp_replmonitorhelppublisher, sp_replmonitorhelppublication,
// sp_replmonitorhelpsubscription): they compute them from
// MSreplication_monitordata, which they refresh when it is stale, and SSMS
// shows exactly these. Agents, their sessions and a session's actions are
// the sp_MSenum_* procedures', and an error's detail sp_MSget_repl_error's —
// again what SSMS reads, and the only way in for replmonitor, which has no
// SELECT on the MS*_agents/_history tables beneath them (seen on 2025).
// Result sets gain columns between versions, so they are read by column name
// (namedRow) and a column a version does not emit reads as zero or nil.
//
// Everything but Server.MonitorPublishers runs in one distribution database:
// take it from ReplicationInfo().DistributionDatabases. A publisher whose
// distributor is another instance is monitored there, not here.
//
// Rights: sysadmin, or db_owner or replmonitor in the distribution database
// (CanMonitor). The procedures refuse anyone else (Msg 14260).
//
// No version gate: every procedure here exists on 2016.

// Server returns the distributor instance the distribution database is on.
func (dd *DistributionDatabase) Server() *Server { return dd.srv }

// database is dd as a Database handle, for reads that run inside it.
func (dd *DistributionDatabase) database() *Database { return dd.srv.DatabaseRef(dd.Name) }

// ReplAgentStatus is a replication agent's run status, as the MS*_history tables
// and the monitor procedures record it. ReplNeverRun is an agent with no
// history yet.
type ReplAgentStatus int

const (
	ReplNeverRun   ReplAgentStatus = 0
	ReplStarted    ReplAgentStatus = 1
	ReplSucceeded  ReplAgentStatus = 2
	ReplInProgress ReplAgentStatus = 3
	ReplIdle       ReplAgentStatus = 4
	ReplRetrying   ReplAgentStatus = 5
	ReplFailed     ReplAgentStatus = 6
)

func (s ReplAgentStatus) String() string {
	switch s {
	case ReplNeverRun:
		return "Never run"
	case ReplStarted:
		return "Started"
	case ReplSucceeded:
		return "Succeeded"
	case ReplInProgress:
		return "In progress"
	case ReplIdle:
		return "Idle"
	case ReplRetrying:
		return "Retrying"
	case ReplFailed:
		return "Failed"
	}
	return fmt.Sprintf("ReplAgentStatus(%d)", int(s))
}

// MonitorWarning is the monitor procedures' warning bitmask: each bit a
// threshold (sp_replmonitorchangepublicationthreshold) the publication or
// subscription has crossed.
type MonitorWarning int

// monitorWarningBits names each documented bit, lowest first.
var monitorWarningBits = []struct {
	bit  MonitorWarning
	desc string
}{
	{1, "Subscription will expire soon"},
	{2, "Latency exceeds the threshold"},
	{4, "Merge subscription will expire soon"},
	{8, "Merge run duration exceeds the threshold (fast link)"},
	{16, "Merge run speed below the threshold (fast link)"},
	{32, "Merge run duration exceeds the threshold (slow link)"},
	{64, "Merge run speed below the threshold (slow link)"},
}

// Warnings describes each bit set in w, lowest first. A bit the list does not
// document reads as its value.
func (w MonitorWarning) Warnings() []string {
	var out []string
	known := MonitorWarning(0)
	for _, b := range monitorWarningBits {
		known |= b.bit
		if w&b.bit != 0 {
			out = append(out, b.desc)
		}
	}
	for rest := w &^ known; rest != 0; rest &= rest - 1 {
		out = append(out, fmt.Sprintf("warning %d", rest&-rest))
	}
	return out
}

// monitorPublicationType maps the monitor procedures' publication_type
// (0 transactional, 1 snapshot, 2 merge). Peer-to-peer reads as
// transactional: the procedures do not tell it apart.
func monitorPublicationType(code int) PublicationType {
	switch code {
	case 1:
		return PublicationSnapshot
	case 2:
		return PublicationMerge
	}
	return PublicationTransactional
}

// scanNamed reads a procedure's result set by column name — see namedRow.
func scanNamed[T any](rows *dbRows, err error, what string, scan func(*namedRow) T) ([]T, error) {
	if err != nil {
		return nil, fmt.Errorf("gosmo: %s: %w", what, err)
	}
	defer rows.Close()
	nr, err := newNamedRow(rows.Rows)
	if err != nil {
		return nil, fmt.Errorf("gosmo: %s: %w", what, err)
	}
	var out []T
	for rows.Next() {
		if err := nr.scan(); err != nil {
			return nil, fmt.Errorf("gosmo: %s: %w", what, err)
		}
		out = append(out, scan(nr))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: %s: %w", what, err)
	}
	return out, nil
}

// -- Access --------------------------------------------------------------------

// CanMonitor reports whether this login may read dd's monitor data: it is
// sysadmin, or a user of dd in db_owner or replmonitor. A login with no
// access to dd at all answers false, not an error.
func (dd *DistributionDatabase) CanMonitor(ctx context.Context) (bool, error) {
	what := fmt.Sprintf("check replication monitor access to %q", dd.Name)
	var access, sysadmin bool
	err := dd.srv.queryRowScan(ctx, `
SELECT CAST(ISNULL(HAS_DBACCESS(@p1), 0) AS bit), CAST(ISNULL(IS_SRVROLEMEMBER(N'sysadmin'), 0) AS bit)`,
		[]any{dd.Name}, &access, &sysadmin)
	if err != nil {
		return false, fmt.Errorf("gosmo: %s: %w", what, err)
	}
	if sysadmin {
		return true, nil
	}
	if !access {
		return false, nil
	}
	var member bool
	err = dd.database().queryRow(ctx, func(row *sql.Row) error { return row.Scan(&member) }, `
SELECT CAST(CASE WHEN IS_MEMBER(N'db_owner') = 1 OR IS_MEMBER(N'replmonitor') = 1 THEN 1 ELSE 0 END AS bit)`)
	if err != nil {
		return false, fmt.Errorf("gosmo: %s: %w", what, err)
	}
	return member, nil
}

// -- Publishers, publications, subscriptions -----------------------------------

// MonitorPublisher is one publisher a distributor here serves, as
// sp_replmonitorhelppublisher reports it.
type MonitorPublisher struct {
	Name           string
	DistributionDB string
	// Status is the worst status among the publisher's agents.
	Status           ReplAgentStatus
	Warning          MonitorWarning
	PublicationCount int
}

// MonitorPublishers returns every publisher served by a distribution database
// on this instance, in the procedure's order. An instance that is not a
// distributor has none (the procedure itself fails there, on a missing
// msdb table).
func (s *Server) MonitorPublishers(ctx context.Context) ([]MonitorPublisher, error) {
	const what = "read replication monitor publishers"
	if err := s.refuseAzureReplication(what); err != nil {
		return nil, err
	}
	var distributor bool
	err := s.queryRowScan(ctx, `
SELECT CAST(CASE WHEN OBJECT_ID(N'msdb.dbo.MSdistributiondbs', N'U') IS NOT NULL
                  AND EXISTS (SELECT 1 FROM sys.databases WHERE is_distributor = 1) THEN 1 ELSE 0 END AS bit)`,
		nil, &distributor)
	if err != nil {
		return nil, fmt.Errorf("gosmo: %s: %w", what, err)
	}
	if !distributor {
		return nil, nil
	}
	rows, err := s.query(ctx, `EXEC sys.sp_replmonitorhelppublisher`)
	return scanNamed(rows, err, what, func(r *namedRow) MonitorPublisher {
		return MonitorPublisher{
			Name:             r.str("publisher"),
			DistributionDB:   r.str("distribution_db"),
			Status:           ReplAgentStatus(r.intv("status")),
			Warning:          MonitorWarning(r.intv("warning")),
			PublicationCount: r.intv("publicationcount"),
		}
	})
}

// MonitorPublication is one publication as sp_replmonitorhelppublication
// reports it. Latencies are in seconds, nil where the publication has none
// measured (merge, or no tracer token or command delivered yet).
type MonitorPublication struct {
	Publisher     string
	PublisherDB   string
	Publication   string
	PublicationID int
	Type          PublicationType
	// Status is the worst status among the publication's agents.
	Status  ReplAgentStatus
	Warning MonitorWarning

	WorstLatency   *int
	BestLatency    *int
	AverageLatency *int
	// LastDistSync is when a Distribution or Merge Agent last ran for it.
	LastDistSync time.Time
	// Retention is the subscription expiration, in RetentionUnit.
	Retention     int
	RetentionUnit string

	// The thresholds the warnings are computed from: latency in seconds,
	// expiration as a percentage of the retention period, agent-not-running
	// in hours. Nil when not set or not applicable to the type.
	LatencyThreshold         *int
	ExpirationThreshold      *int
	AgentNotRunningThreshold *int

	SubscriptionCount     int
	RunningDistAgentCount int

	// The publication's own agents, by name — ReplicationAgent.Name.
	SnapshotAgent    string
	LogReaderAgent   string
	QueueReaderAgent string

	// Merge run speed, rows per second, across its subscriptions.
	WorstRunSpeedPerf   *int
	BestRunSpeedPerf    *int
	AverageRunSpeedPerf *int
}

// MonitorPublications returns every publication dd distributes, of every
// publisher, in the procedure's order.
func (dd *DistributionDatabase) MonitorPublications(ctx context.Context) ([]MonitorPublication, error) {
	what := fmt.Sprintf("read replication monitor publications in %q", dd.Name)
	rows, err := dd.srv.query(ctx, "EXEC "+quoteIdent(dd.Name)+".dbo.sp_replmonitorhelppublication")
	return scanNamed(rows, err, what, func(r *namedRow) MonitorPublication {
		p := MonitorPublication{
			Publisher:                r.str("publisher"),
			PublisherDB:              r.str("publisher_db"),
			Publication:              r.str("publication"),
			PublicationID:            r.intv("publication_id"),
			Type:                     monitorPublicationType(r.intv("publication_type")),
			Status:                   ReplAgentStatus(r.intv("status")),
			Warning:                  MonitorWarning(r.intv("warning")),
			WorstLatency:             r.intp("worst_latency"),
			BestLatency:              r.intp("best_latency"),
			AverageLatency:           r.intp("average_latency"),
			LastDistSync:             r.timev("last_distsync"),
			Retention:                r.intv("retention"),
			LatencyThreshold:         r.intp("latencythreshold"),
			ExpirationThreshold:      r.intp("expirationthreshold"),
			AgentNotRunningThreshold: r.intp("agentnotrunningthreshold"),
			SubscriptionCount:        r.intv("subscriptioncount"),
			RunningDistAgentCount:    r.intv("runningdistagentcount"),
			SnapshotAgent:            r.str("snapshot_agentname"),
			LogReaderAgent:           r.str("logreader_agentname"),
			QueueReaderAgent:         r.str("qreader_agentname"),
			WorstRunSpeedPerf:        r.intp("worst_runspeedPerf"),
			BestRunSpeedPerf:         r.intp("best_runspeedPerf"),
			AverageRunSpeedPerf:      r.intp("average_runspeedPerf"),
		}
		// retention_period_unit is merge's (and NULL until set, meaning
		// days); a transactional retention is always hours.
		p.RetentionUnit = "hour"
		if p.Type == PublicationMerge {
			p.RetentionUnit = mergeRetentionUnits[r.intv("retention_period_unit")]
		}
		return p
	})
}

// MonitorSubscription is one subscription as sp_replmonitorhelpsubscription
// reports it, with the agent that serves it.
type MonitorSubscription struct {
	Status  ReplAgentStatus
	Warning MonitorWarning

	Subscriber      string
	SubscriberDB    string
	Publisher       string
	PublisherDB     string
	Publication     string
	PublicationType PublicationType
	Type            SubscriptionType

	// Latency is the last measured latency in seconds (transactional),
	// nil where none is.
	Latency          *int
	LatencyThreshold *int
	// AgentNotRunning is how many hours the agent has not run, nil while
	// it does.
	AgentNotRunning          *int
	AgentNotRunningThreshold *int
	// TimeToExpiration is hours until the subscription expires unless it
	// synchronizes; ExpirationThreshold is the warning's percentage.
	TimeToExpiration    *int
	ExpirationThreshold *int
	LastDistSync        time.Time

	// The serving agent, by name — ReplicationAgent.Name. Exactly one is
	// set, by PublicationType.
	DistributionAgent string
	MergeAgent        string
	// LogReaderAgent is the publication database's Log Reader
	// (transactional).
	LogReaderAgent string

	// Merge only (zero otherwise).
	MergeFriendlyName  string
	MergeAgentLocation string
	// MergeConnectionType is 1 LAN, 2 dial-up, 3 web synchronization.
	MergeConnectionType *int
	// MergePerformance is the last session's run speed as a percentage of
	// the subscription's historical average.
	MergePerformance *int
	MergeRunSpeed    *float64
	// MergeRunDuration is the last session's length in seconds.
	MergeRunDuration *int

	// MonitorRanking orders subscriptions worst first, the way Replication
	// Monitor sorts them.
	MonitorRanking int
}

// MonitorSubscriptions returns every subscription to a publication dd
// distributes, of every type: snapshot, transactional and merge, worst first
// within each.
func (dd *DistributionDatabase) MonitorSubscriptions(ctx context.Context) ([]MonitorSubscription, error) {
	what := fmt.Sprintf("read replication monitor subscriptions in %q", dd.Name)
	var out []MonitorSubscription
	// @publication_type is required, so one call per type.
	for _, pt := range []int{0, 1, 2} {
		rows, err := dd.srv.query(ctx, "EXEC "+quoteIdent(dd.Name)+".dbo.sp_replmonitorhelpsubscription @publication_type = @p1", pt)
		subs, err := scanNamed(rows, err, what, func(r *namedRow) MonitorSubscription {
			return MonitorSubscription{
				Status:                   ReplAgentStatus(r.intv("status")),
				Warning:                  MonitorWarning(r.intv("warning")),
				Subscriber:               r.str("subscriber"),
				SubscriberDB:             r.str("subscriber_db"),
				Publisher:                r.str("publisher"),
				PublisherDB:              r.str("publisher_db"),
				Publication:              r.str("publication"),
				PublicationType:          monitorPublicationType(r.intv("publication_type")),
				Type:                     SubscriptionType(r.intv("subtype")),
				Latency:                  r.intp("latency"),
				LatencyThreshold:         r.intp("latencythreshold"),
				AgentNotRunning:          r.intp("agentnotrunning"),
				AgentNotRunningThreshold: r.intp("agentnotrunningthreshold"),
				TimeToExpiration:         r.intp("timetoexpiration"),
				ExpirationThreshold:      r.intp("expirationthreshold"),
				LastDistSync:             r.timev("last_distsync"),
				DistributionAgent:        r.str("distribution_agentname"),
				MergeAgent:               r.str("mergeagentname"),
				LogReaderAgent:           r.str("logreaderagentname"),
				MergeFriendlyName:        r.str("mergesubscriptionfriendlyname"),
				MergeAgentLocation:       r.str("mergeagentlocation"),
				MergeConnectionType:      r.intp("mergeconnectiontype"),
				MergePerformance:         r.intp("mergePerformance"),
				MergeRunSpeed:            r.floatp("mergerunspeed"),
				MergeRunDuration:         r.intp("mergerunduration"),
				MonitorRanking:           r.intv("monitorranking"),
			}
		})
		if err != nil {
			return nil, err
		}
		out = append(out, subs...)
	}
	return out, nil
}

// -- Agents --------------------------------------------------------------------

// ReplAgentKind is which replication agent an agent is.
type ReplAgentKind int

const (
	ReplSnapshotAgent ReplAgentKind = iota + 1
	ReplLogReaderAgent
	ReplDistributionAgent
	ReplMergeAgent
)

func (k ReplAgentKind) String() string {
	switch k {
	case ReplSnapshotAgent:
		return "Snapshot Agent"
	case ReplLogReaderAgent:
		return "Log Reader Agent"
	case ReplDistributionAgent:
		return "Distribution Agent"
	case ReplMergeAgent:
		return "Merge Agent"
	}
	return fmt.Sprintf("ReplAgentKind(%d)", int(k))
}

// replAgentProcs names each kind's sp_MSenum_* family: the agent list, its
// sessions (_s) and one session's actions (_sd). These are what SSMS's
// Replication Monitor reads; unlike the tables beneath them they are open to
// replmonitor, which has no SELECT on MS*_history.
var replAgentProcs = []struct {
	kind ReplAgentKind
	proc string
}{
	{ReplSnapshotAgent, "sp_MSenum_snapshot"},
	{ReplLogReaderAgent, "sp_MSenum_logreader"},
	{ReplDistributionAgent, "sp_MSenum_distribution"},
	{ReplMergeAgent, "sp_MSenum_merge"},
}

func replAgentProc(k ReplAgentKind) string {
	for _, p := range replAgentProcs {
		if p.kind == k {
			return p.proc
		}
	}
	return ""
}

// replTimeLayout is sys.fn_replformatdatetime's text, which the sp_MSenum_*
// procedures return their times as.
const replTimeLayout = "20060102 15:04:05.000"

// replTime reads a sp_MSenum_* time column: fn_replformatdatetime text, or a
// datetime where a version returns one. Zoneless, so UTC (see CLAUDE.md).
func (r *namedRow) replTime(name string) time.Time {
	switch v := r.val(name).(type) {
	case time.Time:
		return v
	case string:
		return parseReplTime(v)
	case []byte:
		return parseReplTime(string(v))
	}
	return time.Time{}
}

func parseReplTime(s string) time.Time {
	t, err := time.ParseInLocation(replTimeLayout, strings.TrimSpace(s), time.UTC)
	if err != nil {
		return time.Time{}
	}
	return t
}

// float64v is the named column as a float64, 0 when NULL or absent.
func (r *namedRow) float64v(name string) float64 {
	if f := r.floatp(name); f != nil {
		return *f
	}
	return 0
}

// ReplicationAgent is one agent registered at the distributor, with its
// last run as sp_MSenum_snapshot, _logreader, _distribution and _merge
// report it.
type ReplicationAgent struct {
	dd *DistributionDatabase

	Kind ReplAgentKind
	ID   int
	// Name is the agent's name, which the monitor procedures report too
	// (MonitorPublication.SnapshotAgent, MonitorSubscription.MergeAgent, …).
	Name        string
	Publisher   string
	PublisherDB string
	// Publication is empty for a Log Reader, which serves every
	// publication of its database.
	Publication string
	// Subscriber and SubscriberDB are a Distribution or Merge Agent's.
	Subscriber   string
	SubscriberDB string

	// The last run; Status is ReplNeverRun and the rest zero when there is
	// none. Duration is seconds from LastStart to LastTime, LastAction the
	// last message logged.
	Status                ReplAgentStatus
	LastStart             time.Time
	LastTime              time.Time
	Duration              int
	LastAction            string
	DeliveredTransactions int64
	DeliveredCommands     int64
	// DeliveryRate is commands per second (merge: rows per second).
	DeliveryRate float64
	// LatencyMs is the delivery latency the Log Reader or Distribution
	// Agent last reported, in milliseconds; nil for the others.
	LatencyMs *int
	// Merge only: rows the last run sent each way, and conflicts.
	Downloaded int64
	Uploaded   int64
	Conflicts  int64
	// ErrorID keys the run's error (DistributionDatabase.Errors); 0 for
	// none.
	ErrorID int
}

// DistributionDatabase returns the distribution database the agent is
// registered in.
func (a *ReplicationAgent) DistributionDatabase() *DistributionDatabase { return a.dd }

// Agents returns every agent registered in dd — Snapshot, Log Reader,
// Distribution and Merge, in that order, each in the procedure's order —
// with its last run. Queue Reader agents are not read.
func (dd *DistributionDatabase) Agents(ctx context.Context) ([]*ReplicationAgent, error) {
	what := fmt.Sprintf("read replication agents in %q", dd.Name)
	var out []*ReplicationAgent
	for _, p := range replAgentProcs {
		rows, err := dd.srv.query(ctx, "EXEC "+quoteIdent(dd.Name)+".dbo."+p.proc)
		agents, err := scanNamed(rows, err, what, func(r *namedRow) *ReplicationAgent {
			a := &ReplicationAgent{
				dd:                    dd,
				Kind:                  p.kind,
				ID:                    r.intv("agent_id"),
				Name:                  r.str("name"),
				Publisher:             r.str("publisher"),
				PublisherDB:           r.str("publisher_db"),
				Publication:           r.str("publication"),
				Subscriber:            r.str("subscriber"),
				SubscriberDB:          r.str("subscriber_db"),
				Status:                ReplAgentStatus(r.intv("status")),
				LastStart:             r.replTime("start_time"),
				LastTime:              r.replTime("time"),
				Duration:              r.intv("duration"),
				LastAction:            r.str("comments"),
				DeliveredTransactions: r.int64v("delivered_transactions"),
				DeliveredCommands:     r.int64v("delivered_commands"),
				DeliveryRate:          r.float64v("delivery_rate"),
				ErrorID:               r.intv("error_id"),
			}
			if p.kind == ReplLogReaderAgent || p.kind == ReplDistributionAgent {
				a.LatencyMs = new(r.intv("delivery_latency"))
			}
			if p.kind == ReplMergeAgent {
				a.Downloaded = r.int64v("download_inserts") + r.int64v("download_updates") + r.int64v("download_deletes")
				a.Uploaded = r.int64v("upload_inserts") + r.int64v("upload_updates") + r.int64v("upload_deletes")
				// sp_MSenum_merge spells both "conficts".
				a.Conflicts = r.int64v("publisher_conficts") + r.int64v("subscriber_conficts")
				// For a merge subscription registered by subscriber name,
				// sp_MSenum_merge appends "-<agent id>" to the database (it
				// takes it for anonymous); the database is the part before.
				a.SubscriberDB = strings.TrimSuffix(a.SubscriberDB, "-"+strconv.Itoa(a.ID))
			}
			return a
		})
		if err != nil {
			return nil, err
		}
		out = append(out, agents...)
	}
	return out, nil
}

// ReplAgentSession is one run of an agent: from its start to its last
// logged action, with that action's figures.
type ReplAgentSession struct {
	Status ReplAgentStatus
	Start  time.Time
	// End is the time of the session's last action (still moving while the
	// session runs).
	End time.Time
	// Duration is seconds from Start to End.
	Duration   int
	LastAction string
	// Actions is how many messages the session logged.
	Actions               int
	DeliveredTransactions int64
	DeliveredCommands     int64
	DeliveryRate          float64
	LatencyMs             *int
	Downloaded            int64
	Uploaded              int64
	Conflicts             int64
	ErrorID               int
}

// Sessions returns the agent's sessions, newest first: those of the last
// hours hours, or every one history retention keeps when hours is 0. With
// errorsOnly, only sessions that failed.
func (a *ReplicationAgent) Sessions(ctx context.Context, hours int, errorsOnly bool) ([]ReplAgentSession, error) {
	what := fmt.Sprintf("read sessions of replication agent %q", a.Name)
	if hours < 0 {
		// The procedures' negative @hours is an unordered TOP 100.
		hours = 0
	}
	sessionType := 1
	if errorsOnly {
		sessionType = 2
	}
	rows, err := a.dd.srv.query(ctx, "EXEC "+quoteIdent(a.dd.Name)+".dbo."+replAgentProc(a.Kind)+
		"_s @name = @p1, @hours = @p2, @session_type = @p3", a.Name, hours, sessionType)
	out, err := scanNamed(rows, err, what, func(r *namedRow) ReplAgentSession {
		s := ReplAgentSession{
			Status:                ReplAgentStatus(r.intv("runstatus")),
			Start:                 r.replTime("start_time"),
			End:                   r.replTime("time"),
			Duration:              r.intv("duration"),
			LastAction:            r.str("comments"),
			Actions:               r.intv("action_count"),
			DeliveredTransactions: r.int64v("delivered_transactions"),
			DeliveredCommands:     r.int64v("delivered_commands"),
			DeliveryRate:          r.float64v("delivery_rate"),
			LatencyMs:             r.intp("delivery_latency"),
			ErrorID:               r.intv("error_id"),
		}
		s.Downloaded = r.int64v("download_inserts") + r.int64v("download_updates") + r.int64v("download_deletes")
		s.Uploaded = r.int64v("upload_inserts") + r.int64v("upload_updates") + r.int64v("upload_deletes")
		s.Conflicts = r.int64v("download_conflicts") + r.int64v("upload_conflicts")
		return s
	})
	if err != nil {
		return nil, err
	}
	// The procedures do not order their result.
	slices.SortStableFunc(out, func(x, y ReplAgentSession) int { return y.Start.Compare(x.Start) })
	return out, nil
}

// ReplAgentAction is one message an agent logged during a session.
type ReplAgentAction struct {
	Status  ReplAgentStatus
	Time    time.Time
	Message string
	// Duration is seconds since the session started.
	Duration              int
	DeliveredTransactions int64
	DeliveredCommands     int64
	DeliveryRate          float64
	LatencyMs             *int
	ErrorID               int
}

// SessionActions returns the actions of one of the agent's sessions, newest
// first, as Sessions returned it. A merge session's actions all carry the
// session's figures: merge history keeps only its messages per action.
func (a *ReplicationAgent) SessionActions(ctx context.Context, s ReplAgentSession) ([]ReplAgentAction, error) {
	what := fmt.Sprintf("read a session of replication agent %q", a.Name)
	// The merge procedure finds the last session ending by @time, the others
	// the session starting at it.
	at := s.Start
	if a.Kind == ReplMergeAgent {
		at = s.End
	}
	rows, err := a.dd.srv.query(ctx, "EXEC "+quoteIdent(a.dd.Name)+".dbo."+replAgentProc(a.Kind)+
		"_sd @name = @p1, @time = @p2", a.Name, at.Format(replTimeLayout))
	return scanNamed(rows, err, what, func(r *namedRow) ReplAgentAction {
		return ReplAgentAction{
			Status:                ReplAgentStatus(r.intv("runstatus")),
			Time:                  r.replTime("time"),
			Message:               r.str("comments"),
			Duration:              r.intv("duration"),
			DeliveredTransactions: r.int64v("delivered_transactions"),
			DeliveredCommands:     r.int64v("delivered_commands"),
			DeliveryRate:          r.float64v("delivery_rate"),
			LatencyMs:             r.intp("delivery_latency"),
			ErrorID:               r.intv("error_id"),
		}
	})
}

// -- Errors --------------------------------------------------------------------

// ReplicationError is one row of an agent error, from dbo.MSrepl_errors. One
// failure usually leaves several, from the agent's message down to the
// server's or provider's own.
type ReplicationError struct {
	Time time.Time
	// SourceName names what raised it: the server, data source or agent.
	SourceName string
	// Code is the error number, as text (a provider's code need not be
	// numeric).
	Code string
	Text string
	// XactSeqno and CommandID locate the failing command in the
	// distribution database (transactional): XactSeqno as 0x-hex, empty
	// when there is none.
	XactSeqno string
	CommandID int
}

// Errors returns the rows of dd's error id, oldest first — the detail behind
// a ReplicationAgent's, ReplAgentSession's or ReplAgentAction's ErrorID. An
// id history retention has cleaned up returns none.
func (dd *DistributionDatabase) Errors(ctx context.Context, id int) ([]ReplicationError, error) {
	what := fmt.Sprintf("read replication error %d in %q", id, dd.Name)
	// sp_MSget_repl_error: source_type_id, source_name, error_code,
	// error_text, the time (unnamed), error_type_id, has_xact_seqno,
	// xact_seqno, command_id.
	rows, err := dd.srv.query(ctx, "EXEC "+quoteIdent(dd.Name)+".dbo.sp_MSget_repl_error @id = @p1", id)
	return scanRows(rows, err, what, func(scan func(...any) error) (ReplicationError, error) {
		var e ReplicationError
		var sourceType, errType, cmd sql.NullInt64
		var source, code, text, at sql.NullString
		var hasSeqno sql.NullBool
		var seqno []byte
		if err := scan(&sourceType, &source, &code, &text, &at, &errType, &hasSeqno, &seqno, &cmd); err != nil {
			return e, err
		}
		e.Time = parseReplTime(at.String)
		e.SourceName = source.String
		e.Code = strings.TrimSpace(code.String)
		e.Text = text.String
		if hasSeqno.Bool && len(seqno) > 0 {
			e.XactSeqno = fmt.Sprintf("0x%X", seqno)
		}
		e.CommandID = int(cmd.Int64)
		return e, nil
	})
}

// -- namedRow nullable readers ---------------------------------------------------

// intp is the named column as an int, nil when it is NULL or absent.
func (r *namedRow) intp(name string) *int {
	if r.val(name) == nil {
		return nil
	}
	return new(r.intv(name))
}

// floatp is the named column as a float64, nil when it is NULL or absent.
// DECIMAL arrives as text.
func (r *namedRow) floatp(name string) *float64 {
	switch v := r.val(name).(type) {
	case float64:
		return &v
	case float32:
		return new(float64(v))
	case int64:
		return new(float64(v))
	case []byte:
		if f, err := strconv.ParseFloat(string(v), 64); err == nil {
			return &f
		}
	case string:
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return &f
		}
	}
	return nil
}
