package gosmo

// Service Broker, part three — Queues.
//
// A queue is the one schema-scoped member of the Service Broker tree: it is a
// sys.objects row of type 'SQ', so it is transferable between schemas, its
// permissions are object-scoped where the other six families' are
// database-scoped, and its system membership is read from is_ms_shipped
// rather than from an id range. service_broker.go's file comment covers what
// it shares with the rest.
//
// It is also the only family here with settings rather than only structure,
// which is what ALTER QUEUE below exists for.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ============================================================
// Queues
// ============================================================

// BrokerQueue mirrors a sys.service_queues row — the table a service
// receives messages into.
//
// A queue is the one schema-scoped member of the Service Broker tree: it is a
// sys.objects row of type 'SQ', so it is transferable between schemas and its
// permissions are object-scoped, where the other six families' are
// database-scoped.
type BrokerQueue struct {
	db *Database

	Name     string
	Schema   string
	ObjectID int

	// Owner is the database principal that owns the queue, empty when
	// principal_id is NULL (the usual case — the queue then belongs to its
	// schema's owner).
	Owner string

	// IsEnqueueEnabled and IsReceiveEnabled are the two halves of the
	// queue's STATUS. ALTER QUEUE … WITH STATUS sets both together, so a
	// queue taken out of service reads false on both; they are kept apart
	// because the catalog does.
	IsEnqueueEnabled bool
	IsReceiveEnabled bool

	// IsRetentionEnabled reports RETENTION = ON: messages stay in the queue
	// until the conversation ends, rather than being removed on RECEIVE.
	IsRetentionEnabled bool

	// IsPoisonMessageHandlingEnabled reports POISON_MESSAGE_HANDLING
	// (STATUS = ON), the default. With it off, five consecutive rollbacks no
	// longer disable the queue.
	IsPoisonMessageHandlingEnabled bool

	// IsActivationEnabled reports ACTIVATION (STATUS = ON).
	IsActivationEnabled bool

	// ActivationProcedure is the activation procedure, already
	// bracket-quoted and schema-qualified the way the catalog stores it
	// ("[dbo].[usp_x]"). Empty when the queue has none.
	ActivationProcedure string

	// ActivationProcedureSchema and ActivationProcedureName are the same
	// procedure unquoted, resolved through OBJECT_ID — the form
	// QueueActivation takes, so a queue read back can be handed to
	// AlterBrokerQueue without parsing the bracketed text. Both are empty
	// when the queue has no activation procedure, and also when it names one
	// that has since been dropped: the catalog keeps the text either way.
	ActivationProcedureSchema string
	ActivationProcedureName   string

	// MaxReaders is MAX_QUEUE_READERS, 0 when the queue has no activation.
	MaxReaders int

	// ActivationExecuteAs is the principal the activation procedure runs as:
	// a user name, the literal "OWNER" for EXECUTE AS OWNER, or empty when
	// the queue has no activation.
	//
	// EXECUTE AS SELF does not survive as itself — the server resolves it at
	// CREATE time and stores the creating principal, so a queue created with
	// SELF reads back as that user's name and scripts as EXECUTE AS N'<user>'.
	// That is the same object, not a lost setting.
	ActivationExecuteAs string

	// FileGroup is the filegroup the queue's internal table lives on, empty
	// when it could not be read.
	FileGroup string

	// IsSystemObject reports a queue SQL Server ships — read from
	// is_ms_shipped, the only discriminator that works for this family.
	IsSystemObject bool

	CreateDate time.Time
	ModifyDate time.Time
}

// Database returns the database the queue belongs to.
func (q *BrokerQueue) Database() *Database { return q.db }

// FullName returns the schema-qualified, bracket-quoted name.
func (q *BrokerQueue) FullName() string { return qualifiedName(q.Schema, q.Name) }

// queueSelect is the SELECT the listing and the by-name finder share.
//
// The filegroup comes from the queue's internal table through an OUTER APPLY
// rather than a join, so a queue whose internal table the caller cannot see
// still lists, with an empty FileGroup. The message count is deliberately not
// here: it needs sys.dm_db_partition_stats and VIEW DATABASE STATE with it,
// and a caller without that right must still get the listing — see
// Database.QueueMessageCountsContext.
//
// The activation procedure is read twice: once as the catalog's own
// bracketed text and once resolved through OBJECT_ID into its two unquoted
// halves, which is what an ALTER has to be given. Splitting the text in Go
// would be a parser for bracket-quoted names with embedded dots.
//
// execute_as_principal_id is decoded here rather than in Go because the
// mapping needs USER_NAME: -2 is EXECUTE AS OWNER, NULL is a queue with no
// activation principal, and every other value is a real principal id (the
// server resolves EXECUTE AS SELF to the creator's at CREATE time).
const queueSelect = `
SELECT q.object_id, q.name, SCHEMA_NAME(q.schema_id),
       ISNULL(USER_NAME(q.principal_id), ''),
       q.is_ms_shipped, q.is_enqueue_enabled, q.is_receive_enabled,
       q.is_retention_enabled, ISNULL(q.is_poison_message_handling_enabled, 1),
       q.is_activation_enabled, ISNULL(q.activation_procedure, ''),
       ISNULL(OBJECT_SCHEMA_NAME(OBJECT_ID(q.activation_procedure)), ''),
       ISNULL(OBJECT_NAME(OBJECT_ID(q.activation_procedure)), ''),
       ISNULL(q.max_readers, 0),
       CASE WHEN q.execute_as_principal_id IS NULL THEN ''
            WHEN q.execute_as_principal_id = -2 THEN 'OWNER'
            ELSE ISNULL(USER_NAME(q.execute_as_principal_id), '') END,
       ISNULL(fg.name, ''), q.create_date, q.modify_date
FROM   sys.service_queues q
OUTER  APPLY (SELECT TOP (1) ds.name AS name
              FROM   sys.internal_tables it
              JOIN   sys.indexes i ON i.object_id = it.object_id AND i.index_id < 2
              JOIN   sys.data_spaces ds ON ds.data_space_id = i.data_space_id
              WHERE  it.parent_object_id = q.object_id) fg`

func scanBrokerQueue(d *Database, scan func(...any) error) (*BrokerQueue, error) {
	q := &BrokerQueue{db: d}
	if err := scan(&q.ObjectID, &q.Name, &q.Schema, &q.Owner,
		&q.IsSystemObject, &q.IsEnqueueEnabled, &q.IsReceiveEnabled,
		&q.IsRetentionEnabled, &q.IsPoisonMessageHandlingEnabled,
		&q.IsActivationEnabled, &q.ActivationProcedure,
		&q.ActivationProcedureSchema, &q.ActivationProcedureName,
		&q.MaxReaders, &q.ActivationExecuteAs,
		&q.FileGroup, &q.CreateDate, &q.ModifyDate); err != nil {
		return nil, err
	}
	return q, nil
}

// BrokerQueues returns the queues defined in the database, the ones SQL
// Server ships included (marked IsSystemObject).
func (d *Database) BrokerQueues() ([]*BrokerQueue, error) {
	return d.BrokerQueuesContext(context.Background())
}

// BrokerQueuesContext is the context-aware variant of BrokerQueues.
func (d *Database) BrokerQueuesContext(ctx context.Context) ([]*BrokerQueue, error) {
	const q = queueSelect + `
ORDER  BY SCHEMA_NAME(q.schema_id), q.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list queues in %q: %w", d.Name, err)
	}
	defer rows.Close()

	var out []*BrokerQueue
	for rows.Next() {
		bq, err := scanBrokerQueue(d, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list queues in %q: %w", d.Name, err)
		}
		out = append(out, bq)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list queues in %q: %w", d.Name, err)
	}
	return out, nil
}

// BrokerQueueByName returns one queue, or a not-found error (errors.Is
// ErrNotFound) when the database has none by that name. An empty schema
// means dbo.
func (d *Database) BrokerQueueByName(schema, name string) (*BrokerQueue, error) {
	return d.BrokerQueueByNameContext(context.Background(), schema, name)
}

// BrokerQueueByNameContext is the context-aware variant of BrokerQueueByName.
func (d *Database) BrokerQueueByNameContext(ctx context.Context, schema, name string) (*BrokerQueue, error) {
	if schema == "" {
		schema = "dbo"
	}
	var bq *BrokerQueue
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		bq, err = scanBrokerQueue(d, row.Scan)
		return err
	}, queueSelect+`
WHERE  SCHEMA_NAME(q.schema_id) = @p1 AND q.name = @p2`, schema, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: queue [%s].[%s] not found in %q", schema, name, d.Name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read queue [%s].[%s] in %q: %w", schema, name, d.Name, err)
	}
	return bq, nil
}

// DropBrokerQueue drops a queue by name. A queue a service still receives on
// is refused by the server until the service goes. An empty schema means dbo.
func (d *Database) DropBrokerQueue(schema, name string) error {
	return d.DropBrokerQueueContext(context.Background(), schema, name)
}

// DropBrokerQueueContext is the context-aware variant of DropBrokerQueue.
func (d *Database) DropBrokerQueueContext(ctx context.Context, schema, name string) error {
	if schema == "" {
		schema = "dbo"
	}
	if _, err := d.exec(ctx, "DROP QUEUE "+qualifiedName(schema, name)); err != nil {
		return fmt.Errorf("gosmo: drop queue [%s].[%s]: %w", schema, name, err)
	}
	return nil
}

// Drop drops the queue.
func (q *BrokerQueue) Drop() error { return q.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (q *BrokerQueue) DropContext(ctx context.Context) error {
	return q.db.DropBrokerQueueContext(ctx, q.Schema, q.Name)
}

// queueMessageCountSelect counts the rows in each queue's internal table.
//
// It never selects from the queue itself: SELECT COUNT(*) FROM <queue> needs
// RECEIVE on the queue and takes locks on a live one, where this reads
// sys.dm_db_partition_stats and needs only VIEW DATABASE STATE.
const queueMessageCountSelect = `
SELECT it.parent_object_id, SUM(ps.row_count)
FROM   sys.internal_tables it
JOIN   sys.dm_db_partition_stats ps
         ON ps.object_id = it.object_id AND ps.index_id < 2
JOIN   sys.service_queues q ON q.object_id = it.parent_object_id
GROUP  BY it.parent_object_id`

// QueueMessageCounts returns the number of messages currently in each queue,
// keyed by the queue's ObjectID.
func (d *Database) QueueMessageCounts() (map[int]int64, error) {
	return d.QueueMessageCountsContext(context.Background())
}

// QueueMessageCountsContext is the context-aware variant of
// QueueMessageCounts.
//
// It is a separate call rather than a column on BrokerQueue because it reads
// a DMV: a caller without VIEW DATABASE STATE gets an error here and a
// complete queue listing anyway, where one query for both would lose the
// listing too. A caller displaying the count treats a failure, and a queue
// missing from the map, as "unknown" rather than as zero — a queue whose
// internal table has not been materialised has no row here.
func (d *Database) QueueMessageCountsContext(ctx context.Context) (map[int]int64, error) {
	rows, err := d.query(ctx, queueMessageCountSelect)
	if err != nil {
		return nil, fmt.Errorf("gosmo: read queue message counts in %q: %w", d.Name, err)
	}
	defer rows.Close()

	out := make(map[int]int64)
	for rows.Next() {
		var id int
		var count sql.NullInt64
		if err := rows.Scan(&id, &count); err != nil {
			return nil, fmt.Errorf("gosmo: read queue message counts in %q: %w", d.Name, err)
		}
		if count.Valid {
			out[id] = count.Int64
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: read queue message counts in %q: %w", d.Name, err)
	}
	return out, nil
}

// MessageCount returns the number of messages currently in the queue.
func (q *BrokerQueue) MessageCount() (int64, error) {
	return q.MessageCountContext(context.Background())
}

// MessageCountContext is the context-aware variant of MessageCount. It needs
// VIEW DATABASE STATE, and returns 0 with no error for a queue whose
// internal table has no statistics row yet.
func (q *BrokerQueue) MessageCountContext(ctx context.Context) (int64, error) {
	var count sql.NullInt64
	err := q.db.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&count)
	}, `
SELECT SUM(ps.row_count)
FROM   sys.internal_tables it
JOIN   sys.dm_db_partition_stats ps
         ON ps.object_id = it.object_id AND ps.index_id < 2
WHERE  it.parent_object_id = @p1`, q.ObjectID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("gosmo: read message count of queue %s in %q: %w",
			q.FullName(), q.db.Name, err)
	}
	return count.Int64, nil
}

// QueueMonitor mirrors a sys.dm_broker_queue_monitors row — the activation
// state of one queue the broker is monitoring.
type QueueMonitor struct {
	// QueueID is the queue's object_id, matching BrokerQueue.ObjectID.
	QueueID int

	// State is the monitor's state as the DMV reports it: INACTIVE,
	// NOTIFIED or RECEIVES_OCCURRING.
	State string

	// TasksWaiting is the number of activated tasks waiting on the queue.
	TasksWaiting int

	// LastActivatedTime and LastEmptyRowsetTime are zero when the DMV
	// reports NULL, which is the normal state for a queue never activated.
	LastActivatedTime   time.Time
	LastEmptyRowsetTime time.Time
}

// QueueMonitors returns the broker's activation state for the queues it is
// monitoring in the database.
func (d *Database) QueueMonitors() ([]*QueueMonitor, error) {
	return d.QueueMonitorsContext(context.Background())
}

// QueueMonitorsContext is the context-aware variant of QueueMonitors.
//
// A queue with no row here is the normal case, not an error: the broker
// creates a monitor when it first has reason to look at the queue. Callers
// index the result by QueueID and treat a missing queue as "never
// activated". It needs VIEW DATABASE STATE, the same as the message counts.
func (d *Database) QueueMonitorsContext(ctx context.Context) ([]*QueueMonitor, error) {
	const q = `
SELECT m.queue_id, ISNULL(m.state, ''), ISNULL(m.tasks_waiting, 0),
       m.last_activated_time, m.last_empty_rowset_time
FROM   sys.dm_broker_queue_monitors m
WHERE  m.database_id = DB_ID()`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list queue monitors in %q: %w", d.Name, err)
	}
	defer rows.Close()

	var out []*QueueMonitor
	for rows.Next() {
		m := &QueueMonitor{}
		var activated, empty sql.NullTime
		if err := rows.Scan(&m.QueueID, &m.State, &m.TasksWaiting, &activated, &empty); err != nil {
			return nil, fmt.Errorf("gosmo: list queue monitors in %q: %w", d.Name, err)
		}
		m.LastActivatedTime = activated.Time
		m.LastEmptyRowsetTime = empty.Time
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list queue monitors in %q: %w", d.Name, err)
	}
	return out, nil
}

// ============================================================
// ALTER QUEUE
// ============================================================

// The values QueueActivation.ExecuteAs takes besides a database user's name.
//
// SELF does not survive a round trip: the server resolves it at ALTER time
// and stores the principal it resolved to, so a queue altered with
// QueueExecuteAsSelf reads back as that user's name. OWNER is stored as
// itself (principal id -2) and does read back as "OWNER".
const (
	// QueueExecuteAsOwner runs the activation procedure as the queue's owner.
	QueueExecuteAsOwner = "OWNER"
	// QueueExecuteAsSelf runs it as the principal issuing the ALTER.
	QueueExecuteAsSelf = "SELF"
)

// QueueActivation is the ACTIVATION block of an ALTER QUEUE, restated in
// full.
//
// It is a full restatement rather than a set of optional fields because the
// server requires one: enabling activation on a queue that has none is
// refused unless PROCEDURE_NAME, MAX_QUEUE_READERS and EXECUTE AS are all
// given ("the activation user is not specified"). A caller editing a queue
// has read it first, so restating what it already has costs nothing —
// BrokerQueue's ActivationProcedureSchema/Name, MaxReaders and
// ActivationExecuteAs are exactly the four fields here.
type QueueActivation struct {
	// Enabled is ACTIVATION's own STATUS. False keeps the procedure, the
	// reader count and the principal on the queue and stops the broker
	// starting it — use QueueSettings.DropActivation to remove it instead.
	Enabled bool

	// ProcedureSchema and ProcedureName name the activation procedure,
	// unquoted. An empty schema means dbo.
	ProcedureSchema string
	ProcedureName   string

	// MaxQueueReaders is MAX_QUEUE_READERS, 0 to 32767.
	MaxQueueReaders int

	// ExecuteAs is QueueExecuteAsOwner, QueueExecuteAsSelf, or the name of a
	// database user. Empty means QueueExecuteAsOwner.
	ExecuteAs string
}

// QueueSettings is what ALTER QUEUE can change. A nil field leaves that
// setting exactly as it is — the statement omits the clause, and SQL Server
// changes nothing it is not told about.
//
// At least one field must be set: ALTER QUEUE with no clause does not parse.
type QueueSettings struct {
	// Status is the queue's STATUS. False takes the queue out of service:
	// the server clears the enqueue *and* receive halves together, so a
	// queue with Status false reads back false on both IsEnqueueEnabled and
	// IsReceiveEnabled.
	Status *bool

	// Retention is RETENTION. With it on, messages stay in the queue until
	// the conversation ends rather than being removed on RECEIVE.
	Retention *bool

	// PoisonMessageHandling is POISON_MESSAGE_HANDLING (STATUS = …). With it
	// off, five consecutive rollbacks no longer disable the queue.
	PoisonMessageHandling *bool

	// Activation restates the ACTIVATION block. Nil leaves the queue's
	// activation alone.
	Activation *QueueActivation

	// DropActivation emits ACTIVATION (DROP), which removes the activation
	// procedure, the reader count and the principal outright. It cannot be
	// combined with Activation — one statement cannot both drop and restate
	// the block.
	DropActivation bool
}

// AlterBrokerQueue changes a queue's settings. An empty schema means dbo.
func (d *Database) AlterBrokerQueue(schema, name string, s QueueSettings) error {
	return d.AlterBrokerQueueContext(context.Background(), schema, name, s)
}

// AlterBrokerQueueContext is the context-aware variant of AlterBrokerQueue.
//
// It needs ALTER on the queue itself (ALTER ON OBJECT::<queue>), CONTROL on
// it, or ALTER on its schema or the database — measured on majors 13, 14 and
// 17, which answered identically. The object-scoped ALTER is *not* enough to
// drop the same queue, which needs CONTROL on it or ALTER on its schema: the
// two verbs take different rights, and a caller gating them shares no entry
// between them.
func (d *Database) AlterBrokerQueueContext(ctx context.Context, schema, name string, s QueueSettings) error {
	if schema == "" {
		schema = "dbo"
	}
	clauses, err := queueSettingClauses(s)
	if err != nil {
		return fmt.Errorf("gosmo: alter queue [%s].[%s]: %w", schema, name, err)
	}
	q := "ALTER QUEUE " + qualifiedName(schema, name) + "\n    WITH " +
		strings.Join(clauses, ",\n         ")
	if _, err := d.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: alter queue [%s].[%s]: %w", schema, name, err)
	}
	return nil
}

// queueSettingClauses renders the WITH clauses, or reports why the settings
// cannot be a statement. Every refusal here is one the server would give
// anyway, raised before a round trip and naming the field rather than the
// generated SQL.
func queueSettingClauses(s QueueSettings) ([]string, error) {
	if s.Activation != nil && s.DropActivation {
		return nil, errors.New("the Activation and DropActivation settings are mutually exclusive; " +
			"one statement cannot both restate and drop the ACTIVATION block")
	}
	var clauses []string
	if s.Status != nil {
		clauses = append(clauses, "STATUS = "+onOff(*s.Status))
	}
	if s.Retention != nil {
		clauses = append(clauses, "RETENTION = "+onOff(*s.Retention))
	}
	if a := s.Activation; a != nil {
		if a.ProcedureName == "" {
			return nil, errors.New("QueueActivation.ProcedureName is empty; " +
				"the server refuses an ACTIVATION block without a procedure")
		}
		if a.MaxQueueReaders < 0 || a.MaxQueueReaders > 32767 {
			return nil, fmt.Errorf("QueueActivation.MaxQueueReaders is %d, "+
				"outside MAX_QUEUE_READERS' range of 0 to 32767", a.MaxQueueReaders)
		}
		schema := a.ProcedureSchema
		if schema == "" {
			schema = "dbo"
		}
		clauses = append(clauses, fmt.Sprintf(
			"ACTIVATION (STATUS = %s, PROCEDURE_NAME = %s, MAX_QUEUE_READERS = %d, EXECUTE AS %s)",
			onOff(a.Enabled), qualifiedName(schema, a.ProcedureName),
			a.MaxQueueReaders, queueExecuteAsValue(a.ExecuteAs)))
	}
	if s.DropActivation {
		clauses = append(clauses, "ACTIVATION (DROP)")
	}
	if s.PoisonMessageHandling != nil {
		clauses = append(clauses, "POISON_MESSAGE_HANDLING (STATUS = "+
			onOff(*s.PoisonMessageHandling)+")")
	}
	if len(clauses) == 0 {
		return nil, errors.New("no setting was given; ALTER QUEUE with no clause does not parse")
	}
	return clauses, nil
}

// queueExecuteAsValue renders EXECUTE AS. OWNER and SELF are keywords;
// anything else is a user name and goes in a string literal, which is the
// form the statement takes and the form BrokerQueue.ActivationExecuteAs reads
// back as.
func queueExecuteAsValue(executeAs string) string {
	switch strings.ToUpper(executeAs) {
	case "", QueueExecuteAsOwner:
		return QueueExecuteAsOwner
	case QueueExecuteAsSelf:
		return QueueExecuteAsSelf
	default:
		return nStringLiteral(executeAs)
	}
}

// Alter changes the queue's settings and mirrors them onto the receiver.
func (q *BrokerQueue) Alter(s QueueSettings) error {
	return q.AlterContext(context.Background(), s)
}

// AlterContext is the context-aware variant of Alter.
//
// The fields it changed are mirrored onto the receiver — see
// mirrorQueueSettings, which is also where EXECUTE AS SELF's one exception
// is.
func (q *BrokerQueue) AlterContext(ctx context.Context, s QueueSettings) error {
	if err := q.db.AlterBrokerQueueContext(ctx, q.Schema, q.Name, s); err != nil {
		return err
	}
	mirrorQueueSettings(ctx, q, s)
	return nil
}

// mirrorQueueSettings copies the settings an ALTER applied onto the queue.
// Every write goes through setIfApplied, which does nothing under WithScript:
// there the statement was collected and never ran, so the server's queue
// still has what it had.
func mirrorQueueSettings(ctx context.Context, q *BrokerQueue, s QueueSettings) {
	if s.Status != nil {
		// One STATUS moves both halves, because the server moves both.
		setIfApplied(ctx, &q.IsEnqueueEnabled, *s.Status)
		setIfApplied(ctx, &q.IsReceiveEnabled, *s.Status)
	}
	if s.Retention != nil {
		setIfApplied(ctx, &q.IsRetentionEnabled, *s.Retention)
	}
	if s.PoisonMessageHandling != nil {
		setIfApplied(ctx, &q.IsPoisonMessageHandlingEnabled, *s.PoisonMessageHandling)
	}
	if a := s.Activation; a != nil {
		schema := a.ProcedureSchema
		if schema == "" {
			schema = "dbo"
		}
		setIfApplied(ctx, &q.IsActivationEnabled, a.Enabled)
		setIfApplied(ctx, &q.ActivationProcedure, qualifiedName(schema, a.ProcedureName))
		setIfApplied(ctx, &q.ActivationProcedureSchema, schema)
		setIfApplied(ctx, &q.ActivationProcedureName, a.ProcedureName)
		setIfApplied(ctx, &q.MaxReaders, a.MaxQueueReaders)
		// EXECUTE AS SELF is resolved server-side to whoever ran the
		// statement, and only a re-read can say which principal that was, so
		// the receiver keeps the one it had.
		if !strings.EqualFold(a.ExecuteAs, QueueExecuteAsSelf) {
			executeAs := a.ExecuteAs
			if executeAs == "" {
				executeAs = QueueExecuteAsOwner
			}
			setIfApplied(ctx, &q.ActivationExecuteAs, executeAs)
		}
	}
	if s.DropActivation {
		setIfApplied(ctx, &q.IsActivationEnabled, false)
		setIfApplied(ctx, &q.ActivationProcedure, "")
		setIfApplied(ctx, &q.ActivationProcedureSchema, "")
		setIfApplied(ctx, &q.ActivationProcedureName, "")
		setIfApplied(ctx, &q.MaxReaders, 0)
		setIfApplied(ctx, &q.ActivationExecuteAs, "")
	}
}
