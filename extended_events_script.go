package gosmo

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// extended_events_script.go is the write side of Extended Events sessions:
// CREATE / ALTER / DROP EVENT SESSION, STATE = START | STOP, and the scripter.
//
// # What ALTER EVENT SESSION accepts — verified live on major 17, 2026-09-29
//
//   - One statement may ADD several events, or DROP several, or ADD / DROP
//     several targets — but naming events and targets in one statement is a
//     syntax error ("Incorrect syntax near 'TARGET'"). Alter therefore emits
//     one statement per group.
//   - Events and targets can be added and dropped on a running session.
//   - A session may be left with no events, stopped or running (it stays
//     running), so re-adding a changed event after dropping it is safe even
//     when it is the only one. DROP EVENT x, ADD EVENT x in one statement is a
//     syntax error (Msg 156). Verified on 13, 14 and 17, 2026-10-02;
//     TestLiveEventSessionAlterOnlyEvent.
//   - Every WITH option but STARTUP_STATE is refused on a running session
//     (Msg 25707, "cannot be changed while the session is running"). Alter
//     stops the session around such a change and starts it again — the
//     audit's disable window (audit.go), and for the same reason: a caller
//     doing it by hand gets the failure path wrong. Stopping discards what a
//     ring_buffer target held.
//   - There is no rename.

// XEInfinite is MaxDispatchLatency's INFINITE, which the catalog stores as 0.
// A spec's zero MaxDispatchLatency means "the server default" instead — see
// EventSessionSpec.
const XEInfinite time.Duration = -1

// EventSessionSpec describes an event session to create, or the whole desired
// state of one to alter to.
//
// The options follow one rule for their zero values: those with no
// meaningful zero — RetentionMode, MaxDispatchLatency, MaxMemory,
// MemoryPartitionMode — mean "server default" on a create and "leave alone" on
// an alter. The rest are compared and written literally: MaxEventSize 0 is no
// large-event buffer, MaxDuration 0 UNLIMITED, and the two flags OFF.
type EventSessionSpec struct {
	Name    string
	Events  []SessionEvent
	Targets []SessionTarget

	RetentionMode       string        // an XERetention* constant
	MaxDispatchLatency  time.Duration // whole seconds; XEInfinite for INFINITE
	MaxMemory           int           // KB
	MaxEventSize        int           // KB
	MemoryPartitionMode string        // an XEPartition* constant
	TrackCausality      bool
	StartupState        bool

	// MaxDuration is SQL Server 2025's time-bound session, in whole seconds;
	// 0 is UNLIMITED. A non-zero value on an older server is a syntax error
	// there, which is the server's to report.
	MaxDuration time.Duration
}

// Spec returns the session's definition as a spec — the round trip the
// scripter and a Properties dialog both start from.
func (es *EventSession) Spec() EventSessionSpec {
	latency := es.MaxDispatchLatency
	if latency == 0 {
		latency = XEInfinite
	}
	return EventSessionSpec{
		Name:                es.Name,
		Events:              slices.Clone(es.Events),
		Targets:             slices.Clone(es.Targets),
		RetentionMode:       es.RetentionMode,
		MaxDispatchLatency:  latency,
		MaxMemory:           es.MaxMemory,
		MaxEventSize:        es.MaxEventSize,
		MemoryPartitionMode: es.MemoryPartitionMode,
		TrackCausality:      es.TrackCausality,
		StartupState:        es.StartupState,
		MaxDuration:         es.MaxDuration,
	}
}

// -- Clause builders ---------------------------------------------------------------

// literal renders one SET value as the DDL wants it: N'…' for a string,
// (n) for anything else — the form SSMS scripts and the catalog round-trips.
// The (n) form splices Value raw, so every statement builder runs
// checkFields first.
func (f SessionField) literal() string {
	if f.IsString {
		return QuoteLiteral(f.Value)
	}
	return "(" + f.Value + ")"
}

// xeNumericValue is what a non-string field value may be: an integer, a
// decimal, a float as CONVERT(nvarchar, float) renders it (1e+006), or a
// boolean token. Anything else would be spliced into the DDL as typed — a
// ")" there makes different, still-parseable DDL.
var xeNumericValue = regexp.MustCompile(`(?i)^([+-]?(\d+(\.\d*)?|\.\d+)(e[+-]?\d+)?|true|false)$`)

// Validate reports a non-string value that is not a number or a boolean
// token — one literal would splice into the DDL raw. A string value is quoted
// and always valid. Create, Alter and AddTarget run it on every field; a
// caller editing fields can run it first to report the error where it was
// typed.
func (f SessionField) Validate() error {
	if !f.IsString && !xeNumericValue.MatchString(f.Value) {
		return invalidf("field %s: value %q is not a number", f.Name, f.Value)
	}
	return nil
}

// checkFields validates fields, naming the event or target they belong to.
func checkFields(owner string, fields []SessionField) error {
	for _, f := range fields {
		if err := f.Validate(); err != nil {
			return fmt.Errorf("%s: %w", owner, err)
		}
	}
	return nil
}

// checkFields runs the package-level check over every event and target.
func (spec EventSessionSpec) checkFields() error {
	for _, e := range spec.Events {
		if err := checkFields("event "+e.QualifiedName(), e.Fields); err != nil {
			return err
		}
	}
	for _, t := range spec.Targets {
		if err := checkFields("target "+t.QualifiedName(), t.Fields); err != nil {
			return err
		}
	}
	return nil
}

func setClause(fields []SessionField) string {
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = f.Name + "=" + f.literal()
	}
	return "SET " + strings.Join(parts, ",")
}

// eventClause renders "pkg.name(SET … ACTION(…) WHERE …)", or the bare name
// when the event sets nothing.
func (e SessionEvent) clause() string {
	var parts []string
	if len(e.Fields) > 0 {
		parts = append(parts, setClause(e.Fields))
	}
	if len(e.Actions) > 0 {
		parts = append(parts, "ACTION("+strings.Join(e.Actions, ",")+")")
	}
	if e.Predicate != "" {
		parts = append(parts, "WHERE "+e.Predicate)
	}
	if len(parts) == 0 {
		return e.QualifiedName()
	}
	return e.QualifiedName() + "(" + strings.Join(parts, "\n    ") + ")"
}

func (t SessionTarget) clause() string {
	if len(t.Fields) == 0 {
		return t.QualifiedName()
	}
	return t.QualifiedName() + "(" + setClause(t.Fields) + ")"
}

func latencyKeyword(d time.Duration) string {
	if d == XEInfinite {
		return "INFINITE"
	}
	return fmt.Sprintf("%d SECONDS", int64(d/time.Second))
}

func durationKeyword(d time.Duration) string {
	if d <= 0 {
		return "UNLIMITED"
	}
	return fmt.Sprintf("%d SECONDS", int64(d/time.Second))
}

// createOptions lists the WITH options of a create, in SSMS's order. An
// option at its "server default" zero is left out; MAX_DURATION only when set,
// so a spec that does not use it stays valid on a pre-2025 server.
func (spec EventSessionSpec) createOptions() []string {
	var opts []string
	if spec.MaxMemory > 0 {
		opts = append(opts, fmt.Sprintf("MAX_MEMORY=%d KB", spec.MaxMemory))
	}
	if spec.RetentionMode != "" {
		opts = append(opts, "EVENT_RETENTION_MODE="+spec.RetentionMode)
	}
	if spec.MaxDispatchLatency != 0 {
		opts = append(opts, "MAX_DISPATCH_LATENCY="+latencyKeyword(spec.MaxDispatchLatency))
	}
	opts = append(opts, fmt.Sprintf("MAX_EVENT_SIZE=%d KB", spec.MaxEventSize))
	if spec.MemoryPartitionMode != "" {
		opts = append(opts, "MEMORY_PARTITION_MODE="+spec.MemoryPartitionMode)
	}
	opts = append(opts, "TRACK_CAUSALITY="+onOff(spec.TrackCausality))
	opts = append(opts, "STARTUP_STATE="+onOff(spec.StartupState))
	if spec.MaxDuration > 0 {
		opts = append(opts, "MAX_DURATION="+durationKeyword(spec.MaxDuration))
	}
	return opts
}

func (spec EventSessionSpec) validate() error {
	if strings.TrimSpace(spec.Name) == "" {
		return invalidf("event session has no name")
	}
	if len(spec.Events) == 0 {
		return invalidf("event session %q has no events", spec.Name)
	}
	for _, e := range spec.Events {
		if e.Package == "" || e.Name == "" {
			return invalidf("event session %q: event %q has no package or name", spec.Name, e.QualifiedName())
		}
	}
	for _, t := range spec.Targets {
		if t.Package == "" || t.Name == "" {
			return invalidf("event session %q: target %q has no package or name", spec.Name, t.QualifiedName())
		}
	}
	if err := spec.checkFields(); err != nil {
		return fmt.Errorf("event session %q: %w", spec.Name, err)
	}
	return nil
}

// createStatement builds CREATE EVENT SESSION in SSMS's layout: events
// comma-separated, then targets comma-separated, no comma between the two
// lists.
func (spec EventSessionSpec) createStatement(sc xeScope) (string, error) {
	if err := spec.validate(); err != nil {
		return "", err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "CREATE EVENT SESSION %s %s", quoteIdent(spec.Name), sc.on)
	for i, e := range spec.Events {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("\nADD EVENT ")
		sb.WriteString(e.clause())
	}
	for i, t := range spec.Targets {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("\nADD TARGET ")
		sb.WriteString(t.clause())
	}
	sb.WriteString("\nWITH (")
	sb.WriteString(strings.Join(spec.createOptions(), ","))
	sb.WriteString(")")
	return sb.String(), nil
}

// -- Writes -------------------------------------------------------------------------

// CreateEventSession creates a server-scoped event session. It is created
// stopped, which is what CREATE EVENT SESSION does; use Start to run it.
func (s *Server) CreateEventSession(ctx context.Context, spec EventSessionSpec) (*EventSession, error) {
	return createEventSession(ctx, s.EventSessionRef(spec.Name), spec, func() (*EventSession, error) {
		return s.EventSessionByName(ctx, spec.Name)
	})
}

// CreateEventSession creates a database-scoped event session (Azure SQL
// Database).
func (d *Database) CreateEventSession(ctx context.Context, spec EventSessionSpec) (*EventSession, error) {
	return createEventSession(ctx, d.EventSessionRef(spec.Name), spec, func() (*EventSession, error) {
		return d.EventSessionByName(ctx, spec.Name)
	})
}

func createEventSession(ctx context.Context, ref *EventSession, spec EventSessionSpec, read func() (*EventSession, error)) (*EventSession, error) {
	stmt, err := spec.createStatement(ref.scope())
	if err != nil {
		return nil, fmt.Errorf("gosmo: create event session: %w", err)
	}
	if err := ref.exec(ctx, stmt); err != nil {
		return nil, fmt.Errorf("gosmo: create event session %q: %w", spec.Name, err)
	}
	return createdObject(ctx, ref, read)
}

// Start starts the session (STATE = START).
func (es *EventSession) Start(ctx context.Context) error { return es.setState(ctx, true) }

// Stop stops the session (STATE = STOP). A ring_buffer target's contents go
// with it.
func (es *EventSession) Stop(ctx context.Context) error { return es.setState(ctx, false) }

func (es *EventSession) setState(ctx context.Context, start bool) error {
	state := "STOP"
	if start {
		state = "START"
	}
	stmt := fmt.Sprintf("ALTER EVENT SESSION %s %s STATE = %s", quoteIdent(es.Name), es.scope().on, state)
	if err := es.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: %s event session %q: %w", strings.ToLower(state), es.Name, err)
	}
	setIfApplied(ctx, &es.IsRunning, start)
	return nil
}

// AddTarget adds one target to the session — running or not, since adding a
// target needs no stop (see the notes at the top of this file). It is the
// one-statement form of an Alter whose spec differs only by the target; the
// session's other targets are left as they are.
func (es *EventSession) AddTarget(ctx context.Context, t SessionTarget) error {
	if t.Package == "" || t.Name == "" {
		return invalidf("gosmo: add target to event session %q: target %q has no package or name", es.Name, t.QualifiedName())
	}
	if err := checkFields("target "+t.QualifiedName(), t.Fields); err != nil {
		return fmt.Errorf("gosmo: add target to event session %q: %w", es.Name, err)
	}
	stmt := fmt.Sprintf("ALTER EVENT SESSION %s %s\nADD TARGET %s", quoteIdent(es.Name), es.scope().on, t.clause())
	if err := es.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: add target %s to event session %q: %w", t.QualifiedName(), es.Name, err)
	}
	setIfApplied(ctx, &es.Targets, append(slices.Clone(es.Targets), t))
	return nil
}

// Drop deletes the session. A running session needs no stopping first.
func (es *EventSession) Drop(ctx context.Context) error {
	stmt := fmt.Sprintf("DROP EVENT SESSION %s %s", quoteIdent(es.Name), es.scope().on)
	if err := es.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: drop event session %q: %w", es.Name, err)
	}
	return nil
}

// eventSessionAlter is the diff Alter applies, split by whether a statement
// needs the session stopped.
type eventSessionAlter struct {
	statements  []string // DROP EVENT, DROP TARGET, ADD EVENT, ADD TARGET, STARTUP_STATE
	stoppedOnly string   // WITH (…) of the options a running session refuses; empty if none
}

// alterStatements diffs cur against the spec — SSMS's minimal form:
//
//   - an event or target only in cur is dropped, one only in the spec added;
//   - one in both that differs (predicate, actions, fields) is dropped and
//     re-added, there being no ALTER of an event in place;
//   - an option is written only when it changes, and the spec's "server
//     default" zeros leave it alone (EventSessionSpec).
//
// Events are matched by package.name, targets likewise; a session cannot hold
// two of either with one name.
func (spec EventSessionSpec) alterStatements(cur *EventSession) eventSessionAlter {
	head := fmt.Sprintf("ALTER EVENT SESSION %s %s", quoteIdent(cur.Name), cur.scope().on)
	var out eventSessionAlter

	curEvents := map[string]SessionEvent{}
	for _, e := range cur.Events {
		curEvents[strings.ToLower(e.QualifiedName())] = e
	}
	wantEvents := map[string]bool{}
	var dropEvents, addEvents []string
	for _, e := range spec.Events {
		k := strings.ToLower(e.QualifiedName())
		wantEvents[k] = true
		if old, ok := curEvents[k]; ok {
			if eventsEqual(old, e) {
				continue
			}
			dropEvents = append(dropEvents, "DROP EVENT "+old.QualifiedName())
		}
		addEvents = append(addEvents, "ADD EVENT "+e.clause())
	}
	for _, e := range cur.Events {
		if !wantEvents[strings.ToLower(e.QualifiedName())] {
			dropEvents = append(dropEvents, "DROP EVENT "+e.QualifiedName())
		}
	}

	curTargets := map[string]SessionTarget{}
	for _, t := range cur.Targets {
		curTargets[strings.ToLower(t.QualifiedName())] = t
	}
	wantTargets := map[string]bool{}
	var dropTargets, addTargets []string
	for _, t := range spec.Targets {
		k := strings.ToLower(t.QualifiedName())
		wantTargets[k] = true
		if old, ok := curTargets[k]; ok {
			if fieldsEqual(old.Fields, t.Fields) {
				continue
			}
			dropTargets = append(dropTargets, "DROP TARGET "+old.QualifiedName())
		}
		addTargets = append(addTargets, "ADD TARGET "+t.clause())
	}
	for _, t := range cur.Targets {
		if !wantTargets[strings.ToLower(t.QualifiedName())] {
			dropTargets = append(dropTargets, "DROP TARGET "+t.QualifiedName())
		}
	}

	for _, group := range [][]string{dropEvents, dropTargets, addEvents, addTargets} {
		if len(group) > 0 {
			out.statements = append(out.statements, head+"\n"+strings.Join(group, ",\n"))
		}
	}

	var opts []string
	if spec.MaxMemory > 0 && spec.MaxMemory != cur.MaxMemory {
		opts = append(opts, fmt.Sprintf("MAX_MEMORY=%d KB", spec.MaxMemory))
	}
	if spec.RetentionMode != "" && !strings.EqualFold(spec.RetentionMode, cur.RetentionMode) {
		opts = append(opts, "EVENT_RETENTION_MODE="+spec.RetentionMode)
	}
	if spec.MaxDispatchLatency != 0 && spec.MaxDispatchLatency != cur.Spec().MaxDispatchLatency {
		opts = append(opts, "MAX_DISPATCH_LATENCY="+latencyKeyword(spec.MaxDispatchLatency))
	}
	if spec.MaxEventSize != cur.MaxEventSize {
		opts = append(opts, fmt.Sprintf("MAX_EVENT_SIZE=%d KB", spec.MaxEventSize))
	}
	if spec.MemoryPartitionMode != "" && !strings.EqualFold(spec.MemoryPartitionMode, cur.MemoryPartitionMode) {
		opts = append(opts, "MEMORY_PARTITION_MODE="+spec.MemoryPartitionMode)
	}
	if spec.TrackCausality != cur.TrackCausality {
		opts = append(opts, "TRACK_CAUSALITY="+onOff(spec.TrackCausality))
	}
	if spec.MaxDuration != cur.MaxDuration {
		opts = append(opts, "MAX_DURATION="+durationKeyword(spec.MaxDuration))
	}
	if len(opts) > 0 {
		out.stoppedOnly = head + "\nWITH (" + strings.Join(opts, ",") + ")"
	}
	if spec.StartupState != cur.StartupState {
		out.statements = append(out.statements, head+"\nWITH (STARTUP_STATE="+onOff(spec.StartupState)+")")
	}
	return out
}

func eventsEqual(a, b SessionEvent) bool {
	return a.Predicate == b.Predicate &&
		slices.Equal(sortedFold(a.Actions), sortedFold(b.Actions)) &&
		fieldsEqual(a.Fields, b.Fields)
}

func sortedFold(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = strings.ToLower(v)
	}
	slices.Sort(out)
	return out
}

func fieldsEqual(a, b []SessionField) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(f SessionField) string { return strings.ToLower(f.Name) + "=" + f.Value }
	ka, kb := make([]string, len(a)), make([]string, len(b))
	for i := range a {
		ka[i], kb[i] = key(a[i]), key(b[i])
	}
	slices.Sort(ka)
	slices.Sort(kb)
	return slices.Equal(ka, kb)
}

// Alter brings the session to the spec's definition with the minimal ALTER
// statements (alterStatements). The current definition is read from the
// catalog, not taken from the receiver, so a name-only handle works.
//
// Options a running session refuses are applied inside a stop window: the
// session is stopped, altered and started again, and started again on the
// failure path too — see restoreWindow. The spec's Name is ignored; there is
// no rename.
//
// The statements are not one transaction — the server runs event-session DDL
// outside any — so a failure part-way leaves the groups before it applied.
func (es *EventSession) Alter(ctx context.Context, spec EventSessionSpec) error {
	if err := spec.checkFields(); err != nil {
		return fmt.Errorf("gosmo: alter event session %q: %w", es.Name, err)
	}
	cur, err := eventSessionByName(ctx, es.refLike())
	if err != nil {
		return err
	}
	diff := spec.alterStatements(cur)
	for _, stmt := range diff.statements {
		if err := es.exec(ctx, stmt); err != nil {
			return fmt.Errorf("gosmo: alter event session %q: %w", es.Name, err)
		}
	}
	if diff.stoppedOnly == "" {
		return nil
	}
	apply := func(ctx context.Context) error {
		if err := es.exec(ctx, diff.stoppedOnly); err != nil {
			return fmt.Errorf("gosmo: alter event session %q: %w", es.Name, err)
		}
		return nil
	}
	if !cur.IsRunning {
		return apply(ctx)
	}
	if err := es.setState(unobserved(ctx), false); err != nil {
		return err
	}
	start := func(ctx context.Context) error { return es.setState(ctx, true) }
	if err := apply(ctx); err != nil {
		_ = restoreWindow(ctx, start)
		return err
	}
	return restoreWindow(ctx, start)
}

// refLike returns a name-only handle in the receiver's scope.
func (es *EventSession) refLike() *EventSession {
	return &EventSession{server: es.server, db: es.db, Name: es.Name}
}

// -- Scripter ------------------------------------------------------------------------

// ScriptEventSession generates the CREATE (or DROP) script for one
// server-scoped event session.
func (sc *ServerScripter) ScriptEventSession(ctx context.Context, name string) (string, error) {
	es, err := sc.server.EventSessionByName(ctx, name)
	if err != nil {
		return "", err
	}
	return buildEventSessionScript(es, sc.opts)
}

// ScriptEventSession generates the CREATE (or DROP) script for one
// database-scoped event session.
func (sc *Scripter) ScriptEventSession(ctx context.Context, name string) (string, error) {
	es, err := sc.db.EventSessionByName(ctx, name)
	if err != nil {
		return "", err
	}
	return buildEventSessionScript(es, sc.opts)
}

// buildEventSessionScript assembles one session's script. The running state
// is not scripted, as SSMS does not script it: STARTUP_STATE is in the
// definition, a session that happens to be running now is not.
func buildEventSessionScript(es *EventSession, opts ScriptOptions) (string, error) {
	sc := es.scope()
	exists := fmt.Sprintf("SELECT 1 FROM %s WHERE name = N'%s'", sc.sessions, escapeSingle(es.Name))
	drop := fmt.Sprintf("IF EXISTS (%s)\n    DROP EVENT SESSION %s %s;\nGO\n", exists, quoteIdent(es.Name), sc.on)
	guard := fmt.Sprintf("IF NOT EXISTS (%s)\n", exists)
	return opts.envelopeErr(drop, guard, func(sb *strings.Builder) error {
		create, err := es.Spec().createStatement(sc)
		if err != nil {
			return fmt.Errorf("gosmo: script event session %q: %w", es.Name, err)
		}
		sb.WriteString(create)
		sb.WriteString(";\nGO\n")
		return nil
	})
}
