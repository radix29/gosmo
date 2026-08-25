package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
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

func (c *capConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(q, "HAS_DBACCESS"):
		return &capRows{cols: 1, rows: [][]driver.Value{{capCurrent.dbAccess}}}, nil
	case strings.Contains(q, "IS_SRVROLEMEMBER"):
		return &capRows{cols: 3, rows: capCurrent.serverRows}, nil
	case strings.Contains(q, "IS_ROLEMEMBER"):
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
