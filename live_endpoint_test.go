//go:build livedb

// Live verification of the general endpoint family: that sys.endpoints reads
// back the columns endpoint.go scans, that the built-in endpoints are marked
// IsSystem and refused both writes, that a user endpoint's state can be
// changed and put back, and that its generated script names the certificate
// and authentication the original actually uses.
//
//	go test -tags livedb . -run TestLiveEndpoint -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// This test creates nothing and drops nothing: win10cli's AGEP mirroring
// endpoint is deliberately left in place (see gossms's open-threads §
// win10cli as a third instance), so the state change is made and undone.
package gosmo

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestLiveEndpointsReadAndGuard(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	s, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	all, err := s.EndpointsContext(ctx)
	if err != nil {
		t.Fatalf("EndpointsContext: %v", err)
	}
	if len(all) < 5 {
		t.Fatalf("got %d endpoints, want at least the five built-in ones", len(all))
	}

	// Every instance has these five, and all five must be marked IsSystem or
	// the Object Explorer offers Delete on the Dedicated Admin Connection.
	var system, user []*Endpoint
	for _, e := range all {
		if e.IsSystem {
			system = append(system, e)
		} else {
			user = append(user, e)
		}
		if e.Protocol == "" || e.Type == "" || e.State == "" {
			t.Errorf("endpoint %q has an empty descriptor: %+v", e.Name, e)
		}
	}
	if len(system) < 5 {
		t.Errorf("only %d endpoints are marked IsSystem, want at least 5", len(system))
	}
	for _, e := range system {
		if e.EndpointID >= firstUserEndpointID {
			t.Errorf("endpoint %q has id %d but is marked system", e.Name, e.EndpointID)
		}
		if err := e.DropContext(ctx); !errors.Is(err, ErrSystemEndpoint) {
			t.Errorf("dropping system endpoint %q returned %v, want ErrSystemEndpoint", e.Name, err)
		}
	}

	dac := slices.IndexFunc(all, func(e *Endpoint) bool { return e.IsAdmin })
	if dac < 0 {
		t.Error("no endpoint is marked IsAdmin — the Dedicated Admin Connection is one")
	}

	// The by-name read must agree with the list; they are separate queries.
	byName, err := s.EndpointByNameContext(ctx, all[0].Name)
	if err != nil {
		t.Fatalf("EndpointByNameContext(%q): %v", all[0].Name, err)
	}
	if byName.EndpointID != all[0].EndpointID || byName.State != all[0].State || byName.IsSystem != all[0].IsSystem {
		t.Errorf("by-name read %+v disagrees with the list row %+v", byName, all[0])
	}
	if _, err := s.EndpointByNameContext(ctx, "no such endpoint here"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing endpoint returned %v, want ErrNotFound", err)
	}

	if len(user) == 0 {
		t.Skip("no user endpoint on this instance — the state and script halves need one")
	}
	e := user[0]

	// The script has to name the real authentication and certificate: an
	// endpoint scripted as WINDOWS NEGOTIATE when it authenticates by
	// certificate is one that recreates a listener nothing can connect to.
	script, err := NewServerScripter(s, ScriptOptions{Verb: ScriptDropAndCreate}).ScriptEndpointContext(ctx, e.Name)
	if err != nil {
		t.Fatalf("ScriptEndpointContext(%q): %v", e.Name, err)
	}
	for _, want := range []string{"DROP ENDPOINT", "CREATE ENDPOINT", "LISTENER_PORT", "FOR " + e.Type} {
		if !strings.Contains(script, want) {
			t.Errorf("script is missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "<certificate name>") {
		t.Errorf("the script could not resolve the endpoint's certificate:\n%s", script)
	}

	// And the state really changes. Put it back whatever happens: AGEP is
	// ubusql1's mirroring peer and is left STARTED deliberately.
	// Restored with defer, not t.Cleanup: liveDB's own deferred done() closes
	// the connection when this function returns, and a cleanup runs after
	// that — leaving AGEP STOPPED on the way out.
	was := e.State
	defer func() {
		if err := e.SetStateContext(ctx, EndpointState(was)); err != nil {
			t.Errorf("restoring endpoint %q to %s: %v", e.Name, was, err)
		}
	}()
	if err := e.SetStateContext(ctx, EndpointStopped); err != nil {
		t.Fatalf("SetStateContext(STOPPED): %v", err)
	}
	after, err := s.EndpointByNameContext(ctx, e.Name)
	if err != nil {
		t.Fatalf("re-read after stop: %v", err)
	}
	if after.State != "STOPPED" {
		t.Errorf("endpoint %q reads as %q after STOPPED", e.Name, after.State)
	}
}
