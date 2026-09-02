package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"strings"
	"sync"
	"testing"
)

// -- what this file pins ------------------------------------------------------
//
// iter.go's 91 *Seq methods are one line each: seqFrom(ctx, x.FooContext). A
// line that names the wrong FooContext still compiles whenever the element
// types match, and there are several such families here — Database.TableSeq
// against ViewsContext, Table.TriggerSeq against Database.TriggersContext,
// Server.AlertSeq against EventAlertsContext. iter_test.go covers seqFrom
// itself, so every one of those wrong wirings passes it: the adapter is fine,
// it is fetching the wrong collection.
//
// So each iterator is ranged over against a driver that records the SQL it
// issues, the matching ...Context method is called the same way, and the two
// recordings must be identical. A misnamed target then shows up as different
// SQL. Both halves are called with the same argument values (the arg*
// constants below) so a difference can only come from the wiring.
//
// Two guards keep that honest, and neither is optional:
//
//   - Every entry must record at least one statement. An iterator wired to a
//     method that returns early without querying would otherwise match a
//     wrongly-wired partner that also returns early, both recording nothing.
//   - The table is checked against iter.go's own declarations, so a new *Seq
//     method fails here until it is pinned rather than silently joining the
//     unexercised majority.
//
// The fake driver answers every query with zero rows, so what is exercised is
// the statement each collection method builds, never that the T-SQL is valid —
// that is the live tests' job.

// -- recording driver ---------------------------------------------------------

// recorder collects the statement text of every Exec and Query the driver
// sees. USE included: it is how Database.query reaches a database, so it is
// part of what distinguishes two database-scoped reads.
type recorder struct {
	mu         sync.Mutex
	statements []string
}

func (r *recorder) record(q string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statements = append(r.statements, q)
}

func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.statements
	r.statements = nil
	return out
}

// theRecorder is where recordingConn writes. The sql package owns connection
// creation, so there is nowhere to thread a per-test recorder through; the
// tests here run sequentially and take() drains between halves.
var theRecorder = &recorder{}

type recordingDriver struct{}

func (recordingDriver) Open(string) (driver.Conn, error) { return &recordingConn{}, nil }

type recordingConn struct{}

func (c *recordingConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *recordingConn) Close() error                        { return nil }
func (c *recordingConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (c *recordingConn) ExecContext(ctx context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	theRecorder.record(q)
	return driver.ResultNoRows, nil
}

func (c *recordingConn) QueryContext(ctx context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	theRecorder.record(q)
	return &emptyRows{}, nil
}

// emptyRows reports no columns and no rows, so no collection method ever
// scans and every one of them returns an empty slice.
type emptyRows struct{}

func (r *emptyRows) Columns() []string         { return nil }
func (r *emptyRows) Close() error              { return nil }
func (r *emptyRows) Next([]driver.Value) error { return io.EOF }

func init() { sql.Register("recordq", recordingDriver{}) }

// -- shared arguments ---------------------------------------------------------

// The *Seq methods that take arguments are called with these on both halves,
// so any difference in the recorded SQL is the wiring and not the input.
const (
	argSchema        = "dbo"
	argName          = "T"
	argPrincipal     = "dbo"
	argRole          = "db_owner"
	argLogin         = "sa"
	argDevice        = "/var/opt/mssql/backup/b.bak"
	argDatabase      = "testdb"
	argPattern       = "%"
	argFragMode      = "LIMITED"
	argLimit         = 10
	argLogNumber     = 0
	argIncludeSystem = true
	argLogType       = ErrorLogSQLServer
	argCategoryClass = CategoryClassJob
)

var argExtPropLevel = ExtendedPropertyLevel{
	Level0Type: "SCHEMA", Level0Name: "dbo",
	Level1Type: "TABLE", Level1Name: "T",
}

// seqWiring is one iterator and the collection method it is supposed to run.
type seqWiring struct {
	name   string
	seq    func(ctx context.Context)
	direct func(ctx context.Context)
}

// iterWirings builds the table against receivers hung off one recording
// Server. Every receiver carries a plausible identity — an ObjectID, a name,
// an ID — because a collection method that finds a zero-valued key can take a
// short path and query nothing, which the "at least one statement" guard would
// then fail on.
func iterWirings(t *testing.T) []seqWiring {
	t.Helper()

	sdb, err := sql.Open("recordq", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { sdb.Close() })

	srv := &Server{db: sdb, info: &ServerInfo{VersionMajor: 16}}
	dbo := &Database{server: srv, name: argDatabase, id: 7}
	tbl := &Table{db: dbo, ObjectID: 42, Schema: argSchema, Name: argName}
	stat := &Statistic{table: tbl, Name: "IX_stat", StatID: 3}
	job := &Job{server: srv, JobID: "8A2C4E1F-0000-0000-0000-000000000001", Name: "nightly"}
	ag := &AvailabilityGroup{server: srv, ID: "8A2C4E1F-0000-0000-0000-000000000002", Name: "ag1"}
	op := &Operator{server: srv, ID: 5, Name: "dba"}
	sch := &Schedule{server: srv, ID: 9, Name: "nightly-sched"}
	lg := &Login{server: srv, Name: argLogin}
	al := &Alert{server: srv, ID: 4, Name: "sev17"}

	return []seqWiring{
		{"Alert.NotificationSeq",
			func(ctx context.Context) {
				for range al.NotificationSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = al.NotificationsContext(ctx) }},
		{"AvailabilityGroup.DatabaseSeq",
			func(ctx context.Context) {
				for range ag.DatabaseSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = ag.DatabasesContext(ctx) }},
		{"AvailabilityGroup.ListenerSeq",
			func(ctx context.Context) {
				for range ag.ListenerSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = ag.ListenersContext(ctx) }},
		{"AvailabilityGroup.ReplicaSeq",
			func(ctx context.Context) {
				for range ag.ReplicaSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = ag.ReplicasContext(ctx) }},
		{"Database.CertificateSeq",
			func(ctx context.Context) {
				for range dbo.CertificateSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.CertificatesContext(ctx) }},
		{"Database.ColumnEncryptionKeySeq",
			func(ctx context.Context) {
				for range dbo.ColumnEncryptionKeySeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.ColumnEncryptionKeysContext(ctx) }},
		{"Database.ColumnMasterKeySeq",
			func(ctx context.Context) {
				for range dbo.ColumnMasterKeySeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.ColumnMasterKeysContext(ctx) }},
		{"Database.ColumnPermissionSeq",
			func(ctx context.Context) {
				for range dbo.ColumnPermissionSeq(ctx, argSchema, argName) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.ColumnPermissionsContext(ctx, argSchema, argName) }},
		{"Database.ColumnPermissionsForPrincipalSeq",
			func(ctx context.Context) {
				for range dbo.ColumnPermissionsForPrincipalSeq(ctx, argPrincipal) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.ColumnPermissionsForPrincipalContext(ctx, argPrincipal) }},
		{"Database.DatabaseExtendedPropertySeq",
			func(ctx context.Context) {
				for range dbo.DatabaseExtendedPropertySeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.DatabaseExtendedPropertiesContext(ctx) }},
		{"Database.DatabasePermissionSeq",
			func(ctx context.Context) {
				for range dbo.DatabasePermissionSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.DatabasePermissionsContext(ctx) }},
		{"Database.DatabaseRoleSeq",
			func(ctx context.Context) {
				for range dbo.DatabaseRoleSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.DatabaseRolesContext(ctx) }},
		{"Database.DatabaseScopedConfigSeq",
			func(ctx context.Context) {
				for range dbo.DatabaseScopedConfigSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.DatabaseScopedConfigsContext(ctx) }},
		{"Database.DependencySeq",
			func(ctx context.Context) {
				for range dbo.DependencySeq(ctx, argSchema, argName) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.DependenciesContext(ctx, argSchema, argName) }},
		{"Database.DependentSeq",
			func(ctx context.Context) {
				for range dbo.DependentSeq(ctx, argSchema, argName) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.DependentsContext(ctx, argSchema, argName) }},
		{"Database.EffectiveObjectPermissionSeq",
			func(ctx context.Context) {
				for range dbo.EffectiveObjectPermissionSeq(ctx, argSchema, argName, argPrincipal) {
				}
			},
			func(ctx context.Context) {
				_, _ = dbo.EffectiveObjectPermissionsContext(ctx, argSchema, argName, argPrincipal)
			}},
		{"Database.EffectivePermissionSeq",
			func(ctx context.Context) {
				for range dbo.EffectivePermissionSeq(ctx, argPrincipal) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.EffectivePermissionsContext(ctx, argPrincipal) }},
		{"Database.EffectiveSchemaPermissionSeq",
			func(ctx context.Context) {
				for range dbo.EffectiveSchemaPermissionSeq(ctx, argSchema, argPrincipal) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.EffectiveSchemaPermissionsContext(ctx, argSchema, argPrincipal) }},
		{"Database.ExtendedPropertySeq",
			func(ctx context.Context) {
				for range dbo.ExtendedPropertySeq(ctx, argExtPropLevel) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.ExtendedPropertiesContext(ctx, argExtPropLevel) }},
		{"Database.FileGroupSeq",
			func(ctx context.Context) {
				for range dbo.FileGroupSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.FileGroupsContext(ctx) }},
		{"Database.FileSeq",
			func(ctx context.Context) {
				for range dbo.FileSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.FilesContext(ctx) }},
		{"Database.ObjectColumnSeq",
			func(ctx context.Context) {
				for range dbo.ObjectColumnSeq(ctx, argSchema, argName) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.ObjectColumnsContext(ctx, argSchema, argName) }},
		{"Database.ParameterSeq",
			func(ctx context.Context) {
				for range dbo.ParameterSeq(ctx, argSchema, argName) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.ParametersContext(ctx, argSchema, argName) }},
		{"Database.PartitionFunctionSeq",
			func(ctx context.Context) {
				for range dbo.PartitionFunctionSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.PartitionFunctionsContext(ctx) }},
		{"Database.PartitionSchemeSeq",
			func(ctx context.Context) {
				for range dbo.PartitionSchemeSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.PartitionSchemesContext(ctx) }},
		{"Database.PermissionSeq",
			func(ctx context.Context) {
				for range dbo.PermissionSeq(ctx, argSchema, argName) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.PermissionsContext(ctx, argSchema, argName) }},
		{"Database.PermissionsForPrincipalSeq",
			func(ctx context.Context) {
				for range dbo.PermissionsForPrincipalSeq(ctx, argPrincipal) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.PermissionsForPrincipalContext(ctx, argPrincipal) }},
		{"Database.RoleMemberSeq",
			func(ctx context.Context) {
				for range dbo.RoleMemberSeq(ctx, argRole) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.RoleMembersContext(ctx, argRole) }},
		{"Database.SchemaPermissionSeq",
			func(ctx context.Context) {
				for range dbo.SchemaPermissionSeq(ctx, argSchema) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.SchemaPermissionsContext(ctx, argSchema) }},
		{"Database.SchemaSeq",
			func(ctx context.Context) {
				for range dbo.SchemaSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.SchemasContext(ctx) }},
		{"Database.SearchSeq",
			func(ctx context.Context) {
				for range dbo.SearchSeq(ctx, argPattern) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.SearchContext(ctx, argPattern) }},
		{"Database.SecurityPolicySeq",
			func(ctx context.Context) {
				for range dbo.SecurityPolicySeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.SecurityPoliciesContext(ctx) }},
		{"Database.SequenceSeq",
			func(ctx context.Context) {
				for range dbo.SequenceSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.SequencesContext(ctx) }},
		{"Database.StoredProcedureSeq",
			func(ctx context.Context) {
				for range dbo.StoredProcedureSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.StoredProceduresContext(ctx) }},
		{"Database.SynonymSeq",
			func(ctx context.Context) {
				for range dbo.SynonymSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.SynonymsContext(ctx) }},
		{"Database.SystemFunctionSeq",
			func(ctx context.Context) {
				for range dbo.SystemFunctionSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.SystemFunctionsContext(ctx) }},
		{"Database.SystemStoredProcedureSeq",
			func(ctx context.Context) {
				for range dbo.SystemStoredProcedureSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.SystemStoredProceduresContext(ctx) }},
		{"Database.SystemViewSeq",
			func(ctx context.Context) {
				for range dbo.SystemViewSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.SystemViewsContext(ctx) }},
		{"Database.TableChangeTrackingSeq",
			func(ctx context.Context) {
				for range dbo.TableChangeTrackingSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.TableChangeTrackingContext(ctx) }},
		{"Database.TableSeq",
			func(ctx context.Context) {
				for range dbo.TableSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.TablesContext(ctx) }},
		{"Database.TablesBySchemaSeq",
			func(ctx context.Context) {
				for range dbo.TablesBySchemaSeq(ctx, argSchema) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.TablesBySchemaContext(ctx, argSchema) }},
		{"Database.TriggerSeq",
			func(ctx context.Context) {
				for range dbo.TriggerSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.TriggersContext(ctx) }},
		{"Database.UserDefinedFunctionSeq",
			func(ctx context.Context) {
				for range dbo.UserDefinedFunctionSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.UserDefinedFunctionsContext(ctx) }},
		{"Database.UserSeq",
			func(ctx context.Context) {
				for range dbo.UserSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.UsersContext(ctx) }},
		{"Database.ViewSeq",
			func(ctx context.Context) {
				for range dbo.ViewSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = dbo.ViewsContext(ctx) }},
		{"Job.HistorySeq",
			func(ctx context.Context) {
				for range job.HistorySeq(ctx, argLimit) {
				}
			},
			func(ctx context.Context) { _, _ = job.HistoryContext(ctx, argLimit) }},
		{"Job.ScheduleSeq",
			func(ctx context.Context) {
				for range job.ScheduleSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = job.SchedulesContext(ctx) }},
		{"Job.StepSeq",
			func(ctx context.Context) {
				for range job.StepSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = job.StepsContext(ctx) }},
		{"Login.UserMappingSeq",
			func(ctx context.Context) {
				for range lg.UserMappingSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = lg.UserMappingsContext(ctx) }},
		{"Operator.NotifyingAlertSeq",
			func(ctx context.Context) {
				for range op.NotifyingAlertSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = op.NotifyingAlertsContext(ctx) }},
		{"Operator.NotifyingJobSeq",
			func(ctx context.Context) {
				for range op.NotifyingJobSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = op.NotifyingJobsContext(ctx) }},
		{"Schedule.JobSeq",
			func(ctx context.Context) {
				for range sch.JobSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = sch.JobsContext(ctx) }},
		{"Server.ActiveSessionSeq",
			func(ctx context.Context) {
				for range srv.ActiveSessionSeq(ctx, argIncludeSystem) {
				}
			},
			func(ctx context.Context) { _, _ = srv.ActiveSessionsContext(ctx, argIncludeSystem) }},
		{"Server.AlertSeq",
			func(ctx context.Context) {
				for range srv.AlertSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = srv.AlertsContext(ctx) }},
		{"Server.AvailabilityGroupSeq",
			func(ctx context.Context) {
				for range srv.AvailabilityGroupSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = srv.AvailabilityGroupsContext(ctx) }},
		{"Server.BackupFileSeq",
			func(ctx context.Context) {
				for range srv.BackupFileSeq(ctx, argDevice) {
				}
			},
			func(ctx context.Context) { _, _ = srv.BackupFileListContext(ctx, argDevice) }},
		{"Server.BackupHeaderSeq",
			func(ctx context.Context) {
				for range srv.BackupHeaderSeq(ctx, argDevice) {
				}
			},
			func(ctx context.Context) { _, _ = srv.BackupHeadersContext(ctx, argDevice) }},
		{"Server.BackupHistorySeq",
			func(ctx context.Context) {
				for range srv.BackupHistorySeq(ctx, argDatabase) {
				}
			},
			func(ctx context.Context) { _, _ = srv.BackupHistoryContext(ctx, argDatabase) }},
		{"Server.CategorySeq",
			func(ctx context.Context) {
				for range srv.CategorySeq(ctx, argCategoryClass) {
				}
			},
			func(ctx context.Context) { _, _ = srv.CategoriesContext(ctx, argCategoryClass) }},
		{"Server.BackupDeviceSeq",
			func(ctx context.Context) {
				for range srv.BackupDeviceSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = srv.BackupDevicesContext(ctx) }},
		{"Server.ServerTriggerSeq",
			func(ctx context.Context) {
				for range srv.ServerTriggerSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = srv.ServerTriggersContext(ctx) }},
		{"Server.EndpointSeq",
			func(ctx context.Context) {
				for range srv.EndpointSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = srv.EndpointsContext(ctx) }},
		{"Server.ServerAuditSeq",
			func(ctx context.Context) {
				for range srv.ServerAuditSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = srv.ServerAuditsContext(ctx) }},
		{"Server.ServerAuditSpecificationSeq",
			func(ctx context.Context) {
				for range srv.ServerAuditSpecificationSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = srv.ServerAuditSpecificationsContext(ctx) }},
		{"Server.ConfigurationSeq",
			func(ctx context.Context) {
				for range srv.ConfigurationSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = srv.ConfigurationsContext(ctx) }},
		{"Server.CredentialSeq",
			func(ctx context.Context) {
				for range srv.CredentialSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = srv.CredentialsContext(ctx) }},
		{"Server.DatabaseSeq",
			func(ctx context.Context) {
				for range srv.DatabaseSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = srv.DatabasesContext(ctx) }},
		{"Server.DiskVolumeSeq",
			func(ctx context.Context) {
				for range srv.DiskVolumeSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = srv.DiskVolumesContext(ctx) }},
		{"Server.EffectiveServerPermissionSeq",
			func(ctx context.Context) {
				for range srv.EffectiveServerPermissionSeq(ctx, argLogin) {
				}
			},
			func(ctx context.Context) { _, _ = srv.EffectiveServerPermissionsContext(ctx, argLogin) }},
		{"Server.EnumErrorLogSeq",
			func(ctx context.Context) {
				for range srv.EnumErrorLogSeq(ctx, argLogType) {
				}
			},
			func(ctx context.Context) { _, _ = srv.EnumErrorLogsContext(ctx, argLogType) }},
		{"Server.EventAlertSeq",
			func(ctx context.Context) {
				for range srv.EventAlertSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = srv.EventAlertsContext(ctx) }},
		{"Server.JobHistorySeq",
			func(ctx context.Context) {
				for range srv.JobHistorySeq(ctx, argLimit) {
				}
			},
			func(ctx context.Context) { _, _ = srv.JobHistoryContext(ctx, argLimit) }},
		{"Server.JobSeq",
			func(ctx context.Context) {
				for range srv.JobSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = srv.JobsContext(ctx) }},
		{"Server.LanguageSeq",
			func(ctx context.Context) {
				for range srv.LanguageSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = srv.LanguagesContext(ctx) }},
		{"Server.LinkedServerSeq",
			func(ctx context.Context) {
				for range srv.LinkedServerSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = srv.LinkedServersContext(ctx) }},
		{"Server.LoginSeq",
			func(ctx context.Context) {
				for range srv.LoginSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = srv.LoginsContext(ctx) }},
		{"Server.MailProfileSeq",
			func(ctx context.Context) {
				for range srv.MailProfileSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = srv.MailProfilesContext(ctx) }},
		{"Server.OperatorSeq",
			func(ctx context.Context) {
				for range srv.OperatorSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = srv.OperatorsContext(ctx) }},
		{"Server.ReadErrorLogSeq",
			func(ctx context.Context) {
				for range srv.ReadErrorLogSeq(ctx, argLogNumber) {
				}
			},
			func(ctx context.Context) { _, _ = srv.ReadErrorLogContext(ctx, argLogNumber) }},
		{"Server.ReadLogSeq",
			func(ctx context.Context) {
				for range srv.ReadLogSeq(ctx, argLogType, argLogNumber) {
				}
			},
			func(ctx context.Context) { _, _ = srv.ReadLogContext(ctx, argLogType, argLogNumber) }},
		{"Server.ScheduleSeq",
			func(ctx context.Context) {
				for range srv.ScheduleSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = srv.SchedulesContext(ctx) }},
		{"Server.ServerPermissionSeq",
			func(ctx context.Context) {
				for range srv.ServerPermissionSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = srv.ServerPermissionsContext(ctx) }},
		{"Server.ServerRoleMemberSeq",
			func(ctx context.Context) {
				for range srv.ServerRoleMemberSeq(ctx, argRole) {
				}
			},
			func(ctx context.Context) { _, _ = srv.ServerRoleMembersContext(ctx, argRole) }},
		{"Server.ServerRoleSeq",
			func(ctx context.Context) {
				for range srv.ServerRoleSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = srv.ServerRolesContext(ctx) }},
		{"Statistic.ColumnSeq",
			func(ctx context.Context) {
				for range stat.ColumnSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = stat.ColumnsContext(ctx) }},
		{"Statistic.DensityVectorSeq",
			func(ctx context.Context) {
				for range stat.DensityVectorSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = stat.DensityVectorContext(ctx) }},
		{"Statistic.HistogramSeq",
			func(ctx context.Context) {
				for range stat.HistogramSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = stat.HistogramContext(ctx) }},
		{"Table.CheckConstraintSeq",
			func(ctx context.Context) {
				for range tbl.CheckConstraintSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = tbl.CheckConstraintsContext(ctx) }},
		{"Table.ColumnSeq",
			func(ctx context.Context) {
				for range tbl.ColumnSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = tbl.ColumnsContext(ctx) }},
		{"Table.ForeignKeySeq",
			func(ctx context.Context) {
				for range tbl.ForeignKeySeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = tbl.ForeignKeysContext(ctx) }},
		{"Table.FragmentationStatsSeq",
			func(ctx context.Context) {
				for range tbl.FragmentationStatsSeq(ctx, argFragMode) {
				}
			},
			func(ctx context.Context) { _, _ = tbl.FragmentationStatsContext(ctx, argFragMode) }},
		{"Table.IndexSeq",
			func(ctx context.Context) {
				for range tbl.IndexSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = tbl.IndexesContext(ctx) }},
		{"Table.PartitionSeq",
			func(ctx context.Context) {
				for range tbl.PartitionSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = tbl.PartitionsContext(ctx) }},
		{"Table.StatisticSeq",
			func(ctx context.Context) {
				for range tbl.StatisticSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = tbl.StatisticsContext(ctx) }},
		{"Table.TriggerSeq",
			func(ctx context.Context) {
				for range tbl.TriggerSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = tbl.TriggersContext(ctx) }},
		{"Table.XMLIndexSeq",
			func(ctx context.Context) {
				for range tbl.XMLIndexSeq(ctx) {
				}
			},
			func(ctx context.Context) { _, _ = tbl.XMLIndexesContext(ctx) }}}
}

// TestEverySeqRunsItsOwnCollectionMethod is the wiring pin: ranging an
// iterator must issue exactly the statements its ...Context partner issues.
func TestEverySeqRunsItsOwnCollectionMethod(t *testing.T) {
	for _, w := range iterWirings(t) {
		t.Run(w.name, func(t *testing.T) {
			theRecorder.take()

			w.seq(context.Background())
			viaSeq := theRecorder.take()

			w.direct(context.Background())
			viaDirect := theRecorder.take()

			if len(viaSeq) == 0 {
				t.Fatalf("%s issued no statements — the comparison below would pass against any wiring", w.name)
			}
			if strings.Join(viaSeq, "\n;\n") != strings.Join(viaDirect, "\n;\n") {
				t.Errorf("%s ran different SQL from its collection method.\n--- via the iterator ---\n%s\n--- via the method ---\n%s",
					w.name, strings.Join(viaSeq, "\n;\n"), strings.Join(viaDirect, "\n;\n"))
			}
		})
	}
}

// TestEverySeqInIterGoIsPinned keeps the table above complete. Without it a
// newly added iterator is simply not covered, which is the state iter.go was
// in before this file existed.
func TestEverySeqInIterGoIsPinned(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "iter.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing iter.go: %v", err)
	}

	declared := map[string]bool{}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || !strings.HasSuffix(fn.Name.Name, "Seq") {
			continue
		}
		star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		recv, ok := star.X.(*ast.Ident)
		if !ok {
			continue
		}
		declared[recv.Name+"."+fn.Name.Name] = true
	}

	pinned := map[string]bool{}
	for _, w := range iterWirings(t) {
		if pinned[w.name] {
			t.Errorf("%s appears twice in the table", w.name)
		}
		pinned[w.name] = true
	}

	for name := range declared {
		if !pinned[name] {
			t.Errorf("%s is declared in iter.go but not pinned by the table above", name)
		}
	}
	for name := range pinned {
		if !declared[name] {
			t.Errorf("%s is pinned by the table above but no longer declared in iter.go", name)
		}
	}
}
