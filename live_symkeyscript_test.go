//go:build livedb

package gosmo

import (
	"errors"
	"strings"
	"testing"
)

// TestLiveSymmetricKeyReadScript drives the symmetric-key reads and scripter
// end to end in two scratch databases: a key encrypted three ways (certificate,
// password, asymmetric key) and one encrypted by another symmetric key are
// read back with every encryptor resolved by name; the database master key is
// in neither the list nor the finder; and each key's scripted CREATE, run in a
// second database holding same-named encryptors, recreates the same shape.
//
// There are no symmetric-key writes in gosmo yet, so the keys are made with
// plain T-SQL.
func TestLiveSymmetricKeyReadScript(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_symscript_src")
	defer dropSrc()
	dst, dropDst := liveScratchDB(t, db, ctx, "gosmo_symscript_dst")
	defer dropDst()

	const (
		pass       = "Sym!Key#Pass1"
		parentPass = "Sym!Parent#Pass2"
		name       = "round'trip"
		openParent = "OPEN SYMMETRIC KEY sk_parent DECRYPTION BY PASSWORD = N'" + parentPass + "';\n"
	)
	for _, d := range []*Database{src, dst} {
		for _, stmt := range []string{
			"CREATE USER key_owner WITHOUT LOGIN",
			"CREATE CERTIFICATE sk_cert ENCRYPTION BY PASSWORD = N'Sk!Cert#Pass' WITH SUBJECT = N'gosmo symmetric key test'",
			"CREATE ASYMMETRIC KEY sk_asym WITH ALGORITHM = RSA_2048 ENCRYPTION BY PASSWORD = N'Sk!Asym#Pass'",
			"CREATE SYMMETRIC KEY sk_parent WITH ALGORITHM = AES_128 ENCRYPTION BY PASSWORD = N'" + parentPass + "'",
		} {
			if _, err := d.exec(ctx, stmt); err != nil {
				t.Fatalf("%s in %s: %v", stmt, d.Name, err)
			}
		}
	}
	// The master key is a row of sys.symmetric_keys too; it must stay out.
	for _, stmt := range []string{
		"CREATE MASTER KEY ENCRYPTION BY PASSWORD = N'Sym!Dmk#Pass3'",
		`CREATE SYMMETRIC KEY [round'trip] AUTHORIZATION key_owner
		   WITH ALGORITHM = AES_256, KEY_SOURCE = N'gosmo source', IDENTITY_VALUE = N'gosmo identity'
		   ENCRYPTION BY CERTIFICATE sk_cert, PASSWORD = N'` + pass + `', ASYMMETRIC KEY sk_asym`,
		// The parent must be open for the CREATE, on the same connection:
		// one batch guarantees it.
		openParent + "CREATE SYMMETRIC KEY child WITH ALGORITHM = AES_192 ENCRYPTION BY SYMMETRIC KEY sk_parent;\nCLOSE SYMMETRIC KEY sk_parent;",
	} {
		if _, err := src.exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	keys, err := src.SymmetricKeys(ctx)
	if err != nil {
		t.Fatalf("SymmetricKeys: %v", err)
	}
	var names []string
	byName := map[string]*SymmetricKey{}
	for _, k := range keys {
		names = append(names, k.Name)
		byName[k.Name] = k
	}
	if strings.Join(names, ",") != "child,round'trip,sk_parent" {
		t.Fatalf("listed %q, want child, round'trip, sk_parent and no master key", names)
	}

	kinds := func(k *SymmetricKey) string {
		var s []string
		for _, e := range k.Encryptions {
			s = append(s, string(e.Kind)+" "+e.Name)
		}
		return strings.Join(s, "; ")
	}
	rt := byName[name]
	if rt.Owner != "key_owner" || rt.Algorithm != "AES_256" || rt.KeyLength != 256 ||
		rt.KeyGUID == "" || rt.CreateDate.IsZero() || rt.ProviderType != "" {
		t.Errorf("%s read back as %+v", name, rt)
	}
	if got, want := kinds(rt), "CERTIFICATE sk_cert; PASSWORD ; ASYMMETRIC KEY sk_asym"; got != want {
		t.Errorf("%s encryptions %q, want %q", name, got, want)
	}
	if got, want := kinds(byName["child"]), "SYMMETRIC KEY sk_parent"; got != want {
		t.Errorf("child encryptions %q, want %q", got, want)
	}
	for _, k := range keys {
		for _, e := range k.Encryptions {
			t.Logf("%s: %s (%x)", k.Name, e.CryptTypeDesc, e.Thumbprint)
		}
	}

	// The finder agrees with the list, and follows ErrNotFound for an absent
	// key and for the master key alike.
	one, err := src.SymmetricKeyByName(ctx, name)
	if err != nil || kinds(one) != kinds(rt) || one.KeyGUID != rt.KeyGUID {
		t.Errorf("SymmetricKeyByName: %+v, %v", one, err)
	}
	for _, n := range []string{"##MS_DatabaseMasterKey##", "no_such_key"} {
		if _, err := src.SymmetricKeyByName(ctx, n); !errors.Is(err, ErrNotFound) {
			t.Errorf("SymmetricKeyByName(%q): %v, want a not-found error", n, err)
		}
	}

	// Each key's scripted CREATE, run in the second database, recreates the
	// shape — owner, algorithm, encryptors — but not the key: a new GUID.
	for _, n := range []string{name, "child"} {
		script, err := NewScripter(src, ScriptOptions{Verb: ScriptCreate}).ScriptSymmetricKey(ctx, n)
		if err != nil {
			t.Fatalf("ScriptSymmetricKey(%q): %v", n, err)
		}
		t.Logf("script:\n%s", script)
		if !strings.Contains(script, "NEW key") {
			t.Errorf("%s: script does not say it makes a new key", n)
		}
		script = strings.ReplaceAll(script, keyPasswordPlaceholder, pass)
		for _, batch := range splitGoBatches(script) {
			if n == "child" {
				batch = openParent + batch + "\nCLOSE SYMMETRIC KEY sk_parent;"
			}
			if _, err := dst.exec(ctx, batch); err != nil {
				t.Fatalf("run script in %s: %v\n%s", dst.Name, err, batch)
			}
		}
		orig := byName[n]
		got, err := dst.SymmetricKeyByName(ctx, n)
		if err != nil {
			t.Fatalf("read recreated %s: %v", n, err)
		}
		if got.Owner != orig.Owner || got.Algorithm != orig.Algorithm || got.KeyLength != orig.KeyLength ||
			kinds(got) != kinds(orig) {
			t.Errorf("recreated %+v differs from %+v", got, orig)
		}
		if got.KeyGUID == orig.KeyGUID {
			t.Errorf("recreated %s has the original's GUID; the script cannot carry the key", n)
		}
	}

	// The DROP script, then the not-found scripter answer.
	drop, err := NewScripter(dst, ScriptOptions{Verb: ScriptDrop}).ScriptSymmetricKey(ctx, name)
	if err != nil {
		t.Fatalf("script DROP: %v", err)
	}
	for _, batch := range splitGoBatches(drop) {
		if _, err := dst.exec(ctx, batch); err != nil {
			t.Fatalf("run DROP script: %v\n%s", err, batch)
		}
	}
	if _, err := NewScripter(dst, ScriptOptions{}).ScriptSymmetricKey(ctx, name); !errors.Is(err, ErrNotFound) {
		t.Errorf("scripting a dropped key: %v, want a not-found error", err)
	}
}
