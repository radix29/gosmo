package gosmo

// Service Broker, part one — Message Types, Contracts and Services. Queues
// are in service_broker_queue.go; routes, remote service bindings and
// conversation priorities in service_broker_routing.go.
//
// Six of the seven families are database-scoped and *not* schema-scoped: a
// message type, a contract, a service, a route, a remote service binding and
// a broker priority each have an owner (principal_id) and no schema_id at
// all, so every name here is a single identifier. Queues are the exception —
// they are sys.objects rows of type 'SQ' with a schema — and that difference
// runs through the by-name finders, the scripts and the permissions a caller
// needs, which is why they have a file of their own.
//
// None of it is gated on the broker being enabled: ALTER DATABASE … SET
// ENABLE_BROKER decides whether messages are delivered, not whether these
// objects can be read, created or dropped. A database with the broker off
// still lists everything below.
//
// System membership is read three different ways, and the differences were
// measured rather than assumed (majors 13 and 17 answered identically):
//
//   - Message types, contracts and services: id < 65536 is the system range.
//   - Queues: is_ms_shipped, which sys.service_queues carries itself. Their
//     object_ids are ordinary sys.objects ids, nowhere near 65536, so the id
//     rule is meaningless for them.
//   - Routes: neither works, so nothing is marked — see the comment on Route.
//
// The two classifications cannot be made to agree and msdb is the proof:
// Database Mail's queues are is_ms_shipped = 1 while the same feature's
// services, message types and contract are all above 65536, in the user
// range. That asymmetry is SQL Server's, not a bug to be name-matched away.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// firstUserBrokerID is the first id SQL Server hands out to a user-created
// message type, contract, service or conversation priority; everything below
// it ships with the instance. It does not apply to queues (ordinary object
// ids) or to routes (AutoCreatedLocal is 65536 itself).
const firstUserBrokerID = 65536

// ============================================================
// Message types
// ============================================================

// MessageTypeValidation is the validation a message type applies to the
// body of every message sent under it. The constants are spelled as CREATE
// MESSAGE TYPE's VALIDATION clause takes them.
type MessageTypeValidation string

// The four validations CREATE MESSAGE TYPE accepts.
//
// sys.service_message_types.validation_desc distinguishes only three states —
// it reports BINARY for NONE, and plain XML for *both* WELL_FORMED_XML and
// VALID_XML WITH SCHEMA COLLECTION. The fourth is recovered from
// xml_collection_id, which is the only thing that tells them apart; a
// scripter that trusted validation_desc alone would turn a schema-validated
// message type into an unvalidated one.
const (
	// MessageTypeValidationNone accepts any body, including none.
	MessageTypeValidationNone MessageTypeValidation = "NONE"
	// MessageTypeValidationEmpty accepts only an empty body.
	MessageTypeValidationEmpty MessageTypeValidation = "EMPTY"
	// MessageTypeValidationWellFormedXML accepts any well-formed XML body.
	MessageTypeValidationWellFormedXML MessageTypeValidation = "WELL_FORMED_XML"
	// MessageTypeValidationValidXML accepts XML valid against the schema
	// collection the message type names.
	MessageTypeValidationValidXML MessageTypeValidation = "VALID_XML_WITH_SCHEMA_COLLECTION"
)

// MessageType mirrors a sys.service_message_types row.
type MessageType struct {
	db *Database

	Name          string
	MessageTypeID int

	// Owner is the database principal that owns the message type, empty when
	// principal_id is NULL or names a principal that no longer exists.
	Owner string

	// Validation is the decoded validation, schema collection included.
	Validation MessageTypeValidation

	// SchemaCollectionSchema and SchemaCollectionName name the XML schema
	// collection a MessageTypeValidationValidXML message type validates
	// against, unquoted. Both are empty for the other three validations.
	SchemaCollectionSchema string
	SchemaCollectionName   string

	// IsSystemObject reports one of the message types SQL Server ships —
	// the thirteen schemas.microsoft.com/… types and DEFAULT.
	IsSystemObject bool
}

// Database returns the database the message type belongs to.
func (mt *MessageType) Database() *Database { return mt.db }

// FullName returns the bracket-quoted name. A message type is not
// schema-scoped, so this is a single identifier — but message type names are
// conventionally URIs, and unquoted they do not parse.
func (mt *MessageType) FullName() string { return quoteIdent(mt.Name) }

// SchemaCollection returns the schema-qualified, bracket-quoted XML schema
// collection the message type validates against, or an empty string when it
// validates against none.
func (mt *MessageType) SchemaCollection() string {
	if mt.SchemaCollectionName == "" {
		return ""
	}
	return qualifiedName(mt.SchemaCollectionSchema, mt.SchemaCollectionName)
}

// messageTypeSelect is the SELECT the listing and the by-name finder share.
const messageTypeSelect = `
SELECT mt.message_type_id, mt.name, ISNULL(USER_NAME(mt.principal_id), ''),
       ISNULL(mt.validation_desc, ''),
       ISNULL(SCHEMA_NAME(x.schema_id), ''), ISNULL(x.name, '')
FROM   sys.service_message_types mt
LEFT   JOIN sys.xml_schema_collections x
         ON x.xml_collection_id = mt.xml_collection_id`

func scanMessageType(d *Database, scan func(...any) error) (*MessageType, error) {
	mt := &MessageType{db: d}
	var desc string
	if err := scan(&mt.MessageTypeID, &mt.Name, &mt.Owner, &desc,
		&mt.SchemaCollectionSchema, &mt.SchemaCollectionName); err != nil {
		return nil, err
	}
	mt.Validation = decodeMessageTypeValidation(desc, mt.SchemaCollectionName != "")
	mt.IsSystemObject = mt.MessageTypeID < firstUserBrokerID
	return mt, nil
}

// decodeMessageTypeValidation maps validation_desc plus the presence of a
// schema collection onto the clause CREATE MESSAGE TYPE was given.
func decodeMessageTypeValidation(desc string, hasCollection bool) MessageTypeValidation {
	switch desc {
	case "EMPTY":
		return MessageTypeValidationEmpty
	case "XML":
		if hasCollection {
			return MessageTypeValidationValidXML
		}
		return MessageTypeValidationWellFormedXML
	default:
		// BINARY, and anything a later release adds: no validation.
		return MessageTypeValidationNone
	}
}

// MessageTypes returns the message types defined in the database, the ones
// SQL Server ships included (marked IsSystemObject).
func (d *Database) MessageTypes() ([]*MessageType, error) {
	return d.MessageTypesContext(context.Background())
}

// MessageTypesContext is the context-aware variant of MessageTypes.
func (d *Database) MessageTypesContext(ctx context.Context) ([]*MessageType, error) {
	const q = messageTypeSelect + `
ORDER  BY mt.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list message types in %q: %w", d.name, err)
	}
	defer rows.Close()

	var out []*MessageType
	for rows.Next() {
		mt, err := scanMessageType(d, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list message types in %q: %w", d.name, err)
		}
		out = append(out, mt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list message types in %q: %w", d.name, err)
	}
	return out, nil
}

// MessageTypeByName returns one message type, or a not-found error
// (errors.Is ErrNotFound) when the database has none by that name.
func (d *Database) MessageTypeByName(name string) (*MessageType, error) {
	return d.MessageTypeByNameContext(context.Background(), name)
}

// MessageTypeByNameContext is the context-aware variant of MessageTypeByName.
func (d *Database) MessageTypeByNameContext(ctx context.Context, name string) (*MessageType, error) {
	var mt *MessageType
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		mt, err = scanMessageType(d, row.Scan)
		return err
	}, messageTypeSelect+`
WHERE  mt.name = @p1`, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: message type %q not found in %q", name, d.name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read message type %q in %q: %w", name, d.name, err)
	}
	return mt, nil
}

// DropMessageType drops a message type by name. A message type still named
// by a contract is refused by the server (Msg 3716) until the contract goes.
func (d *Database) DropMessageType(name string) error {
	return d.DropMessageTypeContext(context.Background(), name)
}

// DropMessageTypeContext is the context-aware variant of DropMessageType.
func (d *Database) DropMessageTypeContext(ctx context.Context, name string) error {
	if _, err := d.exec(ctx, "DROP MESSAGE TYPE "+quoteIdent(name)); err != nil {
		return fmt.Errorf("gosmo: drop message type %q: %w", name, err)
	}
	return nil
}

// Drop drops the message type.
func (mt *MessageType) Drop() error { return mt.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (mt *MessageType) DropContext(ctx context.Context) error {
	return mt.db.DropMessageTypeContext(ctx, mt.Name)
}

// ============================================================
// Contracts
// ============================================================

// ContractSender is the end of a conversation allowed to send a message
// type under a contract, spelled as CREATE CONTRACT's SENT BY clause.
type ContractSender string

// The three SENT BY values.
const (
	ContractSentByInitiator ContractSender = "INITIATOR"
	ContractSentByTarget    ContractSender = "TARGET"
	ContractSentByAny       ContractSender = "ANY"
)

// ContractMessage is one message type a contract carries, with the end
// allowed to send it.
type ContractMessage struct {
	// MessageType is the message type's name, unquoted.
	MessageType string
	// SentBy is the decoded is_sent_by_initiator / is_sent_by_target pair.
	SentBy ContractSender
}

// ServiceContract mirrors a sys.service_contracts row.
//
// It is named ServiceContract rather than Contract because the catalog view
// is sys.service_contracts and a bare Contract beside Service Broker's other
// families reads as a Go interface contract.
type ServiceContract struct {
	db *Database

	Name       string
	ContractID int

	// Owner is the database principal that owns the contract, empty when
	// principal_id is NULL or names a principal that no longer exists.
	Owner string

	// Messages are the message types the contract carries, in name order.
	// A contract with no messages is possible only for the system DEFAULT
	// contract.
	Messages []ContractMessage

	// IsSystemObject reports one of the contracts SQL Server ships.
	IsSystemObject bool
}

// Database returns the database the contract belongs to.
func (c *ServiceContract) Database() *Database { return c.db }

// FullName returns the bracket-quoted name. A contract is not schema-scoped,
// so this is a single identifier.
func (c *ServiceContract) FullName() string { return quoteIdent(c.Name) }

// contractSelect is the SELECT the listing and the by-name finder share.
const contractSelect = `
SELECT c.service_contract_id, c.name, ISNULL(USER_NAME(c.principal_id), '')
FROM   sys.service_contracts c`

// contractMessageSelect reads the message usages for every contract at once.
// It carries the contract id so the rows can be grouped in Go: a per-contract
// query inside the listing loop would be a round trip per contract while the
// listing's own connection is still held.
const contractMessageSelect = `
SELECT u.service_contract_id, mt.name,
       u.is_sent_by_initiator, u.is_sent_by_target
FROM   sys.service_contract_message_usages u
JOIN   sys.service_message_types mt ON mt.message_type_id = u.message_type_id`

// decodeContractSender decodes the two bits sys.service_contract_message_usages
// stores. Both set is SENT BY ANY — the case a one-column reading gets wrong,
// and the one that produces a contract script that does not parse.
//
// Neither set cannot occur: CREATE CONTRACT requires a SENT BY on every
// message type and the default is INITIATOR. ANY is the answer for it anyway,
// because a contract scripted as ANY accepts everything the original did.
func decodeContractSender(initiator, target bool) ContractSender {
	switch {
	case initiator && target:
		return ContractSentByAny
	case target:
		return ContractSentByTarget
	case initiator:
		return ContractSentByInitiator
	default:
		return ContractSentByAny
	}
}

// contractMessagesContext reads the message usages, optionally for one
// contract, and returns them grouped by contract id.
func (d *Database) contractMessagesContext(ctx context.Context, contractID int) (map[int][]ContractMessage, error) {
	q := contractMessageSelect
	var args []any
	if contractID != 0 {
		q += `
WHERE  u.service_contract_id = @p1`
		args = append(args, contractID)
	}
	q += `
ORDER  BY u.service_contract_id, mt.name`

	rows, err := d.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int][]ContractMessage)
	for rows.Next() {
		var id int
		var msg ContractMessage
		var initiator, target bool
		if err := rows.Scan(&id, &msg.MessageType, &initiator, &target); err != nil {
			return nil, err
		}
		msg.SentBy = decodeContractSender(initiator, target)
		out[id] = append(out[id], msg)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// contractListContext returns the contracts with no messages attached. Its
// rows are drained and closed before the caller asks for the messages, so the
// two queries never hold two pooled connections at once.
func (d *Database) contractListContext(ctx context.Context) ([]*ServiceContract, error) {
	const q = contractSelect + `
ORDER  BY c.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*ServiceContract
	for rows.Next() {
		c := &ServiceContract{db: d}
		if err := rows.Scan(&c.ContractID, &c.Name, &c.Owner); err != nil {
			return nil, err
		}
		c.IsSystemObject = c.ContractID < firstUserBrokerID
		out = append(out, c)
	}
	return out, rows.Err()
}

// Contracts returns the service contracts defined in the database, the ones
// SQL Server ships included (marked IsSystemObject), each with its messages.
func (d *Database) Contracts() ([]*ServiceContract, error) {
	return d.ContractsContext(context.Background())
}

// ContractsContext is the context-aware variant of Contracts. Two queries:
// the message usages for every contract come in one read and are grouped in
// Go, never one read per contract inside the listing loop.
func (d *Database) ContractsContext(ctx context.Context) ([]*ServiceContract, error) {
	out, err := d.contractListContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list contracts in %q: %w", d.name, err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	byID, err := d.contractMessagesContext(ctx, 0)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list contracts in %q: %w", d.name, err)
	}
	for _, c := range out {
		c.Messages = byID[c.ContractID]
	}
	return out, nil
}

// ContractByName returns one service contract with its messages, or a
// not-found error (errors.Is ErrNotFound) when the database has none by that
// name.
func (d *Database) ContractByName(name string) (*ServiceContract, error) {
	return d.ContractByNameContext(context.Background(), name)
}

// ContractByNameContext is the context-aware variant of ContractByName.
func (d *Database) ContractByNameContext(ctx context.Context, name string) (*ServiceContract, error) {
	c := &ServiceContract{db: d}
	err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&c.ContractID, &c.Name, &c.Owner)
	}, contractSelect+`
WHERE  c.name = @p1`, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: contract %q not found in %q", name, d.name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read contract %q in %q: %w", name, d.name, err)
	}
	c.IsSystemObject = c.ContractID < firstUserBrokerID

	byID, err := d.contractMessagesContext(ctx, c.ContractID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: read contract %q in %q: %w", name, d.name, err)
	}
	c.Messages = byID[c.ContractID]
	return c, nil
}

// DropContract drops a service contract by name. A contract still named by a
// service or a conversation priority is refused by the server (Msg 3716).
func (d *Database) DropContract(name string) error {
	return d.DropContractContext(context.Background(), name)
}

// DropContractContext is the context-aware variant of DropContract.
func (d *Database) DropContractContext(ctx context.Context, name string) error {
	if _, err := d.exec(ctx, "DROP CONTRACT "+quoteIdent(name)); err != nil {
		return fmt.Errorf("gosmo: drop contract %q: %w", name, err)
	}
	return nil
}

// Drop drops the contract.
func (c *ServiceContract) Drop() error { return c.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (c *ServiceContract) DropContext(ctx context.Context) error {
	return c.db.DropContractContext(ctx, c.Name)
}

// ============================================================
// Services
// ============================================================

// BrokerService mirrors a sys.services row — a Service Broker service, the
// addressable endpoint a conversation names.
//
// The type is BrokerService rather than Service because gosmo already has
// Server, and a bare Service beside it reads as a Windows service.
type BrokerService struct {
	db *Database

	Name      string
	ServiceID int

	// Owner is the database principal that owns the service, empty when
	// principal_id is NULL or names a principal that no longer exists.
	Owner string

	// QueueSchema and QueueName name the queue the service receives on,
	// unquoted. Every service has one.
	QueueSchema string
	QueueName   string

	// Contracts are the contracts the service accepts conversations on, in
	// name order. A service created with no contract list accepts only the
	// system DEFAULT contract and has one entry here.
	Contracts []string

	// IsSystemObject reports one of the three services SQL Server ships —
	// QueryNotificationService, EventNotificationService, ServiceBroker.
	IsSystemObject bool
}

// Database returns the database the service belongs to.
func (s *BrokerService) Database() *Database { return s.db }

// FullName returns the bracket-quoted name. A service is not schema-scoped,
// so this is a single identifier.
func (s *BrokerService) FullName() string { return quoteIdent(s.Name) }

// Queue returns the schema-qualified, bracket-quoted name of the queue the
// service receives on.
func (s *BrokerService) Queue() string { return qualifiedName(s.QueueSchema, s.QueueName) }

// serviceSelect is the SELECT the listing and the by-name finder share.
//
// The join to sys.service_queues is a LEFT join even though every service has
// a queue: an inner join would drop a service whose queue the caller cannot
// see from the listing entirely, rather than showing it with an empty queue.
const serviceSelect = `
SELECT s.service_id, s.name, ISNULL(USER_NAME(s.principal_id), ''),
       ISNULL(SCHEMA_NAME(q.schema_id), ''), ISNULL(q.name, '')
FROM   sys.services s
LEFT   JOIN sys.service_queues q ON q.object_id = s.service_queue_id`

// serviceContractSelect reads the contract usages for every service at once,
// carrying the service id so the rows can be grouped in Go — same reason as
// contractMessageSelect.
const serviceContractSelect = `
SELECT u.service_id, c.name
FROM   sys.service_contract_usages u
JOIN   sys.service_contracts c ON c.service_contract_id = u.service_contract_id`

// serviceContractsContext reads the contract usages, optionally for one
// service, grouped by service id.
func (d *Database) serviceContractsContext(ctx context.Context, serviceID int) (map[int][]string, error) {
	q := serviceContractSelect
	var args []any
	if serviceID != 0 {
		q += `
WHERE  u.service_id = @p1`
		args = append(args, serviceID)
	}
	q += `
ORDER  BY u.service_id, c.name`

	rows, err := d.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int][]string)
	for rows.Next() {
		var id int
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = append(out[id], name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// brokerServiceListContext returns the services with no contracts attached,
// draining and closing its rows before the caller asks for the contracts.
func (d *Database) brokerServiceListContext(ctx context.Context) ([]*BrokerService, error) {
	const q = serviceSelect + `
ORDER  BY s.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*BrokerService
	for rows.Next() {
		s := &BrokerService{db: d}
		if err := rows.Scan(&s.ServiceID, &s.Name, &s.Owner,
			&s.QueueSchema, &s.QueueName); err != nil {
			return nil, err
		}
		s.IsSystemObject = s.ServiceID < firstUserBrokerID
		out = append(out, s)
	}
	return out, rows.Err()
}

// BrokerServices returns the Service Broker services defined in the
// database, the ones SQL Server ships included (marked IsSystemObject), each
// with its contracts.
func (d *Database) BrokerServices() ([]*BrokerService, error) {
	return d.BrokerServicesContext(context.Background())
}

// BrokerServicesContext is the context-aware variant of BrokerServices. Two
// queries, the same shape as ContractsContext.
func (d *Database) BrokerServicesContext(ctx context.Context) ([]*BrokerService, error) {
	out, err := d.brokerServiceListContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list services in %q: %w", d.name, err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	byID, err := d.serviceContractsContext(ctx, 0)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list services in %q: %w", d.name, err)
	}
	for _, s := range out {
		s.Contracts = byID[s.ServiceID]
	}
	return out, nil
}

// BrokerServiceByName returns one service with its contracts, or a not-found
// error (errors.Is ErrNotFound) when the database has none by that name.
func (d *Database) BrokerServiceByName(name string) (*BrokerService, error) {
	return d.BrokerServiceByNameContext(context.Background(), name)
}

// BrokerServiceByNameContext is the context-aware variant of
// BrokerServiceByName.
func (d *Database) BrokerServiceByNameContext(ctx context.Context, name string) (*BrokerService, error) {
	s := &BrokerService{db: d}
	err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&s.ServiceID, &s.Name, &s.Owner, &s.QueueSchema, &s.QueueName)
	}, serviceSelect+`
WHERE  s.name = @p1`, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: service %q not found in %q", name, d.name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read service %q in %q: %w", name, d.name, err)
	}
	s.IsSystemObject = s.ServiceID < firstUserBrokerID

	byID, err := d.serviceContractsContext(ctx, s.ServiceID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: read service %q in %q: %w", name, d.name, err)
	}
	s.Contracts = byID[s.ServiceID]
	return s, nil
}

// DropBrokerService drops a service by name. A service with conversations
// still open on it is refused by the server.
func (d *Database) DropBrokerService(name string) error {
	return d.DropBrokerServiceContext(context.Background(), name)
}

// DropBrokerServiceContext is the context-aware variant of DropBrokerService.
func (d *Database) DropBrokerServiceContext(ctx context.Context, name string) error {
	if _, err := d.exec(ctx, "DROP SERVICE "+quoteIdent(name)); err != nil {
		return fmt.Errorf("gosmo: drop service %q: %w", name, err)
	}
	return nil
}

// Drop drops the service.
func (s *BrokerService) Drop() error { return s.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (s *BrokerService) DropContext(ctx context.Context) error {
	return s.db.DropBrokerServiceContext(ctx, s.Name)
}
