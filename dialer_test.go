package gosmo

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// TestDialerForOnlyReplacesTheBrowserCase pins which addresses get gosmo's own
// dialer. Installing it more widely would put every TDS connection through
// code that exists only for a UDP probe.
func TestDialerForOnlyReplacesTheBrowserCase(t *testing.T) {
	cases := []struct {
		server string
		want   bool
	}{
		{`host\instance`, true},
		{`host\instance,55253`, false}, // port given: no browser probe
		{"host", false},
		{"host,1433", false},
		{"host:1433", false},
	}
	for _, c := range cases {
		got := dialerFor(ConnectionOptions{Server: c.server})
		if (got != nil) != c.want {
			t.Errorf("dialerFor(%q) non-nil = %v, want %v", c.server, got != nil, c.want)
		}
	}
}

// testDialer is a caller-supplied dialer, distinguishable from browserDialer.
type testDialer struct{}

func (testDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("not dialed")
}

// A caller's own dialer always wins — including for a named instance, where
// gosmo would otherwise substitute its own.
func TestDialerForPrefersTheCallersDialer(t *testing.T) {
	for _, server := range []string{`host\instance`, "host"} {
		got := dialerFor(ConnectionOptions{Server: server, Dialer: testDialer{}})
		if _, ok := got.(testDialer); !ok {
			t.Errorf("dialerFor(%q) = %#v, want the caller's dialer", server, got)
		}
	}
}

// newUDPResponder starts a UDP listener on addr that replies to any datagram
// with reply, and returns its port. A nil reply means "never answer" — the
// silent socket that is the whole reason fanOutConn exists.
func newUDPResponder(t *testing.T, host string, reply []byte) *net.UDPConn {
	t.Helper()
	pc, err := net.ListenPacket("udp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Skipf("listen udp on %s: %v", host, err)
	}
	conn := pc.(*net.UDPConn)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if reply != nil && n > 0 {
				_, _ = conn.WriteTo(reply, from)
			}
		}
	}()
	t.Cleanup(func() { conn.Close() })
	return conn
}

// TestFanOutConnTakesTheAnsweringAddress is the bug this whole file exists for:
// two sockets, only one of which ever answers, and the reply must come back
// regardless of which one that is. A plain net.Dialer picks one address and, on
// the silent one, times out reporting "no instance matching".
func TestFanOutConnTakesTheAnsweringAddress(t *testing.T) {
	want := []byte("ServerName;WIN10CLI;InstanceName;SQL2017;tcp;55253;;")

	for _, answering := range []int{0, 1} {
		silent := newUDPResponder(t, "127.0.0.1", nil)
		talker := newUDPResponder(t, "127.0.0.1", want)

		dial := func(c *net.UDPConn) net.Conn {
			t.Helper()
			conn, err := net.Dial("udp", c.LocalAddr().String())
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			return conn
		}
		conns := []net.Conn{dial(silent), dial(talker)}
		if answering == 0 {
			conns[0], conns[1] = conns[1], conns[0]
		}

		fc := newFanOutConn(conns)
		if err := fc.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatalf("SetDeadline: %v", err)
		}
		if _, err := fc.Write([]byte{3}); err != nil {
			t.Fatalf("Write: %v", err)
		}
		buf := make([]byte, 4096)
		n, err := fc.Read(buf)
		if err != nil {
			t.Fatalf("answering socket at index %d: Read: %v", answering, err)
		}
		if got := string(buf[:n]); got != string(want) {
			t.Errorf("answering socket at index %d: read %q, want %q", answering, got, want)
		}
		fc.Close()
	}
}

// When nothing answers, Read must still fail — with the sockets' own timeout,
// not by hanging past the deadline the driver set.
func TestFanOutConnFailsWhenNoAddressAnswers(t *testing.T) {
	var conns []net.Conn
	for range 2 {
		s := newUDPResponder(t, "127.0.0.1", nil)
		c, err := net.Dial("udp", s.LocalAddr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		conns = append(conns, c)
	}
	fc := newFanOutConn(conns)
	defer fc.Close()
	fc.SetDeadline(time.Now().Add(300 * time.Millisecond))
	fc.Write([]byte{3})

	done := make(chan error, 1)
	go func() { _, err := fc.Read(make([]byte, 4096)); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Read succeeded with no responder, want the sockets' timeout")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Read hung past the deadline set on every socket")
	}
}

// A TCP dial must pass through untouched: browserDialer is installed for the
// whole connection, and the instance itself is reached over TCP.
func TestBrowserDialerPassesTCPThrough(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := browserDialer{}.DialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("DialContext(tcp): %v", err)
	}
	if _, ok := conn.(*fanOutConn); ok {
		t.Error("a TCP dial returned a fanOutConn, want the plain net.Conn")
	}
	conn.Close()
}

// A single-address host keeps the stock behaviour — one socket, no fan-out.
func TestBrowserDialerSingleAddressIsNotFannedOut(t *testing.T) {
	s := newUDPResponder(t, "127.0.0.1", []byte("ok"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := browserDialer{}.DialContext(ctx, "udp", s.LocalAddr().String())
	if err != nil {
		t.Fatalf("DialContext(udp): %v", err)
	}
	defer conn.Close()
	if _, ok := conn.(*fanOutConn); ok {
		t.Error("a single-address host returned a fanOutConn, want the plain net.Conn")
	}
}
