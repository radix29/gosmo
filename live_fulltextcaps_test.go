//go:build livedb

// Live verification of DatabaseCapabilities for full-text catalogs, stoplists
// and search property lists: ALTER ANY FULLTEXT CATALOG and the per-securable
// CONTROL the class 23/29/31 rows read, against what the server enforces for
// DROP.
//
//	go test -tags livedb . -run TestLiveFullTextCapabilities -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database and login; touches nothing else.
// Skips where Full-Text Search is not installed.
package gosmo

import (
	"context"
	"database/sql"
	"testing"
)

// TestLiveFullTextCapabilitiesMatchWhatTheServerEnforces pins the results
// recorded on ProbedDatabasePermissions and ProbedSecurablePermissions,
// identical on majors 14 and 17 when this was written (2026-10-07):
//
//   - CONTROL on a catalog, stoplist or property list, or its ownership,
//     reads 1 and permits the drop with no database-scope right.
//   - ALTER on the securable makes it visible, reads 0 and permits no drop.
//   - One with no right, or carrying DENY CONTROL, is not in the catalog for
//     the user, so it has no row and reads unknown.
//   - ALTER ANY FULLTEXT CATALOG permits every drop CONTROL read 0 for, in all
//     three families.
func TestLiveFullTextCapabilitiesMatchWhatTheServerEnforces(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	if info, err := liveServer(t, db, ctx).FullTextInfo(ctx); err != nil {
		t.Fatalf("FullTextInfo: %v", err)
	} else if !info.Installed {
		t.Skip("Full-Text Search is not installed here")
	}

	const login = "gosmo_live_ftcaps"
	const pass = "P@ssw0rd_gosmo_live"

	// Deferred ahead of the scratch database's drop so it runs after it; see
	// live_keycaps_test.go.
	dropLogin := "DECLARE @k nvarchar(max) = N'';" +
		" SELECT @k += N'KILL ' + CAST(session_id AS nvarchar(10)) + N';'" +
		" FROM sys.dm_exec_sessions WHERE login_name = N'" + login + "' AND session_id <> @@SPID;" +
		" EXEC (@k);" +
		" IF SUSER_ID(N'" + login + "') IS NOT NULL DROP LOGIN [" + login + "]"
	defer func() {
		if _, err := db.ExecContext(context.Background(), dropLogin); err != nil {
			t.Errorf("drop login %s: %v", login, err)
		}
	}()

	d, drop := liveScratchDB(t, db, ctx, "gossms_p5_fts_caps")
	defer drop()

	if _, err := db.ExecContext(ctx, dropLogin); err != nil {
		t.Fatalf("pre-drop login: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE LOGIN ["+login+"] WITH PASSWORD = '"+pass+"', CHECK_POLICY = OFF"); err != nil {
		t.Fatalf("create login: %v", err)
	}

	u := "[" + login + "]"
	// Stoplist and property-list DDL must end in ';' (Msg 10736).
	liveExecIn(t, d, ctx,
		`CREATE USER `+u+` FOR LOGIN `+u,

		`CREATE FULLTEXT CATALOG c_own AUTHORIZATION `+u,
		`CREATE FULLTEXT CATALOG c_ctl`,
		// A name that only reads back if the block QUOTENAMEs it.
		`CREATE FULLTEXT CATALOG [c.dot]`,
		`CREATE FULLTEXT CATALOG c_alt`,
		`CREATE FULLTEXT CATALOG c_none`,
		`CREATE FULLTEXT CATALOG c_hidden`,
		`GRANT CONTROL ON FULLTEXT CATALOG::c_ctl TO `+u,
		`GRANT CONTROL ON FULLTEXT CATALOG::[c.dot] TO `+u,
		`GRANT ALTER ON FULLTEXT CATALOG::c_alt TO `+u,
		`DENY CONTROL ON FULLTEXT CATALOG::c_hidden TO `+u,

		`CREATE FULLTEXT STOPLIST s_own AUTHORIZATION `+u+`;`,
		`CREATE FULLTEXT STOPLIST s_ctl;`,
		`CREATE FULLTEXT STOPLIST s_alt;`,
		`GRANT CONTROL ON FULLTEXT STOPLIST::s_ctl TO `+u,
		`GRANT ALTER ON FULLTEXT STOPLIST::s_alt TO `+u,

		`CREATE SEARCH PROPERTY LIST p_own AUTHORIZATION `+u+`;`,
		`CREATE SEARCH PROPERTY LIST p_ctl;`,
		`CREATE SEARCH PROPERTY LIST p_alt;`,
		`GRANT CONTROL ON SEARCH PROPERTY LIST::p_ctl TO `+u,
		`GRANT ALTER ON SEARCH PROPERTY LIST::p_alt TO `+u,
	)

	pool, err := sql.Open("sqlserver", liveRestrictedDSN(t, login, pass))
	if err != nil {
		t.Fatalf("open as %s: %v", login, err)
	}
	defer pool.Close()
	exec := func(stmt string) error {
		_, err := pool.ExecContext(ctx, "USE ["+d.Name+"]; "+stmt)
		return err
	}
	// Not NewServer, for live_schemacaps_test.go's reason.
	probe := func() *DatabaseCapabilities {
		t.Helper()
		caps, err := (&Server{db: pool}).DatabaseRef(d.Name).Capabilities(ctx)
		if err != nil {
			t.Fatalf("Capabilities as %s: %v", login, err)
		}
		return caps
	}

	caps := probe()
	if got := caps.Permission("ALTER ANY FULLTEXT CATALOG"); got != CapabilityDenied {
		t.Errorf("ALTER ANY FULLTEXT CATALOG = %v before it was granted, want %v", got, CapabilityDenied)
	}
	for _, tc := range []struct {
		kind DatabaseSecurableKind
		name string
		want CapabilityState
		why  string
	}{
		{DatabaseSecurableFullTextCatalog, "c_own", CapabilityGranted, "AUTHORIZATION on the catalog"},
		{DatabaseSecurableFullTextCatalog, "c_ctl", CapabilityGranted, "GRANT CONTROL on the catalog"},
		{DatabaseSecurableFullTextCatalog, "c.dot", CapabilityGranted, "GRANT CONTROL on a catalog whose name needs quoting"},
		{DatabaseSecurableFullTextCatalog, "c_alt", CapabilityDenied, "ALTER alone"},
		{DatabaseSecurableFullTextCatalog, "c_none", CapabilityUnknown, "no right, so not visible"},
		{DatabaseSecurableFullTextCatalog, "c_hidden", CapabilityUnknown, "DENY CONTROL, so not visible"},
		{DatabaseSecurableFullTextStoplist, "s_own", CapabilityGranted, "AUTHORIZATION on the stoplist"},
		{DatabaseSecurableFullTextStoplist, "s_ctl", CapabilityGranted, "GRANT CONTROL on the stoplist"},
		{DatabaseSecurableFullTextStoplist, "s_alt", CapabilityDenied, "ALTER alone"},
		{DatabaseSecurableSearchPropertyList, "p_own", CapabilityGranted, "AUTHORIZATION on the property list"},
		{DatabaseSecurableSearchPropertyList, "p_ctl", CapabilityGranted, "GRANT CONTROL on the property list"},
		{DatabaseSecurableSearchPropertyList, "p_alt", CapabilityDenied, "ALTER alone"},
		// A catalog of the same name must not answer for a stoplist.
		{DatabaseSecurableFullTextStoplist, "c_ctl", CapabilityUnknown, "no stoplist of that name"},
	} {
		if got := caps.SecurablePermission(tc.kind, "", tc.name, "CONTROL"); got != tc.want {
			t.Errorf("%s %s CONTROL = %v, want %v (%s)", tc.kind, tc.name, got, tc.want, tc.why)
		}
	}

	// The oracle: what the server accepts from this login.
	for _, stmt := range []string{
		"DROP FULLTEXT CATALOG c_own",
		"DROP FULLTEXT CATALOG c_ctl",
		"DROP FULLTEXT CATALOG [c.dot]",
		"DROP FULLTEXT STOPLIST s_own;",
		"DROP FULLTEXT STOPLIST s_ctl;",
		"DROP SEARCH PROPERTY LIST p_own;",
		"DROP SEARCH PROPERTY LIST p_ctl;",
	} {
		if err := exec(stmt); err != nil {
			t.Errorf("the server refused %q, which CONTROL read 1 for: %v", stmt, err)
		}
	}
	for _, stmt := range []string{
		"DROP FULLTEXT CATALOG c_alt",
		"DROP FULLTEXT CATALOG c_hidden",
		"DROP FULLTEXT STOPLIST s_alt;",
		"DROP SEARCH PROPERTY LIST p_alt;",
	} {
		if err := exec(stmt); err == nil {
			t.Errorf("the server accepted %q from a user whose CONTROL read 0 or unknown", stmt)
		}
	}

	// ALTER ANY FULLTEXT CATALOG permits the drops CONTROL read 0 for, in all
	// three families, and reads granted once held.
	liveExecIn(t, d, ctx, `GRANT ALTER ANY FULLTEXT CATALOG TO `+u)
	caps = probe()
	if !caps.Allows("ALTER ANY FULLTEXT CATALOG") {
		t.Error("ALTER ANY FULLTEXT CATALOG does not read granted once held")
	}
	if got := caps.SecurablePermission(DatabaseSecurableFullTextCatalog, "", "c_none", "CONTROL"); got != CapabilityDenied {
		t.Errorf("catalog c_none CONTROL under ALTER ANY FULLTEXT CATALOG = %v, want %v — "+
			"the right makes it visible without conferring CONTROL", got, CapabilityDenied)
	}
	for _, stmt := range []string{
		"DROP FULLTEXT CATALOG c_alt",
		"DROP FULLTEXT CATALOG c_none",
		"DROP FULLTEXT STOPLIST s_alt;",
		"DROP SEARCH PROPERTY LIST p_alt;",
	} {
		if err := exec(stmt); err != nil {
			t.Errorf("the server refused %q under ALTER ANY FULLTEXT CATALOG: %v", stmt, err)
		}
	}
}

// TestLiveFullTextCreateAlterAndReferencesMatchWhatTheServerEnforces pins
// the rest of the 2026-10-08 probe (majors 14 and 17, identical), recorded on
// ProbedDatabasePermissions and ProbedSecurablePermissions:
//
//   - CREATE FULLTEXT CATALOG reads 0 with no right and is refused; granted,
//     it reads 1 and creates a catalog, a stoplist and a property list.
//   - ALTER on a catalog, stoplist or property list reads 1 and permits its
//     ALTER statements (REORGANIZE, ADD stopword, ADD property), not AS
//     DEFAULT, and reads REFERENCES 0.
//   - CREATE FULLTEXT INDEX is refused to ALTER on the table while REFERENCES
//     on the catalog reads 0, and runs once it reads 1; SET STOPLIST likewise
//     waits on REFERENCES on the stoplist.
func TestLiveFullTextCreateAlterAndReferencesMatchWhatTheServerEnforces(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	if info, err := liveServer(t, db, ctx).FullTextInfo(ctx); err != nil {
		t.Fatalf("FullTextInfo: %v", err)
	} else if !info.Installed {
		t.Skip("Full-Text Search is not installed here")
	}

	const login = "gosmo_live_ftcaps2"
	const pass = "P@ssw0rd_gosmo_live"
	dropLogin := "DECLARE @k nvarchar(max) = N'';" +
		" SELECT @k += N'KILL ' + CAST(session_id AS nvarchar(10)) + N';'" +
		" FROM sys.dm_exec_sessions WHERE login_name = N'" + login + "' AND session_id <> @@SPID;" +
		" EXEC (@k);" +
		" IF SUSER_ID(N'" + login + "') IS NOT NULL DROP LOGIN [" + login + "]"
	defer func() {
		if _, err := db.ExecContext(context.Background(), dropLogin); err != nil {
			t.Errorf("drop login %s: %v", login, err)
		}
	}()

	d, drop := liveScratchDB(t, db, ctx, "gossms_p5_fts_caps2")
	defer drop()

	if _, err := db.ExecContext(ctx, dropLogin); err != nil {
		t.Fatalf("pre-drop login: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE LOGIN ["+login+"] WITH PASSWORD = '"+pass+"', CHECK_POLICY = OFF"); err != nil {
		t.Fatalf("create login: %v", err)
	}
	u := "[" + login + "]"
	liveExecIn(t, d, ctx,
		`CREATE USER `+u+` FOR LOGIN `+u,
		`CREATE TABLE dbo.T (id int NOT NULL CONSTRAINT PK_T PRIMARY KEY, body nvarchar(200))`,
		`CREATE TABLE dbo.T2 (id int NOT NULL CONSTRAINT PK_T2 PRIMARY KEY, body nvarchar(200))`,
		`CREATE FULLTEXT CATALOG c1`,
		`CREATE FULLTEXT STOPLIST s1;`,
		`CREATE SEARCH PROPERTY LIST p1;`,
		`CREATE FULLTEXT INDEX ON dbo.T2 (body) KEY INDEX PK_T2 ON c1 WITH CHANGE_TRACKING OFF, NO POPULATION`,
	)

	pool, err := sql.Open("sqlserver", liveRestrictedDSN(t, login, pass))
	if err != nil {
		t.Fatalf("open as %s: %v", login, err)
	}
	defer pool.Close()
	exec := func(stmt string) error {
		_, err := pool.ExecContext(ctx, "USE ["+d.Name+"]; "+stmt)
		return err
	}
	probe := func() *DatabaseCapabilities {
		t.Helper()
		caps, err := (&Server{db: pool}).DatabaseRef(d.Name).Capabilities(ctx)
		if err != nil {
			t.Fatalf("Capabilities as %s: %v", login, err)
		}
		return caps
	}
	accepts := func(stmt string, want bool, why string) {
		t.Helper()
		err := exec(stmt)
		switch {
		case want && err != nil:
			t.Errorf("the server refused %q, which %s read 1 for: %v", stmt, why, err)
		case !want && err == nil:
			t.Errorf("the server accepted %q, which %s read 0 for", stmt, why)
		}
	}

	caps := probe()
	if got := caps.Permission("CREATE FULLTEXT CATALOG"); got != CapabilityDenied {
		t.Errorf("CREATE FULLTEXT CATALOG = %v with no right, want %v", got, CapabilityDenied)
	}
	accepts("CREATE FULLTEXT CATALOG c_new", false, "CREATE FULLTEXT CATALOG")

	liveExecIn(t, d, ctx, `GRANT CREATE FULLTEXT CATALOG TO `+u)
	if got := probe().Permission("CREATE FULLTEXT CATALOG"); got != CapabilityGranted {
		t.Errorf("CREATE FULLTEXT CATALOG = %v once granted, want %v", got, CapabilityGranted)
	}
	accepts("CREATE FULLTEXT CATALOG c_new", true, "CREATE FULLTEXT CATALOG")
	accepts("CREATE FULLTEXT STOPLIST s_new FROM SYSTEM STOPLIST;", true, "CREATE FULLTEXT CATALOG")
	accepts("CREATE SEARCH PROPERTY LIST p_new;", true, "CREATE FULLTEXT CATALOG")
	liveExecIn(t, d, ctx, `REVOKE CREATE FULLTEXT CATALOG FROM `+u)

	liveExecIn(t, d, ctx,
		`GRANT ALTER ON FULLTEXT CATALOG::c1 TO `+u,
		`GRANT ALTER ON FULLTEXT STOPLIST::s1 TO `+u,
		`GRANT ALTER ON SEARCH PROPERTY LIST::p1 TO `+u,
		`GRANT ALTER ON dbo.T TO `+u,
		`GRANT ALTER ON dbo.T2 TO `+u,
	)
	caps = probe()
	for _, k := range []struct {
		kind DatabaseSecurableKind
		name string
	}{
		{DatabaseSecurableFullTextCatalog, "c1"},
		{DatabaseSecurableFullTextStoplist, "s1"},
		{DatabaseSecurableSearchPropertyList, "p1"},
	} {
		if got := caps.SecurablePermission(k.kind, "", k.name, "ALTER"); got != CapabilityGranted {
			t.Errorf("%s %s ALTER = %v under ALTER on it, want %v", k.kind, k.name, got, CapabilityGranted)
		}
		if got := caps.SecurablePermission(k.kind, "", k.name, "REFERENCES"); got != CapabilityDenied {
			t.Errorf("%s %s REFERENCES = %v under ALTER on it, want %v", k.kind, k.name, got, CapabilityDenied)
		}
	}
	accepts("ALTER FULLTEXT CATALOG c1 REORGANIZE", true, "ALTER on the catalog")
	accepts("ALTER FULLTEXT STOPLIST s1 ADD 'zzq' LANGUAGE 1033;", true, "ALTER on the stoplist")
	accepts("ALTER SEARCH PROPERTY LIST p1 ADD 'Pz' WITH (PROPERTY_SET_GUID = 'F29F85E0-4FF9-1068-AB91-08002B27B3D9', PROPERTY_INT_ID = 2);",
		true, "ALTER on the property list")
	accepts("ALTER FULLTEXT CATALOG c1 AS DEFAULT", false, "ALTER ANY FULLTEXT CATALOG")
	accepts("CREATE FULLTEXT INDEX ON dbo.T (body) KEY INDEX PK_T ON c1 WITH CHANGE_TRACKING OFF, NO POPULATION",
		false, "REFERENCES on the catalog")
	accepts("ALTER FULLTEXT INDEX ON dbo.T2 SET STOPLIST = s1", false, "REFERENCES on the stoplist")

	liveExecIn(t, d, ctx,
		`GRANT REFERENCES ON FULLTEXT CATALOG::c1 TO `+u,
		`GRANT REFERENCES ON FULLTEXT STOPLIST::s1 TO `+u,
	)
	caps = probe()
	if got := caps.SecurablePermission(DatabaseSecurableFullTextCatalog, "", "c1", "REFERENCES"); got != CapabilityGranted {
		t.Errorf("catalog c1 REFERENCES = %v once granted, want %v", got, CapabilityGranted)
	}
	if got := caps.SecurablePermission(DatabaseSecurableFullTextStoplist, "", "s1", "REFERENCES"); got != CapabilityGranted {
		t.Errorf("stoplist s1 REFERENCES = %v once granted, want %v", got, CapabilityGranted)
	}
	accepts("CREATE FULLTEXT INDEX ON dbo.T (body) KEY INDEX PK_T ON c1 WITH CHANGE_TRACKING OFF, NO POPULATION",
		true, "ALTER on the table and REFERENCES on the catalog")
	accepts("ALTER FULLTEXT INDEX ON dbo.T2 SET STOPLIST = s1", true, "ALTER on the table and REFERENCES on the stoplist")
}
