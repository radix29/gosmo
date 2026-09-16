package gosmo

// The two Service Broker writes, pinned statement for statement under
// WithScript, and the refusals they raise before sending anything.
//
// The values are quote-hostile for the reason script_write_common_test.go
// gives: a queue's schema and name, an activation procedure's two halves and
// an EXECUTE AS user all reach the statement, and each of them is a place a
// lost bracket or an unescaped apostrophe would name a different object.

import (
	"context"
	"strings"
	"testing"
)

func boolPtr(b bool) *bool      { return &b }
func intPtr(i int) *int         { return &i }
func strPtr(s string) *string   { return &s }
func upper(s string) string     { return strings.ToUpper(s) }
func contains(s, x string) bool { return strings.Contains(s, x) }

func TestScriptedQueueAndRouteAlters(t *testing.T) {
	db := scriptTestDB()
	runScriptCases(t, []scriptCase{
		{
			name: "queue status alone",
			call: func(ctx context.Context) error {
				return db.AlterBrokerQueueContext(ctx, "Sales.Archive", "o'brien",
					QueueSettings{Status: boolPtr(false)})
			},
			want: scriptUsePrefix + "ALTER QUEUE [Sales.Archive].[o'brien]\n    WITH STATUS = OFF",
		},
		{
			name: "queue every setting at once",
			call: func(ctx context.Context) error {
				return db.AlterBrokerQueueContext(ctx, "dbo", "a]b", QueueSettings{
					Status:                boolPtr(true),
					Retention:             boolPtr(true),
					PoisonMessageHandling: boolPtr(false),
					Activation: &QueueActivation{
						Enabled:         true,
						ProcedureSchema: "Sales.Archive",
						ProcedureName:   "usp_o'brien",
						MaxQueueReaders: 4,
						ExecuteAs:       "o'brien",
					},
				})
			},
			want: scriptUsePrefix + "ALTER QUEUE [dbo].[a]]b]\n" +
				"    WITH STATUS = ON,\n" +
				"         RETENTION = ON,\n" +
				"         ACTIVATION (STATUS = ON, PROCEDURE_NAME = [Sales.Archive].[usp_o'brien], " +
				"MAX_QUEUE_READERS = 4, EXECUTE AS N'o''brien'),\n" +
				"         POISON_MESSAGE_HANDLING (STATUS = OFF)",
		},
		{
			// OWNER and SELF are keywords: quoted as user names they would
			// address a user by those names, or fail.
			name: "queue execute as OWNER is a keyword",
			call: func(ctx context.Context) error {
				return db.AlterBrokerQueueContext(ctx, "", "q", QueueSettings{
					Activation: &QueueActivation{Enabled: true, ProcedureName: "p",
						MaxQueueReaders: 1, ExecuteAs: QueueExecuteAsOwner},
				})
			},
			want: scriptUsePrefix + "ALTER QUEUE [dbo].[q]\n" +
				"    WITH ACTIVATION (STATUS = ON, PROCEDURE_NAME = [dbo].[p], " +
				"MAX_QUEUE_READERS = 1, EXECUTE AS OWNER)",
		},
		{
			name: "queue execute as SELF is a keyword",
			call: func(ctx context.Context) error {
				return db.AlterBrokerQueueContext(ctx, "", "q", QueueSettings{
					Activation: &QueueActivation{Enabled: false, ProcedureName: "p",
						MaxQueueReaders: 0, ExecuteAs: QueueExecuteAsSelf},
				})
			},
			want: scriptUsePrefix + "ALTER QUEUE [dbo].[q]\n" +
				"    WITH ACTIVATION (STATUS = OFF, PROCEDURE_NAME = [dbo].[p], " +
				"MAX_QUEUE_READERS = 0, EXECUTE AS SELF)",
		},
		{
			name: "queue activation dropped",
			call: func(ctx context.Context) error {
				return db.AlterBrokerQueueContext(ctx, "", "q",
					QueueSettings{DropActivation: true})
			},
			want: scriptUsePrefix + "ALTER QUEUE [dbo].[q]\n    WITH ACTIVATION (DROP)",
		},
		{
			name: "route address alone",
			call: func(ctx context.Context) error {
				return db.AlterRouteContext(ctx, "o'brien",
					RouteSettings{Address: strPtr("TCP://host:4022")})
			},
			want: scriptUsePrefix + "ALTER ROUTE [o'brien]\n    WITH ADDRESS = N'TCP://host:4022'",
		},
		{
			name: "route every setting at once",
			call: func(ctx context.Context) error {
				return db.AlterRouteContext(ctx, "a]b", RouteSettings{
					RemoteService:   strPtr("//app/o'brien"),
					BrokerInstance:  strPtr("AAAA-BBBB"),
					LifetimeSeconds: intPtr(600),
					Address:         strPtr("TCP://host:4022"),
					MirrorAddress:   strPtr("TCP://host2:4022"),
				})
			},
			want: scriptUsePrefix + "ALTER ROUTE [a]]b]\n" +
				"    WITH SERVICE_NAME = N'//app/o''brien',\n" +
				"         BROKER_INSTANCE = N'AAAA-BBBB',\n" +
				"         LIFETIME = 600,\n" +
				"         ADDRESS = N'TCP://host:4022',\n" +
				"         MIRROR_ADDRESS = N'TCP://host2:4022'",
		},
	})
}

// Every refusal below is one the server would give anyway. They are raised
// before the round trip so that the caller gets the field's name rather than
// a syntax error naming the generated SQL.
func TestQueueAndRouteAltersRefuseWhatTheServerWould(t *testing.T) {
	cases := []struct {
		name string
		call func(context.Context) error
		want string
	}{
		{"queue with no setting",
			func(ctx context.Context) error {
				return scriptTestDB().AlterBrokerQueueContext(ctx, "", "q", QueueSettings{})
			}, "no setting was given"},
		{"queue activation and drop together",
			func(ctx context.Context) error {
				return scriptTestDB().AlterBrokerQueueContext(ctx, "", "q", QueueSettings{
					DropActivation: true,
					Activation:     &QueueActivation{ProcedureName: "p"},
				})
			}, "mutually exclusive"},
		{"queue activation with no procedure",
			func(ctx context.Context) error {
				return scriptTestDB().AlterBrokerQueueContext(ctx, "", "q", QueueSettings{
					Activation: &QueueActivation{Enabled: true, MaxQueueReaders: 1},
				})
			}, "ProcedureName is empty"},
		{"queue readers out of range",
			func(ctx context.Context) error {
				return scriptTestDB().AlterBrokerQueueContext(ctx, "", "q", QueueSettings{
					Activation: &QueueActivation{ProcedureName: "p", MaxQueueReaders: 40000},
				})
			}, "0 to 32767"},
		{"route with no setting",
			func(ctx context.Context) error {
				return scriptTestDB().AlterRouteContext(ctx, "r", RouteSettings{})
			}, "no setting was given"},
		// The server cannot clear any of these, and NULL does not even parse,
		// so an empty string must not be sent as one.
		{"route address cleared",
			func(ctx context.Context) error {
				return scriptTestDB().AlterRouteContext(ctx, "r", RouteSettings{Address: strPtr("")})
			}, "ADDRESS is empty"},
		{"route broker instance cleared",
			func(ctx context.Context) error {
				return scriptTestDB().AlterRouteContext(ctx, "r",
					RouteSettings{BrokerInstance: strPtr("")})
			}, "BROKER_INSTANCE is empty"},
		{"route lifetime cleared",
			func(ctx context.Context) error {
				return scriptTestDB().AlterRouteContext(ctx, "r",
					RouteSettings{LifetimeSeconds: intPtr(0)})
			}, "LIFETIME must be 1 or more"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, script := WithScript(context.Background())
			err := c.call(ctx)
			if err == nil {
				t.Fatalf("no error; the statement captured was %q", script.Statements)
			}
			if !contains(err.Error(), c.want) {
				t.Errorf("error %q does not explain the refusal (%q)", err, c.want)
			}
			if len(script.Statements) != 0 {
				t.Errorf("a refused call still emitted %q", script.Statements)
			}
		})
	}
}

// A scripted alter must not move the receiver's state: nothing ran, so the
// queue on the server still has what it had.
func TestScriptedQueueAlterDoesNotMirrorOntoTheReceiver(t *testing.T) {
	q := &BrokerQueue{db: scriptTestDB(), Schema: "dbo", Name: "q",
		IsReceiveEnabled: true, IsEnqueueEnabled: true, MaxReaders: 2,
		ActivationExecuteAs: "dbo"}

	ctx, script := WithScript(context.Background())
	if err := q.AlterContext(ctx, QueueSettings{Status: boolPtr(false)}); err != nil {
		t.Fatalf("scripted alter: %v", err)
	}
	if !q.IsReceiveEnabled || !q.IsEnqueueEnabled {
		t.Error("a scripted alter disabled the queue on the receiver; the statement " +
			"was only captured, so the server's queue is still enabled")
	}
	if len(script.Statements) != 1 {
		t.Fatalf("captured %q, want one statement", script.Statements)
	}
}

// Applied for real, the same alter does mirror — and STATUS moves both halves,
// because the server moves both.
func TestQueueAlterMirrorsBothStatusHalves(t *testing.T) {
	q := &BrokerQueue{db: scriptTestDB(), Schema: "dbo", Name: "q",
		IsReceiveEnabled: true, IsEnqueueEnabled: true}
	// Mirroring is what is under test, not the exec, so the statement is
	// still collected — with a context that is not a script one, the receiver
	// would be updated after a real exec. setIfApplied is keyed on the
	// context, so the two halves are exercised separately: above for the
	// scripted case, here for the applied one.
	mirrorQueueSettings(context.Background(), q, QueueSettings{Status: boolPtr(false)})
	if q.IsReceiveEnabled || q.IsEnqueueEnabled {
		t.Errorf("STATUS = OFF left the receiver at receive=%v enqueue=%v; ALTER QUEUE "+
			"clears both halves together", q.IsReceiveEnabled, q.IsEnqueueEnabled)
	}
}

// EXECUTE AS SELF resolves server-side to whoever ran the statement, so the
// receiver must keep the principal it had rather than claim "SELF".
func TestQueueAlterDoesNotMirrorExecuteAsSelf(t *testing.T) {
	q := &BrokerQueue{db: scriptTestDB(), Schema: "dbo", Name: "q", ActivationExecuteAs: "app_user"}
	mirrorQueueSettings(context.Background(), q, QueueSettings{
		Activation: &QueueActivation{Enabled: true, ProcedureName: "p",
			MaxQueueReaders: 1, ExecuteAs: QueueExecuteAsSelf},
	})
	if q.ActivationExecuteAs != "app_user" {
		t.Errorf("ActivationExecuteAs = %q after EXECUTE AS SELF; only a re-read can "+
			"say which principal the server resolved it to", q.ActivationExecuteAs)
	}
	if upper(q.ActivationProcedure) != "[DBO].[P]" {
		t.Errorf("ActivationProcedure = %q, want the schema-qualified name", q.ActivationProcedure)
	}
}
