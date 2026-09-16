package gosmo

// The Service Broker script builders. Each test names a way the emitted
// statement would still look plausible while creating a different object —
// or one the server refuses to parse, which is the failure mode
// live_service_broker_test.go is the backstop for.

import (
	"strings"
	"testing"
	"time"
)

func TestBuildMessageTypeScriptKeepsTheSchemaCollection(t *testing.T) {
	mt := &MessageType{
		Name: "//probe/mt", Owner: "dbo",
		Validation:             MessageTypeValidationValidXML,
		SchemaCollectionSchema: "app", SchemaCollectionName: "orders_xsd",
	}
	got := buildMessageTypeScript(mt, DefaultScriptOptions())
	if !strings.Contains(got, "VALIDATION = VALID_XML WITH SCHEMA COLLECTION [app].[orders_xsd];") {
		t.Errorf("the schema collection is missing — the script creates a message type "+
			"that accepts any XML:\n%s", got)
	}
	if !strings.Contains(got, "CREATE MESSAGE TYPE [//probe/mt]") {
		t.Errorf("the name is not bracket-quoted; a message type named as a URI does not parse:\n%s", got)
	}
	if !strings.Contains(got, "AUTHORIZATION [dbo]") {
		t.Errorf("the owner is missing:\n%s", got)
	}
}

func TestBuildMessageTypeScriptEmitsValidationNoneExplicitly(t *testing.T) {
	mt := &MessageType{Name: "mt", Validation: MessageTypeValidationNone}
	got := buildMessageTypeScript(mt, ScriptOptions{})
	if !strings.Contains(got, "VALIDATION = NONE;") {
		t.Errorf("VALIDATION = NONE was left out:\n%s", got)
	}
	if strings.Contains(got, "AUTHORIZATION") {
		t.Errorf("an ownerless message type must not carry an AUTHORIZATION clause:\n%s", got)
	}
}

// No Service Broker family supports DROP … IF EXISTS — all seven fail to
// parse with Msg 156. The guard has to be a catalog lookup.
func TestBrokerDropScriptsNeverUseDropIfExists(t *testing.T) {
	opts := ScriptOptions{Verb: ScriptDrop}
	scripts := map[string]string{
		"message type": buildMessageTypeScript(&MessageType{Name: "mt"}, opts),
		"contract":     buildContractScript(&ServiceContract{Name: "c"}, opts),
		"service": buildBrokerServiceScript(&BrokerService{
			Name: "s", QueueSchema: "dbo", QueueName: "q"}, opts),
		"queue":                  buildBrokerQueueScript(&BrokerQueue{Name: "q", Schema: "dbo"}, opts),
		"route":                  buildRouteScript(&Route{Name: "r", Address: "LOCAL"}, opts),
		"remote service binding": buildRemoteServiceBindingScript(&RemoteServiceBinding{Name: "b"}, opts),
		"broker priority":        buildBrokerPriorityScript(&BrokerPriority{Name: "p", Level: 5}, opts),
	}
	for family, got := range scripts {
		if strings.Contains(got, "IF EXISTS "+strings.ToUpper(family)) {
			t.Errorf("%s: emitted DROP … IF EXISTS, which no Service Broker "+
				"statement parses:\n%s", family, got)
		}
		if !strings.Contains(got, "IF") || !strings.Contains(got, "DROP") {
			t.Errorf("%s: the drop is unguarded:\n%s", family, got)
		}
		if strings.Contains(got, "CREATE") {
			t.Errorf("%s: ScriptDrop emitted a CREATE:\n%s", family, got)
		}
	}
}

func TestBuildContractScriptRendersEachSentBy(t *testing.T) {
	c := &ServiceContract{
		Name: "//probe/c", Owner: "dbo",
		Messages: []ContractMessage{
			{MessageType: "//probe/a", SentBy: ContractSentByInitiator},
			{MessageType: "//probe/b", SentBy: ContractSentByTarget},
			{MessageType: "//probe/c", SentBy: ContractSentByAny},
		},
	}
	got := buildContractScript(c, DefaultScriptOptions())
	for _, want := range []string{
		"[//probe/a] SENT BY INITIATOR",
		"[//probe/b] SENT BY TARGET",
		"[//probe/c] SENT BY ANY",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

func TestBuildContractScriptFallsBackForAMessagelessContract(t *testing.T) {
	got := buildContractScript(&ServiceContract{Name: "DEFAULT"}, ScriptOptions{})
	if !strings.Contains(got, "([DEFAULT] SENT BY ANY)") {
		t.Errorf("a contract with no messages scripted a CREATE the server refuses:\n%s", got)
	}
}

func TestBuildBrokerQueueScriptCarriesEverySetting(t *testing.T) {
	q := &BrokerQueue{
		Name: "q", Schema: "app",
		IsReceiveEnabled: true, IsEnqueueEnabled: true,
		IsRetentionEnabled:  true,
		IsActivationEnabled: true,
		ActivationProcedure: "[dbo].[usp_activate]",
		MaxReaders:          3,
		ActivationExecuteAs: "app_user",
		FileGroup:           "BROKER_FG",
	}
	got := buildBrokerQueueScript(q, DefaultScriptOptions())
	for _, want := range []string{
		"CREATE QUEUE [app].[q]",
		"STATUS = ON",
		"RETENTION = ON",
		"PROCEDURE_NAME = [dbo].[usp_activate]",
		"MAX_QUEUE_READERS = 3",
		"EXECUTE AS N'app_user'",
		// The queue was created before the flag defaulted on, so false here
		// is a real setting and not an unread column.
		"POISON_MESSAGE_HANDLING (STATUS = OFF)",
		"ON [BROKER_FG]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

// A queue whose activation is configured but switched off keeps the whole
// ACTIVATION block: dropping it turns a paused activation into none.
func TestBuildBrokerQueueScriptKeepsDisabledActivation(t *testing.T) {
	q := &BrokerQueue{
		Name: "q", Schema: "dbo",
		IsReceiveEnabled:               true,
		IsPoisonMessageHandlingEnabled: true,
		IsActivationEnabled:            false,
		ActivationProcedure:            "[dbo].[p]",
		MaxReaders:                     1,
		ActivationExecuteAs:            "OWNER",
	}
	got := buildBrokerQueueScript(q, ScriptOptions{})
	if !strings.Contains(got, "ACTIVATION") || !strings.Contains(got, "STATUS = OFF") {
		t.Errorf("the disabled activation was dropped from the script:\n%s", got)
	}
	// OWNER is a keyword, not a user name, so it must not be quoted.
	if !strings.Contains(got, "EXECUTE AS OWNER") {
		t.Errorf("EXECUTE AS OWNER was quoted as a user name:\n%s", got)
	}
	if strings.Contains(got, "ON [") {
		t.Errorf("a queue with no filegroup read emitted an ON clause:\n%s", got)
	}
}

func TestBuildBrokerQueueScriptOmitsActivationWhenThereIsNone(t *testing.T) {
	q := &BrokerQueue{Name: "q", Schema: "dbo", IsReceiveEnabled: true,
		IsPoisonMessageHandlingEnabled: true}
	got := buildBrokerQueueScript(q, ScriptOptions{})
	if strings.Contains(got, "ACTIVATION") {
		t.Errorf("a queue with no activation procedure emitted an ACTIVATION block:\n%s", got)
	}
	if !strings.Contains(got, "STATUS = ON") || !strings.Contains(got, "RETENTION = OFF") {
		t.Errorf("the WITH clause is incomplete:\n%s", got)
	}
}

func TestBuildBrokerServiceScriptOmitsAnEmptyContractList(t *testing.T) {
	s := &BrokerService{Name: "//probe/s", QueueSchema: "dbo", QueueName: "q"}
	got := buildBrokerServiceScript(s, ScriptOptions{})
	if strings.Contains(got, "()") {
		t.Errorf("an empty contract list does not parse:\n%s", got)
	}
	if !strings.Contains(got, "ON QUEUE [dbo].[q];") {
		t.Errorf("the queue is missing:\n%s", got)
	}

	s.Contracts = []string{"//probe/c1", "//probe/c2"}
	got = buildBrokerServiceScript(s, ScriptOptions{})
	if !strings.Contains(got, "([//probe/c1],\n     [//probe/c2])") {
		t.Errorf("the contract list is wrong:\n%s", got)
	}
}

func TestBuildRouteScriptEmitsOnlyTheClausesTheRouteHas(t *testing.T) {
	got := buildRouteScript(&Route{Name: "r", Address: "LOCAL"}, ScriptOptions{})
	if !strings.Contains(got, "WITH ADDRESS = N'LOCAL';") {
		t.Errorf("a minimal route scripted more than ADDRESS:\n%s", got)
	}

	full := &Route{
		Name: "r", Owner: "dbo", RemoteService: "//probe/s",
		BrokerInstance: "AAAA-BBBB", Address: "TCP://host:4022",
		MirrorAddress: "TCP://host2:4022",
		Expires:       time.Now().UTC().Add(time.Hour),
	}
	got = buildRouteScript(full, DefaultScriptOptions())
	for _, want := range []string{
		"SERVICE_NAME = N'//probe/s'",
		"BROKER_INSTANCE = N'AAAA-BBBB'",
		"LIFETIME = ",
		"ADDRESS = N'TCP://host:4022'",
		"MIRROR_ADDRESS = N'TCP://host2:4022'",
		"AUTHORIZATION [dbo]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "LIFETIME = 0") || strings.Contains(got, "LIFETIME = -") {
		t.Errorf("LIFETIME is not a positive number of seconds:\n%s", got)
	}
}

func TestBuildRemoteServiceBindingScriptNamesTheUserAsAnIdentifier(t *testing.T) {
	b := &RemoteServiceBinding{Name: "b", Owner: "dbo",
		RemoteService: "//probe/remote", User: "sb user", IsAnonymous: true}
	got := buildRemoteServiceBindingScript(b, ScriptOptions{})
	if !strings.Contains(got, "TO SERVICE N'//probe/remote'") {
		t.Errorf("the remote service must be a string literal:\n%s", got)
	}
	if !strings.Contains(got, "WITH USER = [sb user], ANONYMOUS = ON;") {
		t.Errorf("the user must be a bracket-quoted identifier:\n%s", got)
	}
}

func TestBuildBrokerPriorityScriptSpellsMissingCriteriaAsAny(t *testing.T) {
	got := buildBrokerPriorityScript(&BrokerPriority{Name: "p", Level: 5}, ScriptOptions{})
	for _, want := range []string{
		"CONTRACT_NAME = ANY",
		"LOCAL_SERVICE_NAME = ANY",
		"REMOTE_SERVICE_NAME = ANY",
		"PRIORITY_LEVEL = 5",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}

	// The remote service is a name in another database, so it is a literal
	// where the other two are identifiers.
	p := &BrokerPriority{Name: "p", Contract: "//probe/c",
		LocalService: "//probe/s", RemoteService: "//probe/remote", Level: 7}
	got = buildBrokerPriorityScript(p, ScriptOptions{})
	for _, want := range []string{
		"CONTRACT_NAME = [//probe/c]",
		"LOCAL_SERVICE_NAME = [//probe/s]",
		"REMOTE_SERVICE_NAME = N'//probe/remote'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

func TestBrokerDropAndCreateEmitsBothInOrder(t *testing.T) {
	opts := DefaultScriptOptions()
	opts.Verb = ScriptDropAndCreate
	got := buildMessageTypeScript(&MessageType{Name: "mt",
		Validation: MessageTypeValidationNone}, opts)
	drop := strings.Index(got, "DROP MESSAGE TYPE")
	create := strings.Index(got, "CREATE MESSAGE TYPE")
	if drop < 0 || create < 0 {
		t.Fatalf("DROP-and-CREATE emitted only one half:\n%s", got)
	}
	if drop > create {
		t.Errorf("the CREATE came first; the script is not re-runnable:\n%s", got)
	}
}
