package gosmo

import (
	"slices"
	"strconv"
	"strings"
	"time"
)

// extended_events_templates.go holds ready-made event session definitions —
// SSMS's XEvent Profiler sessions — and builders for the two targets the
// readers in extended_events_read.go understand.

// The XEvent Profiler sessions' names as SSMS creates them.
const (
	XEProfilerStandardName = "QuickSessionStandard"
	XEProfilerTSQLName     = "QuickSessionTSQL"
)

// xeProfilerNotSystem is the predicate SSMS puts on the Profiler's statement
// events — user sessions only — in the form the catalog stores it.
const xeProfilerNotSystem = "([package0].[equal_boolean]([sqlserver].[is_system],(0)))"

// XEProfilerStandard returns SSMS's XEvent Profiler "Standard" session: logins
// and logouts, existing connections, attentions, and every completed batch and
// RPC with the statement that started it, from user sessions.
//
// Like SSMS's, it has no target — SSMS reads the session's live stream, which
// gosmo does not (see extended_events_read.go). A caller that will read what
// it collects adds an EventFileTarget or a RingBufferTarget, and will usually
// rename it too, rather than altering a copy SSMS itself may reuse.
func XEProfilerStandard() EventSessionSpec {
	conn := []string{"package0.event_sequence", "sqlserver.client_app_name", "sqlserver.client_pid",
		"sqlserver.nt_username", "sqlserver.server_principal_name", "sqlserver.session_id"}
	stmt := []string{"package0.event_sequence", "sqlserver.client_app_name", "sqlserver.client_pid",
		"sqlserver.database_id", "sqlserver.database_name", "sqlserver.nt_username", "sqlserver.query_hash",
		"sqlserver.server_principal_name", "sqlserver.session_id"}
	optionsText := []SessionField{{Name: "collect_options_text", Value: "1"}}
	return xeProfilerSpec(XEProfilerStandardName, []SessionEvent{
		{Package: "sqlserver", Name: "attention", Actions: stmt, Predicate: xeProfilerNotSystem},
		{Package: "sqlserver", Name: "existing_connection", Actions: conn, Fields: optionsText},
		{Package: "sqlserver", Name: "login", Actions: conn, Fields: optionsText},
		{Package: "sqlserver", Name: "logout", Actions: conn},
		{Package: "sqlserver", Name: "rpc_completed", Actions: stmt, Predicate: xeProfilerNotSystem},
		{Package: "sqlserver", Name: "sql_batch_completed", Actions: stmt, Predicate: xeProfilerNotSystem},
		{Package: "sqlserver", Name: "sql_batch_starting", Actions: stmt, Predicate: xeProfilerNotSystem},
	})
}

// XEProfilerTSQL returns SSMS's XEvent Profiler "TSQL" session: the text of
// every batch and RPC as it starts, with the logins and logouts around them,
// from user sessions. Target and naming as for XEProfilerStandard.
func XEProfilerTSQL() EventSessionSpec {
	acts := []string{"package0.event_sequence", "sqlserver.client_app_name", "sqlserver.client_pid",
		"sqlserver.database_id", "sqlserver.database_name", "sqlserver.nt_username", "sqlserver.session_id"}
	return xeProfilerSpec(XEProfilerTSQLName, []SessionEvent{
		{Package: "sqlserver", Name: "existing_connection", Actions: acts},
		{Package: "sqlserver", Name: "login", Actions: acts, Fields: []SessionField{{Name: "collect_options_text", Value: "1"}}},
		{Package: "sqlserver", Name: "logout", Actions: acts},
		{Package: "sqlserver", Name: "rpc_starting", Actions: acts, Predicate: xeProfilerNotSystem},
		{Package: "sqlserver", Name: "sql_batch_starting", Actions: acts, Predicate: xeProfilerNotSystem},
	})
}

// xeProfilerSpec wraps the Profiler's events in SSMS's session options.
// Each event gets its own copy of the action list, so a caller appending to
// one event's actions does not reach the others.
func xeProfilerSpec(name string, events []SessionEvent) EventSessionSpec {
	for i := range events {
		events[i].Actions = slices.Clone(events[i].Actions)
	}
	return EventSessionSpec{
		Name:                name,
		Events:              events,
		RetentionMode:       XERetentionAllowSingleEventLoss,
		MaxDispatchLatency:  5 * time.Second,
		MaxMemory:           8192,
		MemoryPartitionMode: XEPartitionPerCPU,
		TrackCausality:      true,
	}
}

// EventFileTarget returns a package0.event_file target writing filename —
// relative to the error-log directory when it has no directory, which is
// where SSMS's New Session puts one — rolling over at maxFileSizeMB and
// keeping maxRolloverFiles files. A size or count of 0 leaves that parameter
// at the server's default (1 GB, 5 files).
func EventFileTarget(filename string, maxFileSizeMB, maxRolloverFiles int) SessionTarget {
	t := SessionTarget{Package: "package0", Name: XETargetEventFile,
		Fields: []SessionField{{Name: "filename", Value: filename, IsString: true}}}
	if maxFileSizeMB > 0 {
		t.Fields = append(t.Fields, SessionField{Name: "max_file_size", Value: strconv.Itoa(maxFileSizeMB)})
	}
	if maxRolloverFiles > 0 {
		t.Fields = append(t.Fields, SessionField{Name: "max_rollover_files", Value: strconv.Itoa(maxRolloverFiles)})
	}
	return t
}

// RingBufferTarget returns a package0.ring_buffer target holding at most
// maxMemoryKB of events; 0 leaves the server's default (4 MB).
func RingBufferTarget(maxMemoryKB int) SessionTarget {
	t := SessionTarget{Package: "package0", Name: XETargetRingBuffer}
	if maxMemoryKB > 0 {
		t.Fields = []SessionField{{Name: "max_memory", Value: strconv.Itoa(maxMemoryKB)}}
	}
	return t
}

// -- New Session templates ----------------------------------------------------

// XESessionTemplate is one ready-made definition a New Session dialog offers
// under Template, as SSMS's does. Spec returns a fresh copy each call, with no
// name and no target: the dialog supplies both.
type XESessionTemplate struct {
	Name        string
	Category    string // the group SSMS lists it under
	Description string
	Spec        func() EventSessionSpec
}

// Predicates the templates share, in the form the catalog stores them, so a
// session created from a template reads back with the predicate it was given.
const (
	xeNotSystem       = xeProfilerNotSystem
	xeUserDatabase    = "[package0].[greater_than_uint64]([sqlserver].[database_id],(4))"
	xeErrorSeverity11 = "[package0].[greater_than_equal_int64]([severity],(11))"
	xeLoginFailed     = "[package0].[equal_int64]([error_number],(18456))"
	xeWaitEnded       = "[package0].[equal_uint64]([opcode],(1))"
	xeWaitedAtAll     = "[package0].[greater_than_uint64]([duration],(0))"
)

// xeAnd joins predicate terms the way the catalog stores a conjunction:
// parenthesised as a whole, the terms bare.
func xeAnd(terms ...string) string {
	for i, t := range terms {
		if strings.HasPrefix(t, "(") && strings.HasSuffix(t, ")") {
			terms[i] = t[1 : len(t)-1]
		}
	}
	return "(" + strings.Join(terms, " AND ") + ")"
}

// xeQueryActions are the global fields the query-tracking templates collect
// with every statement event.
func xeQueryActions() []string {
	return []string{"package0.event_sequence", "sqlserver.client_app_name", "sqlserver.client_hostname",
		"sqlserver.database_name", "sqlserver.query_hash", "sqlserver.query_plan_hash",
		"sqlserver.session_id", "sqlserver.username"}
}

// xeTemplateSpec wraps events in the options the New Session templates share:
// SSMS's defaults, spelled out, so a session created from one says what it is.
func xeTemplateSpec(causality bool, events ...SessionEvent) EventSessionSpec {
	return EventSessionSpec{
		Events:              events,
		RetentionMode:       XERetentionAllowSingleEventLoss,
		MaxDispatchLatency:  30 * time.Second,
		MaxMemory:           4096,
		MemoryPartitionMode: XEPartitionNone,
		TrackCausality:      causality,
	}
}

// XESessionTemplates returns the templates a New Session dialog offers, in the
// order to list them.
//
// The two XEvent Profiler templates are XEProfilerStandard and XEProfilerTSQL
// under no name. The others follow what SSMS's templates of the same name
// collect, reconstructed rather than copied — SSMS ships them as .xml files,
// and none was at hand — so they are gosmo's definitions, each checked live to
// create and to read back unchanged (TestLiveXESessionTemplates).
func XESessionTemplates() []XESessionTemplate {
	profiler := func(f func() EventSessionSpec) func() EventSessionSpec {
		return func() EventSessionSpec {
			s := f()
			s.Name = ""
			return s
		}
	}
	return []XESessionTemplate{
		{Name: "Standard", Category: "XEvent Profiler", Spec: profiler(XEProfilerStandard),
			Description: "Logins, logouts and every completed batch and RPC from user sessions — the XEvent Profiler's Standard session."},
		{Name: "TSQL", Category: "XEvent Profiler", Spec: profiler(XEProfilerTSQL),
			Description: "The text of every batch and RPC as it starts, with logins and logouts — the XEvent Profiler's TSQL session."},
		{Name: "Connection Tracking", Category: "System Monitoring",
			Description: "Logins, logouts and failed logins (error 18456), with the client that made them.",
			Spec: func() EventSessionSpec {
				acts := []string{"package0.event_sequence", "sqlserver.client_app_name", "sqlserver.client_hostname",
					"sqlserver.client_pid", "sqlserver.nt_username", "sqlserver.server_principal_name", "sqlserver.session_id"}
				return xeTemplateSpec(false,
					SessionEvent{Package: "sqlserver", Name: "error_reported", Actions: slices.Clone(acts), Predicate: "(" + xeLoginFailed + ")"},
					SessionEvent{Package: "sqlserver", Name: "login", Actions: slices.Clone(acts),
						Fields: []SessionField{{Name: "collect_options_text", Value: "1"}}},
					SessionEvent{Package: "sqlserver", Name: "logout", Actions: slices.Clone(acts)},
				)
			}},
		{Name: "Count Query Locks", Category: "Locks and Blocks",
			Description: "Counts the locks each query takes in user databases, in a histogram bucketed by query hash.",
			Spec: func() EventSessionSpec {
				s := xeTemplateSpec(false,
					SessionEvent{Package: "sqlserver", Name: "lock_acquired", Actions: []string{"sqlserver.query_hash"},
						Predicate: xeAnd(xeNotSystem, xeUserDatabase)})
				s.Targets = []SessionTarget{{Package: "package0", Name: "histogram", Fields: []SessionField{
					{Name: "filtering_event_name", Value: "sqlserver.lock_acquired", IsString: true},
					{Name: "source", Value: "sqlserver.query_hash", IsString: true},
					{Name: "source_type", Value: "1"},
				}}}
				return s
			}},
		{Name: "Deadlocks", Category: "Locks and Blocks",
			Description: "Every deadlock graph the server reports, as the xml_deadlock_report event carries it.",
			Spec: func() EventSessionSpec {
				return xeTemplateSpec(false,
					SessionEvent{Package: "sqlserver", Name: "xml_deadlock_report",
						Actions: []string{"package0.event_sequence", "sqlserver.database_name", "sqlserver.server_principal_name"}})
			}},
		{Name: "Query Batch Tracking", Category: "Query Execution",
			Description: "Every completed batch and RPC from user sessions, with attentions and the errors they raised.",
			Spec: func() EventSessionSpec {
				return xeTemplateSpec(false,
					SessionEvent{Package: "sqlserver", Name: "attention", Actions: xeQueryActions(), Predicate: xeNotSystem},
					SessionEvent{Package: "sqlserver", Name: "error_reported", Actions: xeQueryActions(),
						Predicate: xeAnd(xeNotSystem, xeErrorSeverity11)},
					SessionEvent{Package: "sqlserver", Name: "rpc_completed", Actions: xeQueryActions(), Predicate: xeNotSystem},
					SessionEvent{Package: "sqlserver", Name: "sql_batch_completed", Actions: xeQueryActions(), Predicate: xeNotSystem},
				)
			}},
		{Name: "Query Detail Tracking", Category: "Query Execution",
			Description: "Batches and RPCs with every statement and module they ran, from user sessions, tied together by causality tracking.",
			Spec: func() EventSessionSpec {
				return xeTemplateSpec(true,
					SessionEvent{Package: "sqlserver", Name: "attention", Actions: xeQueryActions(), Predicate: xeNotSystem},
					SessionEvent{Package: "sqlserver", Name: "error_reported", Actions: xeQueryActions(),
						Predicate: xeAnd(xeNotSystem, xeErrorSeverity11)},
					SessionEvent{Package: "sqlserver", Name: "module_end", Actions: xeQueryActions(), Predicate: xeNotSystem},
					SessionEvent{Package: "sqlserver", Name: "rpc_completed", Actions: xeQueryActions(), Predicate: xeNotSystem},
					SessionEvent{Package: "sqlserver", Name: "sp_statement_completed", Actions: xeQueryActions(), Predicate: xeNotSystem},
					SessionEvent{Package: "sqlserver", Name: "sql_batch_completed", Actions: xeQueryActions(), Predicate: xeNotSystem},
					SessionEvent{Package: "sqlserver", Name: "sql_statement_completed", Actions: xeQueryActions(), Predicate: xeNotSystem},
				)
			}},
		{Name: "Query Wait Statistics", Category: "Query Execution",
			Description: "Every wait a user session finished, with the batches and RPCs it belonged to (causality tracking).",
			Spec: func() EventSessionSpec {
				return xeTemplateSpec(true,
					SessionEvent{Package: "sqlos", Name: "wait_info", Actions: xeQueryActions(),
						Predicate: xeAnd(xeNotSystem, xeWaitEnded, xeWaitedAtAll)},
					SessionEvent{Package: "sqlserver", Name: "rpc_completed", Actions: xeQueryActions(), Predicate: xeNotSystem},
					SessionEvent{Package: "sqlserver", Name: "sql_batch_completed", Actions: xeQueryActions(), Predicate: xeNotSystem},
				)
			}},
	}
}
