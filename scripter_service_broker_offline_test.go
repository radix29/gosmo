package gosmo_test

import (
	"context"
	"testing"

	"github.com/radix29/gosmo"
	"github.com/radix29/gosmo/gosmotest"
)

// The Service Broker scripters, offline: each read keyed on its text and
// arguments, each object's extended properties read at its own class and id
// (15 message type, 16 contract, 1 queue, 17 service, 19 route, 18 binding),
// the whole script compared. See scripter_script_as_test.go for why.
func TestServiceBrokerScriptersGolden(t *testing.T) {
	runScriptCases(t, []scriptCase{{
		name: "ScriptMessageType",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.service_message_types mt", "//app/Order"), gosmotest.Rows(
				[]any{int64(65540), "//app/Order", "dbo", "XML", "", ""}))
			noProps(f, 15, 65540)
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptMessageType(ctx, "//app/Order")
		},
		want: "IF NOT EXISTS (SELECT 1 FROM sys.service_message_types WHERE name = N'//app/Order')\n" +
			"CREATE MESSAGE TYPE [//app/Order]\n" +
			"    AUTHORIZATION [dbo]\n" +
			"    VALIDATION = WELL_FORMED_XML;\n" +
			"GO\n",
	}, {
		// The contract's messages come from a second read by its id, each with
		// the side that sends it.
		name: "ScriptContract",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.service_contracts c", "//app/OrderContract"), gosmotest.Rows(
				[]any{int64(65541), "//app/OrderContract", "dbo"}))
			f.OnFunc(read("u.is_sent_by_initiator", int64(65541)), gosmotest.Rows(
				[]any{int64(65541), "//app/Order", true, false},
				[]any{int64(65541), "//app/Reply", false, true}))
			noProps(f, 16, 65541)
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptContract(ctx, "//app/OrderContract")
		},
		want: "IF NOT EXISTS (SELECT 1 FROM sys.service_contracts WHERE name = N'//app/OrderContract')\n" +
			"CREATE CONTRACT [//app/OrderContract]\n" +
			"    AUTHORIZATION [dbo]\n" +
			"    ([//app/Order] SENT BY INITIATOR,\n" +
			"     [//app/Reply] SENT BY TARGET);\n" +
			"GO\n",
	}, {
		name: "ScriptBrokerQueue",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.service_queues q", "sales", "OrderQueue"), gosmotest.Rows(
				[]any{int64(777), "OrderQueue", "sales", "dbo", false, true, true, false, true,
					true, "[sales].[ProcessOrders]", "sales", "ProcessOrders", int64(4), "OWNER", "PRIMARY", scriptWhen, scriptWhen}))
			noProps(f, 1, 777)
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptBrokerQueue(ctx, "sales", "OrderQueue")
		},
		want: "IF OBJECT_ID(N'[sales].[OrderQueue]', 'SQ') IS NULL\n" +
			"CREATE QUEUE [sales].[OrderQueue]\n" +
			"    WITH STATUS = ON,\n" +
			"         RETENTION = OFF,\n" +
			"         ACTIVATION\n" +
			"         (  STATUS = ON,\n" +
			"            PROCEDURE_NAME = [sales].[ProcessOrders],\n" +
			"            MAX_QUEUE_READERS = 4,\n" +
			"            EXECUTE AS OWNER\n" +
			"         ),\n" +
			"         POISON_MESSAGE_HANDLING (STATUS = ON)\n" +
			"    ON [PRIMARY];\n" +
			"GO\n",
	}, {
		name: "ScriptBrokerService",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.services s", "//app/OrderService"), gosmotest.Rows(
				[]any{int64(65542), "//app/OrderService", "dbo", "sales", "OrderQueue"}))
			f.OnFunc(read("FROM   sys.service_contract_usages u", int64(65542)), gosmotest.Rows(
				[]any{int64(65542), "//app/OrderContract"}))
			noProps(f, 17, 65542)
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptBrokerService(ctx, "//app/OrderService")
		},
		want: "IF NOT EXISTS (SELECT 1 FROM sys.services WHERE name = N'//app/OrderService')\n" +
			"CREATE SERVICE [//app/OrderService]\n" +
			"    AUTHORIZATION [dbo]\n" +
			"    ON QUEUE [sales].[OrderQueue]\n" +
			"    ([//app/OrderContract]);\n" +
			"GO\n",
	}, {
		name: "ScriptRoute",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.routes r", "OrderRoute"), gosmotest.Rows(
				[]any{int64(65543), "OrderRoute", "dbo", "//app/Remote", "8f0c2a4e-0000-0000-0000-000000000003",
					"TCP://remote:4022", "", nil, nil}))
			noProps(f, 19, 65543)
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptRoute(ctx, "OrderRoute")
		},
		want: "IF NOT EXISTS (SELECT 1 FROM sys.routes WHERE name = N'OrderRoute')\n" +
			"CREATE ROUTE [OrderRoute]\n" +
			"    AUTHORIZATION [dbo]\n" +
			"    WITH SERVICE_NAME = N'//app/Remote',\n" +
			"         BROKER_INSTANCE = N'8f0c2a4e-0000-0000-0000-000000000003',\n" +
			"         ADDRESS = N'TCP://remote:4022';\n" +
			"GO\n",
	}, {
		name: "ScriptRemoteServiceBinding",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.remote_service_bindings b", "OrderBinding"), gosmotest.Rows(
				[]any{int64(65544), "OrderBinding", "dbo", "//app/Remote", "remote_user", true, ""}))
			noProps(f, 18, 65544)
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptRemoteServiceBinding(ctx, "OrderBinding")
		},
		want: "IF NOT EXISTS (SELECT 1 FROM sys.remote_service_bindings WHERE name = N'OrderBinding')\n" +
			"CREATE REMOTE SERVICE BINDING [OrderBinding]\n" +
			"    AUTHORIZATION [dbo]\n" +
			"    TO SERVICE N'//app/Remote'\n" +
			"    WITH USER = [remote_user], ANONYMOUS = ON;\n" +
			"GO\n",
	}, {
		// A priority has no extended properties: the scripter reads none.
		name: "ScriptBrokerPriority",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.conversation_priorities p", "HighOrders"), gosmotest.Rows(
				[]any{int64(65545), "HighOrders", "//app/OrderContract", "//app/OrderService", "", int64(8)}))
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptBrokerPriority(ctx, "HighOrders")
		},
		want: "IF NOT EXISTS (SELECT 1 FROM sys.conversation_priorities WHERE name = N'HighOrders')\n" +
			"CREATE BROKER PRIORITY [HighOrders]\n" +
			"FOR CONVERSATION\n" +
			"SET (CONTRACT_NAME = [//app/OrderContract],\n" +
			"     LOCAL_SERVICE_NAME = [//app/OrderService],\n" +
			"     REMOTE_SERVICE_NAME = ANY,\n" +
			"     PRIORITY_LEVEL = 8);\n" +
			"GO\n",
	}})
}
