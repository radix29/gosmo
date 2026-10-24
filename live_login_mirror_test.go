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
	if err := l.SetPasswordPolicy(ctx, PasswordPolicy{CheckPolicy: true}); err != nil {
		t.Fatalf("SetPasswordPolicy: %v", err)
	}
	check("SetPasswordPolicy")
	if err := l.ChangePassword(ctx, "Mirr0r!Pass-2", ChangePasswordOptions{MustChange: true}); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	check("ChangePassword with MustChange")
}

// M1 (review plan 2026-10-13): on a login with CHECK_POLICY = OFF, MUST_CHANGE
// carried with the policy in the password statement applies — the old
// CHECK_EXPIRATION = ON alone was Msg 15122 — and policy rows ride along with
// a plain password change. The handle mirrors what the catalog then reads.
func TestLiveChangePasswordCarriesThePolicy(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	s, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	const name = "gosmo_live_login_policy"
	dropLoginIfPresent(t, s, name)
	defer dropLoginIfPresent(t, s, name)
	l, err := s.CreateLogin(ctx, CreateLoginRequest{Name: name, Password: "a", CheckPolicy: new(false), CheckExpiration: new(false)})
	if err != nil {
		t.Fatalf("CreateLogin: %v", err)
	}
	for i, opts := range []ChangePasswordOptions{
		{MustChange: true},
		{CheckPolicy: new(false), CheckExpiration: new(false)},
		{CheckPolicy: new(true), CheckExpiration: new(true), MustChange: true, Unlock: true},
	} {
		pw := "Mu5t!Change-" + string(rune('a'+i))
		if opts.CheckPolicy != nil && !*opts.CheckPolicy {
			pw = "b" // policy off in the same statement: a weak password is accepted
		}
		if err := l.ChangePassword(ctx, pw, opts); err != nil {
			t.Fatalf("step %d ChangePassword(%+v): %v", i, opts, err)
		}
		fresh, err := s.LoginByName(ctx, name)
		if err != nil {
			t.Fatalf("LoginByName: %v", err)
		}
		det, err := fresh.Details(ctx)
		if err != nil {
			t.Fatalf("Details: %v", err)
		}
		if det.MustChangePassword != opts.MustChange {
			t.Errorf("step %d: must change = %v, want %v", i, det.MustChangePassword, opts.MustChange)
		}
		if l.IsPolicyChecked != fresh.IsPolicyChecked || l.IsExpirationChecked != fresh.IsExpirationChecked {
			t.Errorf("step %d: handle policy %v expiration %v, catalog %v %v", i,
				l.IsPolicyChecked, l.IsExpirationChecked, fresh.IsPolicyChecked, fresh.IsExpirationChecked)
		}
	}
}
