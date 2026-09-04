package gosmo

import (
	"context"
	"database/sql"
	"testing"
)

// captureCEK returns a ColumnEncryptionKey wired to the capture driver, so the
// ALTERs below actually run through Database.exec. That matters here: the
// mirroring these tests pin happens only on the executed path — under
// WithScript nothing reaches the server, so the handle is deliberately left
// describing the catalog (see TestColumnEncryptionKeyScriptingLeavesTheHandle
// in script_security_write_test.go).
func captureCEK(t *testing.T) *ColumnEncryptionKey {
	t.Helper()
	db, err := sql.Open("capture", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	captured.reset()
	return &ColumnEncryptionKey{
		db:            &Database{server: &Server{db: db}, name: "App'DB"},
		Name:          "CEK]1",
		MasterKeyName: "CMK]1", EncryptionAlgorithm: "RSA_OAEP",
		Values: []*ColumnEncryptionKeyValue{
			{MasterKeyName: "CMK]1", EncryptionAlgorithm: "RSA_OAEP", EncryptedValue: []byte{0x01}},
		}}
}

// TestColumnEncryptionKeyValuesTrackTheAlter pins that the in-memory Values
// slice follows an ALTER that ran, so a caller that adds then drops sees the
// rotation's end state without re-reading the catalog.
func TestColumnEncryptionKeyValuesTrackTheAlter(t *testing.T) {
	ctx := context.Background()
	cek := captureCEK(t)
	if err := cek.AddValueContext(ctx, ColumnEncryptionKeyValue{
		MasterKeyName: "CMK]2", EncryptionAlgorithm: "RSA_OAEP", EncryptedValue: []byte{0x02}}); err != nil {
		t.Fatalf("AddValue: %v", err)
	}
	if len(cek.Values) != 2 {
		t.Fatalf("after AddValue, Values = %d, want 2", len(cek.Values))
	}
	if err := cek.DropValueContext(ctx, "cmk]1"); err != nil { // case-insensitive, as SQL Server matches it
		t.Fatalf("DropValue: %v", err)
	}
	if len(cek.Values) != 1 || cek.Values[0].MasterKeyName != "CMK]2" {
		t.Fatalf("after DropValue, Values = %+v, want only CMK]2", cek.Values)
	}
}

// TestColumnEncryptionKeySummaryFollowsTheValues pins the summary fields to the
// first value across a whole rotation: after dropping the value they were read
// from, a caller rendering the handle it already holds would otherwise name the
// master key that was just dropped. The assertions name the surviving master
// key rather than an index, so a summary left pointing at the old one cannot
// agree with the check.
func TestColumnEncryptionKeySummaryFollowsTheValues(t *testing.T) {
	ctx := context.Background()
	cek := captureCEK(t)

	if err := cek.AddValueContext(ctx, ColumnEncryptionKeyValue{
		MasterKeyName: "CMK]2", EncryptionAlgorithm: "RSA_OAEP_256", EncryptedValue: []byte{0x02}}); err != nil {
		t.Fatalf("AddValue: %v", err)
	}
	// Adding leaves the first value first, so the summary must not move.
	if cek.MasterKeyName != "CMK]1" || cek.EncryptionAlgorithm != "RSA_OAEP" {
		t.Errorf("after AddValue, summary = %q/%q, want CMK]1/RSA_OAEP",
			cek.MasterKeyName, cek.EncryptionAlgorithm)
	}

	if err := cek.DropValueContext(ctx, "CMK]1"); err != nil {
		t.Fatalf("DropValue: %v", err)
	}
	if cek.MasterKeyName != "CMK]2" || cek.EncryptionAlgorithm != "RSA_OAEP_256" {
		t.Errorf("after dropping the first value, summary = %q/%q, want the survivor CMK]2/RSA_OAEP_256",
			cek.MasterKeyName, cek.EncryptionAlgorithm)
	}

	if err := cek.DropValueContext(ctx, "CMK]2"); err != nil {
		t.Fatalf("DropValue: %v", err)
	}
	if cek.MasterKeyName != "" || cek.EncryptionAlgorithm != "" {
		t.Errorf("with no values left, summary = %q/%q, want both empty",
			cek.MasterKeyName, cek.EncryptionAlgorithm)
	}
}
