package gosmo_test

import (
	"context"
	"testing"

	"github.com/radix29/gosmo"
	"github.com/radix29/gosmo/gosmotest"
)

// The server-scoped scripters, offline: credential, endpoint, audit and audit
// specification, Database Mail, Resource Governor and event sessions. Each
// read is keyed on its text and arguments and the whole script compared; see
// scripter_script_as_test.go for why.
func TestServerScriptersGolden(t *testing.T) {
	runScriptCases(t, []scriptCase{{
		name: "ScriptCredential",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.credentials c", "blob_cred"), gosmotest.Rows(
				[]any{int64(65536), "blob_cred", "storage_acct", scriptWhen, scriptWhen, nil, nil}))
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return onServer(srv).ScriptCredential(ctx, "blob_cred")
		},
		want: "/* The credential's secret cannot be read from the server. Replace the\n" +
			"   placeholder below, or remove the SECRET clause if it has none. */\n" +
			"IF NOT EXISTS (SELECT 1 FROM sys.credentials WHERE name = N'blob_cred')\n" +
			"CREATE CREDENTIAL [blob_cred] WITH IDENTITY = N'storage_acct', SECRET = N'" + gosmo.SecretPlaceholder + "';\n" +
			"GO\n",
	}, {
		// A Service Broker endpoint's payload is a second read by its id.
		name: "ScriptEndpoint",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.endpoints e", "broker_ep"), gosmotest.Rows(
				[]any{int64(65540), "broker_ep", "sa", "TCP", "SERVICE_BROKER", "STARTED", false, int64(4022)}))
			f.OnFunc(read("FROM   sys.service_broker_endpoints sbe", int64(65540)), gosmotest.Rows(
				[]any{true, int64(20), "CERTIFICATE", "AES", "ep_cert"}))
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return onServer(srv).ScriptEndpoint(ctx, "broker_ep")
		},
		want: "IF NOT EXISTS (SELECT 1 FROM sys.endpoints WHERE name = N'broker_ep')\n" +
			"CREATE ENDPOINT [broker_ep]\n" +
			"    AUTHORIZATION [sa]\n" +
			"    STATE = STARTED\n" +
			"    AS TCP (LISTENER_PORT = 4022, LISTENER_IP = ALL)\n" +
			"    FOR SERVICE_BROKER (AUTHENTICATION = CERTIFICATE [ep_cert], ENCRYPTION = REQUIRED ALGORITHM AES, " +
			"MESSAGE_FORWARDING = ENABLED, MESSAGE_FORWARD_SIZE = 20);\n" +
			"GO\n",
	}, {
		// max_rollover_files at int's maximum is UNLIMITED; max_files 0 is unset.
		name: "ScriptServerAudit",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.server_audits a", "audit_main"), gosmotest.Rows(
				[]any{int64(65536), "audit_main", "8f0c2a4e-0000-0000-0000-000000000004", "FILE", "CONTINUE", int64(1000),
					"([server_principal_name]<>'sa')", true, scriptWhen, scriptWhen,
					"/var/opt/mssql/audit/", nil, int64(100), int64(2147483647), int64(0), false}))
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return onServer(srv).ScriptServerAudit(ctx, "audit_main")
		},
		want: "IF NOT EXISTS (SELECT 1 FROM sys.server_audits WHERE name = N'audit_main')\n" +
			"BEGIN\n" +
			"CREATE SERVER AUDIT [audit_main]\n" +
			"TO FILE\n" +
			"( FILEPATH = N'/var/opt/mssql/audit',\n" +
			"  MAXSIZE = 100 MB,\n" +
			"  MAX_ROLLOVER_FILES = UNLIMITED,\n" +
			"  RESERVE_DISK_SPACE = OFF )\n" +
			"WITH ( QUEUE_DELAY = 1000, ON_FAILURE = CONTINUE )\n" +
			"WHERE ([server_principal_name]<>'sa')\n" +
			"END\n" +
			"GO\n" +
			"\n" +
			"ALTER SERVER AUDIT [audit_main] WITH ( STATE = ON );\n" +
			"GO\n",
	}, {
		name: "ScriptServerAuditSpecification",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.server_audit_specifications s", "spec_logins"), gosmotest.Rows(
				[]any{int64(65537), "spec_logins", "8f0c2a4e-0000-0000-0000-000000000004", "audit_main", false, scriptWhen, scriptWhen,
					`[{"v":"FAILED_LOGIN_GROUP"},{"v":"SUCCESSFUL_LOGIN_GROUP"}]`}))
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return onServer(srv).ScriptServerAuditSpecification(ctx, "spec_logins")
		},
		want: "IF NOT EXISTS (SELECT 1 FROM sys.server_audit_specifications WHERE name = N'spec_logins')\n" +
			"BEGIN\n" +
			"CREATE SERVER AUDIT SPECIFICATION [spec_logins]\n" +
			"FOR SERVER AUDIT [audit_main]\n" +
			"    ADD (FAILED_LOGIN_GROUP),\n" +
			"    ADD (SUCCESSFUL_LOGIN_GROUP)\n" +
			"WITH ( STATE = OFF )\n" +
			"END\n" +
			"GO\n",
	}, {
		name: "ScriptMailAccount",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   msdb.dbo.sysmail_account a", "ops_smtp"), gosmotest.Rows(
				[]any{int64(1), "ops_smtp", "Ops relay", "ops@example.com", "Ops", "noreply@example.com",
					"SMTP", "smtp.example.com", int64(587), true, "relay_user", int64(5), false, int64(60), scriptWhen}))
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return onServer(srv).ScriptMailAccount(ctx, "ops_smtp")
		},
		want: "-- The account's password cannot be scripted. Replace <insert password here> before running.\n" +
			"IF NOT EXISTS (SELECT 1 FROM msdb.dbo.sysmail_account WHERE name = N'ops_smtp')\n" +
			"EXEC msdb.dbo.sysmail_add_account_sp @account_name = N'ops_smtp', @email_address = N'ops@example.com', " +
			"@display_name = N'Ops', @replyto_address = N'noreply@example.com', @description = N'Ops relay', " +
			"@mailserver_name = N'smtp.example.com', @port = 587, @enable_ssl = 1, @use_default_credentials = 0, " +
			"@username = N'relay_user', @password = N'<insert password here>';\n" +
			"GO\n" +
			"EXEC msdb.dbo.sysmail_update_account_sp @account_name = N'ops_smtp', @timeout = 60, @no_credential_change = 1;\n" +
			"GO\n",
	}, {
		// The profile's accounts are read by its id, in sequence; of the grants
		// (read for every profile) only this profile's are scripted, public's
		// by the 0x00 SID.
		name: "ScriptMailProfile",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   msdb.dbo.sysmail_profile p\nLEFT", "ops"), gosmotest.Rows(
				[]any{int64(3), "ops", "Ops profile", true, true, scriptWhen}))
			f.OnFunc(read("FROM   msdb.dbo.sysmail_profileaccount pa", int64(3)), gosmotest.Rows(
				[]any{int64(3), int64(1), "ops_smtp", int64(1)}, []any{int64(3), int64(2), "backup_smtp", int64(2)}))
			f.OnFunc(read("FROM   msdb.dbo.sysmail_principalprofile pp"), gosmotest.Rows(
				[]any{int64(3), "ops", []byte{0}, "public", true},
				[]any{int64(3), "ops", []byte{1, 2, 3}, "app_user", false},
				[]any{int64(9), "other", []byte{0}, "public", false}))
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return onServer(srv).ScriptMailProfile(ctx, "ops")
		},
		want: "IF NOT EXISTS (SELECT 1 FROM msdb.dbo.sysmail_profile WHERE name = N'ops')\n" +
			"EXEC msdb.dbo.sysmail_add_profile_sp @profile_name = N'ops', @description = N'Ops profile';\n" +
			"GO\n" +
			"IF NOT EXISTS (SELECT 1 FROM msdb.dbo.sysmail_profileaccount pa\n" +
			"              JOIN msdb.dbo.sysmail_profile p ON p.profile_id = pa.profile_id\n" +
			"              JOIN msdb.dbo.sysmail_account a ON a.account_id = pa.account_id WHERE p.name = N'ops' AND a.name = N'ops_smtp')\n" +
			"EXEC msdb.dbo.sysmail_add_profileaccount_sp @profile_name = N'ops', @account_name = N'ops_smtp', @sequence_number = 1;\n" +
			"GO\n" +
			"IF NOT EXISTS (SELECT 1 FROM msdb.dbo.sysmail_profileaccount pa\n" +
			"              JOIN msdb.dbo.sysmail_profile p ON p.profile_id = pa.profile_id\n" +
			"              JOIN msdb.dbo.sysmail_account a ON a.account_id = pa.account_id WHERE p.name = N'ops' AND a.name = N'backup_smtp')\n" +
			"EXEC msdb.dbo.sysmail_add_profileaccount_sp @profile_name = N'ops', @account_name = N'backup_smtp', @sequence_number = 2;\n" +
			"GO\n" +
			"IF NOT EXISTS (SELECT 1 FROM msdb.dbo.sysmail_principalprofile pp\n" +
			"              JOIN msdb.dbo.sysmail_profile p ON p.profile_id = pp.profile_id WHERE p.name = N'ops' AND pp.principal_sid = 0x00)\n" +
			"EXEC msdb.dbo.sysmail_add_principalprofile_sp @principal_name = N'public', @profile_name = N'ops', @is_default = 1;\n" +
			"GO\n" +
			"IF NOT EXISTS (SELECT 1 FROM msdb.dbo.sysmail_principalprofile pp\n" +
			"              JOIN msdb.dbo.sysmail_profile p ON p.profile_id = pp.profile_id WHERE p.name = N'ops' AND " +
			"pp.principal_sid = (SELECT sid FROM msdb.sys.database_principals WHERE name = N'app_user'))\n" +
			"EXEC msdb.dbo.sysmail_add_principalprofile_sp @principal_name = N'app_user', @profile_name = N'ops', @is_default = 0;\n" +
			"GO\n",
	}, {
		name: "ScriptResourceGovernor",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.resource_governor_configuration"), gosmotest.Rows(
				[]any{true, int64(1205579333), "dbo", "rg_classifier", int64(0)}))
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return onServer(srv).ScriptResourceGovernor(ctx)
		},
		want: "ALTER RESOURCE GOVERNOR WITH (CLASSIFIER_FUNCTION = [dbo].[rg_classifier]);\n" +
			"GO\n" +
			"ALTER RESOURCE GOVERNOR RECONFIGURE;\n" +
			"GO\n",
	}, {
		// REQUEST_MAX_MEMORY_GRANT_PERCENT 25 is the default, so not restated.
		name: "ScriptWorkloadGroup",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.resource_governor_workload_groups g", "reports"), gosmotest.Rows(
				[]any{int64(256), "reports", int64(257), "pool_reports", int64(2), "", "LOW", float64(25),
					int64(0), int64(0), int64(4), int64(0), nil, nil}))
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return onServer(srv).ScriptWorkloadGroup(ctx, "reports")
		},
		want: "IF NOT EXISTS (SELECT 1 FROM sys.resource_governor_workload_groups WHERE name = N'reports')\n" +
			"CREATE WORKLOAD GROUP [reports] WITH (IMPORTANCE = LOW, MAX_DOP = 4) USING [pool_reports];\n" +
			"GO\n" +
			"-- Takes effect after ALTER RESOURCE GOVERNOR RECONFIGURE, which also enables a disabled governor.\n",
	}, {
		// Group 0's mask maps to schedulers with no read of
		// sys.dm_os_schedulers, which has no rule here.
		name: "ScriptExternalResourcePool",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.resource_governor_external_resource_pools", "ext_r"), gosmotest.Rows(
				[]any{int64(256), "ext_r", int64(50), int64(30), int64(0), int64(1)}))
			f.OnFunc(read("FROM   sys.resource_governor_external_resource_pool_affinity"), gosmotest.Rows(
				[]any{int64(256), int64(0), int64(0b11)}, []any{int64(300), int64(0), int64(0b100)}))
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return onServer(srv).ScriptExternalResourcePool(ctx, "ext_r")
		},
		want: "IF NOT EXISTS (SELECT 1 FROM sys.resource_governor_external_resource_pools WHERE name = N'ext_r')\n" +
			"CREATE EXTERNAL RESOURCE POOL [ext_r] WITH (MAX_CPU_PERCENT = 50, MAX_MEMORY_PERCENT = 30, AFFINITY CPU = (0 TO 1));\n" +
			"GO\n" +
			"-- Takes effect after ALTER RESOURCE GOVERNOR RECONFIGURE, which also enables a disabled governor.\n",
	}, {
		// The session and its events, actions, targets and fields are five
		// reads by name, assembled into one CREATE.
		name: "ScriptEventSession",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("s.event_retention_mode_desc", "track_rpc"), gosmotest.Rows(
				[]any{int64(7), "track_rpc", false, false, string(gosmo.XERetentionAllowSingleEventLoss),
					int64(30000), int64(4096), int64(0), string(gosmo.XEPartitionNone), true, int64(0), int64(0)}))
			f.OnFunc(read("e.predicate", "track_rpc"), gosmotest.Rows(
				[]any{int64(7), int64(1), "sqlserver", "rpc_completed", "([duration]>(1000000))"}))
			f.OnFunc(read("FROM   sys.server_event_session_actions", "track_rpc"), gosmotest.Rows(
				[]any{int64(7), int64(1), "sqlserver", "sql_text"}))
			f.OnFunc(read("FROM   sys.server_event_session_targets", "track_rpc"), gosmotest.Rows(
				[]any{int64(7), int64(2), "package0", "ring_buffer"}))
			f.OnFunc(read("FROM   sys.server_event_session_fields", "track_rpc"), gosmotest.Rows(
				[]any{int64(7), int64(2), "max_events_limit", "100", "int"}))
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return onServer(srv).ScriptEventSession(ctx, "track_rpc")
		},
		want: "IF NOT EXISTS (SELECT 1 FROM sys.server_event_sessions WHERE name = N'track_rpc')\n" +
			"CREATE EVENT SESSION [track_rpc] ON SERVER\n" +
			"ADD EVENT sqlserver.rpc_completed(ACTION(sqlserver.sql_text)\n" +
			"    WHERE ([duration]>(1000000)))\n" +
			"ADD TARGET package0.ring_buffer(SET max_events_limit=(100))\n" +
			"WITH (MAX_MEMORY=4096 KB,EVENT_RETENTION_MODE=ALLOW_SINGLE_EVENT_LOSS,MAX_DISPATCH_LATENCY=30 SECONDS," +
			"MAX_EVENT_SIZE=0 KB,MEMORY_PARTITION_MODE=NONE,TRACK_CAUSALITY=ON,STARTUP_STATE=OFF);\n" +
			"GO\n",
	}})
}
