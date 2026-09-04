package gosmo

// endpoint.go models the database mirroring endpoint — the TCP listener
// replicas use to ship log to each other. Always On is what it exists for
// today: a replica cannot join a group, and a group cannot be created naming
// it, until every instance involved has one started and has granted the other
// instances' service accounts CONNECT on it.
//
// An instance can have at most **one** database mirroring endpoint, whatever
// it is called and however many availability groups use it. That is a server
// rule, not a convention: CREATE ENDPOINT ... FOR DATABASE_MIRRORING fails with
// error 1801-family "The Database Mirroring endpoint already exists" on the
// second one. So a second availability group on the same pair of instances
// reuses the first one's endpoint and port rather than getting its own, and
// code that sets a group up should read the endpoint before considering
// creating it.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// DatabaseMirroringEndpoint is an instance's database mirroring endpoint.
type DatabaseMirroringEndpoint struct {
	server *Server

	Name string
	Port int

	// State is STARTED, STOPPED or DISABLED. Only a STARTED endpoint accepts
	// connections, and an endpoint left STOPPED is the usual reason a replica
	// that looks correctly configured never synchronizes.
	State string

	// Role is ALL, PARTNER or WITNESS. Availability groups need ALL.
	Role string

	IsEncryptionEnabled bool

	// EncryptionAlgorithm is AES, RC4, or one of the mixed forms. RC4 is
	// deprecated and refused outright on recent versions.
	EncryptionAlgorithm string

	// ConnectionAuth is how the far end proves who it is — NTLM, KERBEROS,
	// NEGOTIATE, CERTIFICATE, or one of the combined forms. A Linux
	// availability group is normally CERTIFICATE, since the instances share no
	// domain; a Windows one is normally NEGOTIATE.
	ConnectionAuth string

	// Owner is the login that owns the endpoint.
	Owner string
}

// Server returns the connection this endpoint was read from.
func (e *DatabaseMirroringEndpoint) Server() *Server { return e.server }

// URL is the endpoint's address as an availability replica's ENDPOINT_URL —
// "tcp://<server>:<port>", built from the instance's own name.
//
// The host is the server's name rather than whatever address the client
// connected through, because this string is consumed by the *other* replicas:
// they resolve it themselves, and the address that reached this instance from
// here may be meaningless there.
func (e *DatabaseMirroringEndpoint) URL() string {
	host := ""
	if e.server != nil && e.server.Info() != nil {
		host = e.server.Name()
	}
	return endpointURL(host, e.Port)
}

// endpointURL formats the address, dropping a named instance's suffix: a
// named instance's @@SERVERNAME is HOST\INSTANCE, but an endpoint is a
// server-wide TCP port and the suffix is not part of its address.
func endpointURL(host string, port int) string {
	if i := strings.IndexByte(host, '\\'); i >= 0 {
		host = host[:i]
	}
	return fmt.Sprintf("tcp://%s:%d", host, port)
}

// DatabaseMirroringEndpoint returns the instance's database mirroring
// endpoint, or nil when it has none.
func (s *Server) DatabaseMirroringEndpoint() (*DatabaseMirroringEndpoint, error) {
	return s.DatabaseMirroringEndpointContext(context.Background())
}

// DatabaseMirroringEndpointContext is the context-aware variant of
// DatabaseMirroringEndpoint.
//
// Returns (nil, nil) when the instance has no such endpoint — a normal state
// on an instance that has never been put in an availability group, and not an
// error.
func (s *Server) DatabaseMirroringEndpointContext(ctx context.Context) (*DatabaseMirroringEndpoint, error) {
	const q = `
	SELECT e.name, ISNULL(t.port, 0), ISNULL(e.state_desc,''),
	       ISNULL(dme.role_desc,''), ISNULL(dme.is_encryption_enabled, 0),
	       ISNULL(dme.encryption_algorithm_desc,''), ISNULL(dme.connection_auth_desc,''),
	       ISNULL(SUSER_NAME(e.principal_id),'')
	FROM sys.database_mirroring_endpoints dme
	JOIN sys.endpoints e ON e.endpoint_id = dme.endpoint_id
	LEFT JOIN sys.tcp_endpoints t ON t.endpoint_id = e.endpoint_id`

	rows, err := s.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: read database mirroring endpoint on %q: %w", s.Name(), err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("gosmo: read database mirroring endpoint on %q: %w", s.Name(), err)
		}
		return nil, nil
	}
	e := &DatabaseMirroringEndpoint{server: s}
	if err := rows.Scan(&e.Name, &e.Port, &e.State, &e.Role, &e.IsEncryptionEnabled,
		&e.EncryptionAlgorithm, &e.ConnectionAuth, &e.Owner); err != nil {
		return nil, fmt.Errorf("gosmo: read database mirroring endpoint on %q: %w", s.Name(), err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: read database mirroring endpoint on %q: %w", s.Name(), err)
	}
	return e, nil
}

// EndpointSpec describes a database mirroring endpoint to create.
type EndpointSpec struct {
	// Name is the endpoint's name. Required; it is arbitrary and purely local
	// — replicas address each other by URL, never by endpoint name.
	Name string

	// Port is the TCP port to listen on. Zero means 5022, the conventional
	// database mirroring port.
	Port int

	// Role is ALL, PARTNER or WITNESS. Empty means ALL, which is what an
	// availability group replica needs.
	Role string

	// Authentication is the AUTHENTICATION clause — WINDOWS NEGOTIATE,
	// WINDOWS KERBEROS, CERTIFICATE <name>, and so on. Empty means WINDOWS
	// NEGOTIATE.
	//
	// Passed through as written, because the clause is a small grammar rather
	// than one keyword ("CERTIFICATE x", "WINDOWS NEGOTIATE CERTIFICATE x").
	// Instances with no domain in common — the usual Linux case — need a
	// certificate here, and the certificate has to already exist and have been
	// exchanged with every other replica.
	Authentication string

	// Encryption is the ENCRYPTION clause's state: REQUIRED, SUPPORTED or
	// DISABLED. Empty means REQUIRED.
	Encryption string

	// EncryptionAlgorithm is the ALGORITHM sub-clause. Empty omits it, leaving
	// the server's default; AES is the only sensible value on any supported
	// version.
	EncryptionAlgorithm string
}

var (
	endpointRoles      = map[string]bool{"ALL": true, "PARTNER": true, "WITNESS": true}
	endpointEncryption = map[string]bool{"REQUIRED": true, "SUPPORTED": true, "DISABLED": true}
	// The ALGORITHM sub-clause's whole grammar. The two-word forms name a
	// preference and a fallback, in that order, for a peer that offers only
	// the other.
	endpointAlgorithms = map[string]bool{"RC4": true, "AES": true, "AES RC4": true, "RC4 AES": true}
)

// normalized returns the spec with its defaults filled in and its
// keyword-valued parts validated and upper-cased. Authentication is left as
// written: the clause is a small grammar rather than one keyword.
//
// createEndpointStatement and the handle a scripted create hands back are both
// built from this, so the statement and the handle cannot disagree about what
// was asked for.
func (spec EndpointSpec) normalized() (EndpointSpec, error) {
	if strings.TrimSpace(spec.Name) == "" {
		return spec, fmt.Errorf("endpoint has no name")
	}
	if spec.Port == 0 {
		spec.Port = 5022
	}
	if spec.Port < 1 || spec.Port > 65535 {
		return spec, fmt.Errorf("endpoint port %d out of range 1-65535", spec.Port)
	}
	spec.Role = strings.ToUpper(orElse(spec.Role, "ALL"))
	if !endpointRoles[spec.Role] {
		return spec, fmt.Errorf("unrecognized endpoint role %q", spec.Role)
	}
	spec.Encryption = strings.ToUpper(orElse(spec.Encryption, "REQUIRED"))
	if !endpointEncryption[spec.Encryption] {
		return spec, fmt.Errorf("unrecognized endpoint encryption %q", spec.Encryption)
	}
	spec.EncryptionAlgorithm = strings.ToUpper(spec.EncryptionAlgorithm)
	// Empty still means "omit the sub-clause"; anything else is checked, like
	// Role and Encryption above.
	if spec.EncryptionAlgorithm != "" && !endpointAlgorithms[spec.EncryptionAlgorithm] {
		return spec, fmt.Errorf("unrecognized endpoint encryption algorithm %q", spec.EncryptionAlgorithm)
	}
	spec.Authentication = orElse(spec.Authentication, "WINDOWS NEGOTIATE")
	return spec, nil
}

// createEndpointStatement builds the CREATE ENDPOINT statement, validating the
// keyword-valued parts of the spec.
func (spec EndpointSpec) createEndpointStatement() (string, error) {
	n, err := spec.normalized()
	if err != nil {
		return "", err
	}
	encryption := n.Encryption
	if n.EncryptionAlgorithm != "" {
		encryption += " ALGORITHM " + n.EncryptionAlgorithm
	}

	return fmt.Sprintf(
		"CREATE ENDPOINT %s STATE = STARTED AS TCP (LISTENER_PORT = %d, LISTENER_IP = ALL) "+
			"FOR DATABASE_MIRRORING (AUTHENTICATION = %s, ENCRYPTION = %s, ROLE = %s)",
		quoteIdent(n.Name), n.Port,
		n.Authentication, encryption, n.Role), nil
}

// handle builds the endpoint this spec describes without reading it back, for
// a scripted create where there is nothing on the server to read.
//
// State is STARTED because the CREATE statement says so. ConnectionAuth and
// Owner are left empty: the first is a server-side *_desc keyword rather than
// the spec's clause text, and the second is decided by the connection that
// runs the script, which is not necessarily this one.
func (spec EndpointSpec) handle(s *Server) *DatabaseMirroringEndpoint {
	n, _ := spec.normalized() // already validated by createEndpointStatement
	return &DatabaseMirroringEndpoint{
		server:              s,
		Name:                n.Name,
		Port:                n.Port,
		State:               "STARTED",
		Role:                n.Role,
		IsEncryptionEnabled: n.Encryption != "DISABLED",
		EncryptionAlgorithm: n.EncryptionAlgorithm,
	}
}

// CreateDatabaseMirroringEndpoint creates the instance's database mirroring
// endpoint, started.
//
// Fails if the instance already has one, whatever it is named — see this
// file's doc comment. Read DatabaseMirroringEndpoint first and reuse what is
// there rather than treating "no endpoint of my name" as "no endpoint".
func (s *Server) CreateDatabaseMirroringEndpoint(spec EndpointSpec) (*DatabaseMirroringEndpoint, error) {
	return s.CreateDatabaseMirroringEndpointContext(context.Background(), spec)
}

// CreateDatabaseMirroringEndpointContext is the context-aware variant of
// CreateDatabaseMirroringEndpoint.
func (s *Server) CreateDatabaseMirroringEndpointContext(ctx context.Context, spec EndpointSpec) (*DatabaseMirroringEndpoint, error) {
	stmt, err := spec.createEndpointStatement()
	if err != nil {
		return nil, fmt.Errorf("gosmo: create database mirroring endpoint on %q: %w", s.Name(), err)
	}
	if err := s.execContext(ctx, stmt); err != nil {
		return nil, fmt.Errorf("gosmo: create database mirroring endpoint %q on %q: %w", spec.Name, s.Name(), err)
	}
	if Scripting(ctx) {
		// The read-back is a real query and the CREATE above was only
		// collected, so it would find no endpoint and return (nil, nil) —
		// indistinguishable from a failed create, and leaving the caller
		// nothing to script the GRANT CONNECTs and the ALTERs against. Hand
		// out a handle built from the spec, as every other scripted create
		// does (see CreateScheduleContext).
		return spec.handle(s), nil
	}
	return s.DatabaseMirroringEndpointContext(ctx)
}

// Start starts a stopped endpoint. An endpoint that is not STARTED accepts no
// connections, so a replica behind one never synchronizes.
func (e *DatabaseMirroringEndpoint) Start() error { return e.StartContext(context.Background()) }

// StartContext is the context-aware variant of Start.
func (e *DatabaseMirroringEndpoint) StartContext(ctx context.Context) error {
	return e.setState(ctx, "STARTED")
}

// Stop stops the endpoint, breaking every replica connection through it.
func (e *DatabaseMirroringEndpoint) Stop() error { return e.StopContext(context.Background()) }

// StopContext is the context-aware variant of Stop.
func (e *DatabaseMirroringEndpoint) StopContext(ctx context.Context) error {
	return e.setState(ctx, "STOPPED")
}

func (e *DatabaseMirroringEndpoint) setState(ctx context.Context, state string) error {
	if err := e.server.execContext(ctx,
		fmt.Sprintf("ALTER ENDPOINT %s STATE = %s", quoteIdent(e.Name), state)); err != nil {
		return fmt.Errorf("gosmo: set endpoint %q state to %s: %w", e.Name, state, err)
	}
	setIfApplied(ctx, &e.State, state)
	return nil
}

// Drop deletes the endpoint.
func (e *DatabaseMirroringEndpoint) Drop() error { return e.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (e *DatabaseMirroringEndpoint) DropContext(ctx context.Context) error {
	if err := e.server.execContext(ctx, "DROP ENDPOINT "+quoteIdent(e.Name)); err != nil {
		return fmt.Errorf("gosmo: drop endpoint %q: %w", e.Name, err)
	}
	return nil
}

// GrantConnect grants a login CONNECT on the endpoint — what lets the other
// replicas' service accounts open a connection to it.
func (e *DatabaseMirroringEndpoint) GrantConnect(login string) error {
	return e.GrantConnectContext(context.Background(), login)
}

// GrantConnectContext is the context-aware variant of GrantConnect.
func (e *DatabaseMirroringEndpoint) GrantConnectContext(ctx context.Context, login string) error {
	if strings.TrimSpace(login) == "" {
		return fmt.Errorf("gosmo: grant connect on endpoint %q: empty login", e.Name)
	}
	if err := e.server.execContext(ctx, fmt.Sprintf("GRANT CONNECT ON ENDPOINT::%s TO %s",
		quoteIdent(e.Name), quoteIdent(login))); err != nil {
		return fmt.Errorf("gosmo: grant connect on endpoint %q to %q: %w", e.Name, login, err)
	}
	return nil
}

// orElse returns s, or def when s is empty.
func orElse(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ============================================================
// Endpoints, generally
// ============================================================
//
// Everything above models the *database mirroring* endpoint specifically, for
// Always On. What follows is the general catalog view of sys.endpoints — every
// endpoint of every protocol and payload — for listing, inspecting, and
// starting/stopping/dropping one. The two are deliberately separate: a caller
// setting up an availability group wants the mirroring endpoint's role,
// encryption and connection auth, and a caller browsing the server wants the
// whole list; folding them together would make the first pay for the second.

// ErrSystemEndpoint is returned by Endpoint.SetState and Endpoint.Drop for one
// of the built-in endpoints (endpoint_id < 65536): the Dedicated Admin
// Connection, TSQL Local Machine, TSQL Named Pipes, TSQL Default TCP and TSQL
// Default VIA. SQL Server refuses those writes with a message that names
// neither the endpoint nor the reason, so the refusal happens here instead,
// where a caller can present it.
var ErrSystemEndpoint = errors.New("endpoint is a built-in system endpoint")

// firstUserEndpointID is the lowest endpoint_id SQL Server assigns to an
// endpoint someone created. Everything below it is built in and cannot be
// altered or dropped.
//
// This is why Endpoint has no lightweight Server.Endpoint(name) handle the way
// the other server-level families do: IsSystem is derived here from a scanned
// id, so a name-only handle would carry EndpointID 0, compute IsSystem true,
// and have refuseSystem reject every write on it. Adding such a handle means
// making IsSystem a tri-state or re-reading the endpoint first.
const firstUserEndpointID = 65536

// Endpoint mirrors a row of sys.endpoints — one server endpoint of any
// protocol and payload.
//
// Type-specific detail is not on this struct: MirroringDetail and
// ServiceBrokerDetail read it when it is wanted, so listing every endpoint
// costs one query rather than three.
type Endpoint struct {
	server *Server

	EndpointID int
	Name       string

	// Owner is the login that owns the endpoint, empty when this login cannot
	// resolve the principal.
	Owner string

	// Protocol is TCP, HTTP, SHARED_MEMORY, NAMED_PIPES or VIA.
	Protocol string

	// Type is the payload: TSQL, SERVICE_BROKER, DATABASE_MIRRORING or SOAP.
	Type string

	// State is STARTED, STOPPED or DISABLED. Only a STARTED endpoint accepts
	// connections.
	State string

	// IsAdmin marks the Dedicated Admin Connection.
	IsAdmin bool

	// Port is the TCP port, 0 for an endpoint on another protocol and for the
	// built-in TCP ones, which report 0 rather than the instance's real port.
	Port int

	// IsSystem marks one of the built-in endpoints, which cannot be altered or
	// dropped — see ErrSystemEndpoint.
	IsSystem bool
}

const endpointSelect = `
SELECT e.endpoint_id, e.name, ISNULL(SUSER_NAME(e.principal_id),''),
       ISNULL(e.protocol_desc,''), ISNULL(e.type_desc,''),
       ISNULL(e.state_desc,''), e.is_admin_endpoint, ISNULL(t.port, 0)
FROM   sys.endpoints e
LEFT   JOIN sys.tcp_endpoints t ON t.endpoint_id = e.endpoint_id`

// Endpoints returns every endpoint on the server, built-in ones included.
func (s *Server) Endpoints() ([]*Endpoint, error) {
	return s.EndpointsContext(context.Background())
}

// EndpointsContext is the context-aware variant of Endpoints.
func (s *Server) EndpointsContext(ctx context.Context) ([]*Endpoint, error) {
	rows, err := s.query(ctx, endpointSelect+`
ORDER  BY e.name`)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list endpoints: %w", err)
	}
	defer rows.Close()

	var endpoints []*Endpoint
	for rows.Next() {
		e, err := scanEndpoint(s, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list endpoints: %w", err)
		}
		endpoints = append(endpoints, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list endpoints: %w", err)
	}
	return endpoints, nil
}

// EndpointByName returns one endpoint, or a not-found error (errors.Is
// ErrNotFound) when the server has none by that name.
func (s *Server) EndpointByName(name string) (*Endpoint, error) {
	return s.EndpointByNameContext(context.Background(), name)
}

// EndpointByNameContext is the context-aware variant of EndpointByName.
func (s *Server) EndpointByNameContext(ctx context.Context, name string) (*Endpoint, error) {
	var e *Endpoint
	err := s.queryRow(ctx, func(row *sql.Row) error {
		var err error
		e, err = scanEndpoint(s, row.Scan)
		return err
	}, endpointSelect+`
WHERE  e.name = @p1`, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: endpoint %q not found", name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read endpoint %q: %w", name, err)
	}
	return e, nil
}

func scanEndpoint(s *Server, scan func(...any) error) (*Endpoint, error) {
	e := &Endpoint{server: s}
	if err := scan(&e.EndpointID, &e.Name, &e.Owner, &e.Protocol, &e.Type,
		&e.State, &e.IsAdmin, &e.Port); err != nil {
		return nil, err
	}
	e.IsSystem = e.EndpointID < firstUserEndpointID
	return e, nil
}

// EndpointState is the state an endpoint can be put into.
type EndpointState string

const (
	// EndpointStarted accepts connections.
	EndpointStarted EndpointState = "STARTED"
	// EndpointStopped refuses connections but still listens, answering with
	// an error rather than nothing.
	EndpointStopped EndpointState = "STOPPED"
	// EndpointDisabled does not listen at all.
	EndpointDisabled EndpointState = "DISABLED"
)

// SetState starts, stops or disables the endpoint.
func (e *Endpoint) SetState(state EndpointState) error {
	return e.SetStateContext(context.Background(), state)
}

// SetStateContext is the context-aware variant of SetState. A built-in
// endpoint is refused with ErrSystemEndpoint before any statement is built.
func (e *Endpoint) SetStateContext(ctx context.Context, state EndpointState) error {
	if err := e.refuseSystem("set the state of"); err != nil {
		return err
	}
	switch state {
	case EndpointStarted, EndpointStopped, EndpointDisabled:
	default:
		return fmt.Errorf("gosmo: set state of endpoint %q: unknown state %q", e.Name, state)
	}
	stmt := fmt.Sprintf("ALTER ENDPOINT %s STATE = %s", quoteIdent(e.Name), state)
	if err := e.server.execContext(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: set state of endpoint %q: %w", e.Name, err)
	}
	setIfApplied(ctx, &e.State, string(state))
	return nil
}

// Drop removes the endpoint.
func (e *Endpoint) Drop() error { return e.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop. A built-in endpoint is
// refused with ErrSystemEndpoint before any statement is built.
func (e *Endpoint) DropContext(ctx context.Context) error {
	if err := e.refuseSystem("drop"); err != nil {
		return err
	}
	if err := e.server.execContext(ctx, "DROP ENDPOINT "+quoteIdent(e.Name)); err != nil {
		return fmt.Errorf("gosmo: drop endpoint %q: %w", e.Name, err)
	}
	return nil
}

// refuseSystem is the guard both writes open with. It is checked here rather
// than left to the server because SQL Server's own refusal names neither the
// endpoint nor the reason.
func (e *Endpoint) refuseSystem(verb string) error {
	if e.IsSystem {
		return fmt.Errorf("gosmo: %s endpoint %q: %w", verb, e.Name, ErrSystemEndpoint)
	}
	return nil
}

// MirroringDetail reads the database mirroring settings of a
// DATABASE_MIRRORING endpoint — role, encryption and connection auth.
func (e *Endpoint) MirroringDetail() (*DatabaseMirroringEndpoint, error) {
	return e.MirroringDetailContext(context.Background())
}

// MirroringDetailContext is the context-aware variant of MirroringDetail.
//
// It returns (nil, nil) for an endpoint that is not a mirroring one, matching
// DatabaseMirroringEndpointContext's convention: a caller asking every
// endpoint for its mirroring detail branches on absence as the ordinary case.
// An instance has at most one mirroring endpoint, so the read needs no name.
func (e *Endpoint) MirroringDetailContext(ctx context.Context) (*DatabaseMirroringEndpoint, error) {
	if e.Type != "DATABASE_MIRRORING" {
		return nil, nil
	}
	return e.server.DatabaseMirroringEndpointContext(ctx)
}

// ServiceBrokerEndpointDetail is the SERVICE_BROKER-specific half of an
// endpoint, from sys.service_broker_endpoints.
type ServiceBrokerEndpointDetail struct {
	// IsMessageForwardingEnabled reports whether the endpoint forwards
	// messages it is not the destination for.
	IsMessageForwardingEnabled bool

	// MessageForwardingSize is the megabytes of storage the endpoint may use
	// for forwarded messages.
	MessageForwardingSize int

	// ConnectionAuth is NTLM, KERBEROS, NEGOTIATE, CERTIFICATE, or one of the
	// combined forms.
	ConnectionAuth string

	// EncryptionAlgorithm is AES, RC4, one of the mixed forms, or NONE.
	EncryptionAlgorithm string

	// CertificateName is the certificate the endpoint authenticates with,
	// empty when it authenticates by Windows credentials alone. The catalog
	// records only the id, and the name is what a script needs.
	CertificateName string
}

// ServiceBrokerDetail reads the Service Broker settings of a SERVICE_BROKER
// endpoint.
func (e *Endpoint) ServiceBrokerDetail() (*ServiceBrokerEndpointDetail, error) {
	return e.ServiceBrokerDetailContext(context.Background())
}

// ServiceBrokerDetailContext is the context-aware variant of
// ServiceBrokerDetail. It returns (nil, nil) for an endpoint that is not a
// Service Broker one, the same convention MirroringDetailContext follows.
func (e *Endpoint) ServiceBrokerDetailContext(ctx context.Context) (*ServiceBrokerEndpointDetail, error) {
	if e.Type != "SERVICE_BROKER" {
		return nil, nil
	}
	d := &ServiceBrokerEndpointDetail{}
	err := e.server.queryRowScan(ctx, `
SELECT sbe.is_message_forwarding_enabled, sbe.message_forwarding_size,
       ISNULL(sbe.connection_auth_desc,''), ISNULL(sbe.encryption_algorithm_desc,''),
       ISNULL(c.name,'')
FROM   sys.service_broker_endpoints sbe
LEFT   JOIN master.sys.certificates c ON c.certificate_id = sbe.certificate_id
WHERE  sbe.endpoint_id = @p1`, []any{e.EndpointID},
		&d.IsMessageForwardingEnabled, &d.MessageForwardingSize,
		&d.ConnectionAuth, &d.EncryptionAlgorithm, &d.CertificateName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: service broker detail for endpoint %q not found", e.Name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read service broker detail for endpoint %q: %w", e.Name, err)
	}
	return d, nil
}

// mirroringCertificateName resolves the certificate a DATABASE_MIRRORING
// endpoint authenticates with, empty when it authenticates by Windows
// credentials alone.
//
// It is a read of its own rather than a field on DatabaseMirroringEndpoint
// because Always On reads that struct on every replica check and does not need
// the certificate's name; only a generated script does.
func (e *Endpoint) mirroringCertificateName(ctx context.Context) (string, error) {
	var name string
	err := e.server.queryRowScan(ctx, `
SELECT ISNULL(c.name,'')
FROM   sys.database_mirroring_endpoints dme
LEFT   JOIN master.sys.certificates c ON c.certificate_id = dme.certificate_id
WHERE  dme.endpoint_id = @p1`, []any{e.EndpointID}, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("gosmo: read certificate of endpoint %q: %w", e.Name, err)
	}
	return name, nil
}
