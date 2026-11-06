//go:build livedb

// Live verification of CreateSequence's precision and Sequence.Alter: every
// clause is one the server parses, lands in sys.sequences as written, and is
// mirrored on the handle as the catalog then reads.
//
//	go test -tags livedb . -run TestLiveSequenceAlterRoundTrip -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops one throwaway database.
package gosmo

import "testing"

func TestLiveSequenceAlterRoundTrip(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	d, drop := liveScratchDB(t, db, ctx, "gosmo_sequence_alter_live")
	defer drop()

	seq, err := d.CreateSequence(ctx, CreateSequenceRequest{Schema: "dbo", Name: "s]q", DataType: DataTypeDecimal,
		Precision: 30, StartValue: "10", Increment: "5", MinValue: "-100", MaxValue: "100", Cache: new(0)})
	if err != nil {
		t.Fatalf("CreateSequence: %v", err)
	}
	if seq.Precision != 30 || seq.Scale != 0 || seq.StartValue != "10" || seq.IsCached {
		t.Errorf("created: precision %d scale %d start %s cached %v, want 30/0/10/false",
			seq.Precision, seq.Scale, seq.StartValue, seq.IsCached)
	}
	if _, err := seq.NextValue(ctx); err != nil {
		t.Fatalf("NextValue: %v", err)
	}

	ch := SequenceChanges{RestartWith: "50", Increment: "-7", MinValue: "-1000", MaxValue: "1000",
		Cycle: new(true), Cache: &SequenceCache{Enabled: true, Size: 25}}
	if err := seq.Alter(ctx, ch); err != nil {
		t.Fatalf("Alter: %v", err)
	}
	read, err := d.SequenceByName(ctx, "dbo", "s]q")
	if err != nil {
		t.Fatalf("SequenceByName: %v", err)
	}
	for _, c := range []struct{ field, got, want string }{
		{"StartValue", read.StartValue, "50"},
		{"CurrentValue", read.CurrentValue, "50"},
		{"Increment", read.Increment, "-7"},
		{"MinValue", read.MinValue, "-1000"},
		{"MaxValue", read.MaxValue, "1000"},
	} {
		if c.got != c.want {
			t.Errorf("catalog %s = %s, want %s", c.field, c.got, c.want)
		}
	}
	if !read.IsCycling || !read.IsCached || read.CacheSize != 25 {
		t.Errorf("catalog cycling %v cached %v size %d, want true/true/25", read.IsCycling, read.IsCached, read.CacheSize)
	}
	if seq.StartValue != read.StartValue || seq.CurrentValue != read.CurrentValue || seq.Increment != read.Increment ||
		seq.MinValue != read.MinValue || seq.MaxValue != read.MaxValue || seq.IsCycling != read.IsCycling ||
		seq.IsCached != read.IsCached || seq.CacheSize != read.CacheSize || seq.LastUsedValue != read.LastUsedValue {
		t.Errorf("handle after Alter %+v, catalog %+v", seq, read)
	}

	// The default cache: the server picks the size and reads it back NULL.
	if err := seq.Alter(ctx, SequenceChanges{Cache: &SequenceCache{Enabled: true}, Cycle: new(false)}); err != nil {
		t.Fatalf("Alter to the default cache: %v", err)
	}
	if read, err = d.SequenceByName(ctx, "dbo", "s]q"); err != nil {
		t.Fatalf("SequenceByName: %v", err)
	}
	if !read.IsCached || read.CacheSize != 0 || read.IsCycling || seq.CacheSize != 0 || seq.IsCycling {
		t.Errorf("default cache: catalog cached %v size %d cycling %v; handle size %d cycling %v",
			read.IsCached, read.CacheSize, read.IsCycling, seq.CacheSize, seq.IsCycling)
	}
	if err := seq.Alter(ctx, SequenceChanges{Cache: &SequenceCache{}}); err != nil {
		t.Fatalf("Alter to no cache: %v", err)
	}
	if read, err = d.SequenceByName(ctx, "dbo", "s]q"); err != nil || read.IsCached {
		t.Errorf("no cache: cached %v, err %v", read != nil && read.IsCached, err)
	}
	if err := seq.Drop(ctx); err != nil {
		t.Fatalf("Drop: %v", err)
	}
}

// A decimal(38, 0) sequence's values past bigint go through CreateSequence,
// Alter and Restart and read back as written; last_used_value is reported
// only where the server has it (TracksLastUsedValue).
func TestLiveSequencePastBigint(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	d, drop := liveScratchDB(t, db, ctx, "gosmo_sequence_bigint_live")
	defer drop()

	const (
		start = "9223372036854775808" // MaxInt64 + 1
		max37 = "10000000000000000000000000000000000000"
		min37 = "-10000000000000000000000000000000000000"
	)
	seq, err := d.CreateSequence(ctx, CreateSequenceRequest{Schema: "dbo", Name: "wide", DataType: DataTypeDecimal,
		Precision: 38, StartValue: start, Increment: "1", MinValue: min37, MaxValue: max37})
	if err != nil {
		t.Fatalf("CreateSequence: %v", err)
	}
	if seq.StartValue != start || seq.MinValue != min37 || seq.MaxValue != max37 {
		t.Errorf("created start %s min %s max %s, want %s/%s/%s", seq.StartValue, seq.MinValue, seq.MaxValue, start, min37, max37)
	}
	wantTracks := !d.before(SQLServer2017)
	if seq.TracksLastUsedValue() != wantTracks || seq.LastUsedValue != "" {
		t.Errorf("fresh: TracksLastUsedValue %v LastUsedValue %q, want %v and \"\"", seq.TracksLastUsedValue(), seq.LastUsedValue, wantTracks)
	}

	const restart = "-9223372036854775809" // MinInt64 - 1
	if err := seq.Alter(ctx, SequenceChanges{RestartWith: restart, Increment: "-1000000000000000000000"}); err != nil {
		t.Fatalf("Alter: %v", err)
	}
	read, err := d.SequenceByName(ctx, "dbo", "wide")
	if err != nil {
		t.Fatalf("SequenceByName: %v", err)
	}
	if read.StartValue != restart || read.CurrentValue != restart || read.Increment != "-1000000000000000000000" {
		t.Errorf("after Alter: start %s current %s increment %s", read.StartValue, read.CurrentValue, read.Increment)
	}
	if _, err := d.exec(ctx, "SELECT NEXT VALUE FOR [dbo].[wide]"); err != nil {
		t.Fatalf("NEXT VALUE FOR: %v", err)
	}
	if read, err = d.SequenceByName(ctx, "dbo", "wide"); err != nil {
		t.Fatalf("SequenceByName: %v", err)
	}
	if wantLast := map[bool]string{true: restart, false: ""}[wantTracks]; read.LastUsedValue != wantLast {
		t.Errorf("after one value: LastUsedValue %q, want %q", read.LastUsedValue, wantLast)
	}
	if err := read.Restart(ctx, max37); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if read, err = d.SequenceByName(ctx, "dbo", "wide"); err != nil || read.CurrentValue != max37 {
		t.Errorf("after Restart: current %v, err %v; want %s", read != nil && read.CurrentValue == max37, err, max37)
	}
	if err := seq.Drop(ctx); err != nil {
		t.Fatalf("Drop: %v", err)
	}
}

// NextValue hands back a decimal(38, 0) sequence's values past bigint as
// digits, and two calls are consecutive.
func TestLiveSequenceNextValuePastBigint(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	d, drop := liveScratchDB(t, db, ctx, "gosmo_sequence_next_live")
	defer drop()

	const start = "100000000000000000000" // 10^20
	seq, err := d.CreateSequence(ctx, CreateSequenceRequest{Schema: "dbo", Name: "wide", DataType: DataTypeDecimal,
		Precision: 38, StartValue: start, Increment: "1"})
	if err != nil {
		t.Fatalf("CreateSequence: %v", err)
	}
	for _, want := range []string{start, "100000000000000000001"} {
		got, err := seq.NextValue(ctx)
		if err != nil {
			t.Fatalf("NextValue: %v", err)
		}
		if got != want || seq.CurrentValue != want || seq.LastUsedValue != want {
			t.Errorf("NextValue = %q (CurrentValue %q, LastUsedValue %q), want %s", got, seq.CurrentValue, seq.LastUsedValue, want)
		}
	}
	read, err := d.SequenceByName(ctx, "dbo", "wide")
	if err != nil {
		t.Fatalf("SequenceByName: %v", err)
	}
	if read.CurrentValue != "100000000000000000001" {
		t.Errorf("server current_value %s, want 100000000000000000001", read.CurrentValue)
	}
}
