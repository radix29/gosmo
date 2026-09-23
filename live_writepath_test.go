//go:build livedb

// Live verification of the 2026-09-23 review plan's write-path items:
//
//   - S7: DropTable(cascade) drops the incoming foreign keys and the table in
//     one transaction, so a DROP TABLE the server refuses leaves the keys.
//
//   - S8: a forced rename runs SINGLE_USER, the rename and the MULTI_USER
//     release in one batch, gets past a session parked in the database, and
//     puts a database whose rename failed back to MULTI_USER itself.
//
//   - S9: a FILENAME outside the code page reaches the server intact.
//
//     go test -tags livedb . -run 'TestLiveDropTableCascade|TestLiveForced|TestLiveNonASCII' -v \
//     -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway databases; touches nothing else.
package gosmo

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

func TestLiveDropTableCascadeIsAtomic(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	d, drop := liveScratchDB(t, db, ctx, "gosmo_droptable_live")
	defer drop()

	liveExecIn(t, d, ctx,
		"CREATE TABLE dbo.Parent (ID int PRIMARY KEY)",
		"CREATE TABLE dbo.Child (ID int PRIMARY KEY, ParentID int CONSTRAINT FK_Child_Parent REFERENCES dbo.Parent (ID))",
		"CREATE VIEW dbo.vParent WITH SCHEMABINDING AS SELECT ID FROM dbo.Parent",
	)
	count := func(q string) int {
		t.Helper()
		var n int
		if err := d.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&n) }, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	fks := func() int {
		return count("SELECT COUNT(*) FROM sys.foreign_keys WHERE name = N'FK_Child_Parent'")
	}
	parent := func() int { return count("SELECT COUNT(*) FROM sys.tables WHERE name = N'Parent'") }

	// The schema-bound view makes DROP TABLE fail (Msg 3729) after the key
	// drop has run; the key must survive it.
	if err := d.DropTable(ctx, "dbo", "Parent", true); err == nil {
		t.Fatal("DropTable(cascade) of a table a schema-bound view references succeeded")
	} else if !strings.Contains(err.Error(), "vParent") {
		t.Errorf("DropTable error %v does not name the view", err)
	}
	if fks() != 1 || parent() != 1 {
		t.Fatalf("after a refused cascade drop: FK count %d, Parent count %d, want both 1", fks(), parent())
	}

	liveExecIn(t, d, ctx, "DROP VIEW dbo.vParent")
	if err := d.DropTable(ctx, "dbo", "Parent", true); err != nil {
		t.Fatalf("DropTable(cascade): %v", err)
	}
	if fks() != 0 || parent() != 0 {
		t.Errorf("after the cascade drop: FK count %d, Parent count %d, want both 0", fks(), parent())
	}
	if n := count("SELECT COUNT(*) FROM sys.tables WHERE name = N'Child'"); n != 1 {
		t.Errorf("the referencing table went too (count %d)", n)
	}
}

func TestLiveForcedRenameAndDropAreOneBatch(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	const name, renamed = "gosmo_forced_live", "gosmo_forced_live2"
	for _, n := range []string{name, renamed} {
		db.ExecContext(ctx, "IF DB_ID(N'"+n+"') IS NOT NULL ALTER DATABASE ["+n+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE")
		db.ExecContext(ctx, "IF DB_ID(N'"+n+"') IS NOT NULL DROP DATABASE ["+n+"]")
	}
	d, drop := liveScratchDB(t, db, ctx, name)
	defer drop()
	defer func() {
		c := context.Background()
		db.ExecContext(c, "IF DB_ID(N'"+renamed+"') IS NOT NULL ALTER DATABASE ["+renamed+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE")
		db.ExecContext(c, "IF DB_ID(N'"+renamed+"') IS NOT NULL DROP DATABASE ["+renamed+"]")
	}()
	srv := d.Server()

	// A refused rename — the target name is taken (Msg 1801) — is released
	// by the batch's CATCH: a server error is not a batch cut short, so no
	// repair follows from Go, and the database must still come back
	// MULTI_USER. Not onto master: that fails the batch at compile time
	// (Msg 5058) before anything runs, and so proves nothing.
	const taken = "gosmo_forced_live_taken"
	_, dropTaken := liveScratchDB(t, db, ctx, taken)
	defer dropTaken()
	liveExecIn(t, d, ctx, "ALTER DATABASE ["+taken+"] SET RESTRICTED_USER")
	if err := srv.RenameDatabase(ctx, name, taken, true); err == nil {
		t.Fatal("renaming onto a taken name succeeded")
	} else if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("refused rename error %v is not the server's own (Msg 1801)", err)
	}
	if got := userAccess(t, srv, ctx, name); got != "MULTI_USER" {
		t.Errorf("after a refused forced rename %s is %q, want MULTI_USER", name, got)
	}
	if got := userAccess(t, srv, ctx, taken); got != "RESTRICTED_USER" {
		t.Errorf("%s, whose name the refused rename wanted, is %q — it was touched", taken, got)
	}

	// Park a session in an open transaction; the forced rename must roll it
	// back and finish.
	parked := liveObserver(t, ctx)
	conn, err := parked.Conn(ctx)
	if err != nil {
		t.Fatalf("park: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE ["+name+"]; BEGIN TRANSACTION; CREATE TABLE dbo.Parked (i int);"); err != nil {
		t.Fatalf("park a transaction: %v", err)
	}

	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := srv.RenameDatabase(rctx, name, renamed, true); err != nil {
		t.Fatalf("forced RenameDatabase with a parked session: %v", err)
	}
	if got := userAccess(t, srv, ctx, renamed); got != "MULTI_USER" {
		t.Errorf("after the forced rename %s is %q, want MULTI_USER", renamed, got)
	}

	dctx, cancel2 := context.WithTimeout(ctx, 20*time.Second)
	defer cancel2()
	if err := srv.DropDatabase(dctx, renamed, true); err != nil {
		t.Fatalf("forced DropDatabase: %v", err)
	}
	if got := userAccess(t, srv, ctx, renamed); got != "(no such database)" {
		t.Errorf("%s still exists (%s) after the forced drop", renamed, got)
	}
}

func TestLiveNonASCIIFilePath(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv := liveServer(t, db, ctx)
	const name = "gosmo_unicode_path_live"
	db.ExecContext(ctx, "IF DB_ID(N'"+name+"') IS NOT NULL DROP DATABASE ["+name+"]")
	defer db.ExecContext(context.Background(), "IF DB_ID(N'"+name+"') IS NOT NULL DROP DATABASE ["+name+"]")

	var dataDir, logDir string
	if err := db.QueryRowContext(ctx, `SELECT CONVERT(nvarchar(4000), SERVERPROPERTY('InstanceDefaultDataPath')),
		CONVERT(nvarchar(4000), SERVERPROPERTY('InstanceDefaultLogPath'))`).Scan(&dataDir, &logDir); err != nil {
		t.Fatalf("read default paths: %v", err)
	}
	// Cyrillic and CJK in the file name: outside every single-byte code page
	// at once, so a varchar literal turns them into '?' whatever the
	// collation.
	dataPath := dataDir + "gosmo_Данные_日本.mdf"
	logPath := logDir + "gosmo_Журнал_日本.ldf"
	err := srv.CreateDatabase(ctx, name, &CreateDatabaseOptions{
		PrimaryFile: &DatabaseFileSpec{Name: name, Path: dataPath},
		LogFile:     &DatabaseFileSpec{Name: name + "_log", Path: logPath},
	})
	if err != nil {
		t.Fatalf("CreateDatabase with a non-ASCII path: %v", err)
	}
	rows, err := db.QueryContext(ctx, "SELECT physical_name FROM sys.master_files WHERE database_id = DB_ID(@p1) ORDER BY file_id", name)
	if err != nil {
		t.Fatalf("read files: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		got = append(got, p)
	}
	if len(got) != 2 || got[0] != dataPath || got[1] != logPath {
		t.Errorf("physical names %q, want [%q %q]", got, dataPath, logPath)
	}
}
