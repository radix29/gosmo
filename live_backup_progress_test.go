//go:build livedb

// Live verification that BACKUP's WITH STATS progress reaches the caller for
// a login whose default language is not English. The server localises
// message 3211 to the session language ("50 Prozent verarbeitet."), and
// progress parsing matched the English text, so every such login saw -1
// until the backup finished.
//
//	go test -tags livedb . -run TestLiveBackupProgressLocalised -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops a throwaway database and login; the backup is COPY_ONLY
// to the null device, which leaves one msdb.dbo.backupset row.
package gosmo

import (
	"context"
	"database/sql"
	"net/url"
	"strings"
	"testing"
)

func TestLiveBackupProgressLocalised(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)

	const dbName = "gosmo_progress_de_live"
	_, drop := liveScratchDB(t, db, ctx, dbName)
	t.Cleanup(drop)

	const login = "gosmo_progress_de"
	const password = "Pa55-gosmo_progress_de!"
	dropLogin := func() {
		db.ExecContext(context.Background(), "IF SUSER_ID('"+login+"') IS NOT NULL DROP LOGIN ["+login+"]")
	}
	dropLogin()
	t.Cleanup(dropLogin)
	for _, stmt := range []string{
		"CREATE LOGIN [" + login + "] WITH PASSWORD = N'" + password + "', CHECK_POLICY = OFF, DEFAULT_LANGUAGE = Deutsch",
		"ALTER SERVER ROLE sysadmin ADD MEMBER [" + login + "]",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	u, err := url.Parse(*liveDSN)
	if err != nil {
		t.Fatalf("parse -livedb DSN: %v", err)
	}
	u.User = url.UserPassword(login, password)
	de, err := sql.Open("sqlserver", u.String())
	if err != nil {
		t.Fatalf("open as %s: %v", login, err)
	}
	t.Cleanup(func() { de.Close() })

	var lang string
	if err := de.QueryRowContext(ctx, "SELECT @@LANGUAGE").Scan(&lang); err != nil {
		t.Fatalf("@@LANGUAGE: %v", err)
	}
	if lang != "Deutsch" {
		t.Fatalf("session language = %q, want Deutsch", lang)
	}

	srv := liveServer(t, de, ctx)
	null := "NUL"
	if strings.HasPrefix(srv.Info().DefaultBackupPath, "/") {
		null = "/dev/null"
	}
	var pcts []int
	var texts []string
	err = srv.Backup(ctx, BackupOptions{
		Database: dbName, Devices: []BackupTarget{DiskTarget(null)}, CopyOnly: true, Stats: 50,
		Progress: func(pct int, msg string) {
			texts = append(texts, msg)
			if pct >= 0 {
				pcts = append(pcts, pct)
			}
		},
	})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if len(pcts) == 0 || pcts[len(pcts)-1] != 100 {
		t.Errorf("progress = %v, want it to end at 100; notices: %q", pcts, texts)
	}
	for _, s := range texts {
		if strings.Contains(s, "percent processed") {
			t.Errorf("notice %q is English; the session is not localised, so this test proves nothing", s)
		}
	}
	t.Logf("progress %v from %q", pcts, texts)
}
