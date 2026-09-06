package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
)

// -- the pure parts ----------------------------------------------------------

func TestCapabilityQueryBindsEveryNameAsAParameter(t *testing.T) {
	q, args := capabilityQuery(
		"SELECT 'R', n.v, IS_SRVROLEMEMBER(n.v)", []string{"sysadmin", "dbcreator"},
		"SELECT 'P', n.v, HAS_PERMS_BY_NAME(NULL, NULL, n.v)", []string{"CONTROL SERVER"},
	)

	want := "SELECT 'R', n.v, IS_SRVROLEMEMBER(n.v) FROM (VALUES (@p1),(@p2)) AS n(v)\n" +
		"UNION ALL\n" +
		"SELECT 'P', n.v, HAS_PERMS_BY_NAME(NULL, NULL, n.v) FROM (VALUES (@p3)) AS n(v)"
	if q != want {
		t.Errorf("query =\n%s\nwant\n%s", q, want)
	}
	if got := []any{"sysadmin", "dbcreator", "CONTROL SERVER"}; !slices.Equal(args, got) {
		t.Errorf("args = %v, want %v", args, got)
	}
	// No name may reach the server as text: a permission name is ours, but
	// the placeholder numbering is what keeps args and query aligned, and an
	// interpolated name would hide a misnumbering.
	for _, n := range []string{"sysadmin", "dbcreator", "CONTROL SERVER"} {
		if strings.Contains(q, n) {
			t.Errorf("query interpolates %q instead of binding it:\n%s", n, q)
		}
	}
}

// TestProbedNameListsAreWellFormed catches the failure mode these lists have:
// a duplicate or a stray-cased name produces no error anywhere, just a
// permanently CapabilityUnknown answer that Allows then reads as "fine".
func TestProbedNameListsAreWellFormed(t *testing.T) {
	for _, list := range []struct {
		name  string
		names []string
		upper bool
	}{
		{"ProbedServerPermissions", ProbedServerPermissions, true},
		{"ProbedDatabasePermissions", ProbedDatabasePermissions, true},
		{"ProbedServerRoles", ProbedServerRoles, false},
		{"ProbedDatabaseRoles", ProbedDatabaseRoles, false},
		{"ProbedSchemaPermissions", ProbedSchemaPermissions, true},
	} {
		seen := map[string]bool{}
		for _, n := range list.names {
			if seen[n] {
				t.Errorf("%s: %q listed twice", list.name, n)
			}
			seen[n] = true
			if n != strings.TrimSpace(n) {
				t.Errorf("%s: %q has surrounding space", list.name, n)
			}
			if list.upper && n != strings.ToUpper(n) {
				t.Errorf("%s: %q is not upper-case", list.name, n)
			}
		}
	}
}

// TestHasAndAllowsDisagreeOnlyOnUnknown pins the asymmetry the two exist for.
// Folding CapabilityUnknown into either extreme is the mistake: into Granted
// and a failed probe offers everything, into Denied and it hides everything.
func TestHasAndAllowsDisagreeOnlyOnUnknown(t *testing.T) {
	c := &Capabilities{ServerPermissions: map[string]CapabilityState{
		"CONTROL SERVER":   CapabilityGranted,
		"ALTER ANY LOGIN":  CapabilityDenied,
		"VIEW ANY DEFINIT": CapabilityUnknown,
	}}
	for _, tc := range []struct {
		name              string
		wantHas, wantAllw bool
	}{
		{"CONTROL SERVER", true, true},
		{"ALTER ANY LOGIN", false, false},
		{"VIEW ANY DEFINIT", false, true},
		{"NEVER PROBED AT ALL", false, true},
	} {
		if got := c.Has(tc.name); got != tc.wantHas {
			t.Errorf("Has(%q) = %v, want %v", tc.name, got, tc.wantHas)
		}
		if got := c.Allows(tc.name); got != tc.wantAllw {
			t.Errorf("Allows(%q) = %v, want %v", tc.name, got, tc.wantAllw)
		}
	}
}

// TestNilCapabilitiesFailOpen is the same rule for the case that matters most:
// the probe never ran. Every question must be answerable without a nil check
// at the call site, and Allows must say yes.
func TestNilCapabilitiesFailOpen(t *testing.T) {
	var c *Capabilities
	if c.Has("CONTROL SERVER") {
		t.Error("nil Has = true, want false")
	}
	if !c.Allows("CONTROL SERVER") {
		t.Error("nil Allows = false: a failed probe would lock the user out")
	}
	if c.IsSysadmin() || c.InServerRole("sysadmin") {
		t.Error("nil role membership = true, want false")
	}
	if got := c.Permission("CONTROL SERVER"); got != CapabilityUnknown {
		t.Errorf("nil Permission = %v, want %v", got, CapabilityUnknown)
	}

	var d *DatabaseCapabilities
	if d.Has("ALTER") || d.InRole("db_owner") {
		t.Error("nil DatabaseCapabilities reports a right it cannot know")
	}
	if !d.Allows("ALTER") {
		t.Error("nil DatabaseCapabilities Allows = false: same lockout")
	}
}

// -- the probe, over a scripted driver ---------------------------------------

// capConn answers the two capability probes plus the connect-time load. Each
// answer is scripted by the test through the package-level capScript, since a
// database/sql driver is reached only by name.
type capScript struct {
	dbAccess   any // 1, 0 or nil — HAS_DBACCESS
	serverRows [][]driver.Value
	dbRows     [][]driver.Value
	uses       []string

	// dbQuery and dbArgs are the database probe as it reached the server —
	// the only place the schema block's own text is observable, since the
	// answers below are scripted whatever it asks.
	dbQuery string
	dbArgs  []driver.NamedValue

	// srvQuery and srvArgs are the same for the *server* probe, which grew a
	// catalog block of its own — see explicitServerCapabilityQuery.
	srvQuery string
	srvArgs  []driver.NamedValue
}

var capCurrent *capScript

type capDriver struct{}

func (capDriver) Open(string) (driver.Conn, error) { return &capConn{}, nil }

type capConn struct{}

func (c *capConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *capConn) Close() error                        { return nil }
func (c *capConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (c *capConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	if strings.HasPrefix(strings.TrimSpace(q), "USE ") {
		capCurrent.uses = append(capCurrent.uses, strings.TrimSpace(q))
	}
	return driver.ResultNoRows, nil
}

func (c *capConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(q, "HAS_DBACCESS"):
		return &capRows{cols: 1, rows: [][]driver.Value{{capCurrent.dbAccess}}}, nil
	case strings.Contains(q, "IS_SRVROLEMEMBER"):
		capCurrent.srvQuery, capCurrent.srvArgs = q, args
		return &capRows{cols: 3, rows: capCurrent.serverRows}, nil
	case strings.Contains(q, "IS_ROLEMEMBER"):
		capCurrent.dbQuery, capCurrent.dbArgs = q, args
		return &capRows{cols: 3, rows: capCurrent.dbRows}, nil
	}
	return fakeInfoAnswer(q, nil)
}

type capRows struct {
	cols int
	rows [][]driver.Value
	i    int
}

func (r *capRows) Columns() []string { return make([]string, r.cols) }
func (r *capRows) Close() error      { return nil }
func (r *capRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

func init() { sql.Register("capdb", capDriver{}) }

func capServer(t *testing.T, s *capScript) *Server {
	t.Helper()
	capCurrent = s
	pool, err := sql.Open("capdb", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	srv, err := NewServer(context.Background(), pool)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

// TestServerCapabilitiesReadsAnswersByName is the point of the (kind, name,
// answer) shape: the rows come back deliberately out of the order the name
// lists are in, and every answer must still land on its own name.
func TestServerCapabilitiesReadsAnswersByName(t *testing.T) {
	srv := capServer(t, &capScript{serverRows: [][]driver.Value{
		{"P", "VIEW SERVER STATE", int64(1)},
		{"R", "sysadmin", int64(0)},
		{"P", "CONTROL SERVER", int64(0)},
		{"R", "public", int64(1)},
		// NULL: a permission this instance does not define.
		{"P", "ALTER ANY AVAILABILITY GROUP", nil},
	}})

	c, err := srv.CapabilitiesContext(context.Background())
	if err != nil {
		t.Fatalf("CapabilitiesContext: %v", err)
	}
	if !c.Has("VIEW SERVER STATE") {
		t.Error("VIEW SERVER STATE not granted")
	}
	if c.Permission("CONTROL SERVER") != CapabilityDenied {
		t.Errorf("CONTROL SERVER = %v, want denied", c.Permission("CONTROL SERVER"))
	}
	if c.IsSysadmin() {
		t.Error("IsSysadmin = true, want false")
	}
	if !c.InServerRole("public") {
		t.Error("public role missing")
	}

	// The NULL is the case a two-state answer gets wrong: not denied, and so
	// not something to hide a feature over.
	ag := "ALTER ANY AVAILABILITY GROUP"
	if got := c.Permission(ag); got != CapabilityUnknown {
		t.Errorf("%s = %v, want unknown — NULL is not a denial", ag, got)
	}
	if c.Has(ag) {
		t.Errorf("Has(%s) = true for a NULL answer", ag)
	}
	if !c.Allows(ag) {
		t.Errorf("Allows(%s) = false for a NULL answer", ag)
	}
}

// TestDatabaseCapabilitiesStopAtAnInaccessibleDatabase pins the ordering in
// CapabilitiesContext. The role probe runs inside the database and so opens
// with a USE — the very statement that fails for a login that cannot connect
// there — so accessibility has to be settled at the server scope first, and an
// inaccessible database is not an error.
func TestDatabaseCapabilitiesStopAtAnInaccessibleDatabase(t *testing.T) {
	script := &capScript{dbAccess: int64(0)}
	srv := capServer(t, script)

	c, err := srv.Database("locked").CapabilitiesContext(context.Background())
	if err != nil {
		t.Fatalf("CapabilitiesContext: %v", err)
	}
	if c.Accessible {
		t.Error("Accessible = true for HAS_DBACCESS 0")
	}
	if c.InRole("db_owner") || c.Has("SELECT") {
		t.Error("an inaccessible database reported rights inside itself")
	}
	if len(script.uses) != 0 {
		t.Errorf("USE issued for an inaccessible database: %v", script.uses)
	}
}

// TestPermitsFoldsAccessibilityIntoTheWithholdingTest.
//
// Allows answers only the question it is asked, and an inaccessible database
// was never asked anything: every permission in it is CapabilityUnknown, which
// fails open. A caller following Capabilities.Allows's "gate withholding on
// Allows" would therefore offer Back Up and Delete on exactly the databases the
// login cannot so much as connect to. Permits is the test that does not.
func TestPermitsFoldsAccessibilityIntoTheWithholdingTest(t *testing.T) {
	locked, err := capServer(t, &capScript{dbAccess: int64(0)}).
		Database("locked").CapabilitiesContext(context.Background())
	if err != nil {
		t.Fatalf("CapabilitiesContext: %v", err)
	}

	// The trap, stated as an assertion so it cannot be "fixed" by narrowing
	// Allows: Allows says yes here, and that is correct for what it answers.
	if !locked.Allows("BACKUP DATABASE") {
		t.Fatal("Allows = false for an unknown permission; the premise of this test is gone")
	}
	if locked.Permits("BACKUP DATABASE") {
		t.Error("Permits = true for a database the login cannot open")
	}

	// An accessible database is unchanged: Permits is Allows there.
	open, err := capServer(t, &capScript{
		dbAccess: int64(1),
		dbRows: [][]driver.Value{
			{"P", "SELECT", int64(1)},
			{"P", "ALTER", int64(0)},
		},
	}).Database("HealthClinic").CapabilitiesContext(context.Background())
	if err != nil {
		t.Fatalf("CapabilitiesContext: %v", err)
	}
	for _, name := range []string{"SELECT", "ALTER", "BACKUP DATABASE"} {
		if got, want := open.Permits(name), open.Allows(name); got != want {
			t.Errorf("Permits(%q) = %v, Allows(%q) = %v: they must agree on a database that opens",
				name, got, name, want)
		}
	}

	// nil is "nothing known" and fails open, the same as every other accessor
	// on this type — a probe that could not run must not lock the user out.
	var none *DatabaseCapabilities
	if !none.Permits("BACKUP DATABASE") {
		t.Error("nil Permits = false: a failed probe would withhold everything")
	}
}

// A NULL HAS_DBACCESS — the database does not exist, or is not visible to this
// login — must read as inaccessible rather than panic or pass.
func TestDatabaseCapabilitiesTreatNullAccessAsInaccessible(t *testing.T) {
	srv := capServer(t, &capScript{dbAccess: nil})

	c, err := srv.Database("ghost").CapabilitiesContext(context.Background())
	if err != nil {
		t.Fatalf("CapabilitiesContext: %v", err)
	}
	if c.Accessible {
		t.Error("Accessible = true for a NULL HAS_DBACCESS")
	}
}

func TestDatabaseCapabilitiesReadRolesAndPermissions(t *testing.T) {
	script := &capScript{
		dbAccess: int64(1),
		dbRows: [][]driver.Value{
			{"R", "db_datareader", int64(1)},
			{"R", "db_owner", int64(0)},
			// NULL: SQLAgent* exist only in msdb.
			{"R", "SQLAgentUserRole", nil},
			{"P", "SELECT", int64(1)},
			{"P", "ALTER", int64(0)},
		},
	}
	srv := capServer(t, script)

	c, err := srv.Database("HealthClinic").CapabilitiesContext(context.Background())
	if err != nil {
		t.Fatalf("CapabilitiesContext: %v", err)
	}
	if !c.Accessible {
		t.Fatal("Accessible = false")
	}
	if !c.InRole("db_datareader") || c.InRole("db_owner") {
		t.Error("database role membership read wrong")
	}
	if c.InRole("SQLAgentUserRole") {
		t.Error("a NULL IS_ROLEMEMBER read as membership")
	}
	if !c.Has("SELECT") || c.Allows("ALTER") {
		t.Error("database permission states read wrong")
	}
	// The probe has to run in the database it is about, or every database
	// answers with whatever the connection was last pinned to.
	if !slices.Contains(script.uses, "USE [HealthClinic]") {
		t.Errorf("USE statements = %v, want one for HealthClinic", script.uses)
	}
}

// TestProbedSeparatesNotAMemberFromNeverAsked. Every permission accessor folds
// "never asked" into unknown and fails open on its own, but InServerRole
// cannot: it answers false either way. A caller gating on "not a sysadmin"
// without this loses the gated thing on every connection that could not be
// probed.
func TestProbedSeparatesNotAMemberFromNeverAsked(t *testing.T) {
	var never *Capabilities
	if never.Probed() {
		t.Error("a nil capability set reported itself probed")
	}
	if (&Capabilities{}).Probed() {
		t.Error("the zero value reported itself probed")
	}

	answered := &Capabilities{ServerRoles: map[string]bool{"sysadmin": false}}
	if !answered.Probed() {
		t.Error("a set the server answered did not report itself probed")
	}
	if answered.IsSysadmin() {
		t.Error("IsSysadmin = true for a login the server said is not one")
	}
}

// TestEnumFileSystemIsLegacyMatchesTheGateItReports. The two must agree, or a
// caller reasons about xp_dirtree's quirks while the DMV path is what runs.
func TestEnumFileSystemIsLegacyMatchesTheGateItReports(t *testing.T) {
	for _, tt := range []struct {
		major int
		want  bool
	}{{0, true}, {13, true}, {14, false}, {17, false}} {
		s := &Server{info: &ServerInfo{VersionMajor: tt.major}}
		if got := s.EnumFileSystemIsLegacy(); got != tt.want {
			t.Errorf("major %d: EnumFileSystemIsLegacy = %v, want %v", tt.major, got, tt.want)
		}
	}
	if !(&Server{}).EnumFileSystemIsLegacy() {
		t.Error("an instance of unknown version did not report the legacy path")
	}
}

// TestDatabaseCapabilitiesReadSchemaPermissions. A principal granted ALTER on
// one schema holds no database-wide permission at all, so the database-scope
// map cannot answer for it: without this block a caller gating a rename or a
// drop withholds it from exactly the principal SQL Server would let through.
//
// The schema travels in the *name* column and the permission in the kind, so
// the case that matters here is a schema named like a database-scope
// permission — "ALTER" below — which the other way round would be read back as
// a database permission answer and overwrite a real one.
func TestDatabaseCapabilitiesReadSchemaPermissions(t *testing.T) {
	srv := capServer(t, &capScript{
		dbAccess: int64(1),
		dbRows: [][]driver.Value{
			{"P", "ALTER", int64(0)},
			{"S:ALTER", "Sales", int64(1)},
			{"S:ALTER", "dbo", int64(0)},
			// NULL: the permission does not apply here, which is not a denial.
			{"S:ALTER", "guest", nil},
			{"S:ALTER", "ALTER", int64(1)},
		},
	})

	c, err := srv.Database("HealthClinic").CapabilitiesContext(context.Background())
	if err != nil {
		t.Fatalf("CapabilitiesContext: %v", err)
	}
	if !c.HasOnSchema("Sales", "ALTER") {
		t.Error("a granted schema permission did not read back")
	}
	if c.AllowsOnSchema("dbo", "ALTER") || c.PermitsOnSchema("dbo", "ALTER") {
		t.Error("a denied schema permission read as allowed")
	}
	if !c.AllowsOnSchema("guest", "ALTER") {
		t.Error("a NULL schema answer read as a denial")
	}
	if !c.HasOnSchema("ALTER", "ALTER") {
		t.Error("a schema named like a permission did not read back")
	}
	// And the database-scope answer for the same word is untouched.
	if c.Allows("ALTER") {
		t.Error("a schema row overwrote the database-scope permission of the same name")
	}
	// A schema nobody asked about is unknown, and unknown fails open.
	if c.HasOnSchema("Archive", "ALTER") || !c.PermitsOnSchema("Archive", "ALTER") {
		t.Error("an unprobed schema did not fail open")
	}
}

// TestSchemaPermissionsFoldInAccessibilityAndNil. The two fail-open shapes
// PermitsOnSchema has to keep apart: nothing was ever probed (fail open) and
// the database was measured inaccessible (withhold) — Permits's rule, applied
// at the scope below it.
func TestSchemaPermissionsFoldInAccessibilityAndNil(t *testing.T) {
	var never *DatabaseCapabilities
	if !never.PermitsOnSchema("Sales", "ALTER") || never.HasOnSchema("Sales", "ALTER") {
		t.Error("a nil capability set did not fail open on a schema")
	}

	shut := &DatabaseCapabilities{SchemaPermissions: map[string]map[string]CapabilityState{
		"Sales": {"ALTER": CapabilityGranted},
	}}
	if shut.PermitsOnSchema("Sales", "ALTER") {
		t.Error("an inaccessible database still permitted a schema-scoped action")
	}
}

// TestSchemaCapabilityQueryNumbersItsPlaceholdersAfterTheOthers. The schema
// block is appended to a query that has already bound every role and
// permission name, so its first placeholder is the next free number — off by
// one and the probe reads a role name as a permission with no error.
func TestSchemaCapabilityQueryNumbersItsPlaceholdersAfterTheOthers(t *testing.T) {
	q, args := schemaCapabilityQuery(4, []string{"ALTER", "CONTROL"})

	if !strings.Contains(q, "(VALUES (@p4),(@p5))") {
		t.Errorf("query = %s, want placeholders starting at @p4", q)
	}
	if !slices.Equal(args, []any{"ALTER", "CONTROL"}) {
		t.Errorf("args = %v, want the permission names in order", args)
	}
	for _, n := range []string{"'ALTER'", "'CONTROL'"} {
		if strings.Contains(q, n) {
			t.Errorf("query interpolates %s instead of binding it:\n%s", n, q)
		}
	}
	// QUOTENAME, not the bare name: a schema whose name needs quoting is
	// otherwise asked about as a different securable, and HAS_PERMS_BY_NAME
	// answers NULL for one that does not exist.
	if !strings.Contains(q, "QUOTENAME(s.name)") {
		t.Errorf("query = %s, want the schema name quoted", q)
	}
}

// TestTheDatabaseProbeAsksAboutEverySchemaInOnePass. The schema block is
// appended to a query that has already bound every role and permission name,
// and the answers a test scripts say nothing about what was asked — so the
// query text is the only place its shape is observable. Two mutations survive
// everything else: numbering its placeholders from 1 instead of the next free
// one, and swapping the two string columns so schemas are read back as
// database-scope permissions.
func TestTheDatabaseProbeAsksAboutEverySchemaInOnePass(t *testing.T) {
	script := &capScript{dbAccess: int64(1)}
	srv := capServer(t, script)
	if _, err := srv.Database("HealthClinic").CapabilitiesContext(context.Background()); err != nil {
		t.Fatalf("CapabilitiesContext: %v", err)
	}

	q := script.dbQuery
	if !strings.Contains(q, "FROM sys.schemas") {
		t.Fatalf("the database probe never asked about schemas:\n%s", q)
	}
	// One pass, not one query per schema: the schemas come from the catalog,
	// so nothing is bound per schema.
	want := len(ProbedDatabaseRoles) + len(ProbedDatabasePermissions) +
		len(ProbedSchemaPermissions) + len(ProbedObjectPermissions) +
		len(ProbedSchemaPermissions) + // the schema catalog block binds them again
		len(ProbedDatabasePermissions) + // and the database catalog block binds those again
		len(ProbedPrincipalPermissions) // and the class-4 catalog block binds its own
	if len(script.dbArgs) != want {
		t.Errorf("the probe bound %d names, want %d", len(script.dbArgs), want)
	}
	first := len(ProbedDatabaseRoles) + len(ProbedDatabasePermissions) + 1
	if !strings.Contains(q, fmt.Sprintf("CROSS JOIN (VALUES (@p%d)", first)) {
		t.Errorf("the schema block does not start at @p%d:\n%s", first, q)
	}
	// The permission in the kind column, the schema in the name column.
	if !strings.Contains(q, "SELECT CONCAT('S:', n.v), s.name,") {
		t.Errorf("the schema block's columns are the wrong way round:\n%s", q)
	}
}

// TestDatabaseProbedSeparatesNotAMemberFromNeverAsked is
// TestProbedSeparatesNotAMemberFromNeverAsked at database scope. It matters
// most for the three SQLAgent* roles: they are the only thing that permits
// SQL Agent's New Job / New Schedule / New Alert / New Operator, and a caller
// withholding those on "not a member" would withhold them from every
// connection whose msdb probe had not landed yet.
//
// An inaccessible database is deliberately probed: the probe ran and reported
// that nothing inside could be asked, which is an answer, not a gap.
func TestDatabaseProbedSeparatesNotAMemberFromNeverAsked(t *testing.T) {
	var never *DatabaseCapabilities
	if never.Probed() {
		t.Error("a nil capability set reported itself probed")
	}
	if (&DatabaseCapabilities{}).Probed() {
		t.Error("the zero value reported itself probed")
	}

	answered := &DatabaseCapabilities{
		Accessible: true,
		Roles:      map[string]bool{"SQLAgentUserRole": false},
	}
	if !answered.Probed() {
		t.Error("a set the server answered did not report itself probed")
	}
	if answered.InRole("SQLAgentUserRole") {
		t.Error("InRole = true for a role the server said the login is not in")
	}

	inaccessible := &DatabaseCapabilities{Roles: map[string]bool{}}
	if !inaccessible.Probed() {
		t.Error("an inaccessible database reported itself never asked")
	}
}

// TestTheObjectProbeAsksAboutEveryObjectInOnePass. The OBJECT scope went
// unprobed because HAS_PERMS_BY_NAME answers for one securable per call, which
// is a query per object. Reading the catalog instead costs one pass, and this
// pins the four parts of that read whose absence each produces a wrong answer
// rather than an error.
func TestTheObjectProbeAsksAboutEveryObjectInOnePass(t *testing.T) {
	q, args := objectCapabilityQuery(1, ProbedObjectPermissions)

	if len(args) != len(ProbedObjectPermissions) {
		t.Errorf("the object block bound %d names, want %d — it must not bind per object",
			len(args), len(ProbedObjectPermissions))
	}
	for _, want := range []struct{ frag, why string }{
		{"FROM sys.objects", "ownership is invisible to the permission catalog: an owner holds implicit CONTROL with no permission row"},
		{"FROM sys.database_permissions", "explicit grants come from the permission catalog"},
		{"p.minor_id = 0", "column-level grants share class 1 and would report as grants on the table"},
		{"p.permission_name IN (n.v, 'CONTROL')", "CONTROL implies the permission asked about, and public's catalog grants must stay out"},
		{"OBJECT_NAME(p.major_id) IS NOT NULL", "an object hidden by metadata visibility would key on a bare dot"},
	} {
		if !strings.Contains(q, want.frag) {
			t.Errorf("the object block is missing %q — %s:\n%s", want.frag, want.why, q)
		}
	}
	// The permission in the kind column, the object in the name column, as
	// scanCapabilityRows reads them.
	if !strings.Contains(q, "SELECT CONCAT('O:', n.v), CONCAT(SCHEMA_NAME(o.schema_id), '.', o.name)") {
		t.Errorf("the object block's columns are the wrong way round:\n%s", q)
	}
	if !strings.Contains(capabilityPrincipalCTE, "JOIN cap_me ON rm.member_principal_id = cap_me.id") {
		t.Error("the principal set does not recurse; a permission granted to a nested role would be missed")
	}
}

// TestAnObjectPermissionIsAdditiveNotAWithholdingTest. ObjectPermissions is
// the one sparse map: an object nobody granted anything on has no row at all.
// Reading it the way the other three are read — "not denied means allowed" —
// would report every object in the database as permitted, so HasOnObject is
// the only accessor, and a missing object must answer false.
func TestAnObjectPermissionIsAdditiveNotAWithholdingTest(t *testing.T) {
	c := &DatabaseCapabilities{
		Accessible: true,
		ObjectPermissions: map[string]map[string]CapabilityState{
			"dbo.Granted": {"ALTER": CapabilityGranted},
			"dbo.Denied":  {"ALTER": CapabilityDenied},
		},
	}
	if !c.HasOnObject("dbo", "Granted", "ALTER") {
		t.Error("an explicitly granted object did not report the permission")
	}
	if c.HasOnObject("dbo", "Denied", "ALTER") {
		t.Error("an explicitly denied object reported the permission held")
	}
	if c.HasOnObject("dbo", "NeverMentioned", "ALTER") {
		t.Error("an object with no row reported the permission held — the map is sparse, not exhaustive")
	}
	var nilCaps *DatabaseCapabilities
	if nilCaps.HasOnObject("dbo", "Granted", "ALTER") {
		t.Error("a nil capability set reported a permission held")
	}
}

// TestADenyOnAnObjectSurvivesAGrant. SQL Server resolves DENY over GRANT, and
// the object block can produce both rows for one object — a grant to a role
// the login is in, a deny to the login itself — in either order.
func TestADenyOnAnObjectSurvivesAGrant(t *testing.T) {
	for _, order := range [][]int64{{1, 0}, {0, 1}} {
		objects := map[string]map[string]CapabilityState{}
		for _, answer := range order {
			st, _ := capabilityStateOf(sql.NullInt64{Int64: answer, Valid: true})
			if objects["dbo.T"] == nil {
				objects["dbo.T"] = map[string]CapabilityState{}
			}
			if objects["dbo.T"]["ALTER"] == CapabilityDenied {
				continue
			}
			objects["dbo.T"]["ALTER"] = st
		}
		c := &DatabaseCapabilities{Accessible: true, ObjectPermissions: objects}
		if c.HasOnObject("dbo", "T", "ALTER") {
			t.Errorf("a deny was overwritten by a grant, rows arriving as %v", order)
		}
	}
}

// TestDeniedOnObjectReportsOnlyAnExplicitDeny. DeniedOnObject is the sparse
// map's withholding test, and it is sound only because it asks for the state
// that was recorded: an object nobody mentioned reads unknown, which is not a
// denial, and a caller that withheld on it would withhold on every object in
// the database.
func TestDeniedOnObjectReportsOnlyAnExplicitDeny(t *testing.T) {
	c := &DatabaseCapabilities{
		Accessible: true,
		ObjectPermissions: map[string]map[string]CapabilityState{
			"dbo.Granted": {"ALTER": CapabilityGranted},
			"dbo.Denied":  {"ALTER": CapabilityDenied},
		},
	}
	if !c.DeniedOnObject("dbo", "Denied", "ALTER") {
		t.Error("an explicitly denied object did not report the denial")
	}
	if c.DeniedOnObject("dbo", "Granted", "ALTER") {
		t.Error("an explicitly granted object reported a denial")
	}
	if c.DeniedOnObject("dbo", "NeverMentioned", "ALTER") {
		t.Error("an object with no row reported a denial — the map is sparse, so silence is not a deny")
	}
	if c.DeniedOnObject("dbo", "Denied", "SELECT") {
		t.Error("a permission that was never probed reported a denial")
	}
	var nilCaps *DatabaseCapabilities
	if nilCaps.DeniedOnObject("dbo", "Denied", "ALTER") {
		t.Error("a nil capability set reported a denial")
	}
	unprobed := &DatabaseCapabilities{Accessible: true}
	if unprobed.DeniedOnObject("dbo", "Denied", "ALTER") {
		t.Error("a database that was never probed reported a denial")
	}
}

// TestTheObjectProbeKeepsColumnScopeApart. A column-scope permission row
// shares class 1 with the object-level ones and differs only in minor_id, so
// the block that reads them is the only thing keeping the two scopes from
// answering for each other. Two mutations this catches and nothing else does:
// dropping the column block (a DENY on one column becomes invisible and the
// table's grant answers for it) and folding it into the object block (the same
// DENY becomes a denial of the whole table).
func TestTheObjectProbeKeepsColumnScopeApart(t *testing.T) {
	q, args := objectCapabilityQuery(1, ProbedObjectPermissions)

	if len(args) != len(ProbedObjectPermissions) {
		t.Errorf("the object block bound %d names, want %d — the column block reuses the same placeholders",
			len(args), len(ProbedObjectPermissions))
	}
	for _, want := range []struct{ frag, why string }{
		{"p.minor_id > 0", "the column-scope rows are the ones minor_id > 0 selects"},
		{"COL_NAME(p.major_id, p.minor_id)", "the column name is what makes the row addressable"},
		{"CONCAT('C:', n.v)", "column rows must be tagged apart from the object ones"},
		{"COL_NAME(p.major_id, p.minor_id) IS NOT NULL", "a dropped column would key on a trailing dot"},
	} {
		if !strings.Contains(q, want.frag) {
			t.Errorf("the column block is missing %q — %s:\n%s", want.frag, want.why, q)
		}
	}
	// And the object block still excludes them, or every column row is read
	// twice — once correctly and once as the table.
	if !strings.Contains(q, "p.class = 1 AND p.minor_id = 0") {
		t.Errorf("the object block no longer excludes column rows:\n%s", q)
	}
}

// TestDatabaseCapabilitiesReadColumnPermissionsApartFromTheirTable. The
// end-to-end shape: a table granted ALTER with one of its columns denied it
// must read as granted on the table, denied on that column, and denied on
// "any" column — the last being what a caller gating a table-wide action asks.
func TestDatabaseCapabilitiesReadColumnPermissionsApartFromTheirTable(t *testing.T) {
	srv := capServer(t, &capScript{
		dbAccess: int64(1),
		dbRows: [][]driver.Value{
			{"O:ALTER", "dbo.Patients", int64(1)},
			{"C:ALTER", "dbo.Patients.SSN", int64(0)},
			{"C:ALTER", "dbo.Patients.Notes", int64(1)},
		},
	})

	c, err := srv.Database("HealthClinic").CapabilitiesContext(context.Background())
	if err != nil {
		t.Fatalf("CapabilitiesContext: %v", err)
	}
	if !c.HasOnObject("dbo", "Patients", "ALTER") {
		t.Error("the table's own grant did not read back")
	}
	if c.DeniedOnObject("dbo", "Patients", "ALTER") {
		t.Error("a column denial was recorded as a denial on the table")
	}
	if !c.DeniedOnColumn("dbo", "Patients", "SSN", "ALTER") {
		t.Error("a denied column did not read back as denied")
	}
	if c.DeniedOnColumn("dbo", "Patients", "Notes", "ALTER") {
		t.Error("a granted column read as denied")
	}
	if !c.HasOnColumn("dbo", "Patients", "Notes", "ALTER") {
		t.Error("a granted column did not read back as held")
	}
	if col, denied := c.DeniedOnAnyColumn("dbo", "Patients", "ALTER"); !denied || col != "SSN" {
		t.Errorf("DeniedOnAnyColumn = %q, %v; want \"SSN\", true", col, denied)
	}
	// A column nobody mentioned is unknown, and the map is sparse exactly as
	// the object map is: unknown is not a denial.
	if c.HasOnColumn("dbo", "Patients", "Name", "ALTER") ||
		c.DeniedOnColumn("dbo", "Patients", "Name", "ALTER") {
		t.Error("a column with no row did not read as unknown")
	}
	if _, denied := c.DeniedOnAnyColumn("dbo", "Visits", "ALTER"); denied {
		t.Error("a table with no column rows reported a column denial")
	}
	var nilCaps *DatabaseCapabilities
	if nilCaps.HasOnColumn("dbo", "Patients", "SSN", "ALTER") ||
		nilCaps.DeniedOnColumn("dbo", "Patients", "SSN", "ALTER") {
		t.Error("a nil capability set answered about a column")
	}
	if _, denied := nilCaps.DeniedOnAnyColumn("dbo", "Patients", "ALTER"); denied {
		t.Error("a nil capability set reported a column denial")
	}
}

// TestADenyOnAColumnSurvivesAGrantAndIsNamedStably. The column map takes the
// object map's deny-wins rule — both rows can arrive for one column, in either
// order — and DeniedOnAnyColumn must name the same column every call, which
// map iteration alone does not give.
func TestADenyOnAColumnSurvivesAGrantAndIsNamedStably(t *testing.T) {
	for _, order := range [][]driver.Value{{int64(1), int64(0)}, {int64(0), int64(1)}} {
		srv := capServer(t, &capScript{
			dbAccess: int64(1),
			dbRows: [][]driver.Value{
				{"C:ALTER", "dbo.Patients.SSN", order[0]},
				{"C:ALTER", "dbo.Patients.SSN", order[1]},
			},
		})
		c, err := srv.Database("HealthClinic").CapabilitiesContext(context.Background())
		if err != nil {
			t.Fatalf("CapabilitiesContext: %v", err)
		}
		if !c.DeniedOnColumn("dbo", "Patients", "SSN", "ALTER") {
			t.Errorf("a deny was overwritten by a grant, rows arriving as %v", order)
		}
	}

	c := &DatabaseCapabilities{
		Accessible: true,
		ColumnPermissions: map[string]map[string]CapabilityState{
			"dbo.Patients.SSN":    {"ALTER": CapabilityDenied},
			"dbo.Patients.Notes":  {"ALTER": CapabilityDenied},
			"dbo.Patients.Amount": {"ALTER": CapabilityGranted},
		},
	}
	for range 20 {
		if col, _ := c.DeniedOnAnyColumn("dbo", "Patients", "ALTER"); col != "Notes" {
			t.Fatalf("DeniedOnAnyColumn = %q, want the lowest denied name \"Notes\"", col)
		}
	}
}

// TestTheSchemaCatalogBlockAsksForExplicitRowsOnly. The schema scope is probed
// twice, and the two blocks are not interchangeable: HAS_PERMS_BY_NAME answers
// 0 both for a permission denied on the schema and for one never granted, so
// only a catalog read can report a DENY. This pins the parts of that read whose
// absence produces a wrong answer rather than an error.
func TestTheSchemaCatalogBlockAsksForExplicitRowsOnly(t *testing.T) {
	q, args := explicitSchemaCapabilityQuery(7, ProbedSchemaPermissions)

	if len(args) != len(ProbedSchemaPermissions) {
		t.Errorf("the schema catalog block bound %d names, want %d — it must not bind per schema",
			len(args), len(ProbedSchemaPermissions))
	}
	if !strings.Contains(q, "(VALUES (@p7)") {
		t.Errorf("query = %s, want placeholders starting at @p7", q)
	}
	for _, want := range []struct{ frag, why string }{
		{"p.class = 3", "class 3 is SCHEMA; any other class answers for a different securable"},
		{"SCHEMA_NAME(p.major_id)", "major_id is the schema_id, and the name is what the map is keyed by"},
		{"p.grantee_principal_id IN (SELECT id FROM cap_me)", "a permission reaching the login through a nested role is held just as fully"},
		{"p.permission_name IN (n.v, 'CONTROL')", "CONTROL implies the permission asked about, and public's grants must stay out"},
		{"SCHEMA_NAME(p.major_id) IS NOT NULL", "a schema hidden by metadata visibility would key on an empty name"},
	} {
		if !strings.Contains(q, want.frag) {
			t.Errorf("the schema catalog block is missing %q — %s:\n%s", want.frag, want.why, q)
		}
	}
	// The permission in the kind column, the schema in the name column, as
	// scanCapabilityRows reads them — and tagged apart from the "S:" rows.
	if !strings.Contains(q, "SELECT CONCAT('E:', n.v), SCHEMA_NAME(p.major_id),") {
		t.Errorf("the schema catalog block's columns are the wrong way round:\n%s", q)
	}
}

// TestDeniedOnSchemaReportsOnlyAnExplicitDeny. ExplicitSchemaPermissions is
// sparse like ObjectPermissions, so silence is not a denial; and it is separate
// from SchemaPermissions precisely because that map's CapabilityDenied means
// "HAS_PERMS_BY_NAME said 0", which a permission that was simply never granted
// also produces.
func TestDeniedOnSchemaReportsOnlyAnExplicitDeny(t *testing.T) {
	c := &DatabaseCapabilities{
		Accessible: true,
		// Never granted at schema scope, which is not a denial.
		SchemaPermissions: map[string]map[string]CapabilityState{
			"Reporting": {"ALTER": CapabilityDenied},
		},
		ExplicitSchemaPermissions: map[string]map[string]CapabilityState{
			"Sales": {"ALTER": CapabilityDenied},
			"HR":    {"ALTER": CapabilityGranted},
		},
	}
	if !c.DeniedOnSchema("Sales", "ALTER") {
		t.Error("an explicitly denied schema did not report the denial")
	}
	if c.DeniedOnSchema("HR", "ALTER") {
		t.Error("an explicitly granted schema reported a denial")
	}
	if c.DeniedOnSchema("Reporting", "ALTER") {
		t.Error("a schema HAS_PERMS_BY_NAME answered 0 for reported an explicit denial")
	}
	if c.DeniedOnSchema("dbo", "ALTER") {
		t.Error("a schema with no row reported a denial — the map is sparse, so silence is not a deny")
	}
	if c.DeniedOnSchema("Sales", "SELECT") {
		t.Error("a permission that was never probed reported a denial")
	}
	var nilCaps *DatabaseCapabilities
	if nilCaps.DeniedOnSchema("Sales", "ALTER") {
		t.Error("a nil capability set reported a denial")
	}
}

// TestDatabaseCapabilitiesReadSchemaDenialsApartFromTheProbe. The end-to-end
// shape: the two schema blocks land in two maps, and a DENY recorded on the
// schema must not be readable as anything the HAS_PERMS_BY_NAME half said.
func TestDatabaseCapabilitiesReadSchemaDenialsApartFromTheProbe(t *testing.T) {
	srv := capServer(t, &capScript{
		dbAccess: int64(1),
		dbRows: [][]driver.Value{
			{"P", "ALTER", int64(1)},
			{"S:ALTER", "Sales", int64(0)},
			{"E:ALTER", "Sales", int64(0)},
			{"S:ALTER", "HR", int64(1)},
		},
	})

	c, err := srv.Database("HealthClinic").CapabilitiesContext(context.Background())
	if err != nil {
		t.Fatalf("CapabilitiesContext: %v", err)
	}
	if !c.DeniedOnSchema("Sales", "ALTER") {
		t.Error("the schema's DENY row did not read back")
	}
	if c.DeniedOnSchema("HR", "ALTER") {
		t.Error("a schema with no catalog row read as denied")
	}
	if c.PermitsOnSchema("Sales", "ALTER") {
		t.Error("the HAS_PERMS_BY_NAME half stopped answering once the catalog half arrived")
	}
	if !c.HasOnSchema("HR", "ALTER") {
		t.Error("a schema granted ALTER did not read back as held")
	}
}

// TestADenyOnASchemaSurvivesAGrant. The catalog block can produce both rows for
// one schema — a grant to a role the login is in, a deny to the login itself —
// in either order, and SQL Server resolves DENY over GRANT whichever arrives
// first.
func TestADenyOnASchemaSurvivesAGrant(t *testing.T) {
	for _, order := range [][]int64{{1, 0}, {0, 1}} {
		srv := capServer(t, &capScript{
			dbAccess: int64(1),
			dbRows: [][]driver.Value{
				{"E:ALTER", "Sales", order[0]},
				{"E:ALTER", "Sales", order[1]},
			},
		})
		c, err := srv.Database("HealthClinic").CapabilitiesContext(context.Background())
		if err != nil {
			t.Fatalf("CapabilitiesContext: %v", err)
		}
		if !c.DeniedOnSchema("Sales", "ALTER") {
			t.Errorf("a deny was overwritten by a grant, rows arriving as %v", order)
		}
	}
}

// TestDeniedOnDatabaseReportsOnlyAnExplicitDeny. ExplicitDatabasePermissions
// is sparse, so silence is not a denial; and it is separate from Permissions
// precisely because that map's CapabilityDenied means "HAS_PERMS_BY_NAME said
// 0", which a permission simply never granted also produces — the ordinary
// state of a principal working through an object- or schema-scope grant.
func TestDeniedOnDatabaseReportsOnlyAnExplicitDeny(t *testing.T) {
	c := &DatabaseCapabilities{
		Accessible: true,
		// Never granted at database scope, which is not a denial. This is the
		// case the whole map exists to tell apart: read as a denial it would
		// withhold every write from a principal granted ALTER on one table.
		Permissions: map[string]CapabilityState{"CONTROL": CapabilityDenied},
		ExplicitDatabasePermissions: map[string]CapabilityState{
			"ALTER": CapabilityDenied,
		},
	}
	if !c.DeniedOnDatabase("ALTER") {
		t.Error("an explicitly denied permission did not report the denial")
	}
	if c.DeniedOnDatabase("CONTROL") {
		t.Error("a permission HAS_PERMS_BY_NAME answered 0 for reported an explicit denial")
	}
	if c.DeniedOnDatabase("BACKUP DATABASE") {
		t.Error("a permission with no row reported a denial — the map is sparse, so silence is not a deny")
	}
	var nilCaps *DatabaseCapabilities
	if nilCaps.DeniedOnDatabase("ALTER") {
		t.Error("a nil capability set reported a denial")
	}
}

// TestDatabaseCapabilitiesReadDatabaseDenialsApartFromTheProbe is
// TestDatabaseCapabilitiesReadSchemaDenialsApartFromTheProbe one scope wider:
// the "P" and "D:" blocks land in two maps, and a DENY recorded at database
// scope must not be readable as anything the HAS_PERMS_BY_NAME half said.
func TestDatabaseCapabilitiesReadDatabaseDenialsApartFromTheProbe(t *testing.T) {
	srv := capServer(t, &capScript{
		dbAccess: int64(1),
		dbRows: [][]driver.Value{
			{"P", "ALTER", int64(0)},
			{"P", "CONTROL", int64(0)},
			{"D:ALTER", "HealthClinic", int64(0)},
			{"O:ALTER", "dbo.Patient", int64(1)},
		},
	})

	c, err := srv.Database("HealthClinic").CapabilitiesContext(context.Background())
	if err != nil {
		t.Fatalf("CapabilitiesContext: %v", err)
	}
	if !c.DeniedOnDatabase("ALTER") {
		t.Error("the database's DENY row did not read back")
	}
	// CONTROL reads 0 from HAS_PERMS_BY_NAME and has no catalog row. Reading
	// that as a denial is the bug this map prevents.
	if c.DeniedOnDatabase("CONTROL") {
		t.Error("a permission with no catalog row read as denied")
	}
	// The object grant is untouched by the wider denial at this layer — it is
	// the *caller's* job to resolve one over the other, and it can only do
	// that while both answers survive the probe.
	if !c.HasOnObject("dbo", "Patient", "ALTER") {
		t.Error("the object grant stopped reading back once the database denial arrived")
	}
}

// TestADenyOnTheDatabaseSurvivesAGrant. The catalog block can produce two rows
// for one permission — through the login and through a role it is in — and a
// denial must win whichever arrives first. The block selects only DENY rows
// today, so this pins the recording rule rather than the query: a later change
// that starts selecting grants must not let one overwrite a denial.
func TestADenyOnTheDatabaseSurvivesAGrant(t *testing.T) {
	for _, order := range [][]int64{{1, 0}, {0, 1}} {
		srv := capServer(t, &capScript{
			dbAccess: int64(1),
			dbRows: [][]driver.Value{
				{"D:ALTER", "HealthClinic", order[0]},
				{"D:ALTER", "HealthClinic", order[1]},
			},
		})
		c, err := srv.Database("HealthClinic").CapabilitiesContext(context.Background())
		if err != nil {
			t.Fatalf("CapabilitiesContext: %v", err)
		}
		if !c.DeniedOnDatabase("ALTER") {
			t.Errorf("rows in order %v lost the denial", order)
		}
	}
}

// TestTheDatabaseProbeAsksForExplicitDatabaseDenials. The block's text is the
// only place its shape is observable, since the answers are scripted whatever
// it asks. Three mutations survive every other test: dropping the state filter
// (so every grant row reads back as a denial), matching CONTROL alongside the
// permission the way the schema block does (a DENY CONTROL denies CONNECT with
// it, so the login never reaches this map — but a *grant* row for CONTROL
// would arrive and, without the state filter, read as a denial of ALTER), and
// numbering the placeholders from 1 instead of the next free one.
func TestTheDatabaseProbeAsksForExplicitDatabaseDenials(t *testing.T) {
	script := &capScript{dbAccess: int64(1)}
	srv := capServer(t, script)
	if _, err := srv.Database("HealthClinic").CapabilitiesContext(context.Background()); err != nil {
		t.Fatalf("CapabilitiesContext: %v", err)
	}
	q := script.dbQuery
	if !strings.Contains(q, "SELECT CONCAT('D:', n.v), DB_NAME(), 0") {
		t.Errorf("the database catalog block is missing or its columns are the wrong way round:\n%s", q)
	}
	if !strings.Contains(q, "WHERE p.class = 0") {
		t.Errorf("the database catalog block does not restrict to class 0 (DATABASE):\n%s", q)
	}
	if !strings.Contains(q, "AND p.state = 'D'") {
		t.Errorf("the database catalog block does not restrict to DENY rows — every grant would read as a denial:\n%s", q)
	}
	if !strings.Contains(q, "AND p.permission_name = n.v") {
		t.Errorf("the database catalog block does not match the permission exactly:\n%s", q)
	}
	if strings.Contains(q, "p.permission_name IN (n.v, 'CONTROL')\n\t  AND p.class = 0") {
		t.Errorf("the database catalog block matches CONTROL alongside the permission:\n%s", q)
	}
	// Its placeholders start after the five blocks appended before it. The
	// class-4 block follows it, so this is no longer the last offset in the
	// query — see TestTheDatabaseProbeAsksForExplicitPrincipalDenials.
	first := len(ProbedDatabaseRoles) + len(ProbedDatabasePermissions) +
		len(ProbedSchemaPermissions) + len(ProbedObjectPermissions) +
		len(ProbedSchemaPermissions) + 1
	if !strings.Contains(q, fmt.Sprintf("CROSS JOIN (VALUES (@p%d)", first)) {
		t.Errorf("the database catalog block does not start at @p%d:\n%s", first, q)
	}
}

// TestDeniedOnPrincipalReportsOnlyAnExplicitDeny is
// TestDeniedOnDatabaseReportsOnlyAnExplicitDeny at class 4: the map is sparse,
// so a principal nobody denied anything on is not a denial, and the
// database-scope Permissions map cannot stand in — its CapabilityDenied means
// "HAS_PERMS_BY_NAME said 0", the ordinary reading for a principal that simply
// was never granted ALTER ANY USER.
func TestDeniedOnPrincipalReportsOnlyAnExplicitDeny(t *testing.T) {
	c := &DatabaseCapabilities{
		Accessible:  true,
		Permissions: map[string]CapabilityState{"ALTER ANY USER": CapabilityDenied},
		ExplicitPrincipalPermissions: map[string]map[string]CapabilityState{
			"bob": {"ALTER": CapabilityDenied},
		},
	}
	if !c.DeniedOnPrincipal("bob", "ALTER") {
		t.Error("an explicitly denied principal did not report the denial")
	}
	if c.DeniedOnPrincipal("carol", "ALTER") {
		t.Error("a principal with no row reported a denial — the map is sparse, so silence is not a deny")
	}
	if c.DeniedOnPrincipal("bob", "CONTROL") {
		t.Error("a permission with no row on a denied principal reported a denial")
	}
	var nilCaps *DatabaseCapabilities
	if nilCaps.DeniedOnPrincipal("bob", "ALTER") {
		t.Error("a nil capability set reported a denial")
	}
}

// TestDatabaseCapabilitiesReadPrincipalDenialsApartFromTheProbe. The "P" and
// "N:" blocks land in two maps, and neither may be read as the other: a
// database-scope ALTER ANY USER that HAS_PERMS_BY_NAME answered 1 for must
// survive a class-4 denial on one principal, because resolving the two is the
// caller's job and it can only do that while both answers reach it.
func TestDatabaseCapabilitiesReadPrincipalDenialsApartFromTheProbe(t *testing.T) {
	srv := capServer(t, &capScript{
		dbAccess: int64(1),
		dbRows: [][]driver.Value{
			{"P", "ALTER ANY USER", int64(1)},
			{"N:ALTER", "bob", int64(0)},
		},
	})
	c, err := srv.Database("HealthClinic").CapabilitiesContext(context.Background())
	if err != nil {
		t.Fatalf("CapabilitiesContext: %v", err)
	}
	if !c.DeniedOnPrincipal("bob", "ALTER") {
		t.Error("the principal's DENY row did not read back")
	}
	if c.DeniedOnPrincipal("carol", "ALTER") {
		t.Error("a principal with no catalog row read as denied")
	}
	if !c.Permits("ALTER ANY USER") {
		t.Error("the database-scope grant stopped reading back once the principal denial arrived")
	}
}

// TestTheDatabaseProbeAsksForExplicitPrincipalDenials. The block's text is the
// only place its shape is observable, since the answers are scripted whatever
// it asks. Four mutations survive every other test: dropping the state filter
// (so every grant row reads back as a denial — and at this class a *grant* row
// is the common one, since GRANT ALTER ON USER::x is legal and permits
// nothing); dropping the class filter, which would fold class-1 object rows in
// under OBJECT_NAME-shaped names; matching the permission exactly and so
// losing DENY CONTROL, which withholds the same statements; and numbering the
// placeholders from 1 instead of the next free one.
func TestTheDatabaseProbeAsksForExplicitPrincipalDenials(t *testing.T) {
	script := &capScript{dbAccess: int64(1)}
	srv := capServer(t, script)
	if _, err := srv.Database("HealthClinic").CapabilitiesContext(context.Background()); err != nil {
		t.Fatalf("CapabilitiesContext: %v", err)
	}
	q := script.dbQuery
	if !strings.Contains(q, "SELECT CONCAT('N:', n.v), USER_NAME(p.major_id), 0") {
		t.Errorf("the class-4 catalog block is missing or its columns are the wrong way round:\n%s", q)
	}
	if !strings.Contains(q, "WHERE p.class = 4") {
		t.Errorf("the class-4 catalog block does not restrict to class 4 (DATABASE_PRINCIPAL):\n%s", q)
	}
	if !strings.Contains(q, "AND p.state = 'D'\n\t  AND p.grantee_principal_id IN (SELECT id FROM cap_me)\n\t  AND p.permission_name IN (n.v, 'CONTROL')") {
		t.Errorf("the class-4 catalog block does not select DENY rows matching the permission or CONTROL:\n%s", q)
	}
	// It is appended last, so its placeholders start after every other block's.
	first := len(ProbedDatabaseRoles) + len(ProbedDatabasePermissions) +
		len(ProbedSchemaPermissions) + len(ProbedObjectPermissions) +
		len(ProbedSchemaPermissions) + len(ProbedDatabasePermissions) + 1
	if !strings.Contains(q, fmt.Sprintf("CROSS JOIN (VALUES (@p%d)", first)) {
		t.Errorf("the class-4 catalog block does not start at @p%d:\n%s", first, q)
	}
}

// TestDeniedOnServerSecurableReportsOnlyAnExplicitDeny is
// TestDeniedOnPrincipalReportsOnlyAnExplicitDeny at server scope: the map is
// sparse, so a securable nobody denied anything on is not a denial, and the
// server-scope ServerPermissions map cannot stand in — its CapabilityDenied
// means "HAS_PERMS_BY_NAME said 0", the ordinary reading for a login that
// simply was never granted ALTER ANY LOGIN.
func TestDeniedOnServerSecurableReportsOnlyAnExplicitDeny(t *testing.T) {
	c := &Capabilities{
		ServerPermissions: map[string]CapabilityState{"ALTER ANY LOGIN": CapabilityDenied},
		ExplicitServerPermissions: map[string]map[string]CapabilityState{
			ServerSecurableKey(ServerSecurableLogin, "bob"):          {"ALTER": CapabilityDenied},
			ServerSecurableKey(ServerSecurableServerRole, "ops"):     {"ALTER": CapabilityDenied},
			ServerSecurableKey(ServerSecurableEndpoint, "Mirroring"): {"ALTER": CapabilityDenied},
		},
	}
	if !c.DeniedOnLogin("bob", "ALTER") {
		t.Error("an explicitly denied login did not report the denial")
	}
	if !c.DeniedOnServerRole("ops", "ALTER") {
		t.Error("an explicitly denied server role did not report the denial")
	}
	if !c.DeniedOnEndpoint("Mirroring", "ALTER") {
		t.Error("an explicitly denied endpoint did not report the denial")
	}
	if c.DeniedOnLogin("carol", "ALTER") {
		t.Error("a login with no row reported a denial — the map is sparse, so silence is not a deny")
	}
	if c.DeniedOnLogin("bob", "CONTROL") {
		t.Error("a permission with no row on a denied login reported a denial")
	}
	var nilCaps *Capabilities
	if nilCaps.DeniedOnLogin("bob", "ALTER") {
		t.Error("a nil capability set reported a denial")
	}
}

// TestServerSecurableKindsDoNotAnswerForEachOther. The three kinds share one
// map and one class — logins and server roles are both class 101 — so the kind
// is part of the key, and it has to be: a class-101 DENY withholds everything
// on a login and only the membership edits on a server role, so a caller
// reaching a login's answer while asking about a server role of the same name
// would withhold a rename the server allows.
func TestServerSecurableKindsDoNotAnswerForEachOther(t *testing.T) {
	c := &Capabilities{
		ExplicitServerPermissions: map[string]map[string]CapabilityState{
			ServerSecurableKey(ServerSecurableLogin, "shared"): {"ALTER": CapabilityDenied},
		},
	}
	if !c.DeniedOnLogin("shared", "ALTER") {
		t.Fatal("the login's own denial did not read back")
	}
	if c.DeniedOnServerRole("shared", "ALTER") {
		t.Error("a login's DENY answered for a server role of the same name")
	}
	if c.DeniedOnEndpoint("shared", "ALTER") {
		t.Error("a login's DENY answered for an endpoint of the same name")
	}
}

// TestTheServerCatalogBlockAsksForExplicitRowsOnly pins the parts of the
// server-scope catalog read whose absence produces a wrong answer rather than
// an error — TestTheSchemaCatalogBlockAsksForExplicitRowsOnly's server twin.
func TestTheServerCatalogBlockAsksForExplicitRowsOnly(t *testing.T) {
	q, args := explicitServerCapabilityQuery(7, ProbedServerSecurablePermissions)

	if len(args) != len(ProbedServerSecurablePermissions) {
		t.Errorf("the server catalog block bound %d names, want %d — it must not bind per securable",
			len(args), len(ProbedServerSecurablePermissions))
	}
	if !strings.Contains(q, "(VALUES (@p7)") {
		t.Errorf("query = %s, want placeholders starting at @p7", q)
	}
	for _, want := range []struct{ frag, why string }{
		{"p.class = 101", "class 101 is SERVER_PRINCIPAL, which covers logins and server roles alike"},
		{"p.class = 105", "class 105 is ENDPOINT; there is no class 110, and a role probed at one finds nothing"},
		{"sp.principal_id = p.major_id", "major_id is the principal_id at class 101"},
		{"e.endpoint_id = p.major_id", "major_id is the endpoint_id at class 105, not the principal_id"},
		{"sp.type_desc WHEN 'SERVER_ROLE'", "the two class-101 kinds are told apart by type_desc and by nothing else"},
		{"p.state = 'D'", "only DENY rows are read; a GRANT read back as a denial withholds a write the login holds"},
		{"p.grantee_principal_id IN (SELECT id FROM cap_srv_me)", "a permission reaching the login through a nested server role is held just as fully"},
		{"p.permission_name IN (n.v, 'CONTROL')", "CONTROL implies the permission asked about"},
	} {
		if !strings.Contains(q, want.frag) {
			t.Errorf("the server catalog block is missing %q — %s:\n%s", want.frag, want.why, q)
		}
	}
	// The permission in the kind column and the "<kind>::<name>" securable in
	// the name column, as scanCapabilityRows and ServerSecurableKey read them.
	if !strings.Contains(q, "SELECT CONCAT('V:', n.v)") {
		t.Errorf("the server catalog block's rows are not tagged \"V:\":\n%s", q)
	}
	if !strings.Contains(q, "CONCAT('ENDPOINT::', e.name)") {
		t.Errorf("the endpoint half does not key on ServerSecurableKey's form:\n%s", q)
	}
}

// TestTheServerProbeAsksForExplicitServerDenials. The block rides on the same
// round trip as the role and permission probes, so what pins it is the query
// the server actually received: a block appended without its CTE, or with its
// placeholders overlapping the blocks before it, binds the wrong names and
// reports denials that were never made.
func TestTheServerProbeAsksForExplicitServerDenials(t *testing.T) {
	script := &capScript{serverRows: [][]driver.Value{{"R", "sysadmin", int64(0)}}}
	srv := capServer(t, script)
	if _, err := srv.CapabilitiesContext(context.Background()); err != nil {
		t.Fatalf("CapabilitiesContext: %v", err)
	}
	q := script.srvQuery
	if !strings.HasPrefix(q, capabilityServerPrincipalCTE) {
		t.Errorf("the server probe does not open with the principal CTE its catalog block selects from:\n%s", q)
	}
	if !strings.Contains(q, "sys.server_permissions") {
		t.Error("the server probe never reads sys.server_permissions, so every server-class DENY is invisible")
	}
	// The catalog block's placeholders start after the roles and the
	// permissions, and nothing but a count check catches an overlap: the
	// driver binds happily either way and the answers come back scripted.
	first := len(ProbedServerRoles) + len(ProbedServerPermissions) + 1
	if !strings.Contains(q, fmt.Sprintf("(VALUES (@p%d)", first)) {
		t.Errorf("the server catalog block does not start at @p%d:\n%s", first, q)
	}
	want := len(ProbedServerRoles) + len(ProbedServerPermissions) +
		len(ProbedServerSecurablePermissions) + len(ProbedAvailabilityGroupPermissions)
	if len(script.srvArgs) != want {
		t.Errorf("the server probe bound %d names, want %d", len(script.srvArgs), want)
	}
}

// TestServerCapabilitiesReadSecurableDenialsApartFromTheProbe. The "P" and
// "V:" blocks land in two maps, and neither may be read as the other: a
// server-scope ALTER ANY LOGIN that HAS_PERMS_BY_NAME answered 1 for must
// survive a class-101 denial on one login, because resolving the two is the
// caller's job and it can only do that while both answers reach it.
func TestServerCapabilitiesReadSecurableDenialsApartFromTheProbe(t *testing.T) {
	srv := capServer(t, &capScript{serverRows: [][]driver.Value{
		{"P", "ALTER ANY LOGIN", int64(1)},
		{"V:ALTER", "LOGIN::bob", int64(0)},
		{"V:ALTER", "SERVER ROLE::ops", int64(0)},
		{"V:ALTER", "ENDPOINT::Mirroring", int64(0)},
	}})
	c, err := srv.CapabilitiesContext(context.Background())
	if err != nil {
		t.Fatalf("CapabilitiesContext: %v", err)
	}
	if !c.DeniedOnLogin("bob", "ALTER") {
		t.Error("the login's DENY row did not read back")
	}
	if !c.DeniedOnServerRole("ops", "ALTER") {
		t.Error("the server role's DENY row did not read back")
	}
	if !c.DeniedOnEndpoint("Mirroring", "ALTER") {
		t.Error("the endpoint's DENY row did not read back")
	}
	if c.DeniedOnLogin("carol", "ALTER") {
		t.Error("a login with no catalog row read as denied")
	}
	if !c.Allows("ALTER ANY LOGIN") {
		t.Error("the server-scope grant stopped reading back once the securable denial arrived")
	}
}

// TestPermitsOnAvailabilityGroupReadsTheProbe. Class 108 is the one server
// scope answered by HAS_PERMS_BY_NAME rather than by a catalog read — see
// ProbedAvailabilityGroupPermissions — so unlike the sparse maps beside it,
// this one may be read in the withholding direction: a probed instance has a
// row per group, and unknown still permits.
func TestPermitsOnAvailabilityGroupReadsTheProbe(t *testing.T) {
	c := &Capabilities{
		ServerPermissions: map[string]CapabilityState{"ALTER ANY AVAILABILITY GROUP": CapabilityGranted},
		AvailabilityGroupPermissions: map[string]map[string]CapabilityState{
			"AAG1": {"ALTER": CapabilityDenied},
			"AAG2": {"ALTER": CapabilityGranted},
		},
	}
	if c.PermitsOnAvailabilityGroup("AAG1", "ALTER") {
		t.Error("a denied availability group read as permitted")
	}
	if !c.PermitsOnAvailabilityGroup("AAG2", "ALTER") {
		t.Error("an undenied availability group read as denied")
	}
	if !c.HasOnAvailabilityGroup("AAG2", "ALTER") {
		t.Error("a granted availability group did not read back as held")
	}
	if c.HasOnAvailabilityGroup("AAG1", "ALTER") {
		t.Error("a denied availability group read as held")
	}
	// Unknown permits, which is the rule the whole layer is built on: a group
	// the probe never reached must not lose its menu items.
	if !c.PermitsOnAvailabilityGroup("AAG3", "ALTER") {
		t.Error("a group with no row read as denied; unknown must fail open")
	}
	var nilCaps *Capabilities
	if !nilCaps.PermitsOnAvailabilityGroup("AAG1", "ALTER") {
		t.Error("a nil capability set withheld an availability group")
	}
	// The server-wide answer is separate and must survive a per-group denial,
	// because resolving the two is the caller's job.
	if !c.Allows("ALTER ANY AVAILABILITY GROUP") {
		t.Error("the server-wide grant stopped reading back once the group denial arrived")
	}
}

// TestTheAvailabilityGroupBlockAsksPerGroup pins the block's shape. The
// permission has to ride in the kind column and the group in the name column,
// scanCapabilityRows' arrangement — reversed, a group named "P" is read back as
// a server-scope permission answer.
func TestTheAvailabilityGroupBlockAsksPerGroup(t *testing.T) {
	q, args := availabilityGroupCapabilityQuery(7, ProbedAvailabilityGroupPermissions)

	if len(args) != len(ProbedAvailabilityGroupPermissions) {
		t.Errorf("the availability-group block bound %d names, want %d — it must not bind per group",
			len(args), len(ProbedAvailabilityGroupPermissions))
	}
	for _, want := range []struct{ frag, why string }{
		{"SELECT CONCAT('G:', n.v), ag.name,", "the permission rides in kind and the group in name"},
		{"HAS_PERMS_BY_NAME(QUOTENAME(ag.name), 'AVAILABILITY GROUP', n.v)", "class 108 has no catalog read; the name must be quoted"},
		{"FROM sys.availability_groups AS ag", "every group on the instance is asked in one pass"},
	} {
		if !strings.Contains(q, want.frag) {
			t.Errorf("the availability-group block is missing %q — %s:\n%s", want.frag, want.why, q)
		}
	}
}

// TestServerCapabilitiesReadGroupAnswersApartFromTheProbe. The "P" and "G:"
// blocks land in two maps, and neither may be read as the other: the
// server-wide ALTER ANY AVAILABILITY GROUP reads 1 on an instance where one
// group carries DENY ALTER, and that is precisely the case the per-group map
// exists to report.
func TestServerCapabilitiesReadGroupAnswersApartFromTheProbe(t *testing.T) {
	srv := capServer(t, &capScript{serverRows: [][]driver.Value{
		{"P", "ALTER ANY AVAILABILITY GROUP", int64(1)},
		{"G:ALTER", "AAG1", int64(0)},
		{"G:ALTER", "AAG2", int64(1)},
	}})
	c, err := srv.CapabilitiesContext(context.Background())
	if err != nil {
		t.Fatalf("CapabilitiesContext: %v", err)
	}
	if c.PermitsOnAvailabilityGroup("AAG1", "ALTER") {
		t.Error("the group's denial did not read back")
	}
	if !c.PermitsOnAvailabilityGroup("AAG2", "ALTER") {
		t.Error("an undenied group read as denied")
	}
	if !c.Allows("ALTER ANY AVAILABILITY GROUP") {
		t.Error("the server-wide grant stopped reading back once the group denial arrived")
	}
}
