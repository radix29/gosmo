//go:build livedb

// Live verification of the asymmetric key read side: that the listing and the
// by-name finder describe a key SQL Server actually holds, and agree with each
// other field by field.
//
//	go test -tags livedb . -run TestLiveAsymmetricKeys -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway key in master; touches nothing else.
package gosmo

import (
	"bytes"
	"testing"
)

func TestLiveAsymmetricKeysListingAndFinderAgree(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	s, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	master := s.Database("master")

	const keyName = "gosmo_live_asymkey"
	drop := func() {
		if k, err := master.AsymmetricKeyByNameContext(ctx, keyName); err == nil && k != nil {
			if err := s.execContext(ctx, "USE [master]; DROP ASYMMETRIC KEY "+quoteIdent(keyName)); err != nil {
				t.Logf("cleanup of asymmetric key %q: %v", keyName, err)
			}
		}
	}
	drop()
	defer drop()

	// Generated rather than imported: CREATE ASYMMETRIC KEY's import forms all
	// read the server's filesystem, which is why gosmo has no create method.
	if err := s.execContext(ctx, "USE [master]; CREATE ASYMMETRIC KEY "+quoteIdent(keyName)+" WITH ALGORITHM = RSA_2048"); err != nil {
		t.Fatalf("create asymmetric key: %v", err)
	}

	keys, err := master.AsymmetricKeysContext(ctx)
	if err != nil {
		t.Fatalf("list asymmetric keys: %v", err)
	}
	var listed *AsymmetricKey
	for _, k := range keys {
		if k.Name == keyName {
			listed = k
		}
		if len(k.Name) > 2 && k.Name[:2] == "##" {
			t.Errorf("listing included an internal key %q, which the ##%% filter should have excluded", k.Name)
		}
	}
	if listed == nil {
		t.Fatalf("the key just created is not in the listing of %d", len(keys))
	}

	found, err := master.AsymmetricKeyByNameContext(ctx, keyName)
	if err != nil {
		t.Fatalf("read asymmetric key by name: %v", err)
	}
	if found == nil {
		t.Fatal("by-name finder reported the key absent")
	}
	if found.KeyID != listed.KeyID || found.Algorithm != listed.Algorithm ||
		found.KeyLength != listed.KeyLength || found.PvtKeyEncryptionType != listed.PvtKeyEncryptionType ||
		!bytes.Equal(found.Thumbprint, listed.Thumbprint) {
		t.Errorf("finder and listing disagree:\n finder  = %+v\n listing = %+v", found, listed)
	}
	// The values the picker in gossms's New Login dialog relies on being real.
	if found.Algorithm != "RSA_2048" || found.KeyLength != 2048 {
		t.Errorf("algorithm/length read back as %q/%d, want RSA_2048/2048", found.Algorithm, found.KeyLength)
	}
	if !found.HasPrivateKey() {
		t.Errorf("a generated key reported no private key (pvt_key_encryption_type_desc = %q)", found.PvtKeyEncryptionType)
	}

	absent, err := master.AsymmetricKeyByNameContext(ctx, "gosmo_live_no_such_key")
	if err != nil || absent != nil {
		t.Errorf("absent key: got (%v, %v), want (nil, nil)", absent, err)
	}
}
