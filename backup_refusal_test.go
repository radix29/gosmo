package gosmo

import "testing"

// Each refusal is a certain server error (3147 tempdb, 3002 snapshot,
// 3024/4212 master, 4208 SIMPLE, 3035 no base, 4214 no chain); what is not
// known refuses nothing.
func TestDatabaseBackupRefusal(t *testing.T) {
	full := &Database{ID: 7, Name: "AppDB", RecoveryModel: RecoveryModelFull}
	simple := &Database{ID: 8, Name: "AppDB", RecoveryModel: RecoveryModelSimple}
	master := &Database{ID: 1, Name: "master", RecoveryModel: RecoveryModelSimple}
	tempdb := &Database{ID: 2, Name: "tempdb", RecoveryModel: RecoveryModelSimple}
	snap := &Database{ID: 9, Name: "AppDB_snap", RecoveryModel: RecoveryModelFull, SourceDatabaseID: 7}
	st := func(chain, base, known bool) *DatabaseRecoveryStatus {
		return &DatabaseRecoveryStatus{LogBackupChainStarted: chain, HasDifferentialBase: base, DifferentialBaseKnown: known}
	}
	backedUp := st(true, true, true)
	never := st(false, false, true)
	const (
		F = BackupActionDatabase
		D = BackupActionDifferential
		L = BackupActionLog
	)
	for _, tc := range []struct {
		name   string
		db     *Database
		action BackupAction
		status *DatabaseRecoveryStatus
		want   BackupRefusal
	}{
		{"tempdb full", tempdb, F, backedUp, BackupRefusedTempdb},
		{"tempdb files", tempdb, BackupActionFiles, nil, BackupRefusedTempdb},
		{"snapshot full", snap, F, backedUp, BackupRefusedSnapshot},
		{"snapshot log", snap, L, backedUp, BackupRefusedSnapshot},
		{"master full", master, F, backedUp, BackupAllowed},
		{"master diff", master, D, backedUp, BackupRefusedMasterFullOnly},
		{"master log", master, L, backedUp, BackupRefusedMasterFullOnly},
		{"FULL backed up, diff", full, D, backedUp, BackupAllowed},
		{"FULL backed up, log", full, L, backedUp, BackupAllowed},
		{"FULL never backed up, full", full, F, never, BackupAllowed},
		{"FULL never backed up, diff", full, D, never, BackupRefusedNoDifferentialBase},
		{"FULL never backed up, log", full, L, never, BackupRefusedNoLogChain},
		{"FULL, full taken while SIMPLE, log", full, L, st(false, true, true), BackupRefusedNoLogChain},
		{"FULL, full taken while SIMPLE, diff", full, D, st(false, true, true), BackupAllowed},
		{"SIMPLE log", simple, L, backedUp, BackupRefusedSimpleRecovery},
		{"SIMPLE never backed up, log", simple, L, never, BackupRefusedSimpleRecovery},
		{"SIMPLE never backed up, diff", simple, D, never, BackupRefusedNoDifferentialBase},
		{"SIMPLE no status, log", simple, L, nil, BackupRefusedSimpleRecovery},
		{"FULL no status, diff", full, D, nil, BackupAllowed},
		{"FULL no status, log", full, L, nil, BackupAllowed},
		// A login that cannot see sys.master_files: an unseen base is not "no base".
		{"FULL primary file not visible, diff", full, D, st(true, false, false), BackupAllowed},
		{"empty action is full", full, "", never, BackupAllowed},
		// A Ref handle has no id: the system databases go by name.
		{"tempdb handle", &Database{Name: "TempDB"}, F, nil, BackupRefusedTempdb},
		{"master handle, diff", &Database{Name: "master"}, D, nil, BackupRefusedMasterFullOnly},
		{"handle, log", &Database{Name: "AppDB"}, L, nil, BackupAllowed},
		// With an id, the id decides: a user database named like a system one
		// on a case-sensitive server is not it.
		{"user database named MASTER", &Database{ID: 12, Name: "MASTER", RecoveryModel: RecoveryModelFull}, D, backedUp, BackupAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.db.BackupRefusal(tc.action, tc.status); got != tc.want {
				t.Errorf("BackupRefusal(%q) = %q, want %q", tc.action, got, tc.want)
			}
		})
	}
}
