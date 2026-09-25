package gosmo

import (
	"context"
	"fmt"
)

// -- Settings ------------------------------------------------------------------

// SetRecoveryModel changes the database recovery model.
func (d *Database) SetRecoveryModel(ctx context.Context, model RecoveryModel) error {
	if !validRecoveryModel(model) {
		return fmt.Errorf("gosmo: set recovery model: unrecognized recovery model %q", model)
	}
	if err := d.server.exec(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET RECOVERY %s", quoteIdent(d.Name), model),
	); err != nil {
		return fmt.Errorf("gosmo: set recovery model: %w", err)
	}
	setIfApplied(ctx, &d.RecoveryModel, model)
	return nil
}

// SetCompatibilityLevel changes the database compatibility level.
func (d *Database) SetCompatibilityLevel(ctx context.Context, level CompatibilityLevel) error {
	if err := d.server.exec(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET COMPATIBILITY_LEVEL = %d", quoteIdent(d.Name), level),
	); err != nil {
		return fmt.Errorf("gosmo: set compatibility level: %w", err)
	}
	setIfApplied(ctx, &d.CompatibilityLevel, level)
	return nil
}

// Termination says what an ALTER DATABASE needing exclusive access does about
// the other sessions in the database — the WITH <termination> clause of ALTER
// DATABASE SET.
type Termination int

const (
	// TerminationNone waits for the other sessions to leave, as the bare
	// statement does. Nothing bounds the wait but the caller's context: WITH
	// NO_WAIT was probed on 17.0 and still waited, so it is not offered.
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
	return "", fmt.Errorf("unrecognized termination %d", t)
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
		return fmt.Errorf("gosmo: set read-only %v: %w", readOnly, err)
	}
	d.server.releaseIdle(ctx)
	if err := d.server.exec(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET %s%s", quoteIdent(d.Name), mode, with),
	); err != nil {
		return fmt.Errorf("gosmo: set read-only %v: %w", readOnly, err)
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
func (d *Database) SetUserAccess(ctx context.Context, mode UserAccess) error {
	if !userAccessModes[mode] {
		return fmt.Errorf("gosmo: set user access: unrecognized mode %q", mode)
	}
	if err := d.server.exec(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET %s WITH ROLLBACK IMMEDIATE", quoteIdent(d.Name), mode),
	); err != nil {
		return fmt.Errorf("gosmo: set user access %s: %w", mode, err)
	}
	return nil
}

// SetOffline takes the database offline.
//
// Existing connections are rolled back immediately, matching SSMS's Object
// Explorer "Take Database Offline" behavior.
func (d *Database) SetOffline(ctx context.Context) error {
	if err := d.server.exec(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET OFFLINE WITH ROLLBACK IMMEDIATE", quoteIdent(d.Name)),
	); err != nil {
		return fmt.Errorf("gosmo: set offline: %w", err)
	}
	setIfApplied(ctx, &d.State, "OFFLINE")
	return nil
}

// SetOnline brings an offline database back online.
func (d *Database) SetOnline(ctx context.Context) error {
	if err := d.server.exec(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET ONLINE", quoteIdent(d.Name)),
	); err != nil {
		return fmt.Errorf("gosmo: set online: %w", err)
	}
	setIfApplied(ctx, &d.State, "ONLINE")
	return nil
}
