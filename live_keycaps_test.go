//go:build livedb

// Live verification of DatabaseCapabilities for certificates, asymmetric keys
// and symmetric keys: the database-scope CREATE / ALTER ANY names and the
// per-securable CONTROL the class 24/25/26 rows read, against what the server
// enforces for DROP.
//
//	go test -tags livedb . -run TestLiveKeyCapabilities -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database and login; touches nothing else.
package gosmo

import (
	"context"
	"database/sql"
	"testing"
)

// TestLiveKeyCapabilitiesMatchWhatTheServerEnforces pins the results recorded
// on ProbedDatabasePermissions and ProbedSecurablePermissions, identical on
// majors 13, 14 and 17 when this was written (2026-09-22):
//
//   - A CREATE <family>-only user reads the CREATE name granted and ALTER ANY
//     denied, sees only the rows it has a right on, and — the case the
//     per-securable block exists for — reads CONTROL on what it owns, whether
//     it created it or was made its owner, and can drop exactly that.
//   - CONTROL granted on a certificate or key reads 1 and permits its drop;
//     VIEW DEFINITION makes it visible, reads 0 and permits no drop.
//   - Granting ALTER ANY <family> afterwards permits the drop that CONTROL
//     read 0 for: the block is an additional reason to permit, never the
//     whole test.
//   - A certificate or key the user has no right on is not in the catalog for
//     it, so it has no row and reads unknown, not denied — and so does one
//     carrying DENY CONTROL.
func TestLiveKeyCapabilitiesMatchWhatTheServerEnforces(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const login = "gosmo_live_keycaps"
	const pass = "P@ssw0rd_gosmo_live"
	const pw = ` ENCRYPTION BY PASSWORD = 'K3y_P@ssw0rd_live'`

	d, drop := liveScratchDB(t, db, ctx, "gosmo_keycaps_live")
	defer drop()

	db.ExecContext(ctx, "IF SUSER_ID('"+login+"') IS NOT NULL DROP LOGIN ["+login+"]")
	if _, err := db.ExecContext(ctx, "CREATE LOGIN ["+login+"] WITH PASSWORD = '"+pass+"', CHECK_POLICY = OFF"); err != nil {
		t.Fatalf("create login: %v", err)
	}
	defer db.ExecContext(context.Background(), "DROP LOGIN ["+login+"]")

	u := "[" + login + "]"
	liveExecIn(t, d, ctx,
		`CREATE USER `+u+` FOR LOGIN `+u,
		`GRANT CREATE CERTIFICATE TO `+u,
		`GRANT CREATE ASYMMETRIC KEY TO `+u,
		`GRANT CREATE SYMMETRIC KEY TO `+u,

		`CREATE CERTIFICATE c_own AUTHORIZATION `+u+pw+` WITH SUBJECT = 'c_own'`,
		`CREATE CERTIFICATE c_ctl`+pw+` WITH SUBJECT = 'c_ctl'`,
		// A name that only reads back if the block QUOTENAMEs it.
		`CREATE CERTIFICATE [c.dot]`+pw+` WITH SUBJECT = 'c.dot'`,
		`CREATE CERTIFICATE c_view`+pw+` WITH SUBJECT = 'c_view'`,
		`CREATE CERTIFICATE c_none`+pw+` WITH SUBJECT = 'c_none'`,
		`CREATE CERTIFICATE c_hidden`+pw+` WITH SUBJECT = 'c_hidden'`,
		`GRANT CONTROL ON CERTIFICATE::c_ctl TO `+u,
		`GRANT CONTROL ON CERTIFICATE::[c.dot] TO `+u,
		`GRANT VIEW DEFINITION ON CERTIFICATE::c_view TO `+u,
		`DENY CONTROL ON CERTIFICATE::c_hidden TO `+u,

		`CREATE ASYMMETRIC KEY a_ctl WITH ALGORITHM = RSA_2048`+pw,
		`CREATE ASYMMETRIC KEY a_view WITH ALGORITHM = RSA_2048`+pw,
		`GRANT CONTROL ON ASYMMETRIC KEY::a_ctl TO `+u,
		`GRANT VIEW DEFINITION ON ASYMMETRIC KEY::a_view TO `+u,

		`CREATE SYMMETRIC KEY s_own AUTHORIZATION `+u+` WITH ALGORITHM = AES_256`+pw,
		`CREATE SYMMETRIC KEY s_view WITH ALGORITHM = AES_256`+pw,
		`GRANT VIEW DEFINITION ON SYMMETRIC KEY::s_view TO `+u,
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
	// Not NewServer, for live_schemacaps_test.go's reason: loadInfo reads
	// server-scope DMVs this login has no rights to.
	probe := func() *DatabaseCapabilities {
		t.Helper()
		caps, err := (&Server{db: pool}).DatabaseRef(d.Name).CapabilitiesContext(ctx)
		if err != nil {
			t.Fatalf("CapabilitiesContext as %s: %v", login, err)
		}
		return caps
	}

	// The user's own CREATE, before the first probe, so it is asked about.
	if err := exec(`CREATE CERTIFICATE c_mine` + pw + ` WITH SUBJECT = 'c_mine'`); err != nil {
		t.Fatalf("CREATE CERTIFICATE as a CREATE CERTIFICATE-only user: %v", err)
	}
	caps := probe()

	for _, tc := range []struct {
		name string
		want CapabilityState
	}{
		{"CREATE CERTIFICATE", CapabilityGranted},
		{"CREATE ASYMMETRIC KEY", CapabilityGranted},
		{"CREATE SYMMETRIC KEY", CapabilityGranted},
		{"ALTER ANY CERTIFICATE", CapabilityDenied},
		{"ALTER ANY ASYMMETRIC KEY", CapabilityDenied},
		{"ALTER ANY SYMMETRIC KEY", CapabilityDenied},
	} {
		if got := caps.Permission(tc.name); got != tc.want {
			t.Errorf("database permission %q = %v, want %v", tc.name, got, tc.want)
		}
	}

	for _, tc := range []struct {
		kind DatabaseSecurableKind
		name string
		want CapabilityState
		why  string
	}{
		{DatabaseSecurableCertificate, "c_mine", CapabilityGranted, "a certificate the user created"},
		{DatabaseSecurableCertificate, "c_own", CapabilityGranted, "AUTHORIZATION on the certificate"},
		{DatabaseSecurableCertificate, "c_ctl", CapabilityGranted, "GRANT CONTROL on the certificate"},
		{DatabaseSecurableCertificate, "c.dot", CapabilityGranted, "GRANT CONTROL on a certificate whose name needs quoting"},
		{DatabaseSecurableCertificate, "c_view", CapabilityDenied, "VIEW DEFINITION alone"},
		{DatabaseSecurableCertificate, "c_none", CapabilityUnknown, "no right, so not visible"},
		{DatabaseSecurableCertificate, "c_hidden", CapabilityUnknown, "DENY CONTROL, so not visible"},
		{DatabaseSecurableAsymmetricKey, "a_ctl", CapabilityGranted, "GRANT CONTROL on the asymmetric key"},
		{DatabaseSecurableAsymmetricKey, "a_view", CapabilityDenied, "VIEW DEFINITION alone"},
		{DatabaseSecurableSymmetricKey, "s_own", CapabilityGranted, "AUTHORIZATION on the symmetric key"},
		{DatabaseSecurableSymmetricKey, "s_view", CapabilityDenied, "VIEW DEFINITION alone"},
		// A certificate of the same name must not answer for a key.
		{DatabaseSecurableAsymmetricKey, "c_ctl", CapabilityUnknown, "no asymmetric key of that name"},
	} {
		if got := caps.SecurablePermission(tc.kind, "", tc.name, "CONTROL"); got != tc.want {
			t.Errorf("%s %s CONTROL = %v, want %v (%s)", tc.kind, tc.name, got, tc.want, tc.why)
		}
	}
	for key := range caps.SecurablePermissions {
		if key == "SYMMETRIC KEY::##MS_DatabaseMasterKey##" {
			t.Error("the database master key was asked about")
		}
	}

	// The oracle: what the server accepts from this login.
	for _, stmt := range []string{
		"DROP CERTIFICATE c_mine",
		"DROP CERTIFICATE c_own",
		"DROP CERTIFICATE c_ctl",
		"DROP ASYMMETRIC KEY a_ctl",
		"DROP SYMMETRIC KEY s_own",
	} {
		if err := exec(stmt); err != nil {
			t.Errorf("the server refused %q, which CONTROL read 1 for: %v", stmt, err)
		}
	}
	for _, stmt := range []string{
		"DROP CERTIFICATE c_view",
		"DROP ASYMMETRIC KEY a_view",
		"DROP SYMMETRIC KEY s_view",
		"DROP CERTIFICATE c_hidden",
	} {
		if err := exec(stmt); err == nil {
			t.Errorf("the server accepted %q from a user whose CONTROL read 0 or unknown", stmt)
		}
	}

	// ALTER ANY <family> permits the drops CONTROL read 0 for, and reads
	// granted once held.
	liveExecIn(t, d, ctx,
		`GRANT ALTER ANY CERTIFICATE TO `+u,
		`GRANT ALTER ANY ASYMMETRIC KEY TO `+u,
		`GRANT ALTER ANY SYMMETRIC KEY TO `+u,
	)
	caps = probe()
	for _, name := range []string{"ALTER ANY CERTIFICATE", "ALTER ANY ASYMMETRIC KEY", "ALTER ANY SYMMETRIC KEY"} {
		if !caps.Allows(name) {
			t.Errorf("database permission %q does not read granted once held", name)
		}
	}
	if got := caps.SecurablePermission(DatabaseSecurableCertificate, "", "c_none", "CONTROL"); got != CapabilityDenied {
		t.Errorf("certificate c_none CONTROL under ALTER ANY CERTIFICATE = %v, want %v — "+
			"ALTER ANY makes it visible without conferring CONTROL", got, CapabilityDenied)
	}
	for _, stmt := range []string{
		"DROP CERTIFICATE c_view",
		"DROP CERTIFICATE c_none",
		"DROP ASYMMETRIC KEY a_view",
		"DROP SYMMETRIC KEY s_view",
	} {
		if err := exec(stmt); err != nil {
			t.Errorf("the server refused %q under ALTER ANY: %v", stmt, err)
		}
	}
}
