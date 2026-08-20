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
