//go:build livedb

// Live verification of Detach / Attach. Every statement here is DDL or a
// system procedure a unit test can only pin the text of — and the one read,
// DBCC CHECKPRIMARYFILE, is undocumented, so its result shape and its status
// encoding are known only by asking a server.
//
//	go test -tags livedb . -run TestLiveDetachAttach -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
// Skipped entirely without -livedb.
package gosmo

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// detLiveName is the throwaway database, and detLiveDir the directory its
// files are created in — under the instance's own default data path, so the
// service account can certainly write there.
const detLiveName = "zz_gossms_detach_probe"

// detLiveSetup creates a three-file database (primary, a second filegroup,
// and the log) and returns the server, its file paths, and a dropper that
// works whether the database is attached or not.
func detLiveSetup(t *testing.T, db *sql.DB, ctx context.Context) (*Server, []string, func()) {
	t.Helper()
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	dir := srv.Info().DefaultDataPath
	if dir == "" {
		t.Fatal("the instance reported no default data path")
	}
	sep := "\\"
	if strings.HasPrefix(dir, "/") {
		sep = "/"
	}
	dir = strings.TrimRight(dir, "\\/") + sep
	files := []string{dir + detLiveName + ".mdf", dir + detLiveName + "_2.ndf", dir + detLiveName + "_log.ldf"}

	drop := func() {
		c := context.Background()
		// Attached: drop it, which deletes the files. Detached: the drop
		// fails and the files are already only on disk, so re-attach first.
		if _, err := db.ExecContext(c, "SELECT 1 FROM sys.databases WHERE name = N'"+detLiveName+"'"); err == nil {
			var n int
			if db.QueryRowContext(c, "SELECT COUNT(*) FROM sys.databases WHERE name = N'"+detLiveName+"'").Scan(&n) == nil && n == 0 {
				_ = srv.AttachDatabaseContext(c, AttachSpec{Name: detLiveName, Files: files})
			}
		}
		db.ExecContext(c, "ALTER DATABASE ["+detLiveName+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE")
		db.ExecContext(c, "DROP DATABASE ["+detLiveName+"]")
	}
	drop()
	create := "CREATE DATABASE [" + detLiveName + "]\n" +
		"ON PRIMARY (NAME = " + detLiveName + ", FILENAME = N'" + files[0] + "', SIZE = 8MB),\n" +
		"   FILEGROUP fg2 (NAME = " + detLiveName + "_2, FILENAME = N'" + files[1] + "', SIZE = 8MB)\n" +
		"LOG ON (NAME = " + detLiveName + "_log, FILENAME = N'" + files[2] + "', SIZE = 8MB)"
	if _, err := db.ExecContext(ctx, create); err != nil {
		t.Fatalf("create throwaway database: %v", err)
	}
	return srv, files, drop
}

func databaseExists(t *testing.T, db *sql.DB, ctx context.Context, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sys.databases WHERE name = @p1", name).Scan(&n); err != nil {
		t.Fatalf("sys.databases: %v", err)
	}
	return n > 0
}

// TestLiveDetachAttachRoundTrip is the whole feature end to end: detach a
// three-file database, read its file list back out of the detached primary
// file, and attach it again under a different name from that list alone.
//
// Attaching under a different name is the case that matters: nothing inside
// the files ties them to a name, and a dialog that quietly reused the
// detached one would look right in every other test.
func TestLiveDetachAttachRoundTrip(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv, files, drop := detLiveSetup(t, db, ctx)
	defer drop()

	if err := srv.DetachDatabaseContext(ctx, detLiveName, DetachOptions{DropConnections: true}); err != nil {
		t.Fatalf("DetachDatabaseContext: %v", err)
	}
	if databaseExists(t, db, ctx, detLiveName) {
		t.Fatal("the database is still on the instance after a detach that reported success")
	}

	info, err := srv.DetachedDatabaseInfoContext(ctx, files[0])
	if err != nil {
		t.Fatalf("DetachedDatabaseInfoContext: %v", err)
	}
	if info.Name != detLiveName {
		t.Errorf("detached name = %q, want %q", info.Name, detLiveName)
	}
	if len(info.Files) != 3 {
		t.Fatalf("got %d files, want the primary, the secondary and the log: %+v", len(info.Files), info.Files)
	}
	if len(info.LogFiles()) != 1 {
		t.Errorf("%d files flagged as the log, want 1 — the status bit is what tells them apart: %+v",
			len(info.LogFiles()), info.Files)
	}
	if got := info.LogFiles()[0].PhysicalName; !strings.EqualFold(got, files[2]) {
		t.Errorf("log file = %q, want %q", got, files[2])
	}
	// PrimaryFile has to be the .mdf whatever order DBCC listed the files in.
	// Only a server says what that order is; the row order is undocumented,
	// and a caller taking the first data file it sees is betting on it.
	pf := info.PrimaryFile()
	if pf == nil || !strings.EqualFold(pf.PhysicalName, files[0]) {
		t.Errorf("PrimaryFile = %+v, want the primary at %s; DBCC returned %+v", pf, files[0], info.Files)
	}
	if pf != nil && pf.FileID != 1 {
		t.Errorf("the primary came back as file_id %d, want 1", pf.FileID)
	}
	for i, f := range info.Files {
		t.Logf("DBCC row %d: file_id=%d name=%s log=%v %s", i, f.FileID, f.Name, f.IsLog, f.PhysicalName)
	}

	// Every file must come back with the path it was detached from — that is
	// the whole point of reading them rather than asking the user to type them.
	for i, want := range files {
		found := false
		for _, f := range info.Files {
			if strings.EqualFold(f.PhysicalName, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("file %d (%s) is missing from the detached file list: %+v", i, want, info.Files)
		}
	}

	paths := make([]string, 0, len(info.Files))
	for _, f := range info.Files {
		paths = append(paths, f.PhysicalName)
	}
	const attachedAs = detLiveName + "_2"
	if err := srv.AttachDatabaseContext(ctx, AttachSpec{Name: attachedAs, Files: paths}); err != nil {
		t.Fatalf("AttachDatabaseContext: %v", err)
	}
	defer func() {
		c := context.Background()
		db.ExecContext(c, "ALTER DATABASE ["+attachedAs+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE")
		db.ExecContext(c, "DROP DATABASE ["+attachedAs+"]")
	}()

	if !databaseExists(t, db, ctx, attachedAs) {
		t.Fatal("the database is not on the instance after an attach that reported success")
	}
	var state string
	if err := db.QueryRowContext(ctx, "SELECT state_desc FROM sys.databases WHERE name = @p1", attachedAs).Scan(&state); err != nil {
		t.Fatalf("state of the attached database: %v", err)
	}
	if state != "ONLINE" {
		t.Errorf("the attached database is %s, want ONLINE", state)
	}
	// All three files came along, not just the primary.
	var n int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sys.master_files WHERE database_id = DB_ID(@p1)", attachedAs).Scan(&n); err != nil {
		t.Fatalf("master_files: %v", err)
	}
	if n != 3 {
		t.Errorf("the attached database has %d files, want 3", n)
	}
}

// TestLiveDetachUpdateStatisticsIsAccepted runs the other side of the
// inverted flag against a real sp_detach_db. A unit test pins the text
// '@skipchecks = false'; only the server says the procedure takes it.
func TestLiveDetachUpdateStatisticsIsAccepted(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv, files, drop := detLiveSetup(t, db, ctx)
	defer drop()

	if err := srv.DetachDatabaseContext(ctx, detLiveName, DetachOptions{
		DropConnections: true, UpdateStatistics: true,
	}); err != nil {
		t.Fatalf("detach with UpdateStatistics: %v", err)
	}
	if databaseExists(t, db, ctx, detLiveName) {
		t.Fatal("the database is still attached")
	}
	if err := srv.AttachDatabaseContext(ctx, AttachSpec{Name: detLiveName, Files: files}); err != nil {
		t.Fatalf("re-attach: %v", err)
	}
}

// TestLiveDetachThatFailsAfterSingleUserPutsTheDatabaseBack is the failure
// path the restore exists for, and it is not hypothetical: SET SINGLE_USER
// succeeds, sp_detach_db then refuses, and without the restore the database
// is left single-user — unusable by anyone but the caller, for a reason the
// caller never asked for.
//
// Provoked with a database snapshot, which SQL Server refuses to detach past
// ("Cannot DETACH the database while the database snapshot ... refers to
// it"). Verified by hand first: the bare sequence leaves user_access_desc at
// SINGLE_USER.
func TestLiveDetachThatFailsAfterSingleUserPutsTheDatabaseBack(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv, _, drop := detLiveSetup(t, db, ctx)
	defer drop()

	// A snapshot must name every data file of its source, not just the
	// primary — a partial list is refused with error 5127.
	snapshot := detLiveName + "_ss"
	create := "CREATE DATABASE [" + snapshot + "] ON " +
		strings.Join(snapshotClauses(t, db, ctx, detLiveName), ", ") +
		" AS SNAPSHOT OF [" + detLiveName + "]"
	if _, err := db.ExecContext(ctx, create); err != nil {
		t.Skipf("this edition cannot create a database snapshot: %v", err)
	}
	defer db.ExecContext(context.Background(), "DROP DATABASE ["+snapshot+"]")

	if err := srv.DetachDatabaseContext(ctx, detLiveName, DetachOptions{DropConnections: true}); err == nil {
		t.Fatal("detaching a database with a snapshot on it succeeded, so there is no failure path to check")
	}
	var access string
	if err := db.QueryRowContext(ctx,
		"SELECT user_access_desc FROM sys.databases WHERE name = @p1", detLiveName).Scan(&access); err != nil {
		t.Fatalf("user_access_desc: %v", err)
	}
	if access != "MULTI_USER" {
		t.Errorf("the database is %s after a detach that failed, want it put back to MULTI_USER", access)
	}
}

// snapshotClauses builds one (NAME = ..., FILENAME = ...) clause per data
// file of name, with each snapshot file beside its source under a .ss
// extension.
func snapshotClauses(t *testing.T, db *sql.DB, ctx context.Context, name string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		"SELECT name, physical_name FROM sys.master_files WHERE database_id = DB_ID(@p1) AND type = 0 ORDER BY file_id",
		name)
	if err != nil {
		t.Fatalf("data files of %s: %v", name, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var logical, physical string
		if err := rows.Scan(&logical, &physical); err != nil {
			t.Fatalf("data files of %s: %v", name, err)
		}
		ext := strings.LastIndex(physical, ".")
		out = append(out, "(NAME = ["+logical+"], FILENAME = N'"+physical[:ext]+".ss')")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("data files of %s: %v", name, err)
	}
	if len(out) == 0 {
		t.Fatalf("%s reported no data files", name)
	}
	return out
}
