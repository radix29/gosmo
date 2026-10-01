package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// extended_events.go covers Extended Events sessions — SSMS's Management >
// Extended Events > Sessions folder — and the catalog of what a session can
// collect (sys.dm_xe_packages, _objects, _object_columns, _map_values). The
// DDL and the scripter are in extended_events_script.go, the target readers
// and the event-XML decoder in extended_events_read.go.
//
// # Two scopes
//
// SQL Server's sessions are server-scoped (sys.server_event_sessions, ON
// SERVER). Azure SQL Database has database-scoped ones instead
// (sys.database_event_sessions, ON DATABASE), with the same shape under
// different view names. An EventSession carries its scope — Database() is nil
// for a server-scoped one — and every read and write picks its views and its
// ON clause from it (xeScope). Server.EventSessions lists the server's,
// Database.EventSessions a database's.
//
// # Permissions
//
// The session catalog views need VIEW SERVER STATE on 2016–2019 and VIEW
// SERVER PERFORMANCE STATE on 2022 and later — the same right as the
// sys.dm_xe_sessions DMV the list joins to for running state, which is why the
// join costs no one the folder (contrast ServerAudit.Status). Writes need
// ALTER ANY EVENT SESSION, or on 2022+ the granular name for the verb; all of
// them are in ProbedServerPermissions.

// Event retention modes, as event_retention_mode_desc records them. Unlike the
// audit enums, the description is the keyword.
const (
	XERetentionAllowSingleEventLoss   = "ALLOW_SINGLE_EVENT_LOSS"
	XERetentionAllowMultipleEventLoss = "ALLOW_MULTIPLE_EVENT_LOSS"
	XERetentionNoEventLoss            = "NO_EVENT_LOSS"
)

// Memory partition modes, as memory_partition_mode_desc records them; again
// the description is the keyword.
const (
	XEPartitionNone    = "NONE"
	XEPartitionPerNode = "PER_NODE"
	XEPartitionPerCPU  = "PER_CPU"
)

// EventSession mirrors a row of sys.server_event_sessions (or
// sys.database_event_sessions) with its events and targets.
type EventSession struct {
	server *Server
	db     *Database // nil for a server-scoped session

	ID           int
	Name         string
	StartupState bool // STARTUP_STATE = ON: the session starts with the server
	IsRunning    bool // has a row in sys.dm_xe_sessions

	// DroppedEvents is sys.dm_xe_sessions.dropped_event_count: the events a
	// running session could not buffer since it started (a full buffer under
	// ALLOW_*_EVENT_LOSS). 0 for a stopped session.
	DroppedEvents int64

	RetentionMode       string        // an XERetention* constant
	MaxDispatchLatency  time.Duration // 0 is INFINITE
	MaxMemory           int           // KB
	MaxEventSize        int           // KB; 0 means no separate large-event buffer
	MemoryPartitionMode string        // an XEPartition* constant
	TrackCausality      bool

	// MaxDuration is SQL Server 2025's time-bound session limit; 0 is
	// UNLIMITED, and always 0 on an older instance and on a database-scoped
	// session (see eventSessionSelect).
	MaxDuration time.Duration

	Events  []SessionEvent
	Targets []SessionTarget
}

// Server returns the server the session belongs to.
func (es *EventSession) Server() *Server { return es.server }

// Database returns the database a database-scoped session belongs to, and nil
// for a server-scoped one.
func (es *EventSession) Database() *Database { return es.db }

// SessionEvent is one event a session collects.
type SessionEvent struct {
	Package string
	Name    string

	// Predicate is the WHERE filter without the keyword, as the catalog stores
	// it — parenthesised, with the column and source references bracketed.
	// Empty when the event has none.
	Predicate string

	// Actions are the global fields collected with the event, each written
	// "package.name" as the DDL spells it (sqlserver.sql_text).
	Actions []string

	// Fields are the event's customizable fields (collect_*) set on it —
	// only those, not the ones left at their default.
	Fields []SessionField
}

// QualifiedName returns "package.name", the form ADD EVENT takes.
func (e SessionEvent) QualifiedName() string { return e.Package + "." + e.Name }

// SessionTarget is one target a session writes to.
type SessionTarget struct {
	Package string
	Name    string

	// Fields are the target's parameters set on it (filename,
	// max_file_size, …); a parameter left at its default is not in the
	// catalog and so not here.
	Fields []SessionField
}

// QualifiedName returns "package.name", the form ADD TARGET takes.
func (t SessionTarget) QualifiedName() string { return t.Package + "." + t.Name }

// Field returns the value of the named parameter and whether it is set.
func (t SessionTarget) Field(name string) (string, bool) {
	for _, f := range t.Fields {
		if strings.EqualFold(f.Name, name) {
			return f.Value, true
		}
	}
	return "", false
}

// SessionField is one SET name = value on an event or a target.
type SessionField struct {
	Name  string
	Value string

	// IsString marks a value the DDL quotes (N'…') rather than writes as a
	// number in parentheses — the base type of the catalog's sql_variant.
	IsString bool
}

// Target names package0 provides that the readers in extended_events_read.go
// understand.
const (
	XETargetEventFile  = "event_file"
	XETargetRingBuffer = "ring_buffer"
)

// Target returns the session's target of that name (package-agnostic, since
// every stock target lives in package0) and whether it has one.
func (es *EventSession) Target(name string) (SessionTarget, bool) {
	for _, t := range es.Targets {
		if strings.EqualFold(t.Name, name) {
			return t, true
		}
	}
	return SessionTarget{}, false
}

// -- Scope ----------------------------------------------------------------------

// xeScope names the views and the ON clause of one session scope.
type xeScope struct {
	sessions, events, actions, fields, targets string // catalog views
	dmSessions, dmTargets                      string // runtime DMVs
	on                                         string // ON SERVER / ON DATABASE
}

var (
	xeServerScope = xeScope{
		sessions: "sys.server_event_sessions", events: "sys.server_event_session_events",
		actions: "sys.server_event_session_actions", fields: "sys.server_event_session_fields",
		targets: "sys.server_event_session_targets", dmSessions: "sys.dm_xe_sessions",
		dmTargets: "sys.dm_xe_session_targets", on: "ON SERVER",
	}
	xeDatabaseScope = xeScope{
		sessions: "sys.database_event_sessions", events: "sys.database_event_session_events",
		actions: "sys.database_event_session_actions", fields: "sys.database_event_session_fields",
		targets: "sys.database_event_session_targets", dmSessions: "sys.dm_xe_database_sessions",
		dmTargets: "sys.dm_xe_database_session_targets", on: "ON DATABASE",
	}
)

func (es *EventSession) scope() xeScope {
	if es.db != nil {
		return xeDatabaseScope
	}
	return xeServerScope
}

// scopeSupported refuses a database-scoped session off Azure. Its catalog
// views and its ON DATABASE DDL exist only on Azure SQL Database and Managed
// Instance; on a box product (2016 through 2025 alike) the read fails with
// Msg 208 and the DDL with a syntax error, so every database-scoped read and
// write — scripted ones too, which would not replay — is refused up front, as
// Server.InstanceResourceGovernance is.
func (es *EventSession) scopeSupported() error {
	if es.db != nil && !es.server.info.IsAzure() {
		return unsupportedVersionf("database-scoped event sessions require Azure SQL Database or Managed Instance")
	}
	return nil
}

// query runs a read in the session's scope: on the server, or USE'd into its
// database.
func (es *EventSession) query(ctx context.Context, q string, args ...any) (rowSource, error) {
	if es.db != nil {
		if err := es.scopeSupported(); err != nil {
			return nil, err
		}
		return es.db.query(ctx, q, args...)
	}
	return es.server.query(ctx, q, args...)
}

// exec runs a write in the session's scope.
func (es *EventSession) exec(ctx context.Context, stmt string) error {
	if es.db != nil {
		if err := es.scopeSupported(); err != nil {
			return err
		}
		_, err := es.db.exec(ctx, stmt)
		return err
	}
	return es.server.exec(ctx, stmt)
}

// -- Lookups --------------------------------------------------------------------

// EventSessions returns every server-scoped event session, with its events
// and targets, ordered by name.
func (s *Server) EventSessions(ctx context.Context) ([]*EventSession, error) {
	return readEventSessions(ctx, s.EventSessionRef(""), "")
}

// EventSessionByName returns one server-scoped event session with every field
// populated, or a not-found error (errors.Is ErrNotFound).
func (s *Server) EventSessionByName(ctx context.Context, name string) (*EventSession, error) {
	return eventSessionByName(ctx, s.EventSessionRef(name))
}

// EventSessionRef returns a lightweight handle for a server-scoped event
// session by name, without querying the catalog — the session-side
// counterpart of Server.DatabaseRef. Every cached field stays at its zero
// value (IsRunning is false whatever the server says); EventSessionByName is
// what populates them. Every write addresses the session by name, so the
// handle is enough to start, stop, alter or drop one.
func (s *Server) EventSessionRef(name string) *EventSession {
	return &EventSession{server: s, Name: name}
}

// EventSessions returns the database's database-scoped event sessions — Azure
// SQL Database's kind. Off Azure the views do not exist, and this (like every
// database-scoped read and write) refuses with an ErrUnsupportedVersion error.
func (d *Database) EventSessions(ctx context.Context) ([]*EventSession, error) {
	return readEventSessions(ctx, d.EventSessionRef(""), "")
}

// EventSessionByName returns one database-scoped event session, or a
// not-found error (errors.Is ErrNotFound).
func (d *Database) EventSessionByName(ctx context.Context, name string) (*EventSession, error) {
	return eventSessionByName(ctx, d.EventSessionRef(name))
}

// EventSessionRef returns a lookup-free handle for a database-scoped event
// session, as Server.EventSessionRef does for a server-scoped one.
func (d *Database) EventSessionRef(name string) *EventSession {
	return &EventSession{server: d.server, db: d, Name: name}
}

func eventSessionByName(ctx context.Context, ref *EventSession) (*EventSession, error) {
	list, err := readEventSessions(ctx, ref, ref.Name)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, notFoundf("gosmo: event session %q not found", ref.Name)
	}
	return list[0], nil
}

// eventSessionSelect reads the session rows of one scope. max_duration is
// SQL Server 2025's; a database-scoped read never names it, because Azure SQL
// Database's catalog could not be checked for it and naming an absent column
// fails the whole read.
func eventSessionSelect(sc xeScope, major int) string {
	maxDuration := "CAST(0 AS bigint)"
	if sc == xeServerScope {
		maxDuration = colSince(major, SQLServer2025, "s.max_duration", "CAST(0 AS bigint)")
	}
	return fmt.Sprintf(`
SELECT s.event_session_id, s.name, s.startup_state,
       CAST(CASE WHEN r.name IS NULL THEN 0 ELSE 1 END AS bit),
       s.event_retention_mode_desc, s.max_dispatch_latency, s.max_memory,
       s.max_event_size, s.memory_partition_mode_desc, s.track_causality,
       %s, ISNULL(r.dropped_event_count, 0)
FROM   %s s
LEFT   JOIN %s r ON r.name = s.name`, maxDuration, sc.sessions, sc.dmSessions)
}

// readEventSessions reads the sessions of ref's scope — all of them, or the one
// named — and their events and targets.
//
// Children come in four bulk reads keyed by session id and are grouped in Go,
// never read per session (CLAUDE.md § Conventions). Event and target ids share
// one counter within a session, so a row of the fields view belongs to
// whichever of the two has its object_id.
func readEventSessions(ctx context.Context, ref *EventSession, name string) ([]*EventSession, error) {
	sc := ref.scope()
	what := "list event sessions"
	where, childWhere := "", ""
	var args []any
	if name != "" {
		what = fmt.Sprintf("read event session %q", name)
		where = "\nWHERE  s.name = @p1"
		childWhere = "\nWHERE  s.name = @p1"
		args = []any{name}
	}

	rows, err := ref.query(ctx, eventSessionSelect(sc, ref.server.serverMajorVersion())+where+"\nORDER  BY s.name", args...)
	sessions, err := scanRows(rows, err, what, func(scan func(...any) error) (*EventSession, error) {
		es := &EventSession{server: ref.server, db: ref.db}
		var retention, partition sql.NullString
		var latency, maxMem, maxEvent sql.NullInt64
		var maxDuration int64
		if err := scan(&es.ID, &es.Name, &es.StartupState, &es.IsRunning, &retention, &latency,
			&maxMem, &maxEvent, &partition, &es.TrackCausality, &maxDuration, &es.DroppedEvents); err != nil {
			return nil, err
		}
		es.RetentionMode, es.MemoryPartitionMode = retention.String, partition.String
		es.MaxDispatchLatency = time.Duration(latency.Int64) * time.Millisecond
		es.MaxMemory, es.MaxEventSize = int(maxMem.Int64), int(maxEvent.Int64)
		es.MaxDuration = time.Duration(maxDuration) * time.Second
		return es, nil
	})
	if err != nil || len(sessions) == 0 {
		return sessions, err
	}
	byID := make(map[int]*EventSession, len(sessions))
	for _, es := range sessions {
		byID[es.ID] = es
	}

	// object key: session id and the event/target id within it.
	type objKey struct{ session, object int }
	events := map[objKey]*SessionEvent{}
	targets := map[objKey]*SessionTarget{}
	var eventOrder, targetOrder []objKey

	rows, err = ref.query(ctx, fmt.Sprintf(`
SELECT e.event_session_id, e.event_id, e.package, e.name, e.predicate
FROM   %s e
JOIN   %s s ON s.event_session_id = e.event_session_id%s
ORDER  BY e.event_session_id, e.event_id`, sc.events, sc.sessions, childWhere), args...)
	_, err = scanRows(rows, err, what, func(scan func(...any) error) (struct{}, error) {
		var k objKey
		var ev SessionEvent
		var pred sql.NullString
		if err := scan(&k.session, &k.object, &ev.Package, &ev.Name, &pred); err != nil {
			return struct{}{}, err
		}
		ev.Predicate = pred.String
		events[k] = &ev
		eventOrder = append(eventOrder, k)
		return struct{}{}, nil
	})
	if err != nil {
		return nil, err
	}

	rows, err = ref.query(ctx, fmt.Sprintf(`
SELECT a.event_session_id, a.event_id, a.package, a.name
FROM   %s a
JOIN   %s s ON s.event_session_id = a.event_session_id%s
ORDER  BY a.event_session_id, a.event_id, a.package, a.name`, sc.actions, sc.sessions, childWhere), args...)
	_, err = scanRows(rows, err, what, func(scan func(...any) error) (struct{}, error) {
		var k objKey
		var pkg, n string
		if err := scan(&k.session, &k.object, &pkg, &n); err != nil {
			return struct{}{}, err
		}
		if ev := events[k]; ev != nil {
			ev.Actions = append(ev.Actions, pkg+"."+n)
		}
		return struct{}{}, nil
	})
	if err != nil {
		return nil, err
	}

	rows, err = ref.query(ctx, fmt.Sprintf(`
SELECT t.event_session_id, t.target_id, t.package, t.name
FROM   %s t
JOIN   %s s ON s.event_session_id = t.event_session_id%s
ORDER  BY t.event_session_id, t.target_id`, sc.targets, sc.sessions, childWhere), args...)
	_, err = scanRows(rows, err, what, func(scan func(...any) error) (struct{}, error) {
		var k objKey
		var t SessionTarget
		if err := scan(&k.session, &k.object, &t.Package, &t.Name); err != nil {
			return struct{}{}, err
		}
		targets[k] = &t
		targetOrder = append(targetOrder, k)
		return struct{}{}, nil
	})
	if err != nil {
		return nil, err
	}

	// The value is a sql_variant; converting it here, with its base type
	// alongside, keeps the scan to plain strings and tells the scripter which
	// values to quote.
	rows, err = ref.query(ctx, fmt.Sprintf(`
SELECT f.event_session_id, f.object_id, f.name,
       CONVERT(nvarchar(4000), f.value),
       CONVERT(nvarchar(128), SQL_VARIANT_PROPERTY(f.value, 'BaseType'))
FROM   %s f
JOIN   %s s ON s.event_session_id = f.event_session_id%s
ORDER  BY f.event_session_id, f.object_id, f.name`, sc.fields, sc.sessions, childWhere), args...)
	_, err = scanRows(rows, err, what, func(scan func(...any) error) (struct{}, error) {
		var k objKey
		var f SessionField
		var value, base sql.NullString
		if err := scan(&k.session, &k.object, &f.Name, &value, &base); err != nil {
			return struct{}{}, err
		}
		f.Value = value.String
		f.IsString = strings.Contains(strings.ToLower(base.String), "char")
		if ev := events[k]; ev != nil {
			ev.Fields = append(ev.Fields, f)
		} else if t := targets[k]; t != nil {
			t.Fields = append(t.Fields, f)
		}
		return struct{}{}, nil
	})
	if err != nil {
		return nil, err
	}

	for _, k := range eventOrder {
		if es := byID[k.session]; es != nil {
			es.Events = append(es.Events, *events[k])
		}
	}
	for _, k := range targetOrder {
		if es := byID[k.session]; es != nil {
			es.Targets = append(es.Targets, *targets[k])
		}
	}
	return sessions, nil
}

// -- Runtime status -------------------------------------------------------------

// EventSessionStatus is a running session's state, from sys.dm_xe_sessions and
// sys.dm_xe_session_targets.
type EventSessionStatus struct {
	CreateTime     time.Time // when the session was started
	DroppedEvents  int64
	DroppedBuffers int64
	Targets        []EventTargetStatus
}

// EventTargetStatus is one target's runtime state. The counters parsed out of
// target_data are those the target reports: an event_file fills CurrentFile,
// a ring_buffer the event counts and Truncated, and every other target leaves
// them zero.
type EventTargetStatus struct {
	Package             string
	Name                string
	ExecutionCount      int64
	ExecutionDurationMs int64

	CurrentFile string // event_file: the .xel being written, with its full path

	TotalEventsProcessed int64 // ring_buffer: every event the target saw
	EventCount           int64 // ring_buffer: events currently in the buffer
	DroppedCount         int64 // ring_buffer: events it could not keep
	Truncated            bool  // ring_buffer: target_data was cut to fit ~4 MB
}

// Status returns the session's runtime state, or a not-found error (errors.Is
// ErrNotFound) when it is not running — a stopped session has no row in the
// DMVs.
//
// The target counters are extracted from target_data server-side, so a
// ring_buffer's up-to-4 MB payload is parsed there and never shipped for a
// status line.
func (es *EventSession) Status(ctx context.Context) (*EventSessionStatus, error) {
	sc := es.scope()
	what := fmt.Sprintf("read event session %q status", es.Name)
	rows, err := es.query(ctx, fmt.Sprintf(`
SELECT s.create_time, s.dropped_event_count, s.dropped_buffer_count,
       p.name, t.target_name, t.execution_count, t.execution_duration_ms,
       x.d.value('(/EventFileTarget/File/@name)[1]', 'nvarchar(4000)'),
       x.d.value('(/RingBufferTarget/@totalEventsProcessed)[1]', 'bigint'),
       x.d.value('(/RingBufferTarget/@eventCount)[1]', 'bigint'),
       x.d.value('(/RingBufferTarget/@droppedCount)[1]', 'bigint'),
       x.d.value('(/RingBufferTarget/@truncated)[1]', 'bit')
FROM   %s s
LEFT   JOIN %s t ON t.event_session_address = s.address
LEFT   JOIN sys.dm_xe_packages p ON p.guid = t.target_package_guid
OUTER  APPLY (SELECT TRY_CAST(t.target_data AS xml)) x(d)
WHERE  s.name = @p1
ORDER  BY t.target_name`, sc.dmSessions, sc.dmTargets), es.Name)
	var st *EventSessionStatus
	_, err = scanRows(rows, err, what, func(scan func(...any) error) (struct{}, error) {
		var created time.Time
		var dropEv, dropBuf int64
		var pkg, name, file sql.NullString
		var execCount, execMs, total, count, dropped sql.NullInt64
		var truncated sql.NullBool
		if err := scan(&created, &dropEv, &dropBuf, &pkg, &name, &execCount, &execMs,
			&file, &total, &count, &dropped, &truncated); err != nil {
			return struct{}{}, err
		}
		if st == nil {
			st = &EventSessionStatus{CreateTime: created, DroppedEvents: dropEv, DroppedBuffers: dropBuf}
		}
		if name.Valid {
			st.Targets = append(st.Targets, EventTargetStatus{
				Package: pkg.String, Name: name.String,
				ExecutionCount: execCount.Int64, ExecutionDurationMs: execMs.Int64,
				CurrentFile:          file.String,
				TotalEventsProcessed: total.Int64, EventCount: count.Int64,
				DroppedCount: dropped.Int64, Truncated: truncated.Bool,
			})
		}
		return struct{}{}, nil
	})
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, notFoundf("gosmo: event session %q is not running", es.Name)
	}
	return st, nil
}

// -- What can be collected --------------------------------------------------------

// XE object kinds, as sys.dm_xe_objects.object_type records them.
const (
	XEObjectEvent       = "event"
	XEObjectAction      = "action"
	XEObjectTarget      = "target"
	XEObjectPredSource  = "pred_source"
	XEObjectPredCompare = "pred_compare"
	XEObjectMap         = "map"
	XEObjectType        = "type"
)

// XEPackage is a row of sys.dm_xe_packages.
type XEPackage struct {
	Name         string
	GUID         string
	Description  string
	Capabilities string // capabilities_desc, e.g. "utility"
}

// XEObject is a row of sys.dm_xe_objects: an event, action, target,
// predicate source or comparator, map or type a session can name.
type XEObject struct {
	Package      string
	Name         string
	Kind         string // an XEObject* constant
	Description  string
	Capabilities string // capabilities_desc

	// Channel and Keyword are an event's ETW channel (Admin, Analytic, Debug,
	// Operational) and keyword (execution, deadlock_monitor, …) — SSMS's
	// event-library filters. Empty for every other kind.
	Channel string
	Keyword string

	// TypeName is the XE type of a pred_source's value (unicode_string,
	// uint32, a map name…) — what decides whether a predicate compares it
	// with N'…' or a number. Empty for the kinds that have none (events,
	// targets).
	TypeName string
}

// QualifiedName returns "package.name".
func (o XEObject) QualifiedName() string { return o.Package + "." + o.Name }

// XE column types, as sys.dm_xe_object_columns.column_type records them.
const (
	XEColumnData         = "data"         // an event field every firing carries
	XEColumnCustomizable = "customizable" // an event's collect_* or a target parameter, set with SET
	XEColumnReadonly     = "readonly"     // metadata (UUID, VERSION, CHANNEL, KEYWORD)
)

// XEObjectColumn is a row of sys.dm_xe_object_columns: an event's fields, or a
// target's parameters.
type XEObjectColumn struct {
	Name        string
	ColumnID    int
	TypeName    string // the XE type (uint64, unicode_string, a map name…)
	ColumnType  string // an XEColumn* constant
	Value       string // column_value: a customizable column's default
	Description string

	// Mandatory marks a target parameter that must be set (event_file's
	// filename); capabilities_desc "mandatory".
	Mandatory bool
}

// XEMapValue is one key/text pair of an XE map (sys.dm_xe_map_values).
type XEMapValue struct {
	Key   int
	Value string
}

// xeCatalogCache holds the XE object catalog per Server. The catalog is
// fixed for a build of the engine — it changes with a CU, which means a
// restart and so a reconnect — and an event picker reads all ~3000 events
// every time it opens.
type xeCatalogCache struct {
	mu      sync.Mutex
	entries map[string]any
}

// xeCached returns the cached value for key, or runs load and caches its
// result. A failed load is not cached. The cache hands out a clone, so a
// caller mutating its slice does not change the next caller's.
func xeCached[T any](s *Server, key string, load func() ([]T, error)) ([]T, error) {
	s.xe.mu.Lock()
	v, ok := s.xe.entries[key]
	s.xe.mu.Unlock()
	if ok {
		return slices.Clone(v.([]T)), nil
	}
	out, err := load()
	if err != nil {
		return nil, err
	}
	s.xe.mu.Lock()
	if s.xe.entries == nil {
		s.xe.entries = map[string]any{}
	}
	s.xe.entries[key] = out
	s.xe.mu.Unlock()
	return slices.Clone(out), nil
}

// XEPackages returns the non-private XE packages. The answer is cached on the
// Server — see xeCatalogCache.
func (s *Server) XEPackages(ctx context.Context) ([]XEPackage, error) {
	return xeCached(s, "packages", func() ([]XEPackage, error) {
		rows, err := s.query(ctx, `
SELECT name, CONVERT(varchar(36), guid), ISNULL(description, N''), ISNULL(capabilities_desc, N'')
FROM   sys.dm_xe_packages
WHERE  ISNULL(capabilities, 0) & 1 = 0
ORDER  BY name`)
		return scanRows(rows, err, "list XE packages", func(scan func(...any) error) (XEPackage, error) {
			var p XEPackage
			err := scan(&p.Name, &p.GUID, &p.Description, &p.Capabilities)
			return p, err
		})
	})
}

// XEObjects returns the non-private objects of one kind (an XEObject*
// constant), ordered by package and name — for events, with their channel and
// keyword. Private objects and those of private packages are left out, as
// SSMS leaves them out: the engine refuses to add them to a session. Cached
// per Server.
//
// Package names are not unique — three packages are called sqlserver — so the
// channel and keyword maps are joined by package GUID, never by name.
//
// The DMVs have no indexes, and the optimizer's nested loops over them took
// 7 s for the event list; OPTION (HASH JOIN) takes 0.4 s (major 17,
// 2026-09-29).
func (s *Server) XEObjects(ctx context.Context, kind string) ([]XEObject, error) {
	return xeCached(s, "objects:"+kind, func() ([]XEObject, error) {
		rows, err := s.query(ctx, `
SELECT p.name, o.name, o.object_type, ISNULL(o.description, N''),
       ISNULL(o.capabilities_desc, N''),
       ISNULL(chm.map_value, N''), ISNULL(kwm.map_value, N''), ISNULL(o.type_name, N'')
FROM   sys.dm_xe_objects o
JOIN   sys.dm_xe_packages p ON p.guid = o.package_guid
LEFT   JOIN sys.dm_xe_object_columns ch
       ON ch.object_name = o.name AND ch.object_package_guid = o.package_guid
      AND ch.name = N'CHANNEL' AND ch.column_type = N'readonly'
LEFT   JOIN sys.dm_xe_map_values chm
       ON chm.name = ch.type_name AND chm.object_package_guid = ch.type_package_guid
      AND chm.map_key = TRY_CONVERT(int, ch.column_value)
LEFT   JOIN sys.dm_xe_object_columns kw
       ON kw.object_name = o.name AND kw.object_package_guid = o.package_guid
      AND kw.name = N'KEYWORD' AND kw.column_type = N'readonly'
LEFT   JOIN sys.dm_xe_map_values kwm
       ON kwm.name = kw.type_name AND kwm.object_package_guid = kw.type_package_guid
      AND kwm.map_key = TRY_CONVERT(int, kw.column_value)
WHERE  o.object_type = @p1
  AND  ISNULL(o.capabilities, 0) & 1 = 0
  AND  ISNULL(p.capabilities, 0) & 1 = 0
ORDER  BY p.name, o.name
OPTION (HASH JOIN)`, kind)
		return scanRows(rows, err, fmt.Sprintf("list XE %s objects", kind), func(scan func(...any) error) (XEObject, error) {
			var o XEObject
			err := scan(&o.Package, &o.Name, &o.Kind, &o.Description, &o.Capabilities, &o.Channel, &o.Keyword, &o.TypeName)
			return o, err
		})
	})
}

// XEObjectColumns returns an object's columns — an event's fields (data and
// customizable) or a target's parameters with their defaults — ordered by
// column id. pkg and object name the object as XEObjects returns it; where
// several same-named packages define it, the first is used. Cached per Server.
func (s *Server) XEObjectColumns(ctx context.Context, pkg, object string) ([]XEObjectColumn, error) {
	return xeCached(s, "columns:"+pkg+"."+object, func() ([]XEObjectColumn, error) {
		rows, err := s.query(ctx, `
SELECT c.name, c.column_id, c.type_name, c.column_type, ISNULL(c.column_value, N''),
       ISNULL(c.description, N''),
       CAST(CASE WHEN c.capabilities_desc LIKE N'%mandatory%' THEN 1 ELSE 0 END AS bit)
FROM   sys.dm_xe_object_columns c
WHERE  c.object_name = @p2
  AND  c.object_package_guid = (
         SELECT TOP (1) p.guid
         FROM   sys.dm_xe_packages p
         JOIN   sys.dm_xe_objects o ON o.package_guid = p.guid AND o.name = @p2
         WHERE  p.name = @p1
         ORDER  BY o.object_type)
ORDER  BY c.column_id`, pkg, object)
		return scanRows(rows, err, fmt.Sprintf("list XE columns of %s.%s", pkg, object), func(scan func(...any) error) (XEObjectColumn, error) {
			var c XEObjectColumn
			err := scan(&c.Name, &c.ColumnID, &c.TypeName, &c.ColumnType, &c.Value, &c.Description, &c.Mandatory)
			return c, err
		})
	})
}

// XEMapValues returns one map's key/text pairs, ordered by key. Map names
// repeat across packages (a dozen define keyword_map), so pkg picks one; a
// map-typed event field's type_package is the package to pass. Cached per
// Server.
func (s *Server) XEMapValues(ctx context.Context, pkg, name string) ([]XEMapValue, error) {
	return xeCached(s, "map:"+pkg+"."+name, func() ([]XEMapValue, error) {
		rows, err := s.query(ctx, `
SELECT DISTINCT m.map_key, m.map_value
FROM   sys.dm_xe_map_values m
JOIN   sys.dm_xe_packages p ON p.guid = m.object_package_guid
WHERE  p.name = @p1 AND m.name = @p2
ORDER  BY m.map_key`, pkg, name)
		return scanRows(rows, err, fmt.Sprintf("list XE map %s.%s", pkg, name), func(scan func(...any) error) (XEMapValue, error) {
			var v XEMapValue
			err := scan(&v.Key, &v.Value)
			return v, err
		})
	})
}
