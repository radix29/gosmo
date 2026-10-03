//go:build livedb

// Live verification that the login setters mirror what they write onto the
// handle: after each, the handle reads as LoginByName does.
//
//	go test -tags livedb . -run TestLiveLoginSettersMirror -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops one throwaway login; touches nothing else.
package gosmo

import "testing"

func TestLiveLoginSettersMirror(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	s, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	const name = "gosmo_live_login_mirror"
	dropLoginIfPresent(t, s, name)
	defer dropLoginIfPresent(t, s, name)
	l, err := s.CreateLogin(ctx, CreateLoginRequest{Name: name, Password: "Mirr0r!Pass-1", CheckPolicy: new(false)})
	if err != nil {
		t.Fatalf("CreateLogin: %v", err)
	}
	check := func(step string) {
		t.Helper()
		fresh, err := s.LoginByName(ctx, name)
		if err != nil {
			t.Fatalf("LoginByName: %v", err)
		}
		if l.DefaultLanguage != fresh.DefaultLanguage || l.IsPolicyChecked != fresh.IsPolicyChecked ||
			l.IsExpirationChecked != fresh.IsExpirationChecked || l.DefaultDatabase != fresh.DefaultDatabase {
			t.Errorf("after %s the handle reads language %q, policy %v, expiration %v, database %q; the catalog %q, %v, %v, %q",
				step, l.DefaultLanguage, l.IsPolicyChecked, l.IsExpirationChecked, l.DefaultDatabase,
				fresh.DefaultLanguage, fresh.IsPolicyChecked, fresh.IsExpirationChecked, fresh.DefaultDatabase)
		}
	}
	if err := l.SetDefaultLanguage(ctx, "Deutsch"); err != nil {
		t.Fatalf("SetDefaultLanguage: %v", err)
	}
	check("SetDefaultLanguage")
	if err := l.SetDefaultDatabase(ctx, "tempdb"); err != nil {
		t.Fatalf("SetDefaultDatabase: %v", err)
	}
	check("SetDefaultDatabase")
	if err := l.SetPasswordPolicy(ctx, true, false); err != nil {
		t.Fatalf("SetPasswordPolicy: %v", err)
	}
	check("SetPasswordPolicy")
	if err := l.ChangePassword(ctx, "Mirr0r!Pass-2", ChangePasswordOptions{MustChange: true}); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	check("ChangePassword with MustChange")
}
