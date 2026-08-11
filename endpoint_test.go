package gosmo

import (
	"context"
	"testing"
)

// CREATE ENDPOINT is a one-shot statement against a live instance with no way
// to preview it, and a wrong clause order or a missing LISTENER_IP is a syntax
// error rather than a wrong result — so the text is pinned rather than the
// behaviour.

func TestCreateEndpointStatement(t *testing.T) {
	tests := []struct {
		name string
		spec EndpointSpec
		want string
	}{
		{"defaults", EndpointSpec{Name: "Hadr_endpoint"},
			"CREATE ENDPOINT [Hadr_endpoint] STATE = STARTED AS TCP (LISTENER_PORT = 5022, LISTENER_IP = ALL) " +
				"FOR DATABASE_MIRRORING (AUTHENTICATION = WINDOWS NEGOTIATE, ENCRYPTION = REQUIRED, ROLE = ALL)"},
		// The Linux shape: no shared domain, so the far end is authenticated
		// by certificate, and the clause is a phrase rather than a keyword.
		{"certificate auth", EndpointSpec{
			Name: "AGEP", Port: 5023, Authentication: "CERTIFICATE dbm_certificate",
			Encryption: "REQUIRED", EncryptionAlgorithm: "AES",
		},
			"CREATE ENDPOINT [AGEP] STATE = STARTED AS TCP (LISTENER_PORT = 5023, LISTENER_IP = ALL) " +
				"FOR DATABASE_MIRRORING (AUTHENTICATION = CERTIFICATE dbm_certificate, ENCRYPTION = REQUIRED ALGORITHM AES, ROLE = ALL)"},
		{"witness", EndpointSpec{Name: "w", Role: "witness", Encryption: "disabled"},
			"CREATE ENDPOINT [w] STATE = STARTED AS TCP (LISTENER_PORT = 5022, LISTENER_IP = ALL) " +
				"FOR DATABASE_MIRRORING (AUTHENTICATION = WINDOWS NEGOTIATE, ENCRYPTION = DISABLED, ROLE = WITNESS)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.spec.createEndpointStatement()
			if err != nil {
				t.Fatalf("createEndpointStatement: %v", err)
			}
			if got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestCreateEndpointStatementRejects(t *testing.T) {
	for name, spec := range map[string]EndpointSpec{
		"no name":         {Port: 5022},
		"bad port":        {Name: "e", Port: 70000},
		"unknown role":    {Name: "e", Role: "PRIMARY"},
		"bad encryption":  {Name: "e", Encryption: "MAYBE"},
		"blank name only": {Name: "   "},
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := spec.createEndpointStatement(); err == nil {
				t.Errorf("accepted, produced %s", got)
			}
		})
	}
}

func TestEndpointStateAndGrantStatements(t *testing.T) {
	e := &DatabaseMirroringEndpoint{server: &Server{}, Name: "AG]EP", Port: 5022}
	tests := []struct {
		name string
		fn   func(ctx context.Context) error
		want string
	}{
		{"start", e.StartContext, "ALTER ENDPOINT [AG]]EP] STATE = STARTED"},
		{"stop", e.StopContext, "ALTER ENDPOINT [AG]]EP] STATE = STOPPED"},
		{"drop", e.DropContext, "DROP ENDPOINT [AG]]EP]"},
		{"grant connect", func(ctx context.Context) error {
			return e.GrantConnectContext(ctx, `NT Service\MSSQLSERVER`)
		}, `GRANT CONNECT ON ENDPOINT::[AG]]EP] TO [NT Service\MSSQLSERVER]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, script := WithScript(context.Background())
			if err := tt.fn(ctx); err != nil {
				t.Fatalf("under WithScript: %v", err)
			}
			if got := soleStatement(t, script.Statements); got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}

// The URL is what every other replica is told to connect to, so it must name
// the *instance's own* host — and drop a named instance's suffix, which is not
// part of a TCP address.
func TestEndpointURL(t *testing.T) {
	if got, want := endpointURL("ubusql1", 5022), "tcp://ubusql1:5022"; got != want {
		t.Errorf("endpointURL = %q, want %q", got, want)
	}
	if got, want := endpointURL(`WIN10CLI\SQL2022`, 5022), "tcp://WIN10CLI:5022"; got != want {
		t.Errorf("named instance endpointURL = %q, want %q", got, want)
	}
}
