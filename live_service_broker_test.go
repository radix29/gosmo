//go:build livedb

// Live verification of the seven Service Broker families — the reads, the
// scripters and the drops.
//
// The unit tests pin what the builders emit and what the decoders decide;
// only a server can say whether the queries run on this major, whether what
// the builders emit parses, and whether it creates the *same* object again. So
// one throwaway database gets one object of each family, each is read back,
// scripted, and the script executed against a second database, where the
// object is read again through the same finder and compared field by field. A
// CREATE that parses but produces a different object — a lost schema
// collection on a message type, a SENT BY ANY read as INITIATOR, a queue whose
// activation block was dropped — fails on the comparison rather than on the
// exec.
//
//	go test -tags livedb . -run TestLiveServiceBroker -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops two throwaway databases; touches nothing else. Run it on
// majors 13, 14 and 17, and on a Managed Instance — where the remote service
// binding half is expected to be skipped, not to fail.
package gosmo

import (
	"errors"
	"strings"
	"testing"
)

// serviceBrokerFixture is the dependency order the objects have to be created
// in: a contract needs its message type, a service its queue and contract, a
// broker priority both. It is also the order the live test creates them in on
// both sides.
//
// The user is WITHOUT LOGIN because a remote service binding's user cannot be
// one mapped to a certificate (Msg 28083) — and creating a certificate would
// need a database master key the 2016 and 2017 instances' master does not
// have.
var serviceBrokerFixture = []string{
	`CREATE XML SCHEMA COLLECTION dbo.sb_xsd AS N'<xsd:schema xmlns:xsd="http://www.w3.org/2001/XMLSchema"><xsd:element name="sb" type="xsd:string"/></xsd:schema>'`,
	`CREATE PROCEDURE dbo.usp_sb_activate AS SET NOCOUNT ON`,
	`CREATE USER sb_live_user WITHOUT LOGIN`,
}

// serviceBrokerObjects is the fixture proper, created only in the source
// database: the destination gets them from the generated scripts.
var serviceBrokerObjects = []string{
	`CREATE MESSAGE TYPE [//gosmo/live/mt_xml] VALIDATION = VALID_XML WITH SCHEMA COLLECTION dbo.sb_xsd`,
	`CREATE MESSAGE TYPE [//gosmo/live/mt_any] VALIDATION = WELL_FORMED_XML`,
	`CREATE CONTRACT [//gosmo/live/contract]
	   ([//gosmo/live/mt_xml] SENT BY INITIATOR, [//gosmo/live/mt_any] SENT BY ANY)`,
	`CREATE QUEUE dbo.sb_live_queue WITH STATUS = ON, RETENTION = ON,
	   ACTIVATION (STATUS = ON, PROCEDURE_NAME = dbo.usp_sb_activate,
	               MAX_QUEUE_READERS = 3, EXECUTE AS OWNER),
	   POISON_MESSAGE_HANDLING (STATUS = OFF) ON [PRIMARY]`,
	`CREATE SERVICE [//gosmo/live/service] ON QUEUE dbo.sb_live_queue ([//gosmo/live/contract])`,
	`CREATE BROKER PRIORITY sb_live_priority FOR CONVERSATION
	   SET (CONTRACT_NAME = [//gosmo/live/contract],
	        LOCAL_SERVICE_NAME = [//gosmo/live/service],
	        REMOTE_SERVICE_NAME = N'//gosmo/live/remote', PRIORITY_LEVEL = 7)`,
}

// The route is created apart from the rest because Managed Instance refuses a
// MIRROR_ADDRESS (and ADDRESS = 'TRANSPORT') with Msg 41943, "does not support
// creating route with TRANSPORT or MIRROR address" — an ordinary TCP address
// is accepted there. Unlike the binding's 41906, 41943 is a *runtime* refusal:
// probed on t-qmi-01 on 2026-09-17, a CREATE TABLE before it in the same batch
// ran and an INSERT after it did not, so it aborts the remainder of the batch
// rather than the whole of it.
//
// MIRROR_ADDRESS needs BROKER_INSTANCE with it: without one the server refuses
// the route outright, Msg 9661.
const serviceBrokerRouteMirrored = `CREATE ROUTE sb_live_route WITH SERVICE_NAME = N'//gosmo/live/service',
	   BROKER_INSTANCE = N'AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE', LIFETIME = 6000,
	   ADDRESS = N'TCP://sb.invalid:4022', MIRROR_ADDRESS = N'TCP://sb2.invalid:4022'`

const serviceBrokerRoutePlain = `CREATE ROUTE sb_live_route WITH SERVICE_NAME = N'//gosmo/live/service',
	   LIFETIME = 6000, ADDRESS = N'TCP://sb.invalid:4022'`

func TestLiveServiceBrokerFamiliesRoundTrip(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_sb_src_live")
	defer dropSrc()
	dst, dropDst := liveScratchDB(t, db, ctx, "gosmo_sb_dst_live")
	defer dropDst()

	liveExecIn(t, src, ctx, serviceBrokerFixture...)
	liveExecIn(t, src, ctx, serviceBrokerObjects...)
	// The destination needs the objects the scripts reference by name but do
	// not create: the schema collection, the activation procedure and the
	// binding's user.
	liveExecIn(t, dst, ctx, serviceBrokerFixture...)

	// The route is created separately so its MIRROR_ADDRESS can be dropped
	// where the edition refuses one (Msg 41943, Managed Instance); everything
	// else about the route is the same either way.
	mirroredRoute := true
	if _, err := src.exec(ctx, serviceBrokerRouteMirrored); err != nil {
		mirroredRoute = false
		t.Logf("CREATE ROUTE with MIRROR_ADDRESS refused (expected on Managed Instance): %v", err)
		liveExecIn(t, src, ctx, serviceBrokerRoutePlain)
	}

	// The remote service binding is created separately: Managed Instance
	// refuses CREATE REMOTE SERVICE BINDING with Msg 41906 at compile time,
	// and that is an edition answer, not a failure of anything under test.
	bindingCreated := true
	if _, err := src.exec(ctx, `CREATE REMOTE SERVICE BINDING sb_live_binding
	   TO SERVICE N'//gosmo/live/remote' WITH USER = sb_live_user, ANONYMOUS = ON`); err != nil {
		bindingCreated = false
		t.Logf("CREATE REMOTE SERVICE BINDING refused (expected on Managed Instance): %v", err)
	}

	sc := NewScripter(src, DefaultScriptOptions())

	t.Run("message type", func(t *testing.T) {
		before, err := src.MessageTypeByName(ctx, "//gosmo/live/mt_xml")
		if err != nil {
			t.Fatalf("MessageTypeByName: %v", err)
		}
		if before.Validation != MessageTypeValidationValidXML {
			t.Errorf("validation is %q, want %q — validation_desc alone cannot tell the "+
				"two XML forms apart", before.Validation, MessageTypeValidationValidXML)
		}
		if before.SchemaCollectionName != "sb_xsd" {
			t.Errorf("schema collection is %q, want sb_xsd", before.SchemaCollectionName)
		}
		if before.IsSystemObject {
			t.Error("a user-created message type was marked a system object")
		}

		script, err := sc.ScriptMessageType(ctx, "//gosmo/live/mt_xml")
		if err != nil {
			t.Fatalf("ScriptMessageType: %v", err)
		}
		liveRunScript(t, dst, ctx, script)

		after, err := dst.MessageTypeByName(ctx, "//gosmo/live/mt_xml")
		if err != nil {
			t.Fatalf("the scripted message type does not exist in the destination: %v", err)
		}
		if after.Validation != before.Validation ||
			after.SchemaCollectionName != before.SchemaCollectionName {
			t.Errorf("the script created a different message type: validation %q/%q, collection %q/%q\n%s",
				before.Validation, after.Validation,
				before.SchemaCollectionName, after.SchemaCollectionName, script)
		}
	})

	t.Run("contract", func(t *testing.T) {
		// The second message type is scripted into the destination first:
		// CREATE CONTRACT names both, and a contract naming a message type
		// that does not exist is refused.
		script, err := sc.ScriptMessageType(ctx, "//gosmo/live/mt_any")
		if err != nil {
			t.Fatalf("ScriptMessageType: %v", err)
		}
		liveRunScript(t, dst, ctx, script)

		before, err := src.ContractByName(ctx, "//gosmo/live/contract")
		if err != nil {
			t.Fatalf("ContractByName: %v", err)
		}
		if len(before.Messages) != 2 {
			t.Fatalf("contract has %d messages, want 2", len(before.Messages))
		}
		// The both-bits case: read as INITIATOR or TARGET alone, the
		// contract this scripts refuses half the traffic the original took.
		sent := map[string]ContractSender{}
		for _, m := range before.Messages {
			sent[m.MessageType] = m.SentBy
		}
		if sent["//gosmo/live/mt_any"] != ContractSentByAny {
			t.Errorf("SENT BY ANY read back as %q", sent["//gosmo/live/mt_any"])
		}
		if sent["//gosmo/live/mt_xml"] != ContractSentByInitiator {
			t.Errorf("SENT BY INITIATOR read back as %q", sent["//gosmo/live/mt_xml"])
		}

		script, err = sc.ScriptContract(ctx, "//gosmo/live/contract")
		if err != nil {
			t.Fatalf("ScriptContract: %v", err)
		}
		liveRunScript(t, dst, ctx, script)

		after, err := dst.ContractByName(ctx, "//gosmo/live/contract")
		if err != nil {
			t.Fatalf("the scripted contract does not exist in the destination: %v", err)
		}
		if len(after.Messages) != len(before.Messages) {
			t.Fatalf("the scripted contract carries %d messages, the original %d:\n%s",
				len(after.Messages), len(before.Messages), script)
		}
		for i := range before.Messages {
			if after.Messages[i] != before.Messages[i] {
				t.Errorf("message %d differs: %+v vs %+v\n%s", i,
					before.Messages[i], after.Messages[i], script)
			}
		}
	})

	t.Run("queue", func(t *testing.T) {
		before, err := src.BrokerQueueByName(ctx, "dbo", "sb_live_queue")
		if err != nil {
			t.Fatalf("BrokerQueueByName: %v", err)
		}
		switch {
		case !before.IsReceiveEnabled:
			t.Error("STATUS = ON read back as disabled")
		case !before.IsRetentionEnabled:
			t.Error("RETENTION = ON read back as off")
		case before.IsPoisonMessageHandlingEnabled:
			t.Error("POISON_MESSAGE_HANDLING (STATUS = OFF) read back as on")
		case !before.IsActivationEnabled:
			t.Error("ACTIVATION (STATUS = ON) read back as off")
		case before.MaxReaders != 3:
			t.Errorf("MAX_QUEUE_READERS is %d, want 3", before.MaxReaders)
		case before.ActivationExecuteAs != "OWNER":
			t.Errorf("EXECUTE AS OWNER read back as %q", before.ActivationExecuteAs)
		case before.FileGroup == "":
			t.Error("the queue's filegroup came back empty")
		case before.IsSystemObject:
			t.Error("a user-created queue was marked a system object")
		}

		// The count needs VIEW DATABASE STATE and is a separate call for
		// exactly that reason; an empty queue reports 0, not an error.
		if n, err := before.MessageCount(ctx); err != nil {
			t.Errorf("MessageCount: %v", err)
		} else if n != 0 {
			t.Errorf("a queue nothing has sent to holds %d messages", n)
		}

		script, err := sc.ScriptBrokerQueue(ctx, "dbo", "sb_live_queue")
		if err != nil {
			t.Fatalf("ScriptBrokerQueue: %v", err)
		}
		liveRunScript(t, dst, ctx, script)

		after, err := dst.BrokerQueueByName(ctx, "dbo", "sb_live_queue")
		if err != nil {
			t.Fatalf("the scripted queue does not exist in the destination: %v", err)
		}
		if after.IsReceiveEnabled != before.IsReceiveEnabled ||
			after.IsRetentionEnabled != before.IsRetentionEnabled ||
			after.IsPoisonMessageHandlingEnabled != before.IsPoisonMessageHandlingEnabled ||
			after.IsActivationEnabled != before.IsActivationEnabled ||
			after.MaxReaders != before.MaxReaders ||
			after.ActivationProcedure != before.ActivationProcedure ||
			after.ActivationExecuteAs != before.ActivationExecuteAs ||
			after.FileGroup != before.FileGroup {
			t.Errorf("the script created a different queue:\nbefore %+v\nafter  %+v\n%s",
				before, after, script)
		}
	})

	t.Run("service", func(t *testing.T) {
		before, err := src.BrokerServiceByName(ctx, "//gosmo/live/service")
		if err != nil {
			t.Fatalf("BrokerServiceByName: %v", err)
		}
		if before.QueueName != "sb_live_queue" || before.QueueSchema != "dbo" {
			t.Errorf("the service's queue read back as [%s].[%s]", before.QueueSchema, before.QueueName)
		}
		if len(before.Contracts) != 1 || before.Contracts[0] != "//gosmo/live/contract" {
			t.Errorf("the service's contracts read back as %v", before.Contracts)
		}

		script, err := sc.ScriptBrokerService(ctx, "//gosmo/live/service")
		if err != nil {
			t.Fatalf("ScriptBrokerService: %v", err)
		}
		liveRunScript(t, dst, ctx, script)

		after, err := dst.BrokerServiceByName(ctx, "//gosmo/live/service")
		if err != nil {
			t.Fatalf("the scripted service does not exist in the destination: %v", err)
		}
		if after.QueueName != before.QueueName || len(after.Contracts) != len(before.Contracts) {
			t.Errorf("the script created a different service: queue %q/%q, contracts %v/%v\n%s",
				before.QueueName, after.QueueName, before.Contracts, after.Contracts, script)
		}
	})

	t.Run("route", func(t *testing.T) {
		before, err := src.RouteByName(ctx, "sb_live_route")
		if err != nil {
			t.Fatalf("RouteByName: %v", err)
		}
		wantMirror := ""
		if mirroredRoute {
			wantMirror = "TCP://sb2.invalid:4022"
		}
		if before.Address != "TCP://sb.invalid:4022" || before.MirrorAddress != wantMirror {
			t.Errorf("addresses read back as %q / %q, want %q / %q",
				before.Address, before.MirrorAddress, "TCP://sb.invalid:4022", wantMirror)
		}
		// sys.routes keeps the expiry instant in UTC, so a lifetime read as
		// local time is off by the server's offset — hours, not seconds.
		if secs := before.LifetimeSeconds(); secs < 5000 || secs > 6000 {
			t.Errorf("LIFETIME = 6000 read back as %d seconds remaining — "+
				"sys.routes.lifetime is being read in the wrong time zone", secs)
		}

		script, err := sc.ScriptRoute(ctx, "sb_live_route")
		if err != nil {
			t.Fatalf("ScriptRoute: %v", err)
		}
		liveRunScript(t, dst, ctx, script)

		after, err := dst.RouteByName(ctx, "sb_live_route")
		if err != nil {
			t.Fatalf("the scripted route does not exist in the destination: %v", err)
		}
		if after.Address != before.Address || after.MirrorAddress != before.MirrorAddress ||
			after.RemoteService != before.RemoteService {
			t.Errorf("the script created a different route:\nbefore %+v\nafter  %+v\n%s",
				before, after, script)
		}
		if after.Expires.IsZero() {
			t.Errorf("the scripted route has no lifetime:\n%s", script)
		}
	})

	t.Run("broker priority", func(t *testing.T) {
		before, err := src.BrokerPriorityByName(ctx, "sb_live_priority")
		if err != nil {
			t.Fatalf("BrokerPriorityByName: %v", err)
		}
		if before.Level != 7 || before.Contract != "//gosmo/live/contract" ||
			before.LocalService != "//gosmo/live/service" ||
			before.RemoteService != "//gosmo/live/remote" {
			t.Errorf("the priority read back as %+v", before)
		}

		script, err := sc.ScriptBrokerPriority(ctx, "sb_live_priority")
		if err != nil {
			t.Fatalf("ScriptBrokerPriority: %v", err)
		}
		liveRunScript(t, dst, ctx, script)

		after, err := dst.BrokerPriorityByName(ctx, "sb_live_priority")
		if err != nil {
			t.Fatalf("the scripted priority does not exist in the destination: %v", err)
		}
		if after.Level != before.Level || after.Contract != before.Contract ||
			after.LocalService != before.LocalService || after.RemoteService != before.RemoteService {
			t.Errorf("the script created a different priority:\nbefore %+v\nafter  %+v\n%s",
				before, after, script)
		}
	})

	t.Run("remote service binding", func(t *testing.T) {
		if !bindingCreated {
			t.Skip("CREATE REMOTE SERVICE BINDING is not supported on this edition")
		}
		before, err := src.RemoteServiceBindingByName(ctx, "sb_live_binding")
		if err != nil {
			t.Fatalf("RemoteServiceBindingByName: %v", err)
		}
		if before.User != "sb_live_user" || !before.IsAnonymous ||
			before.RemoteService != "//gosmo/live/remote" {
			t.Errorf("the binding read back as %+v", before)
		}
		// The contract column is 0, not NULL, on a binding created normally;
		// an inner join onto sys.service_contracts would have lost the row
		// entirely rather than leaving this empty.
		if before.Contract != "" {
			t.Errorf("the binding named contract %q; CREATE REMOTE SERVICE BINDING has no contract clause",
				before.Contract)
		}

		script, err := sc.ScriptRemoteServiceBinding(ctx, "sb_live_binding")
		if err != nil {
			t.Fatalf("ScriptRemoteServiceBinding: %v", err)
		}
		liveRunScript(t, dst, ctx, script)

		after, err := dst.RemoteServiceBindingByName(ctx, "sb_live_binding")
		if err != nil {
			t.Fatalf("the scripted binding does not exist in the destination: %v", err)
		}
		if after.User != before.User || after.IsAnonymous != before.IsAnonymous ||
			after.RemoteService != before.RemoteService {
			t.Errorf("the script created a different binding:\nbefore %+v\nafter  %+v\n%s",
				before, after, script)
		}
	})

	// The listings, against a database that has one of everything. Each is
	// checked for the fixture object by name: a listing that returns only the
	// system members is the failure a non-empty result would hide.
	t.Run("listings", func(t *testing.T) {
		mts, err := src.MessageTypes(ctx)
		if err != nil {
			t.Fatalf("MessageTypes: %v", err)
		}
		var systemSeen bool
		if !containsNamed(t, len(mts), func(i int) string { return mts[i].Name }, "//gosmo/live/mt_xml") {
			t.Error("the message type listing does not contain the fixture object")
		}
		for _, mt := range mts {
			if mt.IsSystemObject {
				systemSeen = true
			}
		}
		if !systemSeen {
			t.Error("no message type was marked a system object; every database ships fourteen")
		}

		contracts, err := src.Contracts(ctx)
		if err != nil {
			t.Fatalf("Contracts: %v", err)
		}
		if !containsNamed(t, len(contracts), func(i int) string { return contracts[i].Name }, "//gosmo/live/contract") {
			t.Error("the contract listing does not contain the fixture object")
		}
		for _, c := range contracts {
			if c.Name == "//gosmo/live/contract" && len(c.Messages) != 2 {
				t.Errorf("the listed contract carries %d messages, want 2 — the "+
					"grouped usage read attached them to the wrong contract", len(c.Messages))
			}
		}

		queues, err := src.BrokerQueues(ctx)
		if err != nil {
			t.Fatalf("BrokerQueues: %v", err)
		}
		if !containsNamed(t, len(queues), func(i int) string { return queues[i].Name }, "sb_live_queue") {
			t.Error("the queue listing does not contain the fixture object")
		}
		// Queues are marked from is_ms_shipped, not from an id range: every
		// database ships three, and their object_ids are ordinary ones.
		shipped := 0
		for _, q := range queues {
			if q.IsSystemObject {
				shipped++
			}
		}
		if shipped == 0 {
			t.Error("no queue was marked a system object; is_ms_shipped is not being read")
		}

		services, err := src.BrokerServices(ctx)
		if err != nil {
			t.Fatalf("BrokerServices: %v", err)
		}
		if !containsNamed(t, len(services), func(i int) string { return services[i].Name }, "//gosmo/live/service") {
			t.Error("the service listing does not contain the fixture object")
		}

		routes, err := src.Routes(ctx)
		if err != nil {
			t.Fatalf("Routes: %v", err)
		}
		if !containsNamed(t, len(routes), func(i int) string { return routes[i].Name }, "AutoCreatedLocal") {
			t.Error("AutoCreatedLocal is missing from the route listing; it exists in every database")
		}

		prios, err := src.BrokerPriorities(ctx)
		if err != nil {
			t.Fatalf("BrokerPriorities: %v", err)
		}
		if !containsNamed(t, len(prios), func(i int) string { return prios[i].Name }, "sb_live_priority") {
			t.Error("the broker priority listing does not contain the fixture object")
		}

		if _, err := src.QueueMonitors(ctx); err != nil {
			t.Errorf("QueueMonitors: %v", err)
		}
		counts, err := src.QueueMessageCounts(ctx)
		if err != nil {
			t.Fatalf("QueueMessageCounts: %v", err)
		}
		if len(counts) == 0 {
			t.Error("no queue reported a message count; the sys.internal_tables join found nothing")
		}
	})

	// msdb is the standing fixture for the one asymmetry the classification
	// cannot resolve: Database Mail's queues are is_ms_shipped = 1 while the
	// same feature's services and message types sit above 65536, in the user
	// range. It is pinned here so the mismatch is recorded as intended rather
	// than rediscovered as a bug — and so that a "fix" by name-matching fails.
	t.Run("msdb classification asymmetry", func(t *testing.T) {
		srv, err := NewServer(ctx, db)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		msdb, err := srv.DatabaseByName(ctx, "msdb")
		if err != nil {
			t.Skipf("msdb is not reachable on this instance: %v", err)
		}
		queues, err := msdb.BrokerQueues(ctx)
		if err != nil {
			t.Fatalf("BrokerQueues(msdb): %v", err)
		}
		var mailQueue *BrokerQueue
		for _, q := range queues {
			if strings.EqualFold(q.Name, "InternalMailQueue") {
				mailQueue = q
			}
		}
		if mailQueue == nil {
			t.Skip("Database Mail's queues are not present on this instance")
		}
		if !mailQueue.IsSystemObject {
			t.Error("InternalMailQueue is is_ms_shipped = 1 and must read as a system object")
		}

		services, err := msdb.BrokerServices(ctx)
		if err != nil {
			t.Fatalf("BrokerServices(msdb): %v", err)
		}
		for _, s := range services {
			if strings.EqualFold(s.Name, "InternalMailService") && s.IsSystemObject {
				t.Error("InternalMailService sits above 65536 and must read as a user object — " +
					"the two classifications disagree on purpose, and name-matching them " +
					"into agreement is the thing not to do")
			}
		}
	})

	// The two writes. Everything above is a read; these are the statements a
	// Properties page applies, and the settings they change are the ones that
	// change in operation — a stuck activation, a queue taken out of service,
	// a route repointed at a new address.
	t.Run("queue alter", func(t *testing.T) {
		q, err := src.BrokerQueueByName(ctx, "dbo", "sb_live_queue")
		if err != nil {
			t.Fatalf("BrokerQueueByName: %v", err)
		}

		// Restating ACTIVATION from what the queue already has is the round
		// trip a Properties page makes: the two unquoted halves of the
		// procedure and the principal come straight off the read.
		off, on := false, true
		err = q.Alter(ctx, QueueSettings{
			Status:                &off,
			Retention:             &off,
			PoisonMessageHandling: &on,
			Activation: &QueueActivation{
				Enabled:         false,
				ProcedureSchema: q.ActivationProcedureSchema,
				ProcedureName:   q.ActivationProcedureName,
				MaxQueueReaders: 7,
				ExecuteAs:       q.ActivationExecuteAs,
			},
		})
		if err != nil {
			t.Fatalf("Alter: %v", err)
		}

		after, err := src.BrokerQueueByName(ctx, "dbo", "sb_live_queue")
		if err != nil {
			t.Fatalf("re-read: %v", err)
		}
		switch {
		case after.IsReceiveEnabled || after.IsEnqueueEnabled:
			t.Error("STATUS = OFF left the queue in service — one STATUS moves both halves")
		case after.IsRetentionEnabled:
			t.Error("RETENTION = OFF did not take")
		case !after.IsPoisonMessageHandlingEnabled:
			t.Error("POISON_MESSAGE_HANDLING (STATUS = ON) did not take")
		case after.IsActivationEnabled:
			t.Error("ACTIVATION STATUS = OFF did not take")
		case after.MaxReaders != 7:
			t.Errorf("MAX_QUEUE_READERS is %d after the alter, want 7", after.MaxReaders)
		case after.ActivationProcedure != q.ActivationProcedure:
			t.Errorf("the restated activation procedure came back as %q, want %q",
				after.ActivationProcedure, q.ActivationProcedure)
		case after.ActivationExecuteAs != "OWNER":
			t.Errorf("EXECUTE AS OWNER came back as %q", after.ActivationExecuteAs)
		}

		// The receiver was mirrored, so it must agree with the server without
		// a re-read — that is the whole point of setIfApplied being applied
		// here and not under WithScript.
		if q.IsReceiveEnabled != after.IsReceiveEnabled || q.MaxReaders != after.MaxReaders ||
			q.IsActivationEnabled != after.IsActivationEnabled {
			t.Errorf("the receiver disagrees with the server after an applied alter:\n"+
				"receiver %+v\nserver   %+v", q, after)
		}

		// ACTIVATION (DROP) removes the block outright, where STATUS = OFF
		// only stopped it.
		if err := q.Alter(ctx, QueueSettings{DropActivation: true}); err != nil {
			t.Fatalf("Alter(DropActivation): %v", err)
		}
		after, err = src.BrokerQueueByName(ctx, "dbo", "sb_live_queue")
		if err != nil {
			t.Fatalf("re-read: %v", err)
		}
		if after.ActivationProcedure != "" || after.MaxReaders != 0 ||
			after.ActivationExecuteAs != "" {
			t.Errorf("ACTIVATION (DROP) left %+v", after)
		}

		// Put it back the way the rest of the test found it.
		if err := q.Alter(ctx, QueueSettings{
			Status:                &on,
			Retention:             &on,
			PoisonMessageHandling: &off,
			Activation: &QueueActivation{
				Enabled: true, ProcedureSchema: "dbo", ProcedureName: "usp_sb_activate",
				MaxQueueReaders: 3, ExecuteAs: QueueExecuteAsOwner,
			},
		}); err != nil {
			t.Fatalf("Alter (revert): %v", err)
		}
		reverted, err := src.BrokerQueueByName(ctx, "dbo", "sb_live_queue")
		if err != nil {
			t.Fatalf("re-read: %v", err)
		}
		if !reverted.IsReceiveEnabled || !reverted.IsActivationEnabled || reverted.MaxReaders != 3 {
			t.Errorf("the queue did not revert: %+v", reverted)
		}

		// EXECUTE AS SELF is the one value that does not round-trip: the
		// server resolves it to the principal running the statement.
		if err := q.Alter(ctx, QueueSettings{
			Activation: &QueueActivation{
				Enabled: true, ProcedureSchema: "dbo", ProcedureName: "usp_sb_activate",
				MaxQueueReaders: 3, ExecuteAs: QueueExecuteAsSelf,
			},
		}); err != nil {
			t.Fatalf("Alter (EXECUTE AS SELF): %v", err)
		}
		self, err := src.BrokerQueueByName(ctx, "dbo", "sb_live_queue")
		if err != nil {
			t.Fatalf("re-read: %v", err)
		}
		if self.ActivationExecuteAs == "" || self.ActivationExecuteAs == QueueExecuteAsSelf {
			t.Errorf("EXECUTE AS SELF read back as %q; the server stores the principal "+
				"it resolved to, never the keyword", self.ActivationExecuteAs)
		}
	})

	t.Run("route alter", func(t *testing.T) {
		r, err := src.RouteByName(ctx, "sb_live_route")
		if err != nil {
			t.Fatalf("RouteByName: %v", err)
		}
		addr, lifetime := "TCP://sb3.invalid:4022", 600
		if err := r.Alter(ctx, RouteSettings{
			Address: &addr, LifetimeSeconds: &lifetime,
		}); err != nil {
			t.Fatalf("Alter: %v", err)
		}
		after, err := src.RouteByName(ctx, "sb_live_route")
		if err != nil {
			t.Fatalf("re-read: %v", err)
		}
		if after.Address != addr {
			t.Errorf("ADDRESS is %q after the alter, want %q", after.Address, addr)
		}
		if secs := after.LifetimeSeconds(); secs < 500 || secs > 600 {
			t.Errorf("LIFETIME = 600 read back as %d seconds remaining", secs)
		}
		// The clauses not named must be untouched — that is what a nil field
		// means, and a page that sends only what changed depends on it.
		if after.RemoteService != r.RemoteService || after.BrokerInstance != r.BrokerInstance ||
			after.MirrorAddress != r.MirrorAddress {
			t.Errorf("an omitted clause was changed anyway:\nbefore %+v\nafter  %+v", r, after)
		}
		if r.Address != after.Address {
			t.Errorf("the receiver was not mirrored: %q vs %q", r.Address, after.Address)
		}
	})

	// The drops, in reverse dependency order, each through the typed method.
	// A drop the server refuses because something still depends on the object
	// is the normal case here, so the order is what makes them succeed.
	t.Run("drops", func(t *testing.T) {
		if bindingCreated {
			if err := src.DropRemoteServiceBinding(ctx, "sb_live_binding"); err != nil {
				t.Errorf("DropRemoteServiceBinding: %v", err)
			}
		}
		if err := src.DropBrokerPriority(ctx, "sb_live_priority"); err != nil {
			t.Errorf("DropBrokerPriority: %v", err)
		}
		if err := src.DropRoute(ctx, "sb_live_route"); err != nil {
			t.Errorf("DropRoute: %v", err)
		}
		if err := src.DropBrokerService(ctx, "//gosmo/live/service"); err != nil {
			t.Errorf("DropBrokerService: %v", err)
		}
		if err := src.DropBrokerQueue(ctx, "dbo", "sb_live_queue"); err != nil {
			t.Errorf("DropBrokerQueue: %v", err)
		}
		if err := src.DropContract(ctx, "//gosmo/live/contract"); err != nil {
			t.Errorf("DropContract: %v", err)
		}
		for _, name := range []string{"//gosmo/live/mt_xml", "//gosmo/live/mt_any"} {
			if err := src.DropMessageType(ctx, name); err != nil {
				t.Errorf("DropMessageType %s: %v", name, err)
			}
		}

		// Every finder must now say not-found, not return a stale object.
		if _, err := src.BrokerQueueByName(ctx, "dbo", "sb_live_queue"); !errors.Is(err, ErrNotFound) {
			t.Errorf("after the drop, BrokerQueueByName returned %v, want ErrNotFound", err)
		}
		if _, err := src.RouteByName(ctx, "sb_live_route"); !errors.Is(err, ErrNotFound) {
			t.Errorf("after the drop, RouteByName returned %v, want ErrNotFound", err)
		}
		if _, err := src.BrokerPriorityByName(ctx, "sb_live_priority"); !errors.Is(err, ErrNotFound) {
			t.Errorf("after the drop, BrokerPriorityByName returned %v, want ErrNotFound", err)
		}
	})

	// A dependency refusal is the normal case, and the server's own message
	// is what a caller shows: dropping the destination's message type while
	// its contract still names it must fail, with Msg 3716.
	t.Run("dependency refusal", func(t *testing.T) {
		err := dst.DropMessageType(ctx, "//gosmo/live/mt_any")
		if err == nil {
			t.Fatal("dropping a message type still bound to a contract succeeded")
		}
		if !strings.Contains(err.Error(), "bound to one or more contract") {
			t.Errorf("the server's own refusal did not reach the caller: %v", err)
		}
	})
}

// containsNamed reports whether any of n items has the wanted name.
func containsNamed(t *testing.T, n int, name func(int) string, want string) bool {
	t.Helper()
	for i := range n {
		if name(i) == want {
			return true
		}
	}
	return false
}
