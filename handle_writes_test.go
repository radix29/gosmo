package gosmo

import (
	"context"
	"testing"
)

// The tests here pin what the handle-centric write surface promises beyond the
// statement text the script_*_write_test.go files already check: that a
// listing hands back handles wired to their parent, and that a handle write
// mirrors onto its receiver only when it reached the server.

// A file or filegroup handle from a Ref answers Database() with the database
// it came from — the writes build their ALTER DATABASE from it, so a handle
// with a nil parent would panic on its first write.
func TestFileAndFileGroupRefsCarryTheirDatabase(t *testing.T) {
	d := scriptTestDB()
	if got := d.FileRef("AppDat").Database(); got != d {
		t.Errorf("FileRef.Database() = %p, want %p", got, d)
	}
	if got := d.FileGroupRef("FG2").Database(); got != d {
		t.Errorf("FileGroupRef.Database() = %p, want %p", got, d)
	}
	s := &Server{}
	if got := s.CategoryRef(CategoryClassJob, "c").Server(); got != s {
		t.Errorf("CategoryRef.Server() = %p, want %p", got, s)
	}
}

// Under WithScript nothing ran, so a renaming Alter and the filegroup flags
// leave the handle describing what is actually there (setIfApplied); against
// a server that accepted the statement they follow it.
func TestFileHandleWritesMirrorOnlyWhenApplied(t *testing.T) {
	ctx, _ := WithScript(context.Background())
	f := scriptTestDB().FileRef("AppDat")
	if err := f.Alter(ctx, FileModify{NewName: "AppDat2"}); err != nil {
		t.Fatalf("Alter under WithScript: %v", err)
	}
	fg := scriptTestDB().FileGroupRef("FG2")
	if err := fg.SetReadOnly(ctx, true, TerminationNone); err != nil {
		t.Fatalf("SetReadOnly under WithScript: %v", err)
	}
	if err := fg.SetDefault(ctx); err != nil {
		t.Fatalf("SetDefault under WithScript: %v", err)
	}
	if f.Name != "AppDat" || fg.IsReadOnly || fg.IsDefault {
		t.Errorf("scripted writes mirrored: file %q, filegroup read-only=%v default=%v", f.Name, fg.IsReadOnly, fg.IsDefault)
	}

	d := &Database{server: captureServer(t, 17), Name: "App"}
	f = d.FileRef("AppDat")
	if err := f.Alter(t.Context(), FileModify{NewName: "AppDat2"}); err != nil {
		t.Fatalf("Alter: %v", err)
	}
	fg = d.FileGroupRef("FG2")
	if err := fg.SetReadOnly(t.Context(), true, TerminationNone); err != nil {
		t.Fatalf("SetReadOnly: %v", err)
	}
	if err := fg.SetDefault(t.Context()); err != nil {
		t.Fatalf("SetDefault: %v", err)
	}
	if f.Name != "AppDat2" || !fg.IsReadOnly || !fg.IsDefault {
		t.Errorf("applied writes not mirrored: file %q, filegroup read-only=%v default=%v", f.Name, fg.IsReadOnly, fg.IsDefault)
	}
}

// AddStep and InsertStep under WithScript return a handle on the job, named
// as requested, with the position InsertStep was given — there is no step to
// read back, and its number is the only other thing the caller already knows.
func TestScriptedStepAddsReturnAHandle(t *testing.T) {
	ctx, script := WithScript(context.Background())
	j := (&Server{}).JobRef("Nightly")
	added, err := j.AddStep(ctx, JobStepRequest{Name: "a", Subsystem: "TSQL", Command: "SELECT 1"})
	if err != nil {
		t.Fatalf("AddStep: %v", err)
	}
	inserted, err := j.InsertStep(ctx, JobStepRequest{Name: "b", Subsystem: "TSQL", Command: "SELECT 2"}, 1)
	if err != nil {
		t.Fatalf("InsertStep: %v", err)
	}
	if added.job != j || added.Name != "a" || added.StepID != 0 {
		t.Errorf("AddStep handle = %+v, want step \"a\" of the job with no number", added)
	}
	if inserted.job != j || inserted.Name != "b" || inserted.StepID != 1 {
		t.Errorf("InsertStep handle = %+v, want step \"b\" of the job at 1", inserted)
	}
	if script.Len() != 2 || len(script.Entries()) != 2 {
		t.Errorf("Len() = %d, Entries() = %d, want 2 and 2", script.Len(), len(script.Entries()))
	}
}

// Entries returns a copy: a caller appending to or editing the slice it got
// cannot change what the collector renders.
func TestScriptCollectorEntriesIsACopy(t *testing.T) {
	ctx, script := WithScript(context.Background())
	if err := (&Server{}).CategoryRef(CategoryClassJob, "c").Drop(ctx); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	e := script.Entries()
	e[0].SQL = "DROP DATABASE [prod]"
	if got := script.Entries()[0].SQL; got == e[0].SQL {
		t.Errorf("editing Entries()'s result changed the collector: %q", got)
	}
}
