package gosmo

// Service Broker, part two — Routes, Remote Service Bindings and Broker
// Priorities. Message types, contracts and services are in
// service_broker.go, queues in service_broker_queue.go; the first of those
// file comments covers what the three families here share with them:
// database scope, no schema, and an owner rather than a schema_id.
//
// None of the three can be created here. Routes can be altered — a route is
// repointed at a new address in operation, which is why ALTER ROUTE is the
// one write in this file; remote service bindings and broker priorities are
// listed, found by name and dropped.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ============================================================
// Routes
// ============================================================

// Route mirrors a sys.routes row — where the broker sends messages addressed
// to a remote service.
//
// No route is ever marked a system object, deliberately. sys.routes has no
// is_ms_shipped and no system id range to test: AutoCreatedLocal, which
// exists in every database and is created by SQL Server, has route_id 65536 —
// inside the user range, one below the first route a user creates. SSMS lists
// it plainly and a user may legitimately drop it, so gosmo reports it as what
// the catalog says it is: an ordinary route.
type Route struct {
	db *Database

	Name    string
	RouteID int

	// Owner is the database principal that owns the route, empty when
	// principal_id is NULL or names a principal that no longer exists.
	Owner string

	// RemoteService is the service the route carries messages to, empty for
	// a route that matches any service.
	RemoteService string

	// BrokerInstance is the broker instance identifier the route is
	// restricted to, empty when it matches any.
	BrokerInstance string

	// Address is the network address, "LOCAL" for a route to a service in
	// this instance and "TRANSPORT" for one that takes the address from the
	// service name.
	Address string

	// MirrorAddress is the failover partner's address, empty when the route
	// has none.
	MirrorAddress string

	// Expires is when the route stops being used, in UTC, and is zero for a
	// route with no lifetime. The catalog stores the expiry instant, not the
	// LIFETIME seconds CREATE ROUTE was given; see Route.LifetimeSeconds.
	Expires time.Time

	// remainingMs is the time left until Expires as the *server* measured it
	// when the row was read, and readAt the client's clock at that moment.
	// LifetimeSeconds counts down from the pair, so a client clock that
	// disagrees with the server's does not shift the answer; see there.
	remainingMs int64
	readAt      time.Time
}

// Database returns the database the route belongs to.
func (r *Route) Database() *Database { return r.db }

// FullName returns the bracket-quoted name. A route is not schema-scoped, so
// this is a single identifier.
func (r *Route) FullName() string { return quoteIdent(r.Name) }

// LifetimeSeconds returns the seconds remaining until the route expires, for
// a script that has to restate it as CREATE ROUTE's LIFETIME clause, and 0
// for a route with no lifetime or one that has already expired.
//
// It is a remaining lifetime, not the original one: sys.routes keeps only the
// expiry instant, so the number CREATE ROUTE was given is not recoverable
// once any time has passed. A script generated from an expiring route
// therefore creates one that expires at the same instant, which is the
// closest a script can come to the original.
//
// The remaining time is measured on the server (SYSUTCDATETIME against the
// expiry) when the route is read, and only the time elapsed since is taken
// from the client. Subtracting the client's clock from Expires instead put
// every script out by the skew between the two machines: win10cli running a
// second ahead of the client read LIFETIME = 600 back as 601 remaining — a
// route that outlives the one it was scripted from. A Route not read from
// the catalog (built by hand) falls back to the client's clock.
func (r *Route) LifetimeSeconds() int {
	if r.Expires.IsZero() {
		return 0
	}
	var secs int
	if !r.readAt.IsZero() {
		secs = int((time.Duration(r.remainingMs)*time.Millisecond - time.Since(r.readAt)).Seconds())
	} else {
		secs = int(time.Until(r.Expires).Seconds())
	}
	if secs < 0 {
		return 0
	}
	return secs
}

// routeSelect is the SELECT the listing and the by-name finder share.
//
// lifetime is a datetime in UTC, which is what the scan reads; the driver
// hands back a time.Time with no location, so the value is stamped UTC here
// rather than being left to be read as local time an hour or more out.
//
// The last column is the time left, measured on the server's own clock — see
// Route.LifetimeSeconds. Milliseconds, not seconds: DATEDIFF counts boundaries
// crossed, so a second-grained difference can overstate by up to a second.
const routeSelect = `
SELECT r.route_id, r.name, ISNULL(USER_NAME(r.principal_id), ''),
       ISNULL(r.remote_service_name, ''), ISNULL(r.broker_instance, ''),
       ISNULL(r.address, ''), ISNULL(r.mirror_address, ''), r.lifetime,
       DATEDIFF_BIG(millisecond, SYSUTCDATETIME(), r.lifetime)
FROM   sys.routes r`

func scanRoute(d *Database, scan func(...any) error) (*Route, error) {
	r := &Route{db: d}
	var lifetime sql.NullTime
	var remainingMs sql.NullInt64
	if err := scan(&r.RouteID, &r.Name, &r.Owner, &r.RemoteService,
		&r.BrokerInstance, &r.Address, &r.MirrorAddress, &lifetime, &remainingMs); err != nil {
		return nil, err
	}
	if lifetime.Valid {
		t := lifetime.Time
		r.Expires = time.Date(t.Year(), t.Month(), t.Day(),
			t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
	}
	if remainingMs.Valid {
		r.remainingMs, r.readAt = remainingMs.Int64, time.Now()
	}
	return r, nil
}

// Routes returns the routes defined in the database, AutoCreatedLocal
// included.
func (d *Database) Routes(ctx context.Context) ([]*Route, error) {
	const q = routeSelect + `
ORDER  BY r.name`

	rows, err := d.query(ctx, q)
	return scanRows(rows, err, fmt.Sprintf("list routes in %q", d.Name), func(scan func(...any) error) (*Route, error) {
		return scanRoute(d, scan)
	})
}

// RouteByName returns one route, or a not-found error (errors.Is
// ErrNotFound) when the database has none by that name.
func (d *Database) RouteByName(ctx context.Context, name string) (*Route, error) {
	var r *Route
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		r, err = scanRoute(d, row.Scan)
		return err
	}, routeSelect+`
WHERE  r.name = @p1`, name)
	return foundRow(r, err, notFoundf("gosmo: route %q not found in %q", name, d.Name), fmt.Sprintf("read route %q in %q", name, d.Name))
}

// RouteRef returns a lightweight handle for a route by name, without
// querying the catalog — the counterpart of Server.DatabaseRef. Every field
// but the name stays at its zero value; RouteByName is what populates them.
//
// Every write on *Route addresses it by name, so this handle is enough to
// drop one the caller already knows exists — and is the form to use when
// there is nothing to read yet, such as a script of a CREATE that was only
// collected.
func (d *Database) RouteRef(name string) *Route {
	return &Route{db: d, Name: name}
}

// RouteSettings is what ALTER ROUTE can change. A nil field leaves that
// setting exactly as it is — the statement omits the clause.
//
// **There is no way to clear a setting a route already has.** `= NULL` is a
// parse error on every one of these clauses (Msg 156), an empty
// BROKER_INSTANCE or MIRROR_ADDRESS is refused by the server, and LIFETIME
// must be 1 or more. A route that has to lose its broker instance, mirror
// address or lifetime is dropped and created again; gosmo refuses the empty
// value here rather than sending a statement the server will refuse.
//
// At least one field must be set: ALTER ROUTE with no clause does not parse.
type RouteSettings struct {
	// RemoteService is SERVICE_NAME, the service the route carries messages
	// to. It is matched case-sensitively by the broker, byte for byte, even
	// on a case-insensitive collation.
	RemoteService *string

	// BrokerInstance is BROKER_INSTANCE, the instance identifier the route is
	// restricted to.
	BrokerInstance *string

	// LifetimeSeconds is LIFETIME, counted from when the statement runs.
	LifetimeSeconds *int

	// Address is ADDRESS — a TCP address, "LOCAL", or "TRANSPORT".
	Address *string

	// MirrorAddress is MIRROR_ADDRESS, the failover partner's address.
	MirrorAddress *string
}

// AlterRoute changes a route's settings.
//
// It needs ALTER ANY ROUTE, or ALTER on the database — measured on majors
// 13, 14 and 17, which answered identically. ALTER ANY ROUTE alone is enough
// for both this and the drop, so the two verbs share a right here, unlike a
// queue's.
func (d *Database) AlterRoute(ctx context.Context, name string, s RouteSettings) error {
	clauses, err := routeSettingClauses(s)
	if err != nil {
		return fmt.Errorf("gosmo: alter route %q: %w", name, err)
	}
	q := "ALTER ROUTE " + quoteIdent(name) + "\n    WITH " +
		strings.Join(clauses, ",\n         ")
	if _, err := d.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: alter route %q: %w", name, err)
	}
	return nil
}

// routeSettingClauses renders the WITH clauses, or reports why the settings
// cannot be a statement.
func routeSettingClauses(s RouteSettings) ([]string, error) {
	var clauses []string
	add := func(keyword string, v *string) error {
		if v == nil {
			return nil
		}
		if *v == "" {
			return fmt.Errorf("%s is empty, and ALTER ROUTE cannot clear a setting: "+
				"the server refuses an empty value and NULL does not parse", keyword)
		}
		clauses = append(clauses, keyword+" = "+QuoteLiteral(*v))
		return nil
	}
	if err := add("SERVICE_NAME", s.RemoteService); err != nil {
		return nil, err
	}
	if err := add("BROKER_INSTANCE", s.BrokerInstance); err != nil {
		return nil, err
	}
	if s.LifetimeSeconds != nil {
		if *s.LifetimeSeconds < 1 {
			return nil, fmt.Errorf("LifetimeSeconds is %d; LIFETIME must be 1 or more, "+
				"and a route's lifetime cannot be cleared by an ALTER", *s.LifetimeSeconds)
		}
		clauses = append(clauses, fmt.Sprintf("LIFETIME = %d", *s.LifetimeSeconds))
	}
	if err := add("ADDRESS", s.Address); err != nil {
		return nil, err
	}
	if err := add("MIRROR_ADDRESS", s.MirrorAddress); err != nil {
		return nil, err
	}
	if len(clauses) == 0 {
		return nil, errors.New("no setting was given; ALTER ROUTE with no clause does not parse")
	}
	return clauses, nil
}

// Alter changes the route's settings and mirrors them onto the receiver.
//
// The fields it changed are mirrored onto the receiver through setIfApplied,
// so under WithScript — where nothing ran — the route does not start
// claiming state the server does not have. Expires is mirrored as "now plus
// the lifetime", which is what the server computes, to within the round trip.
func (r *Route) Alter(ctx context.Context, s RouteSettings) error {
	if err := r.db.AlterRoute(ctx, r.Name, s); err != nil {
		return err
	}
	if s.RemoteService != nil {
		setIfApplied(ctx, &r.RemoteService, *s.RemoteService)
	}
	if s.BrokerInstance != nil {
		setIfApplied(ctx, &r.BrokerInstance, *s.BrokerInstance)
	}
	if s.Address != nil {
		setIfApplied(ctx, &r.Address, *s.Address)
	}
	if s.MirrorAddress != nil {
		setIfApplied(ctx, &r.MirrorAddress, *s.MirrorAddress)
	}
	if s.LifetimeSeconds != nil {
		setIfApplied(ctx, &r.Expires,
			time.Now().UTC().Add(time.Duration(*s.LifetimeSeconds)*time.Second))
	}
	return nil
}

// Drop drops the route.
func (r *Route) Drop(ctx context.Context) error {
	if _, err := r.db.exec(ctx, "DROP ROUTE "+quoteIdent(r.Name)); err != nil {
		return fmt.Errorf("gosmo: drop route %q: %w", r.Name, err)
	}
	return nil
}

// ============================================================
// Remote service bindings
// ============================================================

// RemoteServiceBinding mirrors a sys.remote_service_bindings row — the
// certificate-backed user a conversation to one remote service authenticates
// as.
type RemoteServiceBinding struct {
	db *Database

	Name      string
	BindingID int

	// Owner is the database principal that owns the binding, empty when
	// principal_id is NULL or names a principal that no longer exists.
	Owner string

	// RemoteService is the service the binding applies to.
	RemoteService string

	// User is the database user whose certificate authenticates the
	// conversation — CREATE REMOTE SERVICE BINDING's WITH USER clause.
	User string

	// IsAnonymous reports ANONYMOUS = ON: the conversation authenticates as
	// the remote instance's guest user rather than as a named one.
	IsAnonymous bool

	// Contract is the contract named by service_contract_id, and is empty
	// for every binding a caller will meet. CREATE REMOTE SERVICE BINDING
	// has no contract clause, so the column is 0 — not NULL — on a binding
	// created normally, and the join that resolves it must be an outer one
	// or the binding disappears from the listing entirely.
	Contract string
}

// Database returns the database the binding belongs to.
func (b *RemoteServiceBinding) Database() *Database { return b.db }

// FullName returns the bracket-quoted name. A remote service binding is not
// schema-scoped, so this is a single identifier.
func (b *RemoteServiceBinding) FullName() string { return quoteIdent(b.Name) }

// remoteServiceBindingSelect is the SELECT the listing and the by-name finder
// share.
const remoteServiceBindingSelect = `
SELECT b.remote_service_binding_id, b.name,
       ISNULL(USER_NAME(b.principal_id), ''),
       ISNULL(b.remote_service_name, ''),
       ISNULL(USER_NAME(b.remote_principal_id), ''),
       b.is_anonymous_on, ISNULL(c.name, '')
FROM   sys.remote_service_bindings b
LEFT   JOIN sys.service_contracts c
         ON c.service_contract_id = b.service_contract_id`

func scanRemoteServiceBinding(d *Database, scan func(...any) error) (*RemoteServiceBinding, error) {
	b := &RemoteServiceBinding{db: d}
	if err := scan(&b.BindingID, &b.Name, &b.Owner, &b.RemoteService,
		&b.User, &b.IsAnonymous, &b.Contract); err != nil {
		return nil, err
	}
	return b, nil
}

// RemoteServiceBindings returns the remote service bindings defined in the
// database.
func (d *Database) RemoteServiceBindings(ctx context.Context) ([]*RemoteServiceBinding, error) {
	const q = remoteServiceBindingSelect + `
ORDER  BY b.name`

	rows, err := d.query(ctx, q)
	return scanRows(rows, err, fmt.Sprintf("list remote service bindings in %q", d.Name), func(scan func(...any) error) (*RemoteServiceBinding, error) {
		return scanRemoteServiceBinding(d, scan)
	})
}

// RemoteServiceBindingByName returns one remote service binding, or a
// not-found error (errors.Is ErrNotFound) when the database has none by that
// name.
func (d *Database) RemoteServiceBindingByName(ctx context.Context, name string) (*RemoteServiceBinding, error) {
	var b *RemoteServiceBinding
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		b, err = scanRemoteServiceBinding(d, row.Scan)
		return err
	}, remoteServiceBindingSelect+`
WHERE  b.name = @p1`, name)
	return foundRow(b, err, notFoundf("gosmo: remote service binding %q not found in %q", name, d.Name), fmt.Sprintf("read remote service binding %q in %q", name, d.Name))
}

// RemoteServiceBindingRef returns a lightweight handle for a remote service binding by name, without
// querying the catalog — the counterpart of Server.DatabaseRef. Every field
// but the name stays at its zero value; RemoteServiceBindingByName is what populates them.
//
// Every write on *RemoteServiceBinding addresses it by name, so this handle is enough to
// drop one the caller already knows exists — and is the form to use when
// there is nothing to read yet, such as a script of a CREATE that was only
// collected.
func (d *Database) RemoteServiceBindingRef(name string) *RemoteServiceBinding {
	return &RemoteServiceBinding{db: d, Name: name}
}

// Drop drops the remote service binding.  The drop is accepted on Azure SQL
// Managed Instance, where the *create* is not: CREATE REMOTE SERVICE BINDING
// is refused there at compile time with Msg 41906, while ALTER and DROP parse
// and run normally.
func (b *RemoteServiceBinding) Drop(ctx context.Context) error {
	if _, err := b.db.exec(ctx, "DROP REMOTE SERVICE BINDING "+quoteIdent(b.Name)); err != nil {
		return fmt.Errorf("gosmo: drop remote service binding %q: %w", b.Name, err)
	}
	return nil
}

// ============================================================
// Broker priorities
// ============================================================

// BrokerPriority mirrors a sys.conversation_priorities row — the priority
// the broker gives conversations matching a contract, a local service and a
// remote service.
//
// Each of the three criteria is optional; an empty one means ANY, which is
// how CREATE BROKER PRIORITY spells a criterion it was not given.
type BrokerPriority struct {
	db *Database

	Name       string
	PriorityID int

	// Contract is the contract the priority applies to, empty for ANY.
	Contract string

	// LocalService is the local service the priority applies to, empty for
	// ANY.
	LocalService string

	// RemoteService is the remote service name the priority applies to,
	// empty for ANY. It is a name, not an object: the remote service lives
	// in another database or instance, so the catalog stores its text.
	RemoteService string

	// Level is PRIORITY_LEVEL, 1 (lowest) to 10 (highest).
	Level int
}

// Database returns the database the priority belongs to.
func (p *BrokerPriority) Database() *Database { return p.db }

// FullName returns the bracket-quoted name. A broker priority is not
// schema-scoped, so this is a single identifier.
func (p *BrokerPriority) FullName() string { return quoteIdent(p.Name) }

// brokerPrioritySelect is the SELECT the listing and the by-name finder
// share. Both joins are outer: a NULL criterion means ANY, and an inner join
// would drop every priority that does not name all three.
//
// sys.conversation_priorities has no principal_id — a broker priority has no
// owner — and no system range: the view is empty until a user creates one.
const brokerPrioritySelect = `
SELECT p.priority_id, p.name, ISNULL(c.name, ''), ISNULL(s.name, ''),
       ISNULL(p.remote_service_name, ''), p.priority
FROM   sys.conversation_priorities p
LEFT   JOIN sys.service_contracts c
         ON c.service_contract_id = p.service_contract_id
LEFT   JOIN sys.services s ON s.service_id = p.local_service_id`

func scanBrokerPriority(d *Database, scan func(...any) error) (*BrokerPriority, error) {
	p := &BrokerPriority{db: d}
	if err := scan(&p.PriorityID, &p.Name, &p.Contract, &p.LocalService,
		&p.RemoteService, &p.Level); err != nil {
		return nil, err
	}
	return p, nil
}

// BrokerPriorities returns the conversation priorities defined in the
// database.
func (d *Database) BrokerPriorities(ctx context.Context) ([]*BrokerPriority, error) {
	const q = brokerPrioritySelect + `
ORDER  BY p.name`

	rows, err := d.query(ctx, q)
	return scanRows(rows, err, fmt.Sprintf("list broker priorities in %q", d.Name), func(scan func(...any) error) (*BrokerPriority, error) {
		return scanBrokerPriority(d, scan)
	})
}

// BrokerPriorityByName returns one conversation priority, or a not-found
// error (errors.Is ErrNotFound) when the database has none by that name.
func (d *Database) BrokerPriorityByName(ctx context.Context, name string) (*BrokerPriority, error) {
	var p *BrokerPriority
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		p, err = scanBrokerPriority(d, row.Scan)
		return err
	}, brokerPrioritySelect+`
WHERE  p.name = @p1`, name)
	return foundRow(p, err, notFoundf("gosmo: broker priority %q not found in %q", name, d.Name), fmt.Sprintf("read broker priority %q in %q", name, d.Name))
}

// BrokerPriorityRef returns a lightweight handle for a conversation priority by name, without
// querying the catalog — the counterpart of Server.DatabaseRef. Every field
// but the name stays at its zero value; BrokerPriorityByName is what populates them.
//
// Every write on *BrokerPriority addresses it by name, so this handle is enough to
// drop one the caller already knows exists — and is the form to use when
// there is nothing to read yet, such as a script of a CREATE that was only
// collected.
func (d *Database) BrokerPriorityRef(name string) *BrokerPriority {
	return &BrokerPriority{db: d, Name: name}
}

// Drop drops the broker priority.  It needs ALTER on the database: SQL Server
// enforces a BROKER PRIORITY permission it does not publish, so there is no
// ALTER ANY … to grant instead — HAS_PERMS_BY_NAME answers NULL for every
// spelling of one.
func (p *BrokerPriority) Drop(ctx context.Context) error {
	if _, err := p.db.exec(ctx, "DROP BROKER PRIORITY "+quoteIdent(p.Name)); err != nil {
		return fmt.Errorf("gosmo: drop broker priority %q: %w", p.Name, err)
	}
	return nil
}
