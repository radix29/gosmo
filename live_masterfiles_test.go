//go:build livedb

// Live verification of Server.DatabaseFilesContext, the server-scoped file
// read. Its reason for existing is a state a unit test cannot produce: an
// OFFLINE database, whose files Database.FilesContext cannot see at all
// because its read goes through a USE.
//
//	go test -tags livedb . -run TestLiveDatabaseFiles -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Uses the throwaway three-file database detLiveSetup builds; touches
// nothing else.
package gosmo

import (
	"strings"
	"testing"
)

// TestLiveDatabaseFilesAgreesWithTheDatabaseScopedRead pins the server-scoped
// read against the one it stands in for, field by field, while the database
// is ONLINE and both can answer.
func TestLiveDatabaseFilesAgreesWithTheDatabaseScopedRead(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv, _, drop := detLiveSetup(t, db, ctx)
	defer drop()

	dbase, err := srv.DatabaseByNameContext(ctx, detLiveName)
	if err != nil {
		t.Fatalf("DatabaseByNameContext: %v", err)
	}
	want, err := dbase.FilesContext(ctx)
	if err != nil {
		t.Fatalf("FilesContext: %v", err)
	}
	got, err := srv.DatabaseFilesContext(ctx, detLiveName)
	if err != nil {
		t.Fatalf("DatabaseFilesContext: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d files, want %d", len(got), len(want))
	}
	for i := range want {
		w, g := want[i], got[i]
		if g.FileID != w.FileID || g.Name != w.Name || g.PhysicalName != w.PhysicalName ||
			g.Type != w.Type || g.State != w.State || g.SizeKB != w.SizeKB ||
			g.MaxSizeKB != w.MaxSizeKB || g.GrowthKB != w.GrowthKB ||
			g.GrowthPercent != w.GrowthPercent || g.IsPercentGrowth != w.IsPercentGrowth {
			t.Errorf("file %d differs:\n master_files:   %+v\n database_files: %+v", i, *g, *w)
		}
		// The one field the server catalog cannot answer, documented as such.
		if g.FileGroup != "" {
			t.Errorf("file %d: FileGroup = %q, want empty — sys.filegroups is not joinable from sys.master_files", i, g.FileGroup)
		}
	}
}

// TestLiveDatabaseFilesAnswersForAnOfflineDatabase is the case the method
// exists for: the database-scoped read fails, the server-scoped one still
// reports every path.
func TestLiveDatabaseFilesAnswersForAnOfflineDatabase(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv, files, drop := detLiveSetup(t, db, ctx)
	defer drop()

	if _, err := db.ExecContext(ctx, "ALTER DATABASE ["+detLiveName+"] SET OFFLINE WITH ROLLBACK IMMEDIATE"); err != nil {
		t.Fatalf("SET OFFLINE: %v", err)
	}
	defer db.ExecContext(ctx, "ALTER DATABASE ["+detLiveName+"] SET ONLINE")

	dbase, err := srv.DatabaseByNameContext(ctx, detLiveName)
	if err != nil {
		t.Fatalf("DatabaseByNameContext: %v", err)
	}
	if got := dbase.State(); got != "OFFLINE" {
		t.Fatalf("state = %q, want OFFLINE", got)
	}
	if _, err := dbase.FilesContext(ctx); err == nil {
		t.Error("FilesContext succeeded on an OFFLINE database — if the USE ever stops being required, this method has no reason to exist")
	}

	got, err := srv.DatabaseFilesContext(ctx, detLiveName)
	if err != nil {
		t.Fatalf("DatabaseFilesContext on an OFFLINE database: %v", err)
	}
	if len(got) != len(files) {
		t.Fatalf("got %d files, want %d", len(got), len(files))
	}
	for _, want := range files {
		found := false
		for _, f := range got {
			if strings.EqualFold(f.PhysicalName, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is missing from the file list: %+v", want, got)
		}
	}
}

// TestLiveDatabaseFilesForAnUnknownDatabaseIsEmpty pins the listing
// convention: absence is no rows, not an error.
func TestLiveDatabaseFilesForAnUnknownDatabaseIsEmpty(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	got, err := srv.DatabaseFilesContext(ctx, "zz_gossms_no_such_database")
	if err != nil {
		t.Fatalf("DatabaseFilesContext: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d files for a database that does not exist: %+v", len(got), got)
	}
}
