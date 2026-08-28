package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

// -- a driver that answers the two DBCC options differently ----------------
//
// DetachedDatabaseInfo runs two reads in one call and scans them into
// different shapes. A driver that answered both with the same rows would let
// the property scan and the file scan be swapped without any test noticing.

type detDriver struct{}

func (detDriver) Open(string) (driver.Conn, error) { return &detConn{}, nil }

type detConn struct{}

func (c *detConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *detConn) Close() error                        { return nil }
func (c *detConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (c *detConn) ExecContext(ctx context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	detLog.add(q)
	// Before failOn: a statement that exhausts the caller's deadline reports
	// the context's own error, not a server one.
	if detLog.cancelIfMatched(q) {
		return nil, ctx.Err()
	}
	if detLog.failOn != "" && strings.Contains(q, detLog.failOn) {
		return nil, errors.New("scripted failure")
	}
	return driver.ResultNoRows, nil
}

func (c *detConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	detLog.add(q)
	return detLog.reply(q), nil
}

type detRecorder struct {
	mu     sync.Mutex
	calls  []string
	failOn string

	// cancelOn stands in for the caller's deadline expiring *during* a
	// statement — the failure mode the MULTI_USER repair exists for, and the
	// one that cannot be reproduced by cancelling before the call: the
	// statement that put the database into SINGLE_USER has to have succeeded
	// first, or there is nothing to repair. The first statement containing it
	// cancels cancel and then fails with the context's error.
	cancelOn string
	cancel   context.CancelFunc
	// props and files answer DBCC CHECKPRIMARYFILE's option 2 and option 3.
	props [][]driver.Value
	files [][]driver.Value
}

func (l *detRecorder) add(q string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, q)
}

// cancelIfMatched cancels the operation's context if q is the statement the
// test nominated, and reports whether it did. It fires once: the repair that
// follows must be seen to run despite the cancellation, not be cancelled by a
// second match of its own.
func (l *detRecorder) cancelIfMatched(q string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cancelOn == "" || !strings.Contains(q, l.cancelOn) {
		return false
	}
	l.cancelOn = ""
	l.cancel()
	return true
}

func (l *detRecorder) reply(q string) *detRows {
	l.mu.Lock()
	defer l.mu.Unlock()
	if strings.Contains(q, ", 3)") {
		return &detRows{cols: []string{"status", "fileid", "name", "filename"}, rows: l.files}
	}
	return &detRows{cols: []string{"property", "value"}, rows: l.props}
}

func (l *detRecorder) statements() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

// only returns the one recorded statement containing needle, failing the test
// when none or several do — an assertion made against "whatever ran last"
// passes just as happily on a statement that was never issued.
func (l *detRecorder) only(t *testing.T, needle string) string {
	t.Helper()
	var found []string
	for _, s := range l.statements() {
		if strings.Contains(s, needle) {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d statements contain %q, want exactly 1: %v", len(found), needle, l.statements())
	}
	return found[0]
}

type detRows struct {
	cols []string
	rows [][]driver.Value
	next int
}

func (r *detRows) Columns() []string { return r.cols }
func (r *detRows) Close() error      { return nil }
func (r *detRows) Next(dest []driver.Value) error {
	if r.next >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.next])
	r.next++
	return nil
}

var detLog detRecorder

func init() { sql.Register("detach", detDriver{}) }

// detServer returns a Server over the recording driver, with the log reset.
func detServer(t *testing.T) *Server {
	t.Helper()
	pool, err := sql.Open("detach", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	detLog.mu.Lock()
	detLog.calls, detLog.failOn, detLog.props, detLog.files = nil, "", nil, nil
	detLog.cancelOn, detLog.cancel = "", nil
	detLog.mu.Unlock()
	return &Server{db: pool}
}

// detCancelOn arms the recorder to cancel ctx when the statement containing
// needle runs, and returns the context the operation under test should use.
func detCancelOn(t *testing.T, needle string) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	detLog.mu.Lock()
	detLog.cancelOn, detLog.cancel = needle, cancel
	detLog.mu.Unlock()
	return ctx
}

// -- detach ------------------------------------------------------------------

// TestDetachFlagsAreTheInverseOfTheProceduresParameters is the whole reason
// DetachOptions exists rather than three bools passed straight through.
// sp_detach_db's two flags are negatives of what a user is asked — @skipchecks
// is the opposite of "Update Statistics", @keepfulltextindexfile the opposite
// of "drop the full-text index files" — and both are 'true'/'false' *text*,
// which a 0/1 silently is not. Getting either inverted skips exactly the work
// the caller asked for, and nothing about the resulting detach looks wrong.
func TestDetachFlagsAreTheInverseOfTheProceduresParameters(t *testing.T) {
	cases := []struct {
		name string
		opts DetachOptions
		want []string
	}{
		{"zero value: skip the statistics update, keep the full-text files",
			DetachOptions{},
			[]string{"@skipchecks = 'true'", "@keepfulltextindexfile = 'true'"}},
		{"update statistics: skipchecks goes false",
			DetachOptions{UpdateStatistics: true},
			[]string{"@skipchecks = 'false'", "@keepfulltextindexfile = 'true'"}},
		{"drop the full-text files: keepfulltextindexfile goes false",
			DetachOptions{DropFullTextIndexFile: true},
			[]string{"@skipchecks = 'true'", "@keepfulltextindexfile = 'false'"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := detServer(t)
			if err := s.DetachDatabaseContext(context.Background(), "appdb", c.opts); err != nil {
				t.Fatalf("DetachDatabaseContext: %v", err)
			}
			stmt := detLog.only(t, "sp_detach_db")
			for _, want := range c.want {
				if !strings.Contains(stmt, want) {
					t.Errorf("statement %q does not contain %q", stmt, want)
				}
			}
			if !strings.Contains(stmt, `@dbname = N'appdb'`) {
				t.Errorf("statement %q does not name the database", stmt)
			}
		})
	}
}

// TestDetachDropConnectionsSetsSingleUserFirst. A database with any other
// connection open refuses to detach, so the option has to run before the
// procedure — after it, there is no database left to alter.
func TestDetachDropConnectionsSetsSingleUserFirst(t *testing.T) {
	s := detServer(t)
	if err := s.DetachDatabaseContext(context.Background(), "appdb", DetachOptions{DropConnections: true}); err != nil {
		t.Fatalf("DetachDatabaseContext: %v", err)
	}
	stmts := detLog.statements()
	if len(stmts) != 2 {
		t.Fatalf("got %d statements, want the single-user alter and the detach: %v", len(stmts), stmts)
	}
	if !strings.Contains(stmts[0], "SET SINGLE_USER WITH ROLLBACK IMMEDIATE") {
		t.Errorf("first statement is %q, want the single-user alter", stmts[0])
	}
	if !strings.Contains(stmts[1], "sp_detach_db") {
		t.Errorf("second statement is %q, want the detach", stmts[1])
	}
}

// TestAFailedDetachIsPutBackToMultiUser. SINGLE_USER blocks every other
// login, so a detach that dropped the connections and then failed would leave
// the database unusable by anyone but the caller — for a reason the caller
// never asked for. Same contract as RenameDatabaseContext's force.
func TestAFailedDetachIsPutBackToMultiUser(t *testing.T) {
	s := detServer(t)
	detLog.mu.Lock()
	detLog.failOn = "sp_detach_db"
	detLog.mu.Unlock()

	err := s.DetachDatabaseContext(context.Background(), "appdb", DetachOptions{DropConnections: true})
	if err == nil {
		t.Fatal("a failing detach returned no error")
	}
	stmts := detLog.statements()
	last := stmts[len(stmts)-1]
	if !strings.Contains(last, "SET MULTI_USER") {
		t.Errorf("last statement after a failed detach is %q, want the database put back to MULTI_USER: %v", last, stmts)
	}
}

// TestAFailedDetachIsPutBackToMultiUserEvenWhenTheContextIsGone. The repair
// above is unreachable if it runs on the caller's own context, because the
// dominant way a detach fails is the deadline expiring during it: SET
// SINGLE_USER WITH ROLLBACK IMMEDIATE waits out the rollback of every
// transaction it killed, and the caller's budget is spent by the time
// sp_detach_db returns. Issued on the dead context, the repair never reaches
// the server and the database stays locked to one login.
func TestAFailedDetachIsPutBackToMultiUserEvenWhenTheContextIsGone(t *testing.T) {
	s := detServer(t)
	ctx := detCancelOn(t, "sp_detach_db")

	err := s.DetachDatabaseContext(ctx, "appdb", DetachOptions{DropConnections: true})
	if err == nil {
		t.Fatal("a detach whose context expired returned no error")
	}
	stmts := detLog.statements()
	last := stmts[len(stmts)-1]
	if !strings.Contains(last, "SET MULTI_USER") {
		t.Errorf("last statement after a cancelled detach is %q, want the database put back to MULTI_USER: %v", last, stmts)
	}
}

// TestASuccessfulDetachDoesNotTryToAlterTheDatabaseAfterwards. The database
// is gone from the instance by then, so a MULTI_USER alter would fail with
// "not found" — turning a detach that worked into a reported failure.
func TestASuccessfulDetachDoesNotTryToAlterTheDatabaseAfterwards(t *testing.T) {
	s := detServer(t)
	if err := s.DetachDatabaseContext(context.Background(), "appdb", DetachOptions{DropConnections: true}); err != nil {
		t.Fatalf("DetachDatabaseContext: %v", err)
	}
	for _, stmt := range detLog.statements() {
		if strings.Contains(stmt, "SET MULTI_USER") {
			t.Errorf("a successful detach issued %q against a database that no longer exists", stmt)
		}
	}
}

func TestDetachRequiresAName(t *testing.T) {
	s := detServer(t)
	if err := s.DetachDatabaseContext(context.Background(), "", DetachOptions{}); err == nil {
		t.Error("detaching a database with no name returned no error")
	}
	if n := len(detLog.statements()); n != 0 {
		t.Errorf("%d statements ran for a detach with no name, want none", n)
	}
}

// -- attach ------------------------------------------------------------------

func TestBuildAttachStatement(t *testing.T) {
	cases := []struct {
		name string
		spec AttachSpec
		want string
	}{
		{
			name: "primary file only",
			spec: AttachSpec{Name: "appdb", Files: []string{`C:\Data\appdb.mdf`}},
			want: "CREATE DATABASE [appdb] ON\n" +
				"  (FILENAME = N'C:\\Data\\appdb.mdf')\n" +
				"FOR ATTACH",
		},
		{
			name: "data, secondary and log, in the order given",
			spec: AttachSpec{Name: "appdb", Files: []string{
				`C:\Data\appdb.mdf`, `C:\Data\appdb_2.ndf`, `C:\Log\appdb_log.ldf`}},
			want: "CREATE DATABASE [appdb] ON\n" +
				"  (FILENAME = N'C:\\Data\\appdb.mdf'),\n" +
				"  (FILENAME = N'C:\\Data\\appdb_2.ndf'),\n" +
				"  (FILENAME = N'C:\\Log\\appdb_log.ldf')\n" +
				"FOR ATTACH",
		},
		{
			name: "no log file: rebuild one",
			spec: AttachSpec{Name: "appdb", Files: []string{`/var/opt/mssql/data/appdb.mdf`}, RebuildLog: true},
			want: "CREATE DATABASE [appdb] ON\n" +
				"  (FILENAME = N'/var/opt/mssql/data/appdb.mdf')\n" +
				"FOR ATTACH_REBUILD_LOG",
		},
		{
			name: "a bracket in the name and a quote in the path are both escaped",
			spec: AttachSpec{Name: "od]d", Files: []string{`C:\it's\a.mdf`}},
			want: "CREATE DATABASE [od]]d] ON\n" +
				"  (FILENAME = N'C:\\it''s\\a.mdf')\n" +
				"FOR ATTACH",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := buildAttachStatement(c.spec); got != c.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, c.want)
			}
		})
	}
}

// TestAttachSetsTheOwnerAfterCreating. ALTER AUTHORIZATION cannot name a
// database that is not attached yet, so the order is not cosmetic.
func TestAttachSetsTheOwnerAfterCreating(t *testing.T) {
	s := detServer(t)
	err := s.AttachDatabaseContext(context.Background(), AttachSpec{
		Name: "appdb", Files: []string{`C:\Data\appdb.mdf`}, Owner: "sa",
	})
	if err != nil {
		t.Fatalf("AttachDatabaseContext: %v", err)
	}
	stmts := detLog.statements()
	if len(stmts) != 2 {
		t.Fatalf("got %d statements, want the attach and the ownership change: %v", len(stmts), stmts)
	}
	if !strings.HasPrefix(stmts[0], "CREATE DATABASE") {
		t.Errorf("first statement is %q, want the attach", stmts[0])
	}
	if want := "ALTER AUTHORIZATION ON DATABASE::[appdb] TO [sa]"; stmts[1] != want {
		t.Errorf("second statement is %q, want %q", stmts[1], want)
	}
}

// TestAttachWithNoOwnerLeavesOwnershipAlone. CREATE DATABASE ... FOR ATTACH
// already leaves the attaching login as owner; issuing a redundant ALTER
// AUTHORIZATION would fail for a login that may not transfer ownership.
func TestAttachWithNoOwnerLeavesOwnershipAlone(t *testing.T) {
	s := detServer(t)
	if err := s.AttachDatabaseContext(context.Background(), AttachSpec{
		Name: "appdb", Files: []string{`C:\Data\appdb.mdf`},
	}); err != nil {
		t.Fatalf("AttachDatabaseContext: %v", err)
	}
	for _, stmt := range detLog.statements() {
		if strings.Contains(stmt, "ALTER AUTHORIZATION") {
			t.Errorf("an attach with no owner issued %q", stmt)
		}
	}
}

func TestAttachRequiresANameAndAtLeastOneFile(t *testing.T) {
	s := detServer(t)
	if err := s.AttachDatabaseContext(context.Background(), AttachSpec{Files: []string{"a.mdf"}}); err == nil {
		t.Error("attaching with no name returned no error")
	}
	if err := s.AttachDatabaseContext(context.Background(), AttachSpec{Name: "appdb"}); err == nil {
		t.Error("attaching with no files returned no error")
	}
	if n := len(detLog.statements()); n != 0 {
		t.Errorf("%d statements ran for an invalid attach, want none", n)
	}
}

// -- scripting ---------------------------------------------------------------

// TestDetachAndAttachAreScriptable. Both are reachable from a Script Changes
// button, and a write that reaches the server under WithScript is the bug
// that button exists to prevent.
func TestDetachAndAttachAreScriptable(t *testing.T) {
	s := detServer(t)
	ctx, script := WithScript(context.Background())

	if err := s.DetachDatabaseContext(ctx, "appdb", DetachOptions{DropConnections: true, UpdateStatistics: true}); err != nil {
		t.Fatalf("scripted detach: %v", err)
	}
	if err := s.AttachDatabaseContext(ctx, AttachSpec{
		Name: "appdb2", Files: []string{`C:\Data\appdb.mdf`}, Owner: "sa",
	}); err != nil {
		t.Fatalf("scripted attach: %v", err)
	}
	if n := len(detLog.statements()); n != 0 {
		t.Fatalf("%d statements reached the server under WithScript, want none: %v", n, detLog.statements())
	}
	joined := strings.Join(script.Statements, "\n")
	for _, want := range []string{
		"SET SINGLE_USER WITH ROLLBACK IMMEDIATE",
		"sp_detach_db",
		"@skipchecks = 'false'",
		"CREATE DATABASE [appdb2] ON",
		"FOR ATTACH",
		"ALTER AUTHORIZATION ON DATABASE::[appdb2] TO [sa]",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the script does not contain %q:\n%s", want, joined)
		}
	}
}

// -- reading a detached file -------------------------------------------------

// TestDetachedDatabaseInfoReadsTheNameAndEveryFile. The two DBCC options come
// back in different shapes — property/value pairs and one row per file — and
// are scanned by two different functions. Answering both with the same rows
// would let the two be swapped unnoticed, which is why the test driver
// distinguishes them.
func TestDetachedDatabaseInfoReadsTheNameAndEveryFile(t *testing.T) {
	s := detServer(t)
	detLog.mu.Lock()
	detLog.props = [][]driver.Value{
		{"Database name", "appdb"},
		{"Database version", "998"},
		{"Collation", "872468488"},
	}
	// Status as a live instance reports it: 2 for a data file, 66 for the log.
	detLog.files = [][]driver.Value{
		{int64(2), int64(1), "appdb", `C:\Data\appdb.mdf`},
		// Deliberately not named .ldf: the extension fallback below would
		// flag it anyway, and then this would pass with the status bit
		// ignored entirely.
		{int64(66), int64(2), "appdb_log", `C:\Log\appdb_log.translog`},
		{int64(2), int64(3), "appdb_2", `C:\Data\appdb_2.ndf`},
	}
	detLog.mu.Unlock()

	d, err := s.DetachedDatabaseInfoContext(context.Background(), `C:\Data\appdb.mdf`)
	if err != nil {
		t.Fatalf("DetachedDatabaseInfoContext: %v", err)
	}
	if d.Name != "appdb" {
		t.Errorf("Name = %q, want appdb", d.Name)
	}
	if d.Version != "998" || d.Collation != "872468488" {
		t.Errorf("Version/Collation = %q/%q, want 998/872468488", d.Version, d.Collation)
	}
	if len(d.Files) != 3 {
		t.Fatalf("got %d files, want 3", len(d.Files))
	}
	// The log is the second row on purpose: a scan that took the last row, or
	// assumed the log sorts last, passes on a two-file database.
	if !d.Files[1].IsLog {
		t.Error("the log file was not flagged as one")
	}
	if d.Files[0].IsLog || d.Files[2].IsLog {
		t.Error("a data file was flagged as the log")
	}
	if got := d.Files[2]; got.FileID != 3 || got.Name != "appdb_2" || got.PhysicalName != `C:\Data\appdb_2.ndf` {
		t.Errorf("third file = %+v, want file 3 appdb_2 at C:\\Data\\appdb_2.ndf", got)
	}
	if len(d.LogFiles()) != 1 || len(d.DataFiles()) != 2 {
		t.Errorf("LogFiles/DataFiles split %d/%d, want 1/2", len(d.LogFiles()), len(d.DataFiles()))
	}
	// Both options must actually have been asked for — a Name filled in by
	// the file read would look identical here.
	detLog.only(t, ", 2) WITH NO_INFOMSGS")
	detLog.only(t, ", 3) WITH NO_INFOMSGS")
}

// TestDetachedFilesFallBackToTheExtension. The status column is undocumented.
// If its encoding ever changes, no file comes back flagged as the log — and a
// caller reading "no log file" reaches for FOR ATTACH_REBUILD_LOG, throwing
// away a log that was there all along.
func TestDetachedFilesFallBackToTheExtension(t *testing.T) {
	s := detServer(t)
	detLog.mu.Lock()
	detLog.props = [][]driver.Value{{"Database name", "appdb"}}
	detLog.files = [][]driver.Value{
		{int64(2), int64(1), "appdb", `C:\Data\appdb.mdf`},
		{int64(2), int64(2), "appdb_log", `C:\Log\appdb_log.LDF`},
	}
	detLog.mu.Unlock()

	d, err := s.DetachedDatabaseInfoContext(context.Background(), `C:\Data\appdb.mdf`)
	if err != nil {
		t.Fatalf("DetachedDatabaseInfoContext: %v", err)
	}
	if len(d.LogFiles()) != 1 || d.LogFiles()[0].Name != "appdb_log" {
		t.Errorf("log files = %v, want the .LDF recognised by its extension", d.LogFiles())
	}
	if len(d.DataFiles()) != 1 {
		t.Errorf("data files = %d, want the .mdf alone", len(d.DataFiles()))
	}
}

// TestTheExtensionFallbackDoesNotOverrideTheStatusBit. A database whose log
// is not named .ldf is legal; so is a *data* file named .ldf. The fallback
// must only run when the status bit found nothing at all.
func TestTheExtensionFallbackDoesNotOverrideTheStatusBit(t *testing.T) {
	files := []*DetachedFile{
		{Name: "d", PhysicalName: `C:\Data\odd.ldf`},
		{Name: "l", PhysicalName: `C:\Log\odd.log_file`, IsLog: true},
	}
	markLogByExtension(files)
	if files[0].IsLog {
		t.Error("a data file named .ldf was reflagged as the log even though the status bit had already answered")
	}
}

func TestDetachedDatabaseInfoRequiresAPath(t *testing.T) {
	s := detServer(t)
	if _, err := s.DetachedDatabaseInfoContext(context.Background(), "   "); err == nil {
		t.Error("an empty primary file path returned no error")
	}
	if n := len(detLog.statements()); n != 0 {
		t.Errorf("%d statements ran for an empty path, want none", n)
	}
}

// TestPrimaryFileIsTheOneWithFileID1. DBCC CHECKPRIMARYFILE's row order is
// undocumented, so a caller that takes the first data file it sees rewrites a
// secondary file's path with the primary's — sending the attach to the old
// location for the file the user just browsed to a new one.
func TestPrimaryFileIsTheOneWithFileID1(t *testing.T) {
	d := &DetachedDatabase{Files: []*DetachedFile{
		{FileID: 3, Name: "appdb_2", PhysicalName: `C:\Data\appdb_2.ndf`},
		{FileID: 2, Name: "appdb_log", PhysicalName: `C:\Log\appdb_log.ldf`, IsLog: true},
		{FileID: 1, Name: "appdb", PhysicalName: `C:\Data\appdb.mdf`},
	}}
	got := d.PrimaryFile()
	if got == nil || got.Name != "appdb" {
		t.Fatalf("PrimaryFile = %+v, want file 1 appdb", got)
	}
}

// TestPrimaryFileFallsBackWhenNoFileIDCameBack. A NULL fileid column scans as
// 0, and answering nil there reads as "this database has no primary file".
func TestPrimaryFileFallsBackWhenNoFileIDCameBack(t *testing.T) {
	d := &DetachedDatabase{Files: []*DetachedFile{
		{Name: "appdb_log", PhysicalName: `C:\Log\appdb_log.ldf`, IsLog: true},
		{Name: "appdb", PhysicalName: `C:\Data\appdb.mdf`},
	}}
	got := d.PrimaryFile()
	if got == nil || got.Name != "appdb" {
		t.Fatalf("PrimaryFile = %+v, want the first data file", got)
	}
	if (&DetachedDatabase{}).PrimaryFile() != nil {
		t.Error("PrimaryFile on an empty file list returned a file")
	}
}
