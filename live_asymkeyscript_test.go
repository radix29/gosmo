//go:build livedb

package gosmo

import (
	"errors"
	"strings"
	"testing"
)

// TestLiveAsymmetricKeyCreateScriptDrop drives the asymmetric-key write side
// and scripter end to end in two scratch databases: CreateAsymmetricKey with
// an owner, a password and RSA_4096 (major 13 is the floor it needs), the
// scripted CREATE run in a second database with its password placeholder
// filled in, and DROP from a name-only handle.
//
// The script makes a new key pair, so what it must reproduce is the
// algorithm, the owner and the protection — not the thumbprint, which it
// must not reproduce.
func TestLiveAsymmetricKeyCreateScriptDrop(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_asymscript_src")
	defer dropSrc()
	dst, dropDst := liveScratchDB(t, db, ctx, "gosmo_asymscript_dst")
	defer dropDst()

	for _, d := range []*Database{src, dst} {
		if _, err := d.exec(ctx, "CREATE USER key_owner WITHOUT LOGIN"); err != nil {
			t.Fatalf("create owner in %s: %v", d.Name, err)
		}
	}

	const name, pass = "round'trip", "R0und!Trip#Pass"
	if err := src.CreateAsymmetricKey(ctx, AsymmetricKeySpec{
		Name: name, Authorization: "key_owner", Algorithm: AsymmetricKeyRSA4096, EncryptionPassword: pass,
	}); err != nil {
		t.Fatalf("CreateAsymmetricKey: %v", err)
	}
	orig, err := src.AsymmetricKeyByName(ctx, name)
	if err != nil || orig == nil {
		t.Fatalf("read source key: %v, %v", orig, err)
	}
	if orig.Owner != "key_owner" || orig.Algorithm != "RSA_4096" || orig.KeyLength != 4096 ||
		orig.PvtKeyEncryptionType != "ENCRYPTED_BY_PASSWORD" {
		t.Errorf("source key read back as %+v", orig)
	}

	// No master key in a scratch database: a key with no password must fail
	// there (Msg 15581), which is the precondition a New dialog checks for.
	if err := src.CreateAsymmetricKey(ctx, AsymmetricKeySpec{Name: "no_dmk", Algorithm: AsymmetricKeyRSA2048}); err == nil {
		t.Error("a master-key-protected key was created in a database with no master key")
	}

	script, err := NewScripter(src, ScriptOptions{Verb: ScriptCreate}).ScriptAsymmetricKey(ctx, name)
	if err != nil {
		t.Fatalf("ScriptAsymmetricKey: %v", err)
	}
	t.Logf("script:\n%s", script)
	if !strings.Contains(script, "NEW key pair") || !strings.Contains(script, keyPasswordPlaceholder) {
		t.Errorf("script does not say it makes a new key pair, or lacks the password placeholder:\n%s", script)
	}
	script = strings.ReplaceAll(script, keyPasswordPlaceholder, pass)
	for _, batch := range splitGoBatches(script) {
		if _, err := dst.exec(ctx, batch); err != nil {
			t.Fatalf("run script in %s: %v\n%s", dst.Name, err, batch)
		}
	}

	got, err := dst.AsymmetricKeyByName(ctx, name)
	if err != nil || got == nil {
		t.Fatalf("read recreated key: %v, %v", got, err)
	}
	if got.Owner != orig.Owner || got.Algorithm != orig.Algorithm || got.KeyLength != orig.KeyLength ||
		got.PvtKeyEncryptionType != orig.PvtKeyEncryptionType {
		t.Errorf("recreated key %+v differs from %+v", got, orig)
	}
	if string(got.Thumbprint) == string(orig.Thumbprint) {
		t.Error("recreated key has the original's thumbprint; the script cannot carry the key")
	}

	// DROP from a name-only handle, and the not-found scripter answer.
	if err := dst.AsymmetricKeyRef(name).Drop(ctx); err != nil {
		t.Fatalf("drop through AsymmetricKeyRef: %v", err)
	}
	if _, err := NewScripter(dst, ScriptOptions{}).ScriptAsymmetricKey(ctx, name); !errors.Is(err, ErrNotFound) {
		t.Errorf("scripting a dropped key: %v, want a not-found error", err)
	}
}
