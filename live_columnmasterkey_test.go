//go:build livedb

// Live verification of the column master key writes. The point is the
// ENCLAVE_COMPUTATIONS clause: until 2026-08-21 this package emitted
// ENCLAVE_COMPUTATIONS = YES, which is not syntax SQL Server accepts —
// "Msg 102 ... Incorrect syntax near '='", a parse error no unit test that
// only compares statement text could see. The real clause takes a signature,
// and the server keeps it verbatim, so the read back here is what proves the
// statement did what it says.
//
//	go test -tags livedb . -run TestLiveColumnMasterKey -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
package gosmo

import (
	"bytes"
	"strings"
	"testing"
)

func TestLiveColumnMasterKeyWrites(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_cmk_live")
	defer drop()

	const provider = "MSSQL_CERTIFICATE_STORE"
	const keyPath = "CurrentUser/my/DEADBEEF"
	signature := []byte{0x0a, 0xff, 0x10}

	if err := d.CreateColumnMasterKeyContext(ctx, "gosmo_cmk_plain", provider, keyPath, false); err != nil {
		t.Fatalf("CreateColumnMasterKeyContext: %v", err)
	}
	if err := d.CreateColumnMasterKeyWithSignatureContext(ctx, "gosmo_cmk_enclave", provider, keyPath, signature); err != nil {
		t.Fatalf("CreateColumnMasterKeyWithSignatureContext: %v", err)
	}

	plain, err := d.ColumnMasterKeyByNameContext(ctx, "gosmo_cmk_plain")
	if err != nil {
		t.Fatalf("read back gosmo_cmk_plain: %v", err)
	}
	if plain.AllowEnclaveComputations || len(plain.Signature) != 0 {
		t.Errorf("plain key: enclave = %v, signature = %x, want false and empty",
			plain.AllowEnclaveComputations, plain.Signature)
	}

	enclave, err := d.ColumnMasterKeyByNameContext(ctx, "gosmo_cmk_enclave")
	if err != nil {
		t.Fatalf("read back gosmo_cmk_enclave: %v", err)
	}
	if !enclave.AllowEnclaveComputations {
		t.Errorf("enclave key: allow_enclave_computations = false, want true")
	}
	if !bytes.Equal(enclave.Signature, signature) {
		t.Errorf("enclave key: signature = %x, want %x", enclave.Signature, signature)
	}

	// The bool form cannot produce the clause and must say so instead of
	// reaching the server at all.
	err = d.CreateColumnMasterKeyContext(ctx, "gosmo_cmk_refused", provider, keyPath, true)
	if err == nil || !strings.Contains(err.Error(), "CreateColumnMasterKeyWithSignature") {
		t.Errorf("enclave via the bool form: err = %v, want a refusal naming CreateColumnMasterKeyWithSignature", err)
	}
	if _, err := d.ColumnMasterKeyByNameContext(ctx, "gosmo_cmk_refused"); err == nil {
		t.Errorf("gosmo_cmk_refused exists; the refusal still wrote something")
	}

	for _, k := range []*ColumnMasterKey{plain, enclave} {
		if err := k.DropContext(ctx); err != nil {
			t.Errorf("drop %s: %v", k.Name, err)
		}
	}
}

// TestLiveColumnEncryptionKeyWrites covers the CEK create the same way: the
// server keeps the encrypted value verbatim and never decrypts it at create
// time, so reading it back is what proves the WITH VALUES clause carried the
// bytes and the master key the caller named. The two-value form — what a key
// mid-master-key-rotation looks like — is where a missing comma or a repeated
// master key shows up.
func TestLiveColumnEncryptionKeyWrites(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_cek_live")
	defer drop()

	const provider = "MSSQL_CERTIFICATE_STORE"
	for _, name := range []string{"gosmo_cek_cmk1", "gosmo_cek_cmk2"} {
		if err := d.CreateColumnMasterKeyContext(ctx, name, provider, "CurrentUser/my/DEADBEEF", false); err != nil {
			t.Fatalf("CreateColumnMasterKeyContext %s: %v", name, err)
		}
	}

	// Not real key material — the server stores whatever it is given and only
	// a client decrypting a column ever finds out.
	value1 := bytes.Repeat([]byte{0x01, 0x02}, 8)
	value2 := bytes.Repeat([]byte{0x03, 0x04}, 8)

	if err := d.CreateColumnEncryptionKeyContext(ctx, "gosmo_cek_one", []ColumnEncryptionKeyValue{
		{MasterKeyName: "gosmo_cek_cmk1", EncryptionAlgorithm: "RSA_OAEP", EncryptedValue: value1},
	}); err != nil {
		t.Fatalf("CreateColumnEncryptionKeyContext (one value): %v", err)
	}
	if err := d.CreateColumnEncryptionKeyContext(ctx, "gosmo_cek_two", []ColumnEncryptionKeyValue{
		{MasterKeyName: "gosmo_cek_cmk1", EncryptionAlgorithm: "RSA_OAEP", EncryptedValue: value1},
		{MasterKeyName: "gosmo_cek_cmk2", EncryptionAlgorithm: "RSA_OAEP", EncryptedValue: value2},
	}); err != nil {
		t.Fatalf("CreateColumnEncryptionKeyContext (two values): %v", err)
	}

	one, err := d.ColumnEncryptionKeyByNameContext(ctx, "gosmo_cek_one")
	if err != nil {
		t.Fatalf("read back gosmo_cek_one: %v", err)
	}
	if len(one.Values) != 1 {
		t.Fatalf("gosmo_cek_one: %d values, want 1", len(one.Values))
	}
	if one.Values[0].MasterKeyName != "gosmo_cek_cmk1" {
		t.Errorf("gosmo_cek_one: master key = %q, want gosmo_cek_cmk1", one.Values[0].MasterKeyName)
	}
	if one.Values[0].EncryptionAlgorithm != "RSA_OAEP" {
		t.Errorf("gosmo_cek_one: algorithm = %q, want RSA_OAEP", one.Values[0].EncryptionAlgorithm)
	}
	if !bytes.Equal(one.Values[0].EncryptedValue, value1) {
		t.Errorf("gosmo_cek_one: encrypted value = %x, want %x", one.Values[0].EncryptedValue, value1)
	}

	two, err := d.ColumnEncryptionKeyByNameContext(ctx, "gosmo_cek_two")
	if err != nil {
		t.Fatalf("read back gosmo_cek_two: %v", err)
	}
	if len(two.Values) != 2 {
		t.Fatalf("gosmo_cek_two: %d values, want 2", len(two.Values))
	}
	// Each value must have landed under its own master key: a create that
	// dropped the second entry, or repeated the first master key, reads back
	// here as the same name twice.
	for i, want := range []struct {
		masterKey string
		value     []byte
	}{{"gosmo_cek_cmk1", value1}, {"gosmo_cek_cmk2", value2}} {
		got := two.Values[i]
		if got.MasterKeyName != want.masterKey {
			t.Errorf("gosmo_cek_two value %d: master key = %q, want %q", i+1, got.MasterKeyName, want.masterKey)
		}
		if !bytes.Equal(got.EncryptedValue, want.value) {
			t.Errorf("gosmo_cek_two value %d: encrypted value = %x, want %x", i+1, got.EncryptedValue, want.value)
		}
	}

	for _, k := range []*ColumnEncryptionKey{one, two} {
		if err := k.DropContext(ctx); err != nil {
			t.Errorf("drop %s: %v", k.Name, err)
		}
	}
}
