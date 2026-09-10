package gosmo

import (
	"context"
	"database/sql/driver"
	"errors"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

// browserKeepAlive matches the TCP keep-alive go-mssqldb's own dialer applies
// (msdsn's default, 30s). browserDialer replaces that dialer wholesale, so it
// has to reproduce this rather than inherit it.
const browserKeepAlive = 30 * time.Second

// maxBrowserProbes bounds how many addresses a browser probe fans out to, so a
// host with a long DNS answer doesn't open a socket per record.
const maxBrowserProbes = 8

// browserDialer is the dialer gosmo installs when the target names an instance
// with no port, i.e. when the driver has to ask SQL Server Browser (UDP 1434)
// which port the instance listens on.
//
// It exists because that probe is a single unanswered datagram on a dual-stack
// host. go-mssqldb dials "udp" once, net picks whichever address the resolver
// returned first, and Browser very often answers on IPv4 only — a Windows host
// resolving to a global IPv6 address first therefore never gets a reply, the
// read times out, and the driver reports the empty result as "no instance
// matching '<name>'", which reads like a misspelled instance rather than a
// network fault. UDP has no happy-eyeballs fallback in net, so nothing recovers
// it. The fix is to probe every resolved address at once and take the first
// reply.
//
// TCP dials pass straight through to a stock net.Dialer: only the browser probe
// is dual-stack, and changing how the instance itself is reached is not this
// type's business. That is also where the fan-out's one assumption lives: it
// takes the port from whichever address answers first and lets go-mssqldb pick
// an address for the TCP dial independently, which is correct for a dual-stack
// host — the case this exists for — and wrong for round-robin DNS naming
// distinct machines, where the instance could be dialled on another machine's
// port. Accepted: the stock single-socket path probes one address and connects
// to a possibly different one too. Pinning the dial to the address that
// answered needs mssql.HostDialer rather than mssql.Dialer.
type browserDialer struct{}

// DialContext implements mssql.Dialer.
//
// A UDP dial — the Browser probe — returns a browserProbeConn, which answers
// from browserReplies when the same host was asked the same question recently
// and only opens sockets on a miss. See browserReplyCache.
func (browserDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "udp" && network != "udp4" && network != "udp6" {
		nd := &net.Dialer{KeepAlive: browserKeepAlive}
		return nd.DialContext(ctx, network, addr)
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return dialBrowserProbe(ctx, network, addr)
	}
	return &browserProbeConn{ctx: ctx, network: network, addr: addr, host: host}, nil
}

// dialBrowserProbe opens the sockets for one Browser probe: one per resolved
// address, up to maxBrowserProbes, fanned out when there is more than one.
func dialBrowserProbe(ctx context.Context, network, addr string) (net.Conn, error) {
	nd := &net.Dialer{KeepAlive: browserKeepAlive}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nd.DialContext(ctx, network, addr)
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) < 2 {
		// One address (or a literal IP, which LookupIPAddr returns as one) is
		// what the stock dialer already handles correctly.
		return nd.DialContext(ctx, network, addr)
	}

	var conns []net.Conn
	for _, ip := range ips {
		if len(conns) == maxBrowserProbes {
			break
		}
		c, err := nd.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			conns = append(conns, c)
		}
	}
	switch len(conns) {
	case 0:
		return nd.DialContext(ctx, network, addr) // report the stock error
	case 1:
		return conns[0], nil
	}
	return newFanOutConn(conns), nil
}

// browserReplyTTL is how long a Browser reply is reused. The reply maps an
// instance name to its TCP port, which changes only when the instance restarts
// on a dynamic port; a failed connection attempt evicts the entry sooner (see
// evictingConnector), so the TTL only bounds how long an entry nobody has
// failed on is trusted.
const browserReplyTTL = 2 * time.Minute

// browserReplies is the process-wide Browser reply cache. Process-wide rather
// than per pool because every pool to a host asks the same question — an
// application holding several connections to one instance (a browser, a query
// window, a monitor) would otherwise re-probe once per pool.
var browserReplies = &browserReplyCache{entries: map[string]browserReply{}}

// browserReplyCache remembers SQL Server Browser replies per host and request.
//
// go-mssqldb runs the Browser probe for every new physical connection to
// host\instance, not once per pool: a pool growing by eight connections sends
// eight datagrams and waits for eight replies, measured at a third of the time
// the growth takes on a LAN. Each of those probes is also a fresh chance for
// Browser to stay silent, and a silent probe fails that connection with "no
// instance matching", however many succeeded a moment earlier.
//
// The key is the host and the request datagram itself, so an all-instances
// request and a DAC request for one instance are never confused, and every
// instance on a host shares the one all-instances reply, which lists them all.
type browserReplyCache struct {
	mu      sync.Mutex
	entries map[string]browserReply
}

type browserReply struct {
	data    []byte
	expires time.Time
}

func browserReplyKey(host string, request []byte) string {
	return strings.ToLower(host) + "\x00" + string(request)
}

func (c *browserReplyCache) get(key string, now time.Time) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.entries[key]
	if !ok || !now.Before(r.expires) {
		return nil, false
	}
	return r.data, true
}

func (c *browserReplyCache) put(key string, data []byte, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Pruned on insert: the map holds one entry per host and request kind, so
	// the sweep is short, and nothing else would ever remove an entry for a
	// host the process stopped talking to.
	for k, r := range c.entries {
		if !now.Before(r.expires) {
			delete(c.entries, k)
		}
	}
	c.entries[key] = browserReply{data: slices.Clone(data), expires: now.Add(browserReplyTTL)}
}

// evictHost drops every reply cached for host, whatever was asked.
func (c *browserReplyCache) evictHost(host string) {
	prefix := strings.ToLower(host) + "\x00"
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if strings.HasPrefix(k, prefix) {
			delete(c.entries, k)
		}
	}
}

// browserProbeConn is the conn browserDialer returns for a Browser probe. It
// dials nothing until the request is written, because only the request says
// what is being asked: a cached answer to it is returned by the next Read
// without a socket, a DNS lookup or a datagram; otherwise the probe is sent
// for real, through dialBrowserProbe, and its first reply is cached.
//
// It is built for go-mssqldb's exchange — SetDeadline, one Write, one Read —
// and nothing more. It keeps the dial's ctx because the deferred dial must
// still honour it; the driver writes the request within that same call.
type browserProbeConn struct {
	ctx           context.Context
	network, addr string
	host          string

	key    string
	cached []byte // the reply to return, on a hit; nil after it is read
	inner  net.Conn

	// The driver sets its deadline before writing, when nothing is dialled
	// yet, so the deadlines are held here and applied once something is.
	deadline, readDeadline, writeDeadline time.Time
}

func (c *browserProbeConn) Write(b []byte) (int, error) {
	if c.inner != nil {
		return c.inner.Write(b)
	}
	c.key = browserReplyKey(c.host, b)
	if reply, ok := browserReplies.get(c.key, time.Now()); ok {
		c.cached = reply
		return len(b), nil
	}
	inner, err := dialBrowserProbe(c.ctx, c.network, c.addr)
	if err != nil {
		return 0, err
	}
	c.inner = inner
	if !c.deadline.IsZero() {
		inner.SetDeadline(c.deadline)
	}
	if !c.readDeadline.IsZero() {
		inner.SetReadDeadline(c.readDeadline)
	}
	if !c.writeDeadline.IsZero() {
		inner.SetWriteDeadline(c.writeDeadline)
	}
	return inner.Write(b)
}

func (c *browserProbeConn) Read(b []byte) (int, error) {
	if c.cached != nil {
		n := copy(b, c.cached)
		c.cached = nil
		return n, nil
	}
	if c.inner == nil {
		// Nothing was asked, or the one cached answer was already read: a
		// socket with no datagram coming would time out, so say that.
		return 0, os.ErrDeadlineExceeded
	}
	n, err := c.inner.Read(b)
	if err == nil && n > 0 && c.key != "" {
		browserReplies.put(c.key, b[:n], time.Now())
	}
	return n, err
}

func (c *browserProbeConn) Close() error {
	if c.inner == nil {
		return nil
	}
	return c.inner.Close()
}

// LocalAddr and RemoteAddr report the socket's once one is open; on a cache
// hit there is none. Nothing in the Browser exchange consults either.
func (c *browserProbeConn) LocalAddr() net.Addr {
	if c.inner == nil {
		return &net.UDPAddr{}
	}
	return c.inner.LocalAddr()
}

func (c *browserProbeConn) RemoteAddr() net.Addr {
	if c.inner == nil {
		return &net.UDPAddr{}
	}
	return c.inner.RemoteAddr()
}

func (c *browserProbeConn) SetDeadline(t time.Time) error {
	c.deadline = t
	if c.inner != nil {
		return c.inner.SetDeadline(t)
	}
	return nil
}

func (c *browserProbeConn) SetReadDeadline(t time.Time) error {
	c.readDeadline = t
	if c.inner != nil {
		return c.inner.SetReadDeadline(t)
	}
	return nil
}

func (c *browserProbeConn) SetWriteDeadline(t time.Time) error {
	c.writeDeadline = t
	if c.inner != nil {
		return c.inner.SetWriteDeadline(t)
	}
	return nil
}

// evictingConnector is the connector a pool gets when its dialer is
// browserDialer: any failed connection attempt evicts the host's cached
// Browser replies, so the next attempt asks Browser again.
//
// That is what makes caching the reply safe. An instance restarted on a new
// dynamic port leaves the cached port closed; the attempt dialling it fails,
// the entry goes, and the retry (gosmo's own, for a read) learns the new
// port. The eviction is deliberately indiscriminate — a wrong password evicts
// too — because it costs only one re-probe, where telling a stale port apart
// from every other failure would mean second-guessing the driver's errors.
type evictingConnector struct {
	*mssql.Connector
	host string
}

func (c evictingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		browserReplies.evictHost(c.host)
	}
	return conn, err
}

// poolConnector is what sql.OpenDB is given for connector: evictingConnector
// when connector probes Browser through browserDialer, connector itself
// otherwise. host is the one ParseServerAddress reads out of opts.Server,
// which is the host the driver sends the probe to.
func poolConnector(connector *mssql.Connector, opts ConnectionOptions) driver.Connector {
	if _, ok := connector.Dialer.(browserDialer); !ok {
		return connector
	}
	host, _, _ := ParseServerAddress(opts.Server)
	return evictingConnector{Connector: connector, host: host}
}

// fanOutConn is a net.Conn over several UDP sockets aimed at the same host on
// different addresses. A Write goes to all of them and a Read returns the first
// datagram any of them receives, which is what makes the SQL Server Browser
// probe indifferent to whether the host answers on IPv4 or IPv6.
//
// It is deliberately not a general-purpose conn: it suits a request/response
// datagram exchange where every address is the same peer, and nothing else.
type fanOutConn struct {
	conns []net.Conn

	// replies carries datagrams read off any socket and errs one error per
	// socket that has stopped reading. Both are buffered to len(conns), which
	// bounds errs exactly; replies is not bounded by it — one socket can answer
	// several times — so readLoop drops what does not fit rather than blocking.
	// Between the two, a reader goroutine never blocks on a caller that has
	// moved on, which is what lets Close return without waiting for them.
	replies chan []byte
	errs    chan error

	// failed counts the sockets that have reported an error and firstErr holds
	// the first, both latched across calls: Read consumes those events off errs
	// and the goroutines that sent them have exited, so a later Read counting
	// from zero again would block on channels nothing will ever feed. Read is
	// the sole reader of them; this conn is not for concurrent Reads.
	failed   int
	firstErr error

	closeOnce sync.Once
}

func newFanOutConn(conns []net.Conn) *fanOutConn {
	c := &fanOutConn{
		conns:   conns,
		replies: make(chan []byte, len(conns)),
		errs:    make(chan error, len(conns)),
	}
	for _, conn := range conns {
		go c.readLoop(conn)
	}
	return c
}

// readLoop pumps one socket until it fails — a read deadline, or Close closing
// it out from under the goroutine, which is how these are stopped.
//
// The send is non-blocking because a socket can answer more than once (a
// duplicate or a retransmitted Browser reply) and replies only holds len(conns)
// of them: a blocking send past that capacity parks this goroutine forever,
// since Close closes the socket without unblocking a goroutine already past its
// Read and nothing drains replies once the exchange is over. A request/response
// probe wants the first datagram, so the surplus is dropped.
func (c *fanOutConn) readLoop(conn net.Conn) {
	for {
		buf := make([]byte, 64*1024)
		n, err := conn.Read(buf)
		if err != nil {
			c.errs <- err
			return
		}
		select {
		case c.replies <- buf[:n]:
		default: // already queued more than anyone will read
		}
	}
}

// Write sends b on *every* socket — stopping at the first that accepts it
// would probe only one address, which is the behaviour this type exists to
// replace. It fails only if none accepted: a dual-stack host with no route for
// one family fails that family's write immediately, and treating that as fatal
// would defeat the whole point.
func (c *fanOutConn) Write(b []byte) (int, error) {
	var (
		firstErr error
		sent     bool
	)
	for _, conn := range c.conns {
		if _, err := conn.Write(b); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		sent = true
	}
	if !sent {
		return 0, firstErr
	}
	return len(b), nil
}

// Read returns the first datagram to arrive on any socket. It fails only once
// every socket has failed — one address timing out is the expected case here,
// not an error to report — and every Read after that reports the same failure
// immediately, off the latched state, rather than waiting on sockets whose
// reader goroutines have already exited.
func (c *fanOutConn) Read(b []byte) (int, error) {
	for c.failed < len(c.conns) {
		select {
		case reply := <-c.replies:
			return copy(b, reply), nil
		case err := <-c.errs:
			c.failed++
			if c.firstErr == nil {
				c.firstErr = err
			}
		}
	}
	// A late datagram may have landed while the last socket was failing.
	select {
	case reply := <-c.replies:
		return copy(b, reply), nil
	default:
	}
	return 0, c.firstErr
}

// Close closes every socket, which is also what unblocks the reader goroutines.
func (c *fanOutConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		for _, conn := range c.conns {
			err = errors.Join(err, conn.Close())
		}
	})
	return err
}

// LocalAddr and RemoteAddr report the first socket's, there being no single
// answer; nothing in the browser exchange consults either.
func (c *fanOutConn) LocalAddr() net.Addr  { return c.conns[0].LocalAddr() }
func (c *fanOutConn) RemoteAddr() net.Addr { return c.conns[0].RemoteAddr() }

func (c *fanOutConn) SetDeadline(t time.Time) error {
	return c.each(func(conn net.Conn) error { return conn.SetDeadline(t) })
}

func (c *fanOutConn) SetReadDeadline(t time.Time) error {
	return c.each(func(conn net.Conn) error { return conn.SetReadDeadline(t) })
}

func (c *fanOutConn) SetWriteDeadline(t time.Time) error {
	return c.each(func(conn net.Conn) error { return conn.SetWriteDeadline(t) })
}

func (c *fanOutConn) each(fn func(net.Conn) error) error {
	var err error
	for _, conn := range c.conns {
		err = errors.Join(err, fn(conn))
	}
	return err
}

// dialerFor picks the dialer a connector gets: the caller's when
// ConnectionOptions.Dialer is set, otherwise browserDialer for an address that
// names an instance without a port — the only case where the driver runs a
// SQL Server Browser probe — and nil (the driver's own) for everything else.
func dialerFor(opts ConnectionOptions) mssql.Dialer {
	if opts.Dialer != nil {
		return opts.Dialer
	}
	if _, instance, port := ParseServerAddress(opts.Server); instance != "" && port == 0 {
		return browserDialer{}
	}
	return nil
}
