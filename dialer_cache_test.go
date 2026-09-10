package gosmo

import (
	"context"
	"database/sql/driver"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// isolateBrowserReplies gives a test an empty cache of its own, restoring the
// process-wide one afterwards, so tests neither see nor leave entries.
func isolateBrowserReplies(t *testing.T) {
	t.Helper()
	saved := browserReplies
	browserReplies = &browserReplyCache{entries: map[string]browserReply{}}
	t.Cleanup(func() { browserReplies = saved })
}

// countingResponder is newUDPResponder that also counts the requests it got.
func countingResponder(t *testing.T, reply []byte) (addr string, requests *atomic.Int32) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("listen udp: %v", err)
	}
	t.Cleanup(func() { pc.Close() })
	requests = new(atomic.Int32)
	go func() {
		buf := make([]byte, 4096)
		for {
			_, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			requests.Add(1)
			_, _ = pc.WriteTo(reply, from)
		}
	}()
	return pc.LocalAddr().String(), requests
}

// probe runs one Browser exchange through browserDialer the way go-mssqldb's
// getInstances does: dial, deadline, one Write, one Read.
func probe(t *testing.T, addr string, request []byte) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := browserDialer{}.DialContext(ctx, "udp", addr)
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	conn.SetDeadline(deadline)
	if _, err := conn.Write(request); err != nil {
		t.Fatalf("Write: %v", err)
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return string(buf[:n])
}

// The point of the cache: the second probe of a host for the same thing is
// answered without a datagram, with the reply the first one got.
func TestBrowserProbeIsAnsweredFromTheCache(t *testing.T) {
	isolateBrowserReplies(t)
	want := "\x05\x00\x00ServerName;H;InstanceName;SQL2017;tcp;55253;;"
	addr, requests := countingResponder(t, []byte(want))

	for i := range 3 {
		if got := probe(t, addr, []byte{3}); got != want {
			t.Fatalf("probe %d read %q, want %q", i, got, want)
		}
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("the host was asked %d times for three probes, want once", n)
	}
}

// A different request is a different question — a DAC probe names one instance
// and gets a different reply — so it must not be answered with another's.
func TestBrowserProbeCacheIsKeyedByRequest(t *testing.T) {
	isolateBrowserReplies(t)
	addr, requests := countingResponder(t, []byte("reply"))
	probe(t, addr, []byte{3})
	probe(t, addr, []byte{0x0f, 1, 'X', 0})
	if n := requests.Load(); n != 2 {
		t.Errorf("two different requests reached the host %d times, want 2", n)
	}
}

// An entry is trusted for browserReplyTTL and no longer, and evictHost drops
// every request's entry for that host and none of another's.
func TestBrowserReplyCacheExpiryAndEviction(t *testing.T) {
	c := &browserReplyCache{entries: map[string]browserReply{}}
	now := time.Now()
	a, a2, b := browserReplyKey("HostA", []byte{3}), browserReplyKey("hosta", []byte{0x0f}), browserReplyKey("hostB", []byte{3})
	c.put(a, []byte("a"), now)
	c.put(a2, []byte("a2"), now)
	c.put(b, []byte("b"), now)

	if _, ok := c.get(a, now.Add(browserReplyTTL-time.Second)); !ok {
		t.Error("entry missing inside its TTL")
	}
	if _, ok := c.get(a, now.Add(browserReplyTTL)); ok {
		t.Error("entry still served at its TTL")
	}
	c.evictHost("HOSTA") // host names are case-insensitive
	for _, k := range []string{a, a2} {
		if _, ok := c.get(k, now); ok {
			t.Errorf("entry %q survived evictHost of its host", k)
		}
	}
	if _, ok := c.get(b, now); !ok {
		t.Error("evictHost of one host dropped another host's entry")
	}
}

// The reply is copied in: the driver reads into its own buffer, which it may
// reuse, and a cached slice aliasing it would change under the cache.
func TestBrowserReplyCacheCopiesTheReply(t *testing.T) {
	c := &browserReplyCache{entries: map[string]browserReply{}}
	buf := []byte("tcp;55253")
	k := browserReplyKey("h", []byte{3})
	c.put(k, buf, time.Now())
	buf[4] = 'X'
	if got, _ := c.get(k, time.Now()); string(got) != "tcp;55253" {
		t.Errorf("cached reply changed with the caller's buffer: %q", got)
	}
}

// The stale-port case the eviction exists for, end to end through the driver:
// the cache says the instance is on a port nothing listens on any more (it
// restarted on a new dynamic one). The connection attempt fails, and the entry
// must be gone afterwards so the next attempt asks Browser again rather than
// failing the same way for the rest of the TTL.
func TestFailedConnectEvictsTheBrowserReply(t *testing.T) {
	isolateBrowserReplies(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	stalePort := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // now refuses

	key := browserReplyKey("127.0.0.1", []byte{3})
	browserReplies.put(key, []byte("\x05\x00\x00ServerName;H;InstanceName;INST;IsClustered;No;Version;16.0;tcp;"+
		strconv.Itoa(stalePort)+";;"), time.Now())

	opts := ConnectionOptions{Server: `127.0.0.1\INST`, User: "u", Password: "p", ConnectTimeout: 5 * time.Second}
	applyDefaults(&opts)
	connector, err := buildConnector(opts)
	if err != nil {
		t.Fatalf("buildConnector: %v", err)
	}
	dc := poolConnector(connector, opts)
	if _, ok := dc.(evictingConnector); !ok {
		t.Fatalf("poolConnector for a named instance = %T, want evictingConnector", dc)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if conn, err := dc.Connect(ctx); err == nil {
		conn.Close()
		t.Fatal("Connect succeeded against a closed port")
	}
	if _, ok := browserReplies.get(key, time.Now()); ok {
		t.Error("the stale reply is still cached after the attempt it misdirected failed")
	}
}

// Only a pool that probes Browser through gosmo's dialer is wrapped; every
// other connector goes to sql.OpenDB as it was built.
func TestPoolConnectorWrapsOnlyTheBrowserCase(t *testing.T) {
	for server, wrapped := range map[string]bool{`host\inst`: true, "host": false, `host\inst,1433`: false} {
		opts := ConnectionOptions{Server: server, User: "u", Password: "p"}
		applyDefaults(&opts)
		connector, err := buildConnector(opts)
		if err != nil {
			t.Fatalf("buildConnector(%q): %v", server, err)
		}
		var dc driver.Connector = poolConnector(connector, opts)
		if _, ok := dc.(evictingConnector); ok != wrapped {
			t.Errorf("poolConnector(%q) wrapped = %v, want %v", server, ok, wrapped)
		}
	}
}
