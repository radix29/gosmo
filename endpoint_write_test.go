package gosmo

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestEndpointStateStatements(t *testing.T) {
	for _, tc := range []struct {
		state EndpointState
		want  string
	}{
		{EndpointStarted, "ALTER ENDPOINT [AGEP] STATE = STARTED"},
		{EndpointStopped, "ALTER ENDPOINT [AGEP] STATE = STOPPED"},
		{EndpointDisabled, "ALTER ENDPOINT [AGEP] STATE = DISABLED"},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			ctx, col := WithScript(context.Background())
			e := &Endpoint{server: &Server{}, Name: "AGEP", EndpointID: 65536}
			if err := e.SetStateContext(ctx, tc.state); err != nil {
				t.Fatalf("SetStateContext: %v", err)
			}
			if len(col.Statements) != 1 || col.Statements[0] != tc.want {
				t.Errorf("got %v, want [%s]", col.Statements, tc.want)
			}
		})
	}
}

func TestEndpointDropStatement(t *testing.T) {
	ctx, col := WithScript(context.Background())
	e := &Endpoint{server: &Server{}, Name: "odd]name", EndpointID: 65536}
	if err := e.DropContext(ctx); err != nil {
		t.Fatalf("DropContext: %v", err)
	}
	if want := "DROP ENDPOINT [odd]]name]"; col.Statements[0] != want {
		t.Errorf("got %q, want %q", col.Statements[0], want)
	}
}

// The whole point of ErrSystemEndpoint: the server's own refusal names neither
// the endpoint nor the reason, and a statement that reaches it has already
// been offered to the user as something that would work.
func TestASystemEndpointRefusesBothWrites(t *testing.T) {
	for _, tc := range []struct {
		name string
		act  func(*Endpoint, context.Context) error
	}{
		{"drop", func(e *Endpoint, ctx context.Context) error { return e.DropContext(ctx) }},
		{"set state", func(e *Endpoint, ctx context.Context) error { return e.SetStateContext(ctx, EndpointStopped) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, col := WithScript(context.Background())
			e := &Endpoint{server: &Server{}, Name: "TSQL Default TCP", EndpointID: 4, IsSystem: true}
			err := tc.act(e, ctx)
			if !errors.Is(err, ErrSystemEndpoint) {
				t.Fatalf("got %v, want ErrSystemEndpoint", err)
			}
			// Refused *before* the statement is built, so nothing is collected
			// and nothing could be offered as a script either.
			if len(col.Statements) != 0 {
				t.Errorf("a refused write still built %v", col.Statements)
			}
		})
	}
}

func TestAnUnknownEndpointStateIsRefused(t *testing.T) {
	ctx, col := WithScript(context.Background())
	e := &Endpoint{server: &Server{}, Name: "AGEP", EndpointID: 65536}
	if err := e.SetStateContext(ctx, EndpointState("PAUSED")); err == nil {
		t.Error("want an error for an unknown state, got nil")
	}
	if len(col.Statements) != 0 {
		t.Errorf("an unknown state still built %v", col.Statements)
	}
}

func TestEndpointStateIsNotMirroredWhileScripting(t *testing.T) {
	e := &Endpoint{server: &Server{}, Name: "AGEP", EndpointID: 65536, State: "STARTED"}
	ctx, _ := WithScript(context.Background())
	if err := e.SetStateContext(ctx, EndpointStopped); err != nil {
		t.Fatalf("SetStateContext: %v", err)
	}
	if e.State != "STARTED" {
		t.Errorf("State became %q from a scripted (not executed) ALTER", e.State)
	}
}

// A desc is what the catalog records; the clause is what CREATE ENDPOINT
// takes, and the two differ by more than case. A pair that agreed with each
// other but not with the server would still round-trip.
func TestEndpointAuthClause(t *testing.T) {
	for _, tc := range []struct {
		desc, cert, want string
	}{
		{"NEGOTIATE", "", "WINDOWS NEGOTIATE"},
		{"NTLM", "", "WINDOWS NTLM"},
		{"KERBEROS", "", "WINDOWS KERBEROS"},
		{"CERTIFICATE", "win10cli_Cert", "CERTIFICATE [win10cli_Cert]"},
		{"NTLM, CERTIFICATE", "c1", "WINDOWS NTLM CERTIFICATE [c1]"},
		{"CERTIFICATE, NEGOTIATE", "c1", "CERTIFICATE [c1] WINDOWS NEGOTIATE"},
		// An unresolvable certificate must produce a clause the server
		// refuses, not one that silently authenticates by Windows alone.
		{"CERTIFICATE", "", "CERTIFICATE <certificate name>"},
		{"", "", "WINDOWS NEGOTIATE"},
	} {
		if got := endpointAuthClause(tc.desc, tc.cert); got != tc.want {
			t.Errorf("endpointAuthClause(%q, %q) = %q, want %q", tc.desc, tc.cert, got, tc.want)
		}
	}
}

func TestEndpointEncryptionClause(t *testing.T) {
	for _, tc := range []struct {
		enabled bool
		alg     string
		want    string
	}{
		{true, "AES", "REQUIRED ALGORITHM AES"},
		{true, "", "REQUIRED"},
		{true, "NONE", "REQUIRED"},
		{false, "AES", "DISABLED"},
	} {
		if got := endpointEncryptionClause(tc.enabled, tc.alg); got != tc.want {
			t.Errorf("endpointEncryptionClause(%v, %q) = %q, want %q", tc.enabled, tc.alg, got, tc.want)
		}
	}
}

// The payload clause is the half CREATE ENDPOINT cannot do without, so a type
// gosmo cannot reproduce must fail rather than emit a statement missing it.
func TestAnUnscriptableEndpointTypeFails(t *testing.T) {
	sc := &ServerScripter{}
	_, err := sc.endpointPayloadClause(context.Background(), &Endpoint{Name: "soapy", Type: "SOAP"})
	if err == nil {
		t.Fatal("want an error for a SOAP endpoint, got nil")
	}
	if !strings.Contains(err.Error(), "SOAP") {
		t.Errorf("the error does not name the type: %v", err)
	}
}

func TestATSQLEndpointScriptsItsPayload(t *testing.T) {
	sc := &ServerScripter{}
	got, err := sc.endpointPayloadClause(context.Background(), &Endpoint{Name: "custom", Type: "TSQL"})
	if err != nil {
		t.Fatalf("endpointPayloadClause: %v", err)
	}
	if got != "TSQL ()" {
		t.Errorf("got %q", got)
	}
}
