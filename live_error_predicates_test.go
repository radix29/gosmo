//go:build livedb

// Live error predicates: each classifier fed the error a real server raises,
// through gosmo's own writes where there is one — so the numbers they key on
// are the ones the server sends, not the ones the documentation lists.
//
//	go test -tags livedb . -run TestLiveErrorPredicates -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops one throwaway login and its user in tempdb. Sends no mail:
// the send names a profile that does not exist, which sp_send_dbmail refuses
// before queuing anything.
package gosmo

import (
	"database/sql"
	"errors"
	"testing"
)

const (
	liveErrLogin    = "gosmo_live_errpred_login"
	liveErrPassword = "P@ssw0rd_gosmo_live_errpred"
)

func TestLiveErrorPredicates(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done) // registered first, so it runs after the drops below

	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	tempdb, err := srv.DatabaseByName(ctx, "tempdb")
	if err != nil {
		t.Fatalf("DatabaseByName(tempdb): %v", err)
	}

	// -- already exists: CREATE LOGIN (15025) and CREATE USER (15023) --------
	req := CreateLoginRequest{Name: liveErrLogin, Password: liveErrPassword, Source: LoginSourceSQL}
	if _, err := srv.CreateLogin(ctx, req); err != nil {
		t.Fatalf("CreateLogin: %v", err)
	}
	t.Cleanup(func() { _ = srv.LoginRef(liveErrLogin).Drop(ctx) })
	if _, err := srv.CreateLogin(ctx, req); !IsAlreadyExists(err) {
		t.Errorf("second CreateLogin: IsAlreadyExists(%v) = false", err)
	}
	if _, err := tempdb.CreateUser(ctx, CreateUserRequest{Name: liveErrLogin, Login: liveErrLogin}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { _ = tempdb.UserRef(liveErrLogin).Drop(ctx) })
	if _, err := tempdb.CreateUser(ctx, CreateUserRequest{Name: liveErrLogin, Login: liveErrLogin}); !IsAlreadyExists(err) {
		t.Errorf("second CreateUser: IsAlreadyExists(%v) = false", err)
	}

	// -- missing or denied: a login that is not there (15151) ----------------
	if err := srv.LoginRef(liveErrLogin + "_absent").Drop(ctx); !IsMissingOrDenied(err) || IsPermissionDenied(err) {
		t.Errorf("Drop of an absent login: IsMissingOrDenied = %v, IsPermissionDenied = %v (%v); want true, false",
			IsMissingOrDenied(err), IsPermissionDenied(err), err)
	}

	// -- permission denied, named by the first message (300 then 297) --------
	// EXECUTE AS on a pinned connection: the throwaway login has no server
	// rights, so the DMV read is refused.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()
	var n int
	err = conn.QueryRowContext(ctx, "EXECUTE AS LOGIN = @p1; SELECT COUNT(*) FROM sys.dm_os_process_memory;", liveErrLogin).Scan(&n)
	if _, rerr := conn.ExecContext(ctx, "REVERT"); rerr != nil {
		t.Logf("REVERT: %v", rerr)
	}
	kind, m := ClassifyRefusal(err)
	if kind != PermissionDenied || m == nil || m.Number != 300 {
		t.Errorf("refused DMV read: ClassifyRefusal(%v) = %v, %+v; want PermissionDenied from Msg 300", err, kind, m)
	}
	if IsAlreadyExists(err) || IsMissingOrDenied(err) {
		t.Errorf("refused DMV read classified as something else too: %v", err)
	}

	// -- Database Mail: a profile that does not exist ------------------------
	var xps int
	if err := db.QueryRowContext(ctx, "SELECT CAST(value_in_use AS int) FROM sys.configurations WHERE name = 'Database Mail XPs'").Scan(&xps); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("read Database Mail XPs: %v", err)
	}
	want := ErrMailProfileInvalid
	if xps == 0 {
		want = ErrMailXPsDisabled
	}
	_, err = srv.SendMail(ctx, MailMessage{Profile: "gosmo_live_errpred_no_such_profile", To: "nobody@example.com", Subject: "x", Body: "x"})
	if !errors.Is(err, want) {
		t.Errorf("SendMail with Database Mail XPs = %d: errors.Is(%v, %v) = false", xps, err, want)
	}
	if _, ok := AsSQLError(err); !ok {
		t.Errorf("SendMail: the SQL error is no longer reachable: %v", err)
	}
}
