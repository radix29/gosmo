//go:build livedb

// Live verification of the setIfApplied rule for the state-mirroring setters
// on Database and ConfigurationOption. script_test.go pins both halves against
// the capture driver, but only a server can say that a real apply moved the
// server too — a setter that mirrored onto the handle without the ALTER
// landing would pass the unit test and fail here.
//
//	go test -tags livedb . -run TestLiveScriptedSetter -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; the one instance-scoped
// setter (ConfigurationOption.SetValue) is exercised scripted only, so
// nothing here changes the server's own configuration. Skipped entirely
// without -livedb.
package gosmo

import (
	"testing"
)

func TestLiveScriptedSetterMirroring(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv := &Server{db: db}

	const name = "gosmo_setifapplied_probe"
	_ = srv.DropDatabaseContext(ctx, name, true)
	if err := srv.CreateDatabaseContext(ctx, name, nil); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	defer func() {
		if err := srv.DropDatabaseContext(ctx, name, true); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	}()

	// reload reads the database back off sys.databases — the server's own
	// answer, never the handle's memory. Every assertion below checks both,
	// because the bug being pinned is exactly the two disagreeing.
	reload := func() *Database {
		t.Helper()
		d, err := srv.DatabaseByNameContext(ctx, name)
		if err != nil {
			t.Fatalf("reload %s: %v", name, err)
		}
		return d
	}

	t.Logf("created with recovery=%s readonly=%v compat=%d state=%s",
		reload().RecoveryModel(), reload().IsReadOnly(),
		reload().CompatibilityLevel(), reload().State())

	// 1. Scripted: for every setter, neither the handle nor the server may
	// move. All five run under one collector, each asking for a value the
	// database does not currently hold.
	d := reload()
	was := struct {
		recovery RecoveryModel
		compat   CompatibilityLevel
		readOnly bool
		state    string
	}{d.RecoveryModel(), d.CompatibilityLevel(), d.IsReadOnly(), d.State()}

	sctx, script := WithScript(ctx)
	for _, step := range []struct {
		name string
		run  func() error
	}{
		{"SetRecoveryModel", func() error { return d.SetRecoveryModelContext(sctx, RecoveryModelSimple) }},
		{"SetCompatibilityLevel", func() error { return d.SetCompatibilityLevelContext(sctx, 150) }},
		{"SetReadOnly", func() error { return d.SetReadOnlyContext(sctx, true) }},
		{"SetOffline", func() error { return d.SetOfflineContext(sctx) }},
		{"SetOnline", func() error { return d.SetOnlineContext(sctx) }},
	} {
		if err := step.run(); err != nil {
			t.Fatalf("scripted %s: %v", step.name, err)
		}
		// Checked per step, not only at the end: SetOffline and SetOnline
		// write the same field in opposite directions, so a pair that both
		// mirror wrongly lands back on the starting value and a single
		// end-of-loop assertion passes on the bug.
		if got := d.State(); got != was.state {
			t.Errorf("scripted %s moved the handle's state to %s, want it left at %s", step.name, got, was.state)
		}
	}
	if len(script.Statements) != 5 {
		t.Fatalf("Statements = %v, want five", script.Statements)
	}
	for _, s := range script.Statements {
		t.Logf("scripted statement: %s", s)
	}
	srvNow := reload()
	for _, c := range []struct {
		what   string
		handle any
		server any
		want   any
	}{
		{"recovery model", d.RecoveryModel(), srvNow.RecoveryModel(), was.recovery},
		{"compatibility level", d.CompatibilityLevel(), srvNow.CompatibilityLevel(), was.compat},
		{"read-only", d.IsReadOnly(), srvNow.IsReadOnly(), was.readOnly},
		{"state", d.State(), srvNow.State(), was.state},
	} {
		if c.handle != c.want {
			t.Errorf("scripted setters moved the handle's %s to %v, want it left at %v", c.what, c.handle, c.want)
		}
		if c.server != c.want {
			t.Errorf("scripted setters moved the SERVER's %s to %v, want it left at %v", c.what, c.server, c.want)
		}
	}

	// 1b. SetOnline again, this time from a database that really is offline.
	// Scripted from an online one it assigns "ONLINE" over "ONLINE", so the
	// bug leaves no trace and the pass above cannot see it — the only state
	// that distinguishes a mirroring SetOnline is one it would be changing.
	if err := reload().SetOfflineContext(ctx); err != nil {
		t.Fatalf("SetOffline (to set up the scripted SetOnline): %v", err)
	}
	off := reload()
	if off.State() != "OFFLINE" {
		t.Fatalf("setup: server reports %s, want OFFLINE", off.State())
	}
	octx, oscript := WithScript(ctx)
	if err := off.SetOnlineContext(octx); err != nil {
		t.Fatalf("scripted SetOnline: %v", err)
	}
	if len(oscript.Statements) != 1 {
		t.Fatalf("Statements = %v, want one", oscript.Statements)
	}
	if got := off.State(); got != "OFFLINE" {
		t.Errorf("scripted SetOnline moved the handle's state to %s, want it left at OFFLINE", got)
	}
	if got := reload().State(); got != "OFFLINE" {
		t.Errorf("scripted SetOnline moved the SERVER to %s, want it left at OFFLINE", got)
	}
	if err := reload().SetOnlineContext(ctx); err != nil {
		t.Fatalf("SetOnline (restoring the probe database): %v", err)
	}

	// 2. A captured statement, run for real, must produce the change — the
	// handle being left alone is only correct if the script is what applies it.
	if err := srv.execContext(ctx, script.Statements[0]); err != nil {
		t.Fatalf("running the captured statement: %v", err)
	}
	if got := reload().RecoveryModel(); got != RecoveryModelSimple {
		t.Errorf("after running the captured statement the server reports %s, want SIMPLE", got)
	}

	// 3. Applied for real: handle and server must agree, on every setter.
	d = reload()
	if err := d.SetRecoveryModelContext(ctx, RecoveryModelFull); err != nil {
		t.Fatalf("SetRecoveryModel: %v", err)
	}
	if got, srvGot := d.RecoveryModel(), reload().RecoveryModel(); got != RecoveryModelFull || srvGot != RecoveryModelFull {
		t.Errorf("recovery: handle=%s server=%s, want both FULL", got, srvGot)
	}
	if err := d.SetCompatibilityLevelContext(ctx, 160); err != nil {
		t.Fatalf("SetCompatibilityLevel: %v", err)
	}
	if got, srvGot := d.CompatibilityLevel(), reload().CompatibilityLevel(); got != 160 || srvGot != 160 {
		t.Errorf("compat: handle=%d server=%d, want both 160", got, srvGot)
	}
	if err := d.SetReadOnlyContext(ctx, true); err != nil {
		t.Fatalf("SetReadOnly: %v", err)
	}
	if got, srvGot := d.IsReadOnly(), reload().IsReadOnly(); !got || !srvGot {
		t.Errorf("readonly: handle=%v server=%v, want both true", got, srvGot)
	}
	if err := d.SetReadOnlyContext(ctx, false); err != nil {
		t.Fatalf("SetReadOnly back to read-write: %v", err)
	}
	if err := d.SetOfflineContext(ctx); err != nil {
		t.Fatalf("SetOffline: %v", err)
	}
	if got, srvGot := d.State(), reload().State(); got != "OFFLINE" || srvGot != "OFFLINE" {
		t.Errorf("offline: handle=%s server=%s, want both OFFLINE", got, srvGot)
	}
	if err := d.SetOnlineContext(ctx); err != nil {
		t.Fatalf("SetOnline: %v", err)
	}
	if got, srvGot := d.State(), reload().State(); got != "ONLINE" || srvGot != "ONLINE" {
		t.Errorf("online: handle=%s server=%s, want both ONLINE", got, srvGot)
	}

	// 4. ConfigurationOption, scripted only: a real sp_configure would change
	// the shared instance, which the throwaway-object discipline rules out.
	c, err := srv.ConfigurationByNameContext(ctx, "max server memory (MB)")
	if err != nil {
		t.Fatalf("configuration by name: %v", err)
	}
	cbefore := c.Value
	cctx, cscript := WithScript(ctx)
	if err := c.SetValueContext(cctx, cbefore-1); err != nil {
		t.Fatalf("scripted SetValue: %v", err)
	}
	t.Logf("scripted config statement: %s", cscript.Statements[0])
	if c.Value != cbefore {
		t.Errorf("scripted SetValue moved the handle to %d, want it left at %d", c.Value, cbefore)
	}
	reloadedC, err := srv.ConfigurationByNameContext(ctx, "max server memory (MB)")
	if err != nil {
		t.Fatalf("configuration reload: %v", err)
	}
	if reloadedC.Value != cbefore {
		t.Errorf("scripted SetValue moved the SERVER to %d, want %d", reloadedC.Value, cbefore)
	}
}

// Sequence.RestartContext is the same rule on a different object, and worth a
// live check of its own because "current value" is the one field a caller
// reads back immediately: a scripted restart that mirrored anyway left the
// handle claiming a value NEXT VALUE FOR would not produce for a long time.
//
// Creates and drops its own throwaway database, same as above.
func TestLiveScriptedSequenceRestartMirroring(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv := &Server{db: db}

	const name = "gosmo_seq_restart_probe"
	_ = srv.DropDatabaseContext(ctx, name, true)
	if err := srv.CreateDatabaseContext(ctx, name, nil); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	defer func() {
		if err := srv.DropDatabaseContext(ctx, name, true); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	}()

	d, err := srv.DatabaseByNameContext(ctx, name)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	if err := d.CreateSequenceContext(ctx, CreateSequenceRequest{
		Schema: "dbo", Name: "probe_seq", StartValue: 1, Increment: 1,
	}); err != nil {
		t.Fatalf("create sequence: %v", err)
	}

	// reload reads current_value back off sys.sequences — the server's own
	// answer, never the handle's memory.
	reload := func() *Sequence {
		t.Helper()
		seqs, err := d.SequencesContext(ctx)
		if err != nil {
			t.Fatalf("list sequences: %v", err)
		}
		for _, s := range seqs {
			if s.Name == "probe_seq" {
				return s
			}
		}
		t.Fatal("probe_seq is gone")
		return nil
	}

	seq := reload()
	before := seq.CurrentValue
	t.Logf("created at current_value=%d", before)

	// 1. Scripted: neither the handle nor the server may move.
	sctx, script := WithScript(ctx)
	if err := seq.RestartContext(sctx, before+5000); err != nil {
		t.Fatalf("scripted restart: %v", err)
	}
	if len(script.Statements) != 1 {
		t.Fatalf("Statements = %v, want one", script.Statements)
	}
	t.Logf("scripted statement: %s", script.Statements[0])
	if seq.CurrentValue != before {
		t.Errorf("scripted restart moved the handle to %d, want it left at %d", seq.CurrentValue, before)
	}
	if got := reload().CurrentValue; got != before {
		t.Errorf("scripted restart moved the SERVER to %d, want it left at %d", got, before)
	}

	// 2. The captured statement, run for real, must produce the change — the
	// handle being left alone is only correct if the script is what applies it.
	if _, err := d.exec(ctx, script.Statements[0]); err != nil {
		t.Fatalf("running the captured statement: %v", err)
	}
	if got := reload().CurrentValue; got != before+5000 {
		t.Errorf("after running the captured statement the server reports %d, want %d", got, before+5000)
	}

	// 3. Applied for real: handle and server must agree, and the next value
	// the server actually hands out has to come from there — the whole point
	// of the field is predicting that.
	seq = reload()
	if err := seq.RestartContext(ctx, 7777); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if got, srvGot := seq.CurrentValue, reload().CurrentValue; got != 7777 || srvGot != 7777 {
		t.Errorf("restart: handle=%d server=%d, want both 7777", got, srvGot)
	}
	next, err := seq.NextValueContext(ctx)
	if err != nil {
		t.Fatalf("next value: %v", err)
	}
	if next != 7777 {
		t.Errorf("NEXT VALUE FOR returned %d, want 7777 — the restart the handle claims is not the one the server did", next)
	}
}
