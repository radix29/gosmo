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
		len(ProbedSchemaPermissions) + len(ProbedObjectPermissions)
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
