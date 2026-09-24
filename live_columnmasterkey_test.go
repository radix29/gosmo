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

	if _, err := d.CreateColumnMasterKey(ctx, CreateColumnMasterKeyRequest{Name: "gosmo_cmk_plain", KeyStoreProvider: provider, KeyPath: keyPath}); err != nil {
		t.Fatalf("CreateColumnMasterKey: %v", err)
	}
	// B2: ENCLAVE_COMPUTATIONS is 2019 syntax — below that the parser rejects
	// the whole CREATE ("Incorrect syntax near ','"), so gosmo refuses before
	// sending and there is no enclave key to read back.
	if !d.EnclaveComputationsSupported() {
		_, err := d.CreateColumnMasterKey(ctx, CreateColumnMasterKeyRequest{Name: "gosmo_cmk_enclave", KeyStoreProvider: provider, KeyPath: keyPath, Signature: signature})
		if err == nil || !strings.Contains(err.Error(), "SQL Server 2019 or later") {
			t.Errorf("enclave create below 2019: err = %v, want a refusal naming the version requirement", err)
		}
		if _, err := d.ColumnMasterKeyByName(ctx, "gosmo_cmk_enclave"); err == nil {
			t.Errorf("gosmo_cmk_enclave exists; the refusal still wrote something")
		}
	} else if _, err := d.CreateColumnMasterKey(ctx, CreateColumnMasterKeyRequest{Name: "gosmo_cmk_enclave", KeyStoreProvider: provider, KeyPath: keyPath, Signature: signature}); err != nil {
		t.Fatalf("CreateColumnMasterKey with a signature: %v", err)
	}

	plain, err := d.ColumnMasterKeyByName(ctx, "gosmo_cmk_plain")
	if err != nil {
		t.Fatalf("read back gosmo_cmk_plain: %v", err)
	}
	if plain.AllowEnclaveComputations || len(plain.Signature) != 0 {
		t.Errorf("plain key: enclave = %v, signature = %x, want false and empty",
			plain.AllowEnclaveComputations, plain.Signature)
	}

	created := []*ColumnMasterKey{plain}
	if d.EnclaveComputationsSupported() {
		enclave, err := d.ColumnMasterKeyByName(ctx, "gosmo_cmk_enclave")
		if err != nil {
			t.Fatalf("read back gosmo_cmk_enclave: %v", err)
		}
		if !enclave.AllowEnclaveComputations {
			t.Errorf("enclave key: allow_enclave_computations = false, want true")
		}
		if !bytes.Equal(enclave.Signature, signature) {
			t.Errorf("enclave key: signature = %x, want %x", enclave.Signature, signature)
		}
		created = append(created, enclave)
	}

	for _, k := range created {
		if err := k.Drop(ctx); err != nil {
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
		if _, err := d.CreateColumnMasterKey(ctx, CreateColumnMasterKeyRequest{Name: name, KeyStoreProvider: provider, KeyPath: "CurrentUser/my/DEADBEEF"}); err != nil {
			t.Fatalf("CreateColumnMasterKey %s: %v", name, err)
		}
	}

	// Not real key material — the server stores whatever it is given and only
	// a client decrypting a column ever finds out.
	value1 := bytes.Repeat([]byte{0x01, 0x02}, 8)
	value2 := bytes.Repeat([]byte{0x03, 0x04}, 8)

	if _, err := d.CreateColumnEncryptionKey(ctx, CreateColumnEncryptionKeyRequest{
		Name: "gosmo_cek_one",
		Values: []ColumnEncryptionKeyValue{
			{MasterKeyName: "gosmo_cek_cmk1", EncryptionAlgorithm: "RSA_OAEP", EncryptedValue: value1},
		},
	}); err != nil {
		t.Fatalf("CreateColumnEncryptionKey (one value): %v", err)
	}
	if _, err := d.CreateColumnEncryptionKey(ctx, CreateColumnEncryptionKeyRequest{
		Name: "gosmo_cek_two",
		Values: []ColumnEncryptionKeyValue{
			{MasterKeyName: "gosmo_cek_cmk1", EncryptionAlgorithm: "RSA_OAEP", EncryptedValue: value1},
			{MasterKeyName: "gosmo_cek_cmk2", EncryptionAlgorithm: "RSA_OAEP", EncryptedValue: value2},
		},
	}); err != nil {
		t.Fatalf("CreateColumnEncryptionKey (two values): %v", err)
	}

	one, err := d.ColumnEncryptionKeyByName(ctx, "gosmo_cek_one")
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

	two, err := d.ColumnEncryptionKeyByName(ctx, "gosmo_cek_two")
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
		if err := k.Drop(ctx); err != nil {
			t.Errorf("drop %s: %v", k.Name, err)
		}
	}
}
