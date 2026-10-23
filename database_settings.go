package gosmo

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// -- Settings ------------------------------------------------------------------

// SetRecoveryModel changes the database recovery model.
func (d *Database) SetRecoveryModel(ctx context.Context, model RecoveryModel) error {
	if !validRecoveryModel(model) {
		return invalidf("gosmo: set recovery model of %q: unrecognized recovery model %q", d.Name, model)
	}
	if _, err := d.server.exec(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET RECOVERY %s", quoteIdent(d.Name), model),
	); err != nil {
		return fmt.Errorf("gosmo: set recovery model of %q: %w", d.Name, err)
	}
	setIfApplied(ctx, &d.RecoveryModel, model)
	return nil
}

// SetCompatibilityLevel changes the database compatibility level.
func (d *Database) SetCompatibilityLevel(ctx context.Context, level CompatibilityLevel) error {
	if _, err := d.server.exec(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET COMPATIBILITY_LEVEL = %d", quoteIdent(d.Name), level),
	); err != nil {
		return fmt.Errorf("gosmo: set compatibility level of %q: %w", d.Name, err)
	}
	setIfApplied(ctx, &d.CompatibilityLevel, level)
	return nil
}

// Termination says what a statement needing exclusive access to a database
// does about the other sessions in it — the WITH <termination> clause of
// ALTER DATABASE SET, and the force of Database.Drop and Database.Rename.
type Termination int

const (
	// TerminationNone leaves the other sessions alone, as the bare statement
	// does: ALTER DATABASE SET waits for them to leave, and DROP DATABASE and
	// MODIFY NAME fail while any is connected. Nothing bounds the wait but
	// the caller's context: WITH NO_WAIT was probed on 17.0 and still waited,
	// so it is not offered.
	TerminationNone Termination = iota

	// TerminationRollbackImmediate disconnects every other session in the
	// database and rolls back its open transaction, so the statement finishes
	// now. Those sessions' uncommitted work is lost.
	TerminationRollbackImmediate
)

// withClause is t as the suffix of an ALTER DATABASE SET statement.
func (t Termination) withClause() (string, error) {
	switch t {
	case TerminationNone:
		return "", nil
	case TerminationRollbackImmediate:
		return " WITH ROLLBACK IMMEDIATE", nil
	}
	return "", invalidf("unrecognized termination %d", t)
}

// SetReadOnly sets the database to read-only or read-write. Either needs
// exclusive access to the database, so term says what happens to the other
// sessions in it; this Server's own idle sessions are released first either
// way (see Server.ReleaseIdleConnections).
func (d *Database) SetReadOnly(ctx context.Context, readOnly bool, term Termination) error {
	mode := "READ_WRITE"
	if readOnly {
		mode = "READ_ONLY"
	}
	with, err := term.withClause()
	if err != nil {
		return fmt.Errorf("gosmo: set read-only %v on %q: %w", readOnly, d.Name, err)
	}
	d.server.releaseIdle(ctx)
	if _, err := d.server.exec(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET %s%s", quoteIdent(d.Name), mode, with),
	); err != nil {
		return fmt.Errorf("gosmo: set read-only %v on %q: %w", readOnly, d.Name, err)
	}
	setIfApplied(ctx, &d.IsReadOnly, readOnly)
	return nil
}

// UserAccess is a database's user-access mode, spelled as ALTER DATABASE SET
// takes it and sys.databases.user_access_desc reports it.
type UserAccess string

const (
	UserAccessMulti      UserAccess = "MULTI_USER"
	UserAccessSingle     UserAccess = "SINGLE_USER"
	UserAccessRestricted UserAccess = "RESTRICTED_USER"
)

// userAccessModes is UserAccess's validity check. The keyword can't be
// identifier-quoted or parameterised (ALTER DATABASE is DDL), so a value
// outside the constants — a conversion from an arbitrary string — is refused
// here rather than spliced in.
var userAccessModes = map[UserAccess]bool{
	UserAccessMulti: true, UserAccessSingle: true, UserAccessRestricted: true,
}

// SetUserAccess changes the database's user-access mode (MULTI_USER,
// SINGLE_USER, or RESTRICTED_USER — SSMS's Database Properties > Options
// "Restrict access" setting). Existing connections that would violate the
// new mode are rolled back immediately, matching SSMS's own behavior.
//
// SINGLE_USER on the database this Server's pool logs in to moves the pool to
// master (see InitialDatabase): the session that ran the ALTER is the one
// user, and every other login is refused (Msg 924) — including, once that
// session idles out of the pool, the one that would set MULTI_USER back.
//
// Another session entering the database while the ALTER runs — a reader
// switching in, or reconnecting into the single-user slot ROLLBACK IMMEDIATE
// just freed — gets past the termination and fails the ALTER with one of
// userAccessContention, the ALTER usually the deadlock victim. So the ALTER
// runs at DEADLOCK_PRIORITY HIGH and is re-run, a bounded number of times, on
// those refusals only: it is idempotent and a refused one changed nothing. A
// T-SQL retry loop does not do it — on 17 both 1205 and 924 escaped its
// CATCH. Probed on 17.0 (2026-10-24, four sessions running USE into the
// database every 100 ms): nearly every ALTER failed at normal priority, 2 to
// 6 in 30 at HIGH, none with the retry (TestLiveSetUserAccessUnderContention).
// A captured script shows the bare ALTER; the priority is how gosmo runs it,
// and the pool's session reset clears it.
func (d *Database) SetUserAccess(ctx context.Context, mode UserAccess) error {
	if !userAccessModes[mode] {
		return invalidf("gosmo: set user access on %q: unrecognized mode %q", d.Name, mode)
	}
	alter := fmt.Sprintf("ALTER DATABASE %s SET %s WITH ROLLBACK IMMEDIATE", quoteIdent(d.Name), mode)
	d.server.releaseIdle(ctx)
	for attempt := 1; ; attempt++ {
		_, err := d.server.execShown(ctx, "SET DEADLOCK_PRIORITY HIGH;\n"+alter, alter)
		if err == nil {
			break
		}
		if attempt >= userAccessAttempts || !isUserAccessContention(err) {
			return fmt.Errorf("gosmo: set user access %s on %q: %w", mode, d.Name, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("gosmo: set user access %s on %q: %w", mode, d.Name, ctx.Err())
		case <-time.After(time.Duration(attempt) * userAccessRetryDelay):
		}
	}
	if mode == UserAccessSingle {
		d.server.leftLoginDatabase(ctx, d.Name, "master")
	}
	return nil
}

// userAccessContention are the refusals SetUserAccess re-runs its ALTER on —
// another session in the database as it ran, nothing wrong with the
// statement:
//
//	924  Database '…' is already open and can only have one user at a time.
//	1205 Transaction … was deadlocked … chosen as the deadlock victim.
//	5061 ALTER DATABASE failed because a lock could not be placed on database '…'.
//	5064 Changes to the state or options of database '…' cannot be made at
//	     this time. The database is in single-user mode, and a user is
//	     currently connected to it.
var userAccessContention = []int32{924, 1205, 5061, 5064}

// userAccessAttempts and userAccessRetryDelay bound SetUserAccess's re-runs:
// five tries, the nth retry after n × 200 ms — two seconds in all.
const (
	userAccessAttempts   = 5
	userAccessRetryDelay = 200 * time.Millisecond
)

// isUserAccessContention reports whether err carries, in any message of the
// batch, one of userAccessContention.
func isUserAccessContention(err error) bool {
	se, ok := AsSQLError(err)
	if !ok {
		return false
	}
	return slices.ContainsFunc(se.messages(), func(m SQLError) bool {
		return slices.Contains(userAccessContention, m.Number)
	})
}

// SetOffline takes the database offline.
//
// Existing connections are rolled back immediately, matching SSMS's Object
// Explorer "Take Database Offline" behavior. Taking offline the database this
// Server's pool logs in to moves the pool to master (see InitialDatabase).
func (d *Database) SetOffline(ctx context.Context) error {
	if _, err := d.server.exec(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET OFFLINE WITH ROLLBACK IMMEDIATE", quoteIdent(d.Name)),
	); err != nil {
		return fmt.Errorf("gosmo: set %q offline: %w", d.Name, err)
	}
	d.server.leftLoginDatabase(ctx, d.Name, "master")
	setIfApplied(ctx, &d.State, "OFFLINE")
	return nil
}

// SetOnline brings an offline database back online.
func (d *Database) SetOnline(ctx context.Context) error {
	if _, err := d.server.exec(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET ONLINE", quoteIdent(d.Name)),
	); err != nil {
		return fmt.Errorf("gosmo: set %q online: %w", d.Name, err)
	}
	setIfApplied(ctx, &d.State, "ONLINE")
	return nil
}

// RecoverFromRestoring brings a database left RESTORING online by running
// recovery on it: RESTORE DATABASE ... WITH RECOVERY, with no FROM clause.
// That is the state a RESTORE ... WITH NORECOVERY, an interrupted restore,
// or a log-shipping secondary leaves; SetOnline cannot reach it, since the
// server refuses every ALTER DATABASE on a RESTORING database (Msg 5052).
//
// The server refuses recovery while the restore sequence is incomplete —
// Msg 4303, "Additional roll forward past LSN … is required to complete the
// restore sequence", after a file restore whose log is still to restore
// (seen on 17, 2026-10-16). Its error is returned as is, so the caller can
// show what is missing.
func (d *Database) RecoverFromRestoring(ctx context.Context) error {
	if _, err := d.server.exec(ctx,
		fmt.Sprintf("RESTORE DATABASE %s WITH RECOVERY", quoteIdent(d.Name)),
	); err != nil {
		return fmt.Errorf("gosmo: recover %q from restoring: %w", d.Name, err)
	}
	setIfApplied(ctx, &d.State, "ONLINE")
	return nil
}
