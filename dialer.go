package gosmo

import (
	"context"
	"errors"
	"net"
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
func (browserDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	nd := &net.Dialer{KeepAlive: browserKeepAlive}
	if network != "udp" && network != "udp4" && network != "udp6" {
		return nd.DialContext(ctx, network, addr)
	}

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
