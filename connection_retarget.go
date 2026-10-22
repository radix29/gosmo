package gosmo

import (
	"context"
	"database/sql/driver"
	"strings"
	"sync"
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
type retargetingConnector struct {
	mu   sync.Mutex
	cur  driver.Connector
	opts ConnectionOptions // as Connect built cur from, defaults applied
}

// newRetargetingConnector builds opts's connector. opts has its defaults
// applied, and an EntraCache, so a rebuilt connector signs in from the same
// tokens rather than prompting again.
func newRetargetingConnector(opts ConnectionOptions) (*retargetingConnector, error) {
	connector, err := buildConnector(opts)
	if err != nil {
		return nil, err
	}
	return &retargetingConnector{cur: poolConnector(connector, opts), opts: opts}, nil
}

func (c *retargetingConnector) current() driver.Connector {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

func (c *retargetingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	return c.current().Connect(ctx)
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
	connector, err := buildConnector(opts)
	if err != nil {
		return
	}
	c.cur, c.opts = poolConnector(connector, opts), opts
}

// InitialDatabase returns the database a new pooled connection logs in to:
// ConnectionOptions.Database ("master" when it was empty) — or the database
// gosmo moved the pool to after a write on this Server left that one
// unopenable. Dropping, detaching or taking offline the database the pool logs
// in to moves it to master; renaming it moves it to the new name. The
// comparison ignores case.
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
// unopenable, to database: master after a drop, detach or SET OFFLINE, the
// new name after a rename. Nothing changes under WithScript (nothing was
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
