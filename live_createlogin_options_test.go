//go:build livedb

// Live verification of T4 (review plan 2026-10-02): CreateLogin carries the
// SQL-login options in the CREATE's one WITH list, so a weak password with
// the policy off is accepted (it was refused with Msg 15118 while the policy
// went on a follow-up ALTER), and SID plus PasswordHash re-create a login
// that keeps both its SID and its password.
//
//	go test -tags livedb . -run TestLiveCreateLoginOptions -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway logins and credential; touches nothing
// else.
package gosmo

import (
	"bytes"
	"database/sql"
	"fmt"
	"testing"
)

func TestLiveCreateLoginOptions(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	s, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	const (
		first  = "gosmo_live_t4_a"
		clone  = "gosmo_live_t4_b"
		cred   = "gosmo_live_t4_cred"
		weakPw = "a"
	)
	sid := []byte{0x4c, 0x49, 0x56, 0x45, 0x54, 0x34, 0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09}
	cloneSID := append(bytes.Clone(sid[:15]), 0x0a)
	cleanup := func() {
		dropLoginIfPresent(t, s, first)
		dropLoginIfPresent(t, s, clone)
		if _, err := db.ExecContext(ctx, fmt.Sprintf("IF EXISTS (SELECT 1 FROM sys.credentials WHERE name = N'%s') DROP CREDENTIAL [%s]", cred, cred)); err != nil {
			t.Logf("cleanup of credential: %v", err)
		}
	}
	cleanup()
	defer cleanup()
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE CREDENTIAL [%s] WITH IDENTITY = N'gosmo', SECRET = N'x'", cred)); err != nil {
		t.Fatalf("create credential: %v", err)
	}

	l, err := s.CreateLogin(ctx, CreateLoginRequest{
		Name: first, Password: weakPw,
		CheckPolicy: new(false), CheckExpiration: new(false),
		DefaultDatabase: "master", DefaultLanguage: "Deutsch",
		SID: sid, Credential: cred,
	})
	if err != nil {
		t.Fatalf("create a policy-off login with a weak password: %v", err)
	}
	if !bytes.Equal(l.SID, sid) {
		t.Errorf("SID = %X, want %X", l.SID, sid)
	}
	if l.IsPolicyChecked || l.IsExpirationChecked {
		t.Errorf("policy %v, expiration %v; want both off", l.IsPolicyChecked, l.IsExpirationChecked)
	}
	if l.DefaultLanguage != "Deutsch" {
		t.Errorf("DefaultLanguage = %q, want Deutsch", l.DefaultLanguage)
	}
	var credName sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT c.name FROM sys.server_principals p
		LEFT JOIN sys.credentials c ON c.credential_id = p.credential_id WHERE p.name = @p1`, first).Scan(&credName); err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if credName.String != cred {
		t.Errorf("credential = %q, want %q", credName.String, cred)
	}

	var hash []byte
	if err := db.QueryRowContext(ctx, "SELECT CAST(LOGINPROPERTY(@p1, 'PasswordHash') AS varbinary(256))", first).Scan(&hash); err != nil {
		t.Fatalf("read password hash: %v", err)
	}
	c, err := s.CreateLogin(ctx, CreateLoginRequest{Name: clone, PasswordHash: hash, SID: cloneSID, CheckPolicy: new(false)})
	if err != nil {
		t.Fatalf("re-create from the password hash: %v", err)
	}
	if c.LoginType != "SQL_LOGIN" || !bytes.Equal(c.SID, cloneSID) {
		t.Errorf("clone: type %q, SID %X", c.LoginType, c.SID)
	}
	var same int
	if err := db.QueryRowContext(ctx, "SELECT PWDCOMPARE(@p1, CAST(LOGINPROPERTY(@p2, 'PasswordHash') AS varbinary(256)))", weakPw, clone).Scan(&same); err != nil {
		t.Fatalf("PWDCOMPARE: %v", err)
	}
	if same != 1 {
		t.Error("the HASHED clone does not accept the original password")
	}
}
