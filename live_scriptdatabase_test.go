//go:build livedb

// Live verification that ScriptDatabase produces a real script from a
// bare Server.DatabaseRef(name) handle.
//
// The handle carries no metadata by design, and ScriptDatabase used to render
// it anyway — "SET RECOVERY ;" and "COMPATIBILITY_LEVEL = 0", neither of them
// valid T-SQL. Only a live server can settle both halves: that the refresh
// returns the same metadata DatabaseByName would have, and that the
// script the two paths produce is byte-for-byte the same.
//
//	go test -tags livedb . -run TestLiveScriptDatabase -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
package gosmo

import (
	"errors"
	"strings"
	"testing"
)

func TestLiveScriptDatabaseFromABareHandle(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const name = "gosmo_scriptdb_live"
	full, drop := liveScratchDB(t, db, ctx, name)
	defer drop()

	srv := full.server
	if _, err := db.ExecContext(ctx, "ALTER DATABASE ["+name+"] SET RECOVERY BULK_LOGGED"); err != nil {
		t.Fatalf("set recovery: %v", err)
	}
	// Re-read: full was fetched before the ALTER, so its cached recovery
	// model is now stale — which is itself the reason the bare handle has to
	// read rather than guess.
	var err error
	full, err = srv.DatabaseByName(ctx, name)
	if err != nil {
		t.Fatalf("DatabaseByName: %v", err)
	}

	opts := DefaultScriptOptions()
	opts.IncludeHeaders = false

	wantScript, err := NewScripter(full, opts).ScriptDatabase(ctx)
	if err != nil {
		t.Fatalf("script from a fetched database: %v", err)
	}
	bare, err := NewScripter(srv.DatabaseRef(name), opts).ScriptDatabase(ctx)
	if err != nil {
		t.Fatalf("script from a bare handle: %v", err)
	}
	if bare != wantScript {
		t.Errorf("bare handle script differs:\n--- bare ---\n%s\n--- fetched ---\n%s", bare, wantScript)
	}
	for _, want := range []string{
		"CREATE DATABASE [" + name + "]",
		"SET RECOVERY BULK_LOGGED;",
		"SET COMPATIBILITY_LEVEL = ",
	} {
		if !strings.Contains(bare, want) {
			t.Errorf("script missing %q:\n%s", want, bare)
		}
	}
	// The shapes the bug produced. Neither parses.
	for _, bad := range []string{"SET RECOVERY ;", "COMPATIBILITY_LEVEL = 0;"} {
		if strings.Contains(bare, bad) {
			t.Errorf("script contains %q:\n%s", bad, bare)
		}
	}

	// And it has to actually run. The script drops straight back in against a
	// server that no longer has the database.
	if _, err := db.ExecContext(ctx, "ALTER DATABASE ["+name+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE"); err != nil {
		t.Fatalf("pre-drop prep: %v", err)
	}
	if _, err := db.ExecContext(ctx, "DROP DATABASE ["+name+"]"); err != nil {
		t.Fatalf("drop before replay: %v", err)
	}
	for _, batch := range strings.Split(bare, "\nGO") {
		if strings.TrimSpace(batch) == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, batch); err != nil {
			t.Fatalf("replaying %.60q: %v", strings.TrimSpace(batch), err)
		}
	}
	replayed, err := srv.DatabaseByName(ctx, name)
	if err != nil {
		t.Fatalf("database after replay: %v", err)
	}
	if replayed.RecoveryModel != RecoveryModelBulkLogged {
		t.Errorf("replayed recovery model = %q, want BULK_LOGGED", replayed.RecoveryModel)
	}
}

// A DROP AND CREATE script of a database runs: the guarded DROP removes it
// (from master, which the script switches to), and the CREATE puts it back
// with its recovery model. Run twice, so the guards are exercised both ways.
func TestLiveScriptDatabaseDropAndCreateRuns(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const name = "gosmo_scriptdb_drop_live"
	d, drop := liveScratchDB(t, db, ctx, name)
	defer drop()
	if _, err := db.ExecContext(ctx, "ALTER DATABASE ["+name+"] SET RECOVERY SIMPLE"); err != nil {
		t.Fatalf("set recovery: %v", err)
	}

	opts := DefaultScriptOptions()
	opts.IncludeHeaders = false
	opts.Verb = ScriptDropAndCreate
	script, err := NewScripter(d.server.DatabaseRef(name), opts).ScriptDatabase(ctx)
	if err != nil {
		t.Fatalf("ScriptDatabase: %v", err)
	}
	for range 2 {
		for _, batch := range strings.Split(script, "\nGO") {
			if strings.TrimSpace(batch) == "" {
				continue
			}
			if _, err := db.ExecContext(ctx, batch); err != nil {
				t.Fatalf("running %.60q: %v", strings.TrimSpace(batch), err)
			}
		}
	}
	got, err := d.server.DatabaseByName(ctx, name)
	if err != nil {
		t.Fatalf("database after the script: %v", err)
	}
	if got.RecoveryModel != RecoveryModelSimple {
		t.Errorf("recovery model = %q, want SIMPLE", got.RecoveryModel)
	}

	opts.Verb = ScriptDrop
	dropScript, err := NewScripter(d.server.DatabaseRef(name), opts).ScriptDatabase(ctx)
	if err != nil {
		t.Fatalf("ScriptDatabase DROP: %v", err)
	}
	for _, batch := range strings.Split(dropScript, "\nGO") {
		if strings.TrimSpace(batch) == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, batch); err != nil {
			t.Fatalf("running %.60q: %v", strings.TrimSpace(batch), err)
		}
	}
	if _, err := d.server.DatabaseByName(ctx, name); !errors.Is(err, ErrNotFound) {
		t.Errorf("after the DROP script, DatabaseByName = %v, want ErrNotFound", err)
	}
}
