//go:build livedb

// Live InTransaction over msdb (gossms N2): Database Mail's sysmail_*
// procedures and the job-step procedures run inside a user transaction and
// roll back with it. Each half writes something, then fails a later
// statement, and expects the earlier write gone — the Database Mail half
// includes the server credential a Basic account's password creates, the job
// half a step reorder, whose own BEGIN/COMMIT batch nests in the
// transaction.
//
//	go test -tags livedb . -run TestLiveInTransactionMSDB -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates one throwaway job, dropped afterwards; the mail objects never
// commit, and are dropped anyway if a failed run left them.
package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

const (
	liveTxMailAccount = "gosmo_live_tx_mail_acct"
	liveTxMailProfile = "gosmo_live_tx_mail_prof"
	liveTxJob         = "gosmo_live_tx_job"
)

func TestLiveInTransactionMSDB(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done) // registered first, so it runs after every drop below

	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	t.Cleanup(func() {
		c := context.Background()
		for _, stmt := range []string{
			`IF EXISTS (SELECT 1 FROM msdb.dbo.sysmail_profile WHERE name = N'` + liveTxMailProfile + `') EXEC msdb.dbo.sysmail_delete_profile_sp @profile_name = N'` + liveTxMailProfile + `'`,
			`IF EXISTS (SELECT 1 FROM msdb.dbo.sysmail_account WHERE name = N'` + liveTxMailAccount + `') EXEC msdb.dbo.sysmail_delete_account_sp @account_name = N'` + liveTxMailAccount + `'`,
			`IF EXISTS (SELECT 1 FROM msdb.dbo.sysjobs WHERE name = N'` + liveTxJob + `') EXEC msdb.dbo.sp_delete_job @job_name = N'` + liveTxJob + `'`,
		} {
			if _, err := db.ExecContext(c, stmt); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	})
	credentials := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.credentials`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("Database Mail", func(t *testing.T) {
		before := credentials()
		var inside int
		reached := false
		err := srv.InTransaction(ctx, func(ctx context.Context) error {
			if _, err := srv.CreateMailAccount(ctx, CreateMailAccountRequest{
				Name: liveTxMailAccount, EmailAddress: "dba@example.com", ServerName: "192.0.2.1", Timeout: 30,
				Credentials: MailCredentials{Authentication: MailAuthBasic, UserName: "mailer", Password: "P@ssw0rd_gosmo_tx"},
			}); err != nil {
				return err
			}
			if _, err := srv.CreateMailProfile(ctx, CreateMailProfileRequest{Name: liveTxMailProfile}); err != nil {
				return err
			}
			if err := srv.MailProfileRef(liveTxMailProfile).AddAccount(ctx, liveTxMailAccount, 1); err != nil {
				return err
			}
			if err := srv.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&inside) }, `SELECT COUNT(*) FROM sys.credentials`); err != nil {
				return err
			}
			reached = true
			// Fails: there is no such account.
			return srv.MailProfileRef(liveTxMailProfile).AddAccount(ctx, "gosmo_live_tx_no_such_account", 2)
		})
		if err == nil || !reached {
			t.Fatalf("the transaction ended before its last statement, or that succeeded: %v", err)
		}
		if inside != before+1 {
			t.Errorf("credentials inside the transaction = %d, want %d — the premise that the password is a credential", inside, before+1)
		}
		if _, err := srv.MailAccountByName(ctx, liveTxMailAccount); !errors.Is(err, ErrNotFound) {
			t.Errorf("the account survived the rollback: %v", err)
		}
		if _, err := srv.MailProfileByName(ctx, liveTxMailProfile); !errors.Is(err, ErrNotFound) {
			t.Errorf("the profile survived the rollback: %v", err)
		}
		if got := credentials(); got != before {
			t.Errorf("credentials after the rollback = %d, want %d", got, before)
		}
	})

	t.Run("job steps", func(t *testing.T) {
		j, err := srv.CreateJob(ctx, CreateJobRequest{Name: liveTxJob})
		if err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		step := func(name, command string) JobStepRequest {
			return JobStepRequest{Name: name, Subsystem: "TSQL", Command: command, OnSuccessAction: 1, OnFailAction: 2}
		}
		for _, name := range []string{"first", "second"} {
			if _, err := j.AddStep(ctx, step(name, "SELECT N'"+name+"'")); err != nil {
				t.Fatalf("AddStep %s: %v", name, err)
			}
		}
		reached := false
		err = srv.InTransaction(ctx, func(ctx context.Context) error {
			steps, err := j.Steps(ctx)
			if err != nil {
				return err
			}
			if err := steps[0].Alter(ctx, step("first", "SELECT N'changed'")); err != nil {
				return err
			}
			if err := j.MoveStep(ctx, 2, 1); err != nil {
				return err
			}
			reached = true
			// Fails: no such subsystem.
			_, err = j.AddStep(ctx, JobStepRequest{Name: "third", Subsystem: "NoSuchSubsystem", Command: "x", OnSuccessAction: 1, OnFailAction: 2})
			return err
		})
		if err == nil || !reached {
			t.Fatalf("the transaction ended before its last statement, or that succeeded: %v", err)
		}
		steps, err := j.Steps(ctx)
		if err != nil {
			t.Fatalf("Steps: %v", err)
		}
		if len(steps) != 2 || steps[0].Name != "first" || steps[0].Command != "SELECT N'first'" || steps[1].Name != "second" {
			var got []string
			for _, s := range steps {
				got = append(got, s.Name+": "+s.Command)
			}
			t.Errorf("after the rollback the steps are %q, want first (unchanged), second", got)
		}
	})
}
