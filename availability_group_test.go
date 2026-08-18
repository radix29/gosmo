package gosmo

import (
	"context"
	"strings"
	"testing"
)

// The availability-group setters build DDL that is never parameterised and
// runs against a live cluster, so these tests pin the exact statement text
// under WithScript rather than asserting "no error" — a wrong keyword here
// fails on the server, or worse, succeeds on the wrong thing.

func scriptAG(t *testing.T, name string, fn func(ctx context.Context, ag *AvailabilityGroup) error) []string {
	t.Helper()
	ag := &AvailabilityGroup{server: &Server{}, Name: name}
	ctx, script := WithScript(context.Background())
	if err := fn(ctx, ag); err != nil {
		t.Fatalf("under WithScript: %v", err)
	}
	return script.Statements
}

func scriptReplica(t *testing.T, group, replica string, fn func(ctx context.Context, r *AvailabilityReplica) error) []string {
	t.Helper()
	r := &AvailabilityReplica{server: &Server{}, GroupName: group, ReplicaServerName: replica}
	ctx, script := WithScript(context.Background())
	if err := fn(ctx, r); err != nil {
		t.Fatalf("under WithScript: %v", err)
	}
	return script.Statements
}

func soleStatement(t *testing.T, stmts []string) string {
	t.Helper()
	if len(stmts) != 1 {
		t.Fatalf("got %d statements, want 1: %q", len(stmts), stmts)
	}
	return stmts[0]
}

func TestAvailabilityGroupSetStatements(t *testing.T) {
	tests := []struct {
		name string
		fn   func(ctx context.Context, ag *AvailabilityGroup) error
		want string
	}{
		{"backup preference", func(ctx context.Context, ag *AvailabilityGroup) error {
			return ag.SetAutomatedBackupPreferenceContext(ctx, "SECONDARY_ONLY")
		}, "ALTER AVAILABILITY GROUP [AAG1] SET (AUTOMATED_BACKUP_PREFERENCE = SECONDARY_ONLY)"},
		{"failure condition level", func(ctx context.Context, ag *AvailabilityGroup) error {
			return ag.SetFailureConditionLevelContext(ctx, 3)
		}, "ALTER AVAILABILITY GROUP [AAG1] SET (FAILURE_CONDITION_LEVEL = 3)"},
		{"health check timeout", func(ctx context.Context, ag *AvailabilityGroup) error {
			return ag.SetHealthCheckTimeoutContext(ctx, 30000)
		}, "ALTER AVAILABILITY GROUP [AAG1] SET (HEALTH_CHECK_TIMEOUT = 30000)"},
		{"db failover on", func(ctx context.Context, ag *AvailabilityGroup) error {
			return ag.SetDBFailoverContext(ctx, true)
		}, "ALTER AVAILABILITY GROUP [AAG1] SET (DB_FAILOVER = ON)"},
		{"db failover off", func(ctx context.Context, ag *AvailabilityGroup) error {
			return ag.SetDBFailoverContext(ctx, false)
		}, "ALTER AVAILABILITY GROUP [AAG1] SET (DB_FAILOVER = OFF)"},
		// DTC_SUPPORT is the one flag whose off value is not OFF.
		{"dtc support on", func(ctx context.Context, ag *AvailabilityGroup) error {
			return ag.SetDTCSupportContext(ctx, true)
		}, "ALTER AVAILABILITY GROUP [AAG1] SET (DTC_SUPPORT = PER_DB)"},
		{"dtc support off", func(ctx context.Context, ag *AvailabilityGroup) error {
			return ag.SetDTCSupportContext(ctx, false)
		}, "ALTER AVAILABILITY GROUP [AAG1] SET (DTC_SUPPORT = NONE)"},
		{"required synchronized secondaries", func(ctx context.Context, ag *AvailabilityGroup) error {
			return ag.SetRequiredSynchronizedSecondariesToCommitContext(ctx, 1)
		}, "ALTER AVAILABILITY GROUP [AAG1] SET (REQUIRED_SYNCHRONIZED_SECONDARIES_TO_COMMIT = 1)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := soleStatement(t, scriptAG(t, "AAG1", tt.fn)); got != tt.want {
				t.Errorf("statement =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

func TestAvailabilityGroupNameIsBracketQuoted(t *testing.T) {
	// A group name is an identifier, so a "]" in it has to be doubled or the
	// statement runs against a different (or no) group.
	got := soleStatement(t, scriptAG(t, "Odd]Name", func(ctx context.Context, ag *AvailabilityGroup) error {
		return ag.SetDBFailoverContext(ctx, true)
	}))
	if !strings.HasPrefix(got, "ALTER AVAILABILITY GROUP [Odd]]Name] ") {
		t.Errorf("statement = %q, want the group name bracket-quoted with a doubled ]", got)
	}
}

func TestAvailabilityGroupSettersRejectBadValues(t *testing.T) {
	ag := &AvailabilityGroup{server: &Server{}, Name: "AAG1"}
	ctx, script := WithScript(context.Background())

	cases := map[string]error{
		"unknown backup preference": ag.SetAutomatedBackupPreferenceContext(ctx, "MAYBE"),
		"failure level 0":           ag.SetFailureConditionLevelContext(ctx, 0),
		"failure level 6":           ag.SetFailureConditionLevelContext(ctx, 6),
		"timeout below floor":       ag.SetHealthCheckTimeoutContext(ctx, 14999),
		"negative required sync":    ag.SetRequiredSynchronizedSecondariesToCommitContext(ctx, -1),
	}
	for name, err := range cases {
		if err == nil {
			t.Errorf("%s: got nil error, want a rejection", name)
		}
	}
	if len(script.Statements) != 0 {
		t.Errorf("rejected calls still produced statements: %q", script.Statements)
	}
}

func TestAvailabilityReplicaModifyStatements(t *testing.T) {
	tests := []struct {
		name string
		fn   func(ctx context.Context, r *AvailabilityReplica) error
		want string
	}{
		{"availability mode", func(ctx context.Context, r *AvailabilityReplica) error {
			return r.SetAvailabilityModeContext(ctx, "SYNCHRONOUS_COMMIT")
		}, "ALTER AVAILABILITY GROUP [AAG1] MODIFY REPLICA ON N'ubusql2' WITH (AVAILABILITY_MODE = SYNCHRONOUS_COMMIT)"},
		{"failover mode", func(ctx context.Context, r *AvailabilityReplica) error {
			return r.SetFailoverModeContext(ctx, "EXTERNAL")
		}, "ALTER AVAILABILITY GROUP [AAG1] MODIFY REPLICA ON N'ubusql2' WITH (FAILOVER_MODE = EXTERNAL)"},
		{"seeding mode", func(ctx context.Context, r *AvailabilityReplica) error {
			return r.SetSeedingModeContext(ctx, "AUTOMATIC")
		}, "ALTER AVAILABILITY GROUP [AAG1] MODIFY REPLICA ON N'ubusql2' WITH (SEEDING_MODE = AUTOMATIC)"},
		{"session timeout", func(ctx context.Context, r *AvailabilityReplica) error {
			return r.SetSessionTimeoutContext(ctx, 20)
		}, "ALTER AVAILABILITY GROUP [AAG1] MODIFY REPLICA ON N'ubusql2' WITH (SESSION_TIMEOUT = 20)"},
		{"backup priority", func(ctx context.Context, r *AvailabilityReplica) error {
			return r.SetBackupPriorityContext(ctx, 0)
		}, "ALTER AVAILABILITY GROUP [AAG1] MODIFY REPLICA ON N'ubusql2' WITH (BACKUP_PRIORITY = 0)"},
		// The role-scoped options nest inside PRIMARY_ROLE/SECONDARY_ROLE, and
		// which one owns which option is the easy thing to get backwards: the
		// routing URL is a secondary-role property, the routing list a
		// primary-role one.
		{"primary role connections", func(ctx context.Context, r *AvailabilityReplica) error {
			return r.SetPrimaryRoleAllowConnectionsContext(ctx, "READ_WRITE")
		}, "ALTER AVAILABILITY GROUP [AAG1] MODIFY REPLICA ON N'ubusql2' WITH (PRIMARY_ROLE (ALLOW_CONNECTIONS = READ_WRITE))"},
		{"secondary role connections", func(ctx context.Context, r *AvailabilityReplica) error {
			return r.SetSecondaryRoleAllowConnectionsContext(ctx, "READ_ONLY")
		}, "ALTER AVAILABILITY GROUP [AAG1] MODIFY REPLICA ON N'ubusql2' WITH (SECONDARY_ROLE (ALLOW_CONNECTIONS = READ_ONLY))"},
		{"routing url", func(ctx context.Context, r *AvailabilityReplica) error {
			return r.SetReadOnlyRoutingURLContext(ctx, "TCP://ubusql2:1433")
		}, "ALTER AVAILABILITY GROUP [AAG1] MODIFY REPLICA ON N'ubusql2' WITH (SECONDARY_ROLE (READ_ONLY_ROUTING_URL = N'TCP://ubusql2:1433'))"},
		{"routing url cleared", func(ctx context.Context, r *AvailabilityReplica) error {
			return r.SetReadOnlyRoutingURLContext(ctx, "")
		}, "ALTER AVAILABILITY GROUP [AAG1] MODIFY REPLICA ON N'ubusql2' WITH (SECONDARY_ROLE (READ_ONLY_ROUTING_URL = NONE))"},
		{"routing list", func(ctx context.Context, r *AvailabilityReplica) error {
			return r.SetReadOnlyRoutingListContext(ctx, [][]string{{"ubusql1"}})
		}, "ALTER AVAILABILITY GROUP [AAG1] MODIFY REPLICA ON N'ubusql2' WITH (PRIMARY_ROLE (READ_ONLY_ROUTING_LIST = (N'ubusql1')))"},
		{"routing list load balanced", func(ctx context.Context, r *AvailabilityReplica) error {
			return r.SetReadOnlyRoutingListContext(ctx, [][]string{{"a", "b"}, {"c"}})
		}, "ALTER AVAILABILITY GROUP [AAG1] MODIFY REPLICA ON N'ubusql2' WITH (PRIMARY_ROLE (READ_ONLY_ROUTING_LIST = ((N'a', N'b'), N'c')))"},
		{"routing list cleared", func(ctx context.Context, r *AvailabilityReplica) error {
			return r.SetReadOnlyRoutingListContext(ctx, nil)
		}, "ALTER AVAILABILITY GROUP [AAG1] MODIFY REPLICA ON N'ubusql2' WITH (PRIMARY_ROLE (READ_ONLY_ROUTING_LIST = NONE))"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := soleStatement(t, scriptReplica(t, "AAG1", "ubusql2", tt.fn)); got != tt.want {
				t.Errorf("statement =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

func TestAvailabilityReplicaNameIsQuotedAsALiteral(t *testing.T) {
	// MODIFY REPLICA ON takes a string literal, not an identifier, so a quote
	// in the name has to be doubled — bracket-quoting it would be wrong here.
	got := soleStatement(t, scriptReplica(t, "AAG1", "o'brien", func(ctx context.Context, r *AvailabilityReplica) error {
		return r.SetBackupPriorityContext(ctx, 50)
	}))
	if !strings.Contains(got, "MODIFY REPLICA ON N'o''brien'") {
		t.Errorf("statement = %q, want the replica name as a literal with a doubled quote", got)
	}
}

func TestAvailabilityReplicaSettersRejectBadValues(t *testing.T) {
	r := &AvailabilityReplica{server: &Server{}, GroupName: "AAG1", ReplicaServerName: "ubusql2"}
	ctx, script := WithScript(context.Background())

	cases := map[string]error{
		"unknown availability mode": r.SetAvailabilityModeContext(ctx, "SOMETIMES"),
		"unknown failover mode":     r.SetFailoverModeContext(ctx, "SOMETIMES"),
		"unknown seeding mode":      r.SetSeedingModeContext(ctx, "SOMETIMES"),
		// NO is a secondary-role value only: a primary that accepts nothing
		// would be unusable, and SQL Server has no such option.
		"NO in the primary role":     r.SetPrimaryRoleAllowConnectionsContext(ctx, "NO"),
		"unknown secondary role":     r.SetSecondaryRoleAllowConnectionsContext(ctx, "SOMETIMES"),
		"session timeout below5":     r.SetSessionTimeoutContext(ctx, 4),
		"backup priority above 100":  r.SetBackupPriorityContext(ctx, 101),
		"backup priority negative":   r.SetBackupPriorityContext(ctx, -1),
		"routing list blank name":    r.SetReadOnlyRoutingListContext(ctx, [][]string{{" "}}),
		"detached replica has no AG": (&AvailabilityReplica{server: &Server{}, ReplicaServerName: "x"}).SetBackupPriorityContext(ctx, 1),
	}
	for name, err := range cases {
		if err == nil {
			t.Errorf("%s: got nil error, want a rejection", name)
		}
	}
	if len(script.Statements) != 0 {
		t.Errorf("rejected calls still produced statements: %q", script.Statements)
	}
}

func TestFormatRoutingListDropsEmptySets(t *testing.T) {
	// An empty inner slice would render as "()", which is a syntax error, so
	// it is dropped rather than emitted.
	got, err := formatRoutingList([][]string{{"a"}, {}, {"b"}})
	if err != nil {
		t.Fatalf("formatRoutingList: %v", err)
	}
	if want := "(N'a', N'b')"; got != want {
		t.Errorf("formatRoutingList = %q, want %q", got, want)
	}

	// A list that is entirely empty sets means "no routing list", not "()".
	got, err = formatRoutingList([][]string{{}, {}})
	if err != nil {
		t.Fatalf("formatRoutingList: %v", err)
	}
	if got != "NONE" {
		t.Errorf("all-empty list = %q, want NONE", got)
	}
}

func TestAvailabilityGroupSettersMirrorOnlyWhenApplied(t *testing.T) {
	// Under WithScript the server never saw the write, so the receiver must
	// still report the old value — see setIfApplied.
	ag := &AvailabilityGroup{server: &Server{}, Name: "AAG1", AutomatedBackupPreference: "SECONDARY"}
	ctx, _ := WithScript(context.Background())
	if err := ag.SetAutomatedBackupPreferenceContext(ctx, "PRIMARY"); err != nil {
		t.Fatalf("SetAutomatedBackupPreferenceContext: %v", err)
	}
	if ag.AutomatedBackupPreference != "SECONDARY" {
		t.Errorf("scripted write mirrored onto the receiver: AutomatedBackupPreference = %q, want SECONDARY", ag.AutomatedBackupPreference)
	}
}

// -- Operations ------------------------------------------------------------

func TestAvailabilityGroupOperationStatements(t *testing.T) {
	tests := []struct {
		name string
		fn   func(ctx context.Context, ag *AvailabilityGroup) error
		want string
	}{
		{"add database", func(ctx context.Context, ag *AvailabilityGroup) error {
			return ag.AddDatabaseContext(ctx, "testdb_1")
		}, "ALTER AVAILABILITY GROUP [AAG1] ADD DATABASE [testdb_1]"},
		{"remove database", func(ctx context.Context, ag *AvailabilityGroup) error {
			return ag.RemoveDatabaseContext(ctx, "testdb_1")
		}, "ALTER AVAILABILITY GROUP [AAG1] REMOVE DATABASE [testdb_1]"},
		// The secondary-side four are ALTER DATABASE, not ALTER AVAILABILITY
		// GROUP — a mix-up compiles and then fails on the server.
		{"join database", func(ctx context.Context, ag *AvailabilityGroup) error {
			return ag.JoinDatabaseContext(ctx, "testdb_1")
		}, "ALTER DATABASE [testdb_1] SET HADR AVAILABILITY GROUP = [AAG1]"},
		{"unjoin database", func(ctx context.Context, ag *AvailabilityGroup) error {
			return ag.UnjoinDatabaseContext(ctx, "testdb_1")
		}, "ALTER DATABASE [testdb_1] SET HADR OFF"},
		{"suspend database", func(ctx context.Context, ag *AvailabilityGroup) error {
			return ag.SuspendDatabaseContext(ctx, "testdb_1")
		}, "ALTER DATABASE [testdb_1] SET HADR SUSPEND"},
		{"resume database", func(ctx context.Context, ag *AvailabilityGroup) error {
			return ag.ResumeDatabaseContext(ctx, "testdb_1")
		}, "ALTER DATABASE [testdb_1] SET HADR RESUME"},
		// A replica is named as a literal, a database as an identifier.
		{"remove replica", func(ctx context.Context, ag *AvailabilityGroup) error {
			return ag.RemoveReplicaContext(ctx, "ubusql2")
		}, "ALTER AVAILABILITY GROUP [AAG1] REMOVE REPLICA ON N'ubusql2'"},
		{"remove listener", func(ctx context.Context, ag *AvailabilityGroup) error {
			return ag.RemoveListenerContext(ctx, "ubuaag")
		}, "ALTER AVAILABILITY GROUP [AAG1] REMOVE LISTENER N'ubuaag'"},
		{"drop group", func(ctx context.Context, ag *AvailabilityGroup) error {
			return ag.DropContext(ctx)
		}, "DROP AVAILABILITY GROUP [AAG1]"},
		{"failover", func(ctx context.Context, ag *AvailabilityGroup) error {
			return ag.FailoverContext(ctx)
		}, "ALTER AVAILABILITY GROUP [AAG1] FAILOVER"},
		{"forced failover", func(ctx context.Context, ag *AvailabilityGroup) error {
			return ag.ForceFailoverAllowDataLossContext(ctx)
		}, "ALTER AVAILABILITY GROUP [AAG1] FORCE_FAILOVER_ALLOW_DATA_LOSS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := soleStatement(t, scriptAG(t, "AAG1", tt.fn)); got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}

// A database name goes through quoteIdent and a group name into the ALTER
// DATABASE tail, so an awkward name has to survive both halves of the join
// statement.
func TestJoinDatabaseQuotesBothNames(t *testing.T) {
	got := soleStatement(t, scriptAG(t, "Odd]Name", func(ctx context.Context, ag *AvailabilityGroup) error {
		return ag.JoinDatabaseContext(ctx, "db]1")
	}))
	want := "ALTER DATABASE [db]]1] SET HADR AVAILABILITY GROUP = [Odd]]Name]"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// ADD REPLICA and CREATE's REPLICA ON list take the same WITH body, so the two
// have to render it identically — a divergence here means a replica added to an
// existing group is configured differently from one named at create time.
func TestAddReplicaMatchesTheCreateReplicaClause(t *testing.T) {
	spec := AvailabilityReplicaSpec{
		ServerName: "ubusql2", EndpointURL: "tcp://ubusql2:5022",
		AvailabilityMode: "SYNCHRONOUS_COMMIT", FailoverMode: "EXTERNAL",
		SeedingMode: "AUTOMATIC", BackupPriority: 50, SessionTimeout: 10,
		PrimaryRoleAllowConnections: "ALL", SecondaryRoleAllowConnections: "NO",
	}
	got := soleStatement(t, scriptAG(t, "AAG1", func(ctx context.Context, ag *AvailabilityGroup) error {
		return ag.AddReplicaContext(ctx, spec)
	}))
	want := "ALTER AVAILABILITY GROUP [AAG1] ADD REPLICA ON N'ubusql2' WITH (" +
		"ENDPOINT_URL = N'tcp://ubusql2:5022', AVAILABILITY_MODE = SYNCHRONOUS_COMMIT, FAILOVER_MODE = EXTERNAL, " +
		"SEEDING_MODE = AUTOMATIC, BACKUP_PRIORITY = 50, SESSION_TIMEOUT = 10, " +
		"PRIMARY_ROLE (ALLOW_CONNECTIONS = ALL), SECONDARY_ROLE (ALLOW_CONNECTIONS = NO))"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}

	// The same spec inside a CREATE must produce the identical WITH body.
	req := baseCreateRequest()
	req.Replicas = []AvailabilityReplicaSpec{spec}
	stmt, err := req.createStatement()
	if err != nil {
		t.Fatalf("createStatement: %v", err)
	}
	_, createWith, ok := strings.Cut(stmt, "REPLICA ON ")
	if !ok {
		t.Fatalf("no REPLICA ON in %s", stmt)
	}
	_, addWith, _ := strings.Cut(got, "ADD REPLICA ON ")
	if createWith != addWith {
		t.Errorf("create %s\nadd    %s", createWith, addWith)
	}
}

// A spec the create path would reject has to be rejected here too, without
// emitting a statement — the validation lives in withClause, and reaching the
// server with a half-built ADD REPLICA is the failure this guards.
func TestAddReplicaRejectsABadSpec(t *testing.T) {
	tests := map[string]AvailabilityReplicaSpec{
		"no server name":   {EndpointURL: "tcp://ubusql2:5022"},
		"no endpoint":      {ServerName: "ubusql2"},
		"bad mode":         {ServerName: "ubusql2", EndpointURL: "tcp://x:5022", AvailabilityMode: "MAYBE"},
		"bad failover":     {ServerName: "ubusql2", EndpointURL: "tcp://x:5022", FailoverMode: "PACEMAKER"},
		"bad seeding":      {ServerName: "ubusql2", EndpointURL: "tcp://x:5022", SeedingMode: "SOMETIMES"},
		"priority too big": {ServerName: "ubusql2", EndpointURL: "tcp://x:5022", BackupPriority: 101},
	}
	for name, spec := range tests {
		t.Run(name, func(t *testing.T) {
			ag := &AvailabilityGroup{server: &Server{}, Name: "AAG1"}
			ctx, script := WithScript(context.Background())
			if err := ag.AddReplicaContext(ctx, spec); err == nil {
				t.Fatal("accepted")
			}
			if len(script.Statements) != 0 {
				t.Errorf("emitted %q", script.Statements)
			}
		})
	}
}

// AvailabilityReplica.Drop must issue exactly what the group-side form does.
func TestReplicaDropMatchesRemoveReplica(t *testing.T) {
	fromReplica := soleStatement(t, scriptReplica(t, "AAG1", "o'brien", func(ctx context.Context, r *AvailabilityReplica) error {
		return r.DropContext(ctx)
	}))
	fromGroup := soleStatement(t, scriptAG(t, "AAG1", func(ctx context.Context, ag *AvailabilityGroup) error {
		return ag.RemoveReplicaContext(ctx, "o'brien")
	}))
	if fromReplica != fromGroup {
		t.Errorf("replica form %s\ngroup form   %s", fromReplica, fromGroup)
	}
	if want := "ALTER AVAILABILITY GROUP [AAG1] REMOVE REPLICA ON N'o''brien'"; fromGroup != want {
		t.Errorf("got  %s\nwant %s", fromGroup, want)
	}
}

// A replica that never came from AvailabilityGroup.Replicas has no group name
// to address, and must say so rather than build "ALTER AVAILABILITY GROUP []".
func TestReplicaDropWithoutGroupFails(t *testing.T) {
	r := &AvailabilityReplica{server: &Server{}, ReplicaServerName: "ubusql2"}
	ctx, script := WithScript(context.Background())
	if err := r.DropContext(ctx); err == nil {
		t.Fatal("dropping a detached replica succeeded")
	}
	if len(script.Statements) != 0 {
		t.Errorf("emitted %q", script.Statements)
	}
}

func TestAddListenerClause(t *testing.T) {
	tests := []struct {
		name string
		spec AvailabilityListenerSpec
		want string
	}{
		{"static ipv4", AvailabilityListenerSpec{
			DNSName:     "ubuaag",
			Port:        1433,
			IPAddresses: []AvailabilityListenerIPSpec{{IPAddress: "192.168.178.99", SubnetMask: "255.255.255.0"}},
		}, "ADD LISTENER N'ubuaag' (WITH IP ((N'192.168.178.99', N'255.255.255.0')), PORT = 1433)"},
		// An empty mask is IPv6, which takes no mask at all rather than an
		// empty one.
		{"ipv6 takes no mask", AvailabilityListenerSpec{
			DNSName:     "ubuaag",
			IPAddresses: []AvailabilityListenerIPSpec{{IPAddress: "2001:db8::1"}},
		}, "ADD LISTENER N'ubuaag' (WITH IP ((N'2001:db8::1')))"},
		{"multi-subnet", AvailabilityListenerSpec{
			DNSName: "ubuaag",
			Port:    5022,
			IPAddresses: []AvailabilityListenerIPSpec{
				{IPAddress: "10.0.0.9", SubnetMask: "255.255.255.0"},
				{IPAddress: "10.1.0.9", SubnetMask: "255.255.255.0"},
			},
		}, "ADD LISTENER N'ubuaag' (WITH IP ((N'10.0.0.9', N'255.255.255.0'), (N'10.1.0.9', N'255.255.255.0')), PORT = 5022)"},
		{"dhcp", AvailabilityListenerSpec{DNSName: "ubuaag", DHCP: true},
			"ADD LISTENER N'ubuaag' (WITH DHCP)"},
		{"dhcp on a subnet, with a port", AvailabilityListenerSpec{
			DNSName: "ubuaag", DHCP: true, Port: 1433,
			DHCPSubnet: "192.168.178.0", DHCPSubnetMask: "255.255.255.0",
		}, "ADD LISTENER N'ubuaag' (WITH DHCP ON (N'192.168.178.0', N'255.255.255.0'), PORT = 1433)"},
		// Port 0 means "unspecified", not port zero.
		{"no port", AvailabilityListenerSpec{
			DNSName:     "ubuaag",
			IPAddresses: []AvailabilityListenerIPSpec{{IPAddress: "10.0.0.9", SubnetMask: "255.255.255.0"}},
		}, "ADD LISTENER N'ubuaag' (WITH IP ((N'10.0.0.9', N'255.255.255.0')))"},
		{"quoted name", AvailabilityListenerSpec{DNSName: "o'brien", DHCP: true},
			"ADD LISTENER N'o''brien' (WITH DHCP)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.spec.addListenerClause()
			if err != nil {
				t.Fatalf("addListenerClause: %v", err)
			}
			if got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestAddListenerClauseRejects(t *testing.T) {
	tests := []struct {
		name string
		spec AvailabilityListenerSpec
	}{
		{"no name", AvailabilityListenerSpec{DHCP: true}},
		{"no addressing mode", AvailabilityListenerSpec{DNSName: "ubuaag"}},
		{"both modes", AvailabilityListenerSpec{
			DNSName: "ubuaag", DHCP: true,
			IPAddresses: []AvailabilityListenerIPSpec{{IPAddress: "10.0.0.9", SubnetMask: "255.255.255.0"}},
		}},
		{"empty address", AvailabilityListenerSpec{
			DNSName:     "ubuaag",
			IPAddresses: []AvailabilityListenerIPSpec{{SubnetMask: "255.255.255.0"}},
		}},
		{"half a dhcp subnet", AvailabilityListenerSpec{
			DNSName: "ubuaag", DHCP: true, DHCPSubnet: "192.168.178.0",
		}},
		{"port out of range", AvailabilityListenerSpec{
			DNSName: "ubuaag", DHCP: true, Port: 70000,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := tt.spec.addListenerClause(); err == nil {
				t.Errorf("accepted, produced %s", got)
			}
		})
	}
}

// Every operation naming an object must refuse an empty name rather than
// building a statement with an empty identifier in it.
func TestOperationsRejectEmptyNames(t *testing.T) {
	ops := map[string]func(ctx context.Context, ag *AvailabilityGroup) error{
		"add database":    func(ctx context.Context, ag *AvailabilityGroup) error { return ag.AddDatabaseContext(ctx, "") },
		"remove database": func(ctx context.Context, ag *AvailabilityGroup) error { return ag.RemoveDatabaseContext(ctx, "") },
		"join database":   func(ctx context.Context, ag *AvailabilityGroup) error { return ag.JoinDatabaseContext(ctx, " ") },
		"unjoin database": func(ctx context.Context, ag *AvailabilityGroup) error { return ag.UnjoinDatabaseContext(ctx, "") },
		"suspend":         func(ctx context.Context, ag *AvailabilityGroup) error { return ag.SuspendDatabaseContext(ctx, "") },
		"resume":          func(ctx context.Context, ag *AvailabilityGroup) error { return ag.ResumeDatabaseContext(ctx, "") },
		"remove replica":  func(ctx context.Context, ag *AvailabilityGroup) error { return ag.RemoveReplicaContext(ctx, "") },
		"remove listener": func(ctx context.Context, ag *AvailabilityGroup) error { return ag.RemoveListenerContext(ctx, "") },
	}
	for name, fn := range ops {
		t.Run(name, func(t *testing.T) {
			ag := &AvailabilityGroup{server: &Server{}, Name: "AAG1"}
			ctx, script := WithScript(context.Background())
			if err := fn(ctx, ag); err == nil {
				t.Fatal("accepted an empty name")
			}
			if len(script.Statements) != 0 {
				t.Errorf("emitted %q", script.Statements)
			}
		})
	}
}

// -- Creating a group ------------------------------------------------------

// baseCreateRequest is a minimal two-replica request; each test below changes
// one thing about it, so the diff in the expected statement is the assertion.
func baseCreateRequest() CreateAvailabilityGroupRequest {
	return CreateAvailabilityGroupRequest{
		Name:                                    "AAG1",
		RequiredSynchronizedSecondariesToCommit: -1,
		Replicas: []AvailabilityReplicaSpec{
			{ServerName: "ubusql1", EndpointURL: "tcp://ubusql1:5022", BackupPriority: -1},
			{ServerName: "ubusql2", EndpointURL: "tcp://ubusql2:5022", BackupPriority: -1},
		},
	}
}

func TestCreateAvailabilityGroupStatement(t *testing.T) {
	const bothReplicas = " FOR REPLICA ON N'ubusql1' WITH (ENDPOINT_URL = N'tcp://ubusql1:5022', AVAILABILITY_MODE = SYNCHRONOUS_COMMIT, FAILOVER_MODE = MANUAL)," +
		" N'ubusql2' WITH (ENDPOINT_URL = N'tcp://ubusql2:5022', AVAILABILITY_MODE = SYNCHRONOUS_COMMIT, FAILOVER_MODE = MANUAL)"

	tests := []struct {
		name string
		edit func(*CreateAvailabilityGroupRequest)
		want string
	}{
		{"minimal", func(*CreateAvailabilityGroupRequest) {},
			"CREATE AVAILABILITY GROUP [AAG1]" + bothReplicas},
		{"cluster type", func(r *CreateAvailabilityGroupRequest) { r.ClusterType = "external" },
			"CREATE AVAILABILITY GROUP [AAG1] WITH (CLUSTER_TYPE = EXTERNAL)" + bothReplicas},
		{"databases", func(r *CreateAvailabilityGroupRequest) { r.Databases = []string{"testdb_1", "odd]db"} },
			"CREATE AVAILABILITY GROUP [AAG1] FOR DATABASE [testdb_1], [odd]]db]" + strings.TrimPrefix(bothReplicas, " FOR")},
		// Zero is a legitimate value here, so the "omit" sentinel has to be
		// negative — writing 0 when the caller meant "leave it alone" would
		// turn off a guarantee the cluster may depend on.
		{"required synchronized secondaries zero", func(r *CreateAvailabilityGroupRequest) {
			r.RequiredSynchronizedSecondariesToCommit = 0
		},
			"CREATE AVAILABILITY GROUP [AAG1] WITH (REQUIRED_SYNCHRONIZED_SECONDARIES_TO_COMMIT = 0)" + bothReplicas},
		{"every group option", func(r *CreateAvailabilityGroupRequest) {
			r.ClusterType = "NONE"
			r.AutomatedBackupPreference = "SECONDARY"
			r.FailureConditionLevel = 3
			r.HealthCheckTimeout = 30000
			r.DBFailover = true
			r.DTCSupport = true
			r.Contained = true
		},
			"CREATE AVAILABILITY GROUP [AAG1] WITH (CLUSTER_TYPE = NONE, AUTOMATED_BACKUP_PREFERENCE = SECONDARY, " +
				"FAILURE_CONDITION_LEVEL = 3, HEALTH_CHECK_TIMEOUT = 30000, DB_FAILOVER = ON, DTC_SUPPORT = PER_DB, CONTAINED)" + bothReplicas},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := baseCreateRequest()
			tt.edit(&req)
			got, err := req.createStatement()
			if err != nil {
				t.Fatalf("createStatement: %v", err)
			}
			if got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}

// The replica clause is the nested half, and SECONDARY_ROLE holding two
// sub-options is the shape most likely to come out malformed.
func TestCreateAvailabilityGroupReplicaClause(t *testing.T) {
	req := baseCreateRequest()
	req.Replicas = []AvailabilityReplicaSpec{{
		ServerName: "ubusql2", EndpointURL: "tcp://ubusql2:5022",
		AvailabilityMode: "asynchronous_commit", FailoverMode: "external",
		SeedingMode: "automatic", BackupPriority: 0, SessionTimeout: 20,
		PrimaryRoleAllowConnections: "READ_WRITE", SecondaryRoleAllowConnections: "ALL",
		ReadOnlyRoutingURL: "TCP://ubusql2:1433",
	}}
	got, err := req.createStatement()
	if err != nil {
		t.Fatalf("createStatement: %v", err)
	}
	want := "CREATE AVAILABILITY GROUP [AAG1] FOR REPLICA ON N'ubusql2' WITH (" +
		"ENDPOINT_URL = N'tcp://ubusql2:5022', AVAILABILITY_MODE = ASYNCHRONOUS_COMMIT, FAILOVER_MODE = EXTERNAL, " +
		"SEEDING_MODE = AUTOMATIC, BACKUP_PRIORITY = 0, SESSION_TIMEOUT = 20, " +
		"PRIMARY_ROLE (ALLOW_CONNECTIONS = READ_WRITE), " +
		"SECONDARY_ROLE (ALLOW_CONNECTIONS = ALL, READ_ONLY_ROUTING_URL = N'TCP://ubusql2:1433'))"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestCreateAvailabilityGroupStatementRejects(t *testing.T) {
	tests := map[string]func(*CreateAvailabilityGroupRequest){
		"no name":              func(r *CreateAvailabilityGroupRequest) { r.Name = " " },
		"no replicas":          func(r *CreateAvailabilityGroupRequest) { r.Replicas = nil },
		"unknown cluster type": func(r *CreateAvailabilityGroupRequest) { r.ClusterType = "PACEMAKER" },
		"bad failure level":    func(r *CreateAvailabilityGroupRequest) { r.FailureConditionLevel = 9 },
		"bad backup pref":      func(r *CreateAvailabilityGroupRequest) { r.AutomatedBackupPreference = "MAYBE" },
		"empty database name":  func(r *CreateAvailabilityGroupRequest) { r.Databases = []string{"ok", ""} },
		"replica with no name": func(r *CreateAvailabilityGroupRequest) { r.Replicas[0].ServerName = "" },
		"replica with no endpoint": func(r *CreateAvailabilityGroupRequest) {
			r.Replicas[0].EndpointURL = ""
		},
		"bad availability mode": func(r *CreateAvailabilityGroupRequest) {
			r.Replicas[0].AvailabilityMode = "EVENTUAL"
		},
		"bad seeding mode":    func(r *CreateAvailabilityGroupRequest) { r.Replicas[0].SeedingMode = "SOMETIMES" },
		"backup priority 101": func(r *CreateAvailabilityGroupRequest) { r.Replicas[0].BackupPriority = 101 },
	}
	for name, edit := range tests {
		t.Run(name, func(t *testing.T) {
			req := baseCreateRequest()
			edit(&req)
			if got, err := req.createStatement(); err == nil {
				t.Errorf("accepted, produced %s", got)
			}
		})
	}
}

// JOIN has to repeat the cluster type under EXTERNAL and NONE and must not
// carry one under WSFC — the group's own ClusterType is what decides, so a
// handle read back from the server joins correctly without the caller knowing.
func TestJoinRepeatsTheClusterType(t *testing.T) {
	tests := []struct {
		clusterType string
		want        string
	}{
		{"EXTERNAL", "ALTER AVAILABILITY GROUP [AAG1] JOIN WITH (CLUSTER_TYPE = EXTERNAL)"},
		{"NONE", "ALTER AVAILABILITY GROUP [AAG1] JOIN WITH (CLUSTER_TYPE = NONE)"},
		{"external", "ALTER AVAILABILITY GROUP [AAG1] JOIN WITH (CLUSTER_TYPE = EXTERNAL)"},
		{"WSFC", "ALTER AVAILABILITY GROUP [AAG1] JOIN"},
		{"", "ALTER AVAILABILITY GROUP [AAG1] JOIN"},
	}
	for _, tt := range tests {
		t.Run(orEmpty(tt.clusterType), func(t *testing.T) {
			ag := (&Server{}).AvailabilityGroup("AAG1")
			ctx, script := WithScript(context.Background())
			if err := ag.JoinContext(ctx, tt.clusterType); err != nil {
				t.Fatalf("under WithScript: %v", err)
			}
			if got := soleStatement(t, script.Statements); got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestGrantCreateAnyDatabaseStatements(t *testing.T) {
	tests := []struct {
		name string
		fn   func(ctx context.Context, ag *AvailabilityGroup) error
		want string
	}{
		{"grant", func(ctx context.Context, ag *AvailabilityGroup) error {
			return ag.GrantCreateAnyDatabaseContext(ctx)
		}, "ALTER AVAILABILITY GROUP [AAG1] GRANT CREATE ANY DATABASE"},
		{"deny", func(ctx context.Context, ag *AvailabilityGroup) error {
			return ag.DenyCreateAnyDatabaseContext(ctx)
		}, "ALTER AVAILABILITY GROUP [AAG1] DENY CREATE ANY DATABASE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := soleStatement(t, scriptAG(t, "AAG1", tt.fn)); got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}

func orEmpty(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}

func TestModifyListenerStatements(t *testing.T) {
	// Each MODIFY LISTENER carries exactly one option: the grammar has no form
	// that changes the port and adds an address in the same statement.
	tests := []struct {
		name    string
		dnsName string
		option  string
		want    string
	}{
		{"port", "ubuaag", "PORT = 14330", "MODIFY LISTENER N'ubuaag' (PORT = 14330)"},
		{"add ipv4", "ubuaag", "ADD IP (N'10.1.0.9', N'255.255.255.0')",
			"MODIFY LISTENER N'ubuaag' (ADD IP (N'10.1.0.9', N'255.255.255.0'))"},
		{"quoted name", "o'brien", "PORT = 1433", "MODIFY LISTENER N'o''brien' (PORT = 1433)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := modifyListenerClause(tt.dnsName, tt.option)
			if err != nil {
				t.Fatalf("modifyListenerClause() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("modifyListenerClause()\n got %s\nwant %s", got, tt.want)
			}
		})
	}
	if _, err := modifyListenerClause("  ", "PORT = 1433"); err == nil {
		t.Error("modifyListenerClause with a blank DNS name = nil, want an error")
	}
}

func TestListenerIPLiteralMatchesTheAddAndModifyForms(t *testing.T) {
	// The same address has to render identically whether it is going into ADD
	// LISTENER's list or MODIFY LISTENER's ADD IP; a divergence would be a
	// listener that cannot be extended with the address it was created with.
	v4, err := listenerIPLiteral(AvailabilityListenerIPSpec{IPAddress: "10.0.0.9", SubnetMask: "255.255.255.0"})
	if err != nil {
		t.Fatalf("listenerIPLiteral() error = %v", err)
	}
	if want := "(N'10.0.0.9', N'255.255.255.0')"; v4 != want {
		t.Errorf("IPv4 = %s, want %s", v4, want)
	}
	v6, err := listenerIPLiteral(AvailabilityListenerIPSpec{IPAddress: "2001:db8::1"})
	if err != nil {
		t.Fatalf("listenerIPLiteral() error = %v", err)
	}
	if want := "(N'2001:db8::1')"; v6 != want {
		t.Errorf("IPv6 = %s, want %s — IPv6 takes no mask", v6, want)
	}
	if _, err := listenerIPLiteral(AvailabilityListenerIPSpec{SubnetMask: "255.255.255.0"}); err == nil {
		t.Error("listenerIPLiteral with no address = nil, want an error")
	}

	// And the ADD LISTENER clause must be built from it, not from a second
	// copy of the same formatting.
	spec := AvailabilityListenerSpec{
		DNSName:     "ubuaag",
		IPAddresses: []AvailabilityListenerIPSpec{{IPAddress: "10.0.0.9", SubnetMask: "255.255.255.0"}},
	}
	clause, err := spec.addListenerClause()
	if err != nil {
		t.Fatalf("addListenerClause() error = %v", err)
	}
	if !strings.Contains(clause, v4) {
		t.Errorf("ADD LISTENER clause %s does not contain the shared literal %s", clause, v4)
	}
}

func TestSetListenerPortRejectsAnOutOfRangePort(t *testing.T) {
	// The range check has to happen before the statement is built: a port of 0
	// would otherwise produce "PORT = 0", which the server accepts as a request
	// for a dynamic port rather than rejecting.
	ag := &AvailabilityGroup{Name: "AAG1"}
	for _, port := range []int{0, -1, 65536} {
		if err := ag.SetListenerPort("ubuaag", port); err == nil {
			t.Errorf("SetListenerPort(%d) = nil, want an out-of-range error", port)
		}
	}
}

// TestAddListenerIPScriptsTheWholeStatement pins the statement AddListenerIP
// assembles, not just the two halves TestModifyListenerStatements and
// TestListenerIPLiteralMatchesTheAddAndModifyForms already pin. This is the
// only place the ALTER prefix, the group's bracket-quoted name and the
// listener's literal-quoted one appear together — three quoting rules in one
// statement, and the method has no live coverage (adding an address to the
// test cluster's listener would change a group in use).
func TestAddListenerIPScriptsTheWholeStatement(t *testing.T) {
	ag := &AvailabilityGroup{server: &Server{}, Name: "AAG]1"}
	ctx, script := WithScript(context.Background())

	err := ag.AddListenerIPContext(ctx, "o'brien",
		AvailabilityListenerIPSpec{IPAddress: "10.1.0.9", SubnetMask: "255.255.255.0"})
	if err != nil {
		t.Fatalf("AddListenerIPContext under WithScript: %v", err)
	}

	want := "ALTER AVAILABILITY GROUP [AAG]]1] MODIFY LISTENER N'o''brien' (ADD IP (N'10.1.0.9', N'255.255.255.0'))"
	if len(script.Statements) != 1 || script.Statements[0] != want {
		t.Errorf("Statements = %q, want [%q]", script.Statements, want)
	}
}

// TestAddListenerIPRejectsAnEmptyAddress pins that a spec with no address
// fails before any statement is built — "ADD IP ()" is a syntax error, and a
// listener half-modified by a rejected statement is worse than one not
// touched.
func TestAddListenerIPRejectsAnEmptyAddress(t *testing.T) {
	ag := &AvailabilityGroup{server: &Server{}, Name: "AAG1"}
	ctx, script := WithScript(context.Background())

	if err := ag.AddListenerIPContext(ctx, "ubuaag", AvailabilityListenerIPSpec{}); err == nil {
		t.Error("AddListenerIPContext with an empty spec = nil, want an error")
	}
	if len(script.Statements) != 0 {
		t.Errorf("Statements = %q, want none", script.Statements)
	}
}
