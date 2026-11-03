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

	minV, maxV := int64(-100), int64(100)
	seq, err := d.CreateSequence(ctx, CreateSequenceRequest{Schema: "dbo", Name: "s]q", DataType: DataTypeDecimal,
		Precision: 30, StartValue: 10, Increment: 5, MinValue: &minV, MaxValue: &maxV, Cache: new(0)})
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

	newMin, newMax, inc, restart := int64(-1000), int64(1000), int64(-7), int64(50)
	ch := SequenceChanges{RestartWith: &restart, Increment: &inc, MinValue: &newMin, MaxValue: &newMax,
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
