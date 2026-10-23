package gosmo

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"

	mssql "github.com/microsoft/go-mssqldb"
)

// retargetingConnector is the connector a pool Connect opens is given: the
// driver connector poolConnector builds, swappable for one logging in to
// another database.
//
// It exists because a login names its database (ConnectionOptions.Database)
// and fails outright when that database cannot be opened — Msg 4060, "Cannot
// open database "Sales" requested by the login". A Server opened on Sales that
// dropped, detached or took Sales offline itself found every connection the
// pool dialled afterwards refused, so each read failed until the caller
// reconnected; one that renamed it, the same. leftLoginDatabase moves the
// pool to the database the write left the server in instead.
//
// Predicting the refusal cannot cover every cause: another session can take
// the database offline, restore it WITH NORECOVERY or hold it SINGLE_USER, and
// a SINGLE_USER this Server set itself refuses every login but the one session
// holding it. So once fallBack is set, Connect also reacts: a dial the login
// database refuses (loginDatabaseRefused) moves the pool to master and dials
// once more.
type retargetingConnector struct {
	mu   sync.Mutex
	cur  driver.Connector
	opts ConnectionOptions // as Connect built cur from, defaults applied

	// build makes the connector for opts: buildPoolConnector, or a fake in a
	// test.
	build func(ConnectionOptions) (driver.Connector, error)

	// fallBack enables the move to master on a refused dial. Connect sets it
	// once the pool has logged in and the server info is read, so a login
	// database refused from the start — misspelled, or not the login's to
	// open — fails Connect with the server's error rather than landing in
	// master unannounced. It stays unset on Azure SQL Database, where master
	// is a separate logical database that reads cannot reach any other from.
	fallBack bool
}

// newRetargetingConnector builds opts's connector. opts has its defaults
// applied, and an EntraCache, so a rebuilt connector signs in from the same
// tokens rather than prompting again.
func newRetargetingConnector(opts ConnectionOptions) (*retargetingConnector, error) {
	return newRetargetingConnectorWith(opts, buildPoolConnector)
}

func newRetargetingConnectorWith(opts ConnectionOptions, build func(ConnectionOptions) (driver.Connector, error)) (*retargetingConnector, error) {
	connector, err := build(opts)
	if err != nil {
		return nil, err
	}
	return &retargetingConnector{cur: connector, opts: opts, build: build}, nil
}

// buildPoolConnector is the connector sql.OpenDB is given for opts.
func buildPoolConnector(opts ConnectionOptions) (driver.Connector, error) {
	connector, err := buildConnector(opts)
	if err != nil {
		return nil, err
	}
	return poolConnector(connector, opts), nil
}

func (c *retargetingConnector) current() driver.Connector {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

// Connect dials the current connector. A dial refused because the login
// database cannot be opened is retried once on master, when fallBack is set
// and the pool was not already logging in to master; the pool stays there
// (see InitialDatabase), as it does after leftLoginDatabase.
func (c *retargetingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	c.mu.Lock()
	cur, database, fallBack := c.cur, c.opts.Database, c.fallBack
	c.mu.Unlock()
	conn, err := cur.Connect(ctx)
	if err == nil || !fallBack || strings.EqualFold(database, "master") || !loginDatabaseRefused(err) {
		return conn, err
	}
	c.retarget(database, "master")
	next := c.current()
	if next == cur {
		return nil, err // the rebuild failed: the refusal is the answer
	}
	return next.Connect(ctx)
}

// enableFallBack sets fallBack; see the field.
func (c *retargetingConnector) enableFallBack() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fallBack = true
}

// loginRefusalNumbers are the errors a login fails with when the server
// accepted the credentials but cannot put the session in its database. 17, 14
// and 13 send 4063 alone for a SQL login, whatever the cause (offline,
// RESTORING, SINGLE_USER held, missing — probed 2026-10-23); the others are
// documented forms, and err.All is searched as well as the headline in case
// a login's own failure (Msg 18456) comes last:
//
//	4060 Cannot open database "X" requested by the login.
//	4063 Cannot open database "X" that was requested by the login. Using
//	     the user default database "master" instead. (Despite the wording,
//	     the login fails.)
//	 924 Database 'X' is already open and can only have one user at a time.
//	 927 Database 'X' cannot be opened. It is in the middle of a restore.
//	 942 Database 'X' cannot be opened because it is offline.
//	 952 Database 'X' is in transition.
//	 976 The target database, 'X', is participating in an availability
//	     group and is currently not accessible for queries.
var loginRefusalNumbers = []int32{4060, 4063, 924, 927, 942, 952, 976}

// loginDatabaseRefused reports whether err is a dial the login database
// refused (loginRefusalNumbers).
func loginDatabaseRefused(err error) bool {
	me, ok := errors.AsType[mssql.Error](err)
	if !ok {
		return false
	}
	for _, e := range append([]mssql.Error{me}, me.All...) {
		for _, n := range loginRefusalNumbers {
			if e.Number == n {
				return true
			}
		}
	}
	return false
}

func (c *retargetingConnector) Driver() driver.Driver { return c.current().Driver() }

// database is the database new connections log in to.
func (c *retargetingConnector) database() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opts.Database
}

// retarget makes connections dialled from now on log in to database, if the
// pool still logs in to from; connections already open keep theirs. Only the
// database differs from what built the current connector, so a failure to
// rebuild is not expected; it leaves the connector as it was.
func (c *retargetingConnector) retarget(from, database string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !strings.EqualFold(c.opts.Database, from) {
		return
	}
	opts := c.opts
	opts.Database = database
	connector, err := c.build(opts)
	if err != nil {
		return
	}
	c.cur, c.opts = connector, opts
}

// InitialDatabase returns the database a new pooled connection logs in to:
// ConnectionOptions.Database ("master" when it was empty) — or the database
// gosmo moved the pool to after that one became unopenable. Dropping,
// detaching or taking offline the database the pool logs in to, restoring it
// WITH NORECOVERY, setting it SINGLE_USER or unjoining it from an availability
// group on a secondary moves the pool to master; renaming it moves it to the
// new name. So does a login the database refuses for any other reason —
// another session's write, say — once Connect has returned, except on Azure
// SQL Database. The pool does not move back. The comparison ignores case.
//
// It is "" for a Server from NewServer, whose pool gosmo did not open and
// never moves. A caller cloning this Server's ConnectionOptions for another
// connection uses it for Database, so the clone is not refused at login.
func (s *Server) InitialDatabase() string {
	if s == nil || s.login == nil {
		return ""
	}
	return s.login.database()
}

// leftLoginDatabase moves the pool off from after a write that leaves it
// unopenable, to database: master after a drop, detach, SET OFFLINE, RESTORE
// WITH NORECOVERY, SET SINGLE_USER or SET HADR OFF, the new name after a
// rename. Connect would move it on the next refused dial anyway; this saves
// that dial, and releases the idle connections, whose reset into from would
// fail the next statement. Nothing changes under WithScript (nothing was
// written), for a NewServer pool, or when from is not the pool's database.
//
// Idle connections are released too: a session left in a renamed or dropped
// database would answer its next statement from there, or not at all.
func (s *Server) leftLoginDatabase(ctx context.Context, from, database string) {
	if Scripting(ctx) || s.login == nil || !strings.EqualFold(s.login.database(), from) {
		return
	}
	s.login.retarget(from, database)
	s.releaseIdle(ctx)
}
