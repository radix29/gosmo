//go:build livedb

// Live checks of the phase-5 server reads and the trace-flag write:
// ConfigurationOption.Default/HasDefault against the instance's own
// sys.configurations, Server.StartTime against its clock, and
// Server.TraceFlags/SetTraceFlag on a harmless flag.
//
//	go test -tags livedb . -run 'TestLive(ConfigurationDefaults|StartTime|TraceFlags)' -v \
//	  -livedb 'sqlserver://sa:PASS@host/sql2016?TrustServerCertificate=true&encrypt=false'
//
// The defaults test changes nothing; it logs the options the table has no
// default for (the misses a new CU brings) and every option whose configured
// value differs from its default, which on a used instance is expected and is
// for a reader to explain, not for the test to judge. The trace-flag test
// turns 3604 on globally only when nothing has it on already, and turns it
// back off in a t.Cleanup.
package gosmo

import (
	"slices"
	"testing"
	"time"
)

func TestLiveConfigurationDefaults(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	opts, err := srv.Configurations(ctx)
	if err != nil {
		t.Fatalf("Configurations: %v", err)
	}
	var misses, differ []string
	for _, o := range opts {
		switch {
		case !o.HasDefault:
			if o.Default != 0 {
				t.Errorf("%q: HasDefault false but Default %d", o.Name, o.Default)
			}
			misses = append(misses, o.Name)
		case o.Value != o.Default:
			differ = append(differ, o.Name)
			t.Logf("non-default: %-40q value %d, default %d", o.Name, o.Value, o.Default)
		}
		// A self-configuring option's 0 ("let the server decide") sits below
		// the minimum sys.configurations reports — locks 5000, max worker
		// threads 128, index create memory 704 — so 0 is exempt.
		if o.HasDefault && o.Default != 0 && (o.Default < o.Minimum || o.Default > o.Maximum) {
			// A default outside the range is a typo.
			t.Errorf("%q: default %d outside the server's range [%d, %d]", o.Name, o.Default, o.Minimum, o.Maximum)
		}
	}
	t.Logf("major %d (build %d): %d options, %d without a default %q, %d non-default",
		srv.Info().VersionMajor, srv.Info().VersionBuild, len(opts), len(misses), misses, len(differ))

	// The by-name read fills the same two fields.
	for _, name := range []string{"max degree of parallelism", "cost threshold for parallelism"} {
		i := slices.IndexFunc(opts, func(o *ConfigurationOption) bool { return o.Name == name })
		one, err := srv.ConfigurationByName(ctx, name)
		if err != nil || i < 0 {
			t.Fatalf("ConfigurationByName(%q): %v (in list: %v)", name, err, i >= 0)
		}
		if one.Default != opts[i].Default || one.HasDefault != opts[i].HasDefault || !one.HasDefault {
			t.Errorf("%q: by name Default %d/%v, from the list %d/%v", name,
				one.Default, one.HasDefault, opts[i].Default, opts[i].HasDefault)
		}
	}
}

func TestLiveStartTime(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	start, uptime, err := srv.StartTime(ctx)
	if err != nil {
		t.Fatalf("StartTime: %v", err)
	}
	var now time.Time
	if err := db.QueryRowContext(ctx, `SELECT SYSDATETIME()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	if uptime <= 0 {
		t.Errorf("uptime %v, want positive", uptime)
	}
	// Both clocks are the server's; start + uptime is "now" give or take
	// the round trip between the two reads.
	if d := now.Sub(start.Add(uptime)); d < 0 || d > 10*time.Second {
		t.Errorf("start %v + uptime %v is %v away from the server's SYSDATETIME %v", start, uptime, d, now)
	}
	if start.Location() != time.UTC {
		t.Errorf("start time in %v, want UTC (the zoneless convention)", start.Location())
	}
	t.Logf("started %s, up %s", start.Format(time.DateTime), uptime.Round(time.Second))
}

func TestLiveTraceFlags(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	const flag = 3604 // DBCC output to the client: harmless to turn on briefly
	has := func(flags []TraceFlag, n int) (TraceFlag, bool) {
		i := slices.IndexFunc(flags, func(f TraceFlag) bool { return f.Flag == n })
		if i < 0 {
			return TraceFlag{}, false
		}
		return flags[i], true
	}
	before, err := srv.TraceFlags(ctx)
	if err != nil {
		t.Fatalf("TraceFlags: %v", err)
	}
	t.Logf("enabled before: %+v", before)
	if _, on := has(before, flag); on {
		t.Skipf("trace flag %d is already on; not touching it", flag)
	}

	if err := srv.SetTraceFlag(ctx, flag, true); err != nil {
		t.Fatalf("SetTraceFlag on: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.SetTraceFlag(ctx, flag, false); err != nil {
			t.Errorf("cleanup: SetTraceFlag off: %v — trace flag %d is still on", err, flag)
		}
	})
	on, err := srv.TraceFlags(ctx)
	if err != nil {
		t.Fatalf("TraceFlags: %v", err)
	}
	if f, ok := has(on, flag); !ok || !f.Global || f.Session {
		t.Errorf("after TRACEON: %+v, %v; want %d global, not session", f, ok, flag)
	}

	if err := srv.SetTraceFlag(ctx, flag, false); err != nil {
		t.Fatalf("SetTraceFlag off: %v", err)
	}
	off, err := srv.TraceFlags(ctx)
	if err != nil {
		t.Fatalf("TraceFlags: %v", err)
	}
	if _, ok := has(off, flag); ok {
		t.Errorf("after TRACEOFF: %d still listed: %+v", flag, off)
	}

	// D6's open question: a startup-only flag is not an error, and is not
	// set — the caller has to read TraceFlags to know.
	if err := srv.SetTraceFlag(ctx, 834, true); err != nil {
		t.Errorf("SetTraceFlag(834) — startup-only: %v, want no error", err)
	}
	after, err := srv.TraceFlags(ctx)
	if err != nil {
		t.Fatalf("TraceFlags: %v", err)
	}
	if _, ok := has(after, 834); ok {
		t.Errorf("startup-only flag 834 is listed after TRACEON: %+v", after)
		_ = srv.SetTraceFlag(ctx, 834, false)
	}
}
