//go:build livedb

package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// liveMsg returns err's SQL Server error number, 0 for none.
func liveMsg(err error) int32 {
	if e, ok := AsSQLError(err); ok {
		return e.Number
	}
	return 0
}

// TestLiveSymmetricKeyWrites drives CreateSymmetricKey, AddEncryption,
// DropEncryption and Drop in a scratch database, opening the key by every
// decryptor kind step 0 of the keys plan confirmed: a password, a certificate
// the master key protects (no password), a password-protected certificate
// (its private key's password), an asymmetric key, and a symmetric key that is
// itself opened by a password — a chain.
func TestLiveSymmetricKeyWrites(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_symwrite")
	defer drop()
	d2, drop2 := liveScratchDB(t, db, ctx, "gosmo_symwrite2")
	defer drop2()

	const (
		certPass = "Sk!Cert#Pass"
		asymPass = "Sk!Asym#Pass"
		pass     = "Sym!Key#Pass1"
		parentPw = "Sym!Parent#Pass2"
	)
	liveExecIn(t, d, ctx,
		"CREATE MASTER KEY ENCRYPTION BY PASSWORD = N'Sym!Dmk#Pass3'",
		"CREATE USER key_owner WITHOUT LOGIN",
		"CREATE CERTIFICATE dmk_cert WITH SUBJECT = N'gosmo dmk cert'",
		"CREATE CERTIFICATE pw_cert ENCRYPTION BY PASSWORD = N'"+certPass+"' WITH SUBJECT = N'gosmo pw cert'",
		"CREATE ASYMMETRIC KEY sk_asym WITH ALGORITHM = RSA_2048 ENCRYPTION BY PASSWORD = N'"+asymPass+"'",
	)

	kinds := func(name string) string {
		t.Helper()
		k, err := d.SymmetricKeyByName(ctx, name)
		if err != nil {
			t.Fatalf("SymmetricKeyByName(%q): %v", name, err)
		}
		var s []string
		for _, e := range k.Encryptions {
			s = append(s, strings.TrimSpace(string(e.Kind)+" "+e.Name))
		}
		return strings.Join(s, "; ")
	}

	byPassword := func(pw string) SymmetricKeyDecryptor {
		return SymmetricKeyDecryptor{Kind: SymmetricKeyByPassword, Password: pw}
	}
	parentOpen := &SymmetricKeyDecryptor{Kind: SymmetricKeyByPassword, Password: parentPw}

	// A parent, and a key created encrypted by it: the parent is opened for
	// the CREATE in the same batch.
	for _, spec := range []SymmetricKeySpec{
		{Name: "parent", Algorithm: SymmetricKeyAES128,
			Encryptions: []SymmetricKeyEncryptor{{Kind: SymmetricKeyByPassword, Password: parentPw}}},
		{Name: "k'ey", Authorization: "key_owner", Algorithm: SymmetricKeyAES256,
			Encryptions: []SymmetricKeyEncryptor{
				{Kind: SymmetricKeyByPassword, Password: pass},
				{Kind: SymmetricKeyBySymmetricKey, Name: "parent", Open: parentOpen},
			}},
	} {
		if err := d.CreateSymmetricKey(ctx, spec); err != nil {
			t.Fatalf("CreateSymmetricKey(%s): %v", spec.Name, err)
		}
	}
	k, err := d.SymmetricKeyByName(ctx, "k'ey")
	if err != nil {
		t.Fatal(err)
	}
	if k.Owner != "key_owner" || k.Algorithm != "AES_256" {
		t.Errorf("created key read back as %+v", k)
	}
	if got, want := kinds("k'ey"), "PASSWORD; SYMMETRIC KEY parent"; got != want {
		t.Fatalf("after create: %q, want %q", got, want)
	}

	// Add by every encryptor kind, opening the key a different way each time.
	adds := []struct {
		enc SymmetricKeyEncryptor
		dec SymmetricKeyDecryptor
	}{
		{SymmetricKeyEncryptor{Kind: SymmetricKeyByCertificate, Name: "dmk_cert"}, byPassword(pass)},
		// A master-key-protected certificate opens the key with no password.
		{SymmetricKeyEncryptor{Kind: SymmetricKeyByCertificate, Name: "pw_cert"},
			SymmetricKeyDecryptor{Kind: SymmetricKeyByCertificate, Name: "dmk_cert"}},
		{SymmetricKeyEncryptor{Kind: SymmetricKeyByAsymmetricKey, Name: "sk_asym"},
			SymmetricKeyDecryptor{Kind: SymmetricKeyByCertificate, Name: "pw_cert", Password: certPass}},
		{SymmetricKeyEncryptor{Kind: SymmetricKeyByPassword, Password: "Second!Pass4"},
			SymmetricKeyDecryptor{Kind: SymmetricKeyByAsymmetricKey, Name: "sk_asym", Password: asymPass}},
	}
	for _, a := range adds {
		if err := k.AddEncryption(ctx, a.enc, a.dec); err != nil {
			t.Fatalf("AddEncryption(%+v, %+v): %v", a.enc, a.dec, err)
		}
	}
	if got, want := kinds("k'ey"), "CERTIFICATE dmk_cert; CERTIFICATE pw_cert; PASSWORD; PASSWORD; SYMMETRIC KEY parent; ASYMMETRIC KEY sk_asym"; got != want {
		t.Fatalf("after adds: %q, want %q", got, want)
	}

	// A password-protected certificate opened without its password: Msg
	// 15334, reported as itself rather than as a CLOSE's 15315.
	err = k.AddEncryption(ctx, SymmetricKeyEncryptor{Kind: SymmetricKeyByPassword, Password: "Never!Added5"},
		SymmetricKeyDecryptor{Kind: SymmetricKeyByCertificate, Name: "pw_cert"})
	if liveMsg(err) != 15334 {
		t.Errorf("open by pw_cert without its password: %v, want Msg 15334", err)
	}

	// Drop by every kind: the symmetric-key one opens the parent, the first
	// is opened by the password it removes, the last by the parent chain.
	drops := []struct {
		enc SymmetricKeyEncryptor
		dec SymmetricKeyDecryptor
	}{
		{SymmetricKeyEncryptor{Kind: SymmetricKeyByPassword, Password: pass}, byPassword(pass)},
		{SymmetricKeyEncryptor{Kind: SymmetricKeyByCertificate, Name: "pw_cert"}, byPassword("Second!Pass4")},
		{SymmetricKeyEncryptor{Kind: SymmetricKeyByAsymmetricKey, Name: "sk_asym"},
			SymmetricKeyDecryptor{Kind: SymmetricKeyByCertificate, Name: "dmk_cert"}},
		{SymmetricKeyEncryptor{Kind: SymmetricKeyByPassword, Password: "Second!Pass4"},
			SymmetricKeyDecryptor{Kind: SymmetricKeyBySymmetricKey, Name: "parent", Open: parentOpen}},
		{SymmetricKeyEncryptor{Kind: SymmetricKeyBySymmetricKey, Name: "parent", Open: parentOpen},
			SymmetricKeyDecryptor{Kind: SymmetricKeyByCertificate, Name: "dmk_cert"}},
	}
	for _, dr := range drops {
		if err := k.DropEncryption(ctx, dr.enc, dr.dec); err != nil {
			t.Fatalf("DropEncryption(%+v, %+v): %v", dr.enc, dr.dec, err)
		}
	}
	if got, want := kinds("k'ey"), "CERTIFICATE dmk_cert"; got != want {
		t.Fatalf("after drops: %q, want %q", got, want)
	}

	// The last encryption cannot go: Msg 15558. Run on a pinned connection so
	// the session can be asked afterwards whether the CATCH closed the key.
	last := SymmetricKeyEncryptor{Kind: SymmetricKeyByCertificate, Name: "dmk_cert"}
	stmt, err := k.alterEncryptionStatement("DROP", last,
		SymmetricKeyDecryptor{Kind: SymmetricKeyByCertificate, Name: "dmk_cert"})
	if err != nil {
		t.Fatal(err)
	}
	err = d.withConn(ctx, func(c *sql.Conn) error {
		_, err := c.ExecContext(ctx, stmt)
		if liveMsg(err) != 15558 {
			t.Errorf("dropping the last encryption: %v, want Msg 15558", err)
		}
		var open int
		if err := c.QueryRowContext(ctx, "SELECT COUNT(*) FROM sys.openkeys").Scan(&open); err != nil {
			return err
		}
		if open != 0 {
			t.Errorf("%d keys left open after the failed ALTER", open)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Without the batch the ALTER fails: an OPEN in one exec does not reach
	// the next, which is the reason every write here is one batch.
	if _, err := d.exec(ctx, "OPEN SYMMETRIC KEY [k'ey] DECRYPTION BY CERTIFICATE dmk_cert"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.exec(ctx, "ALTER SYMMETRIC KEY [k'ey] ADD ENCRYPTION BY PASSWORD = N'Split!Pass6'"); liveMsg(err) != 15315 {
		t.Errorf("ALTER in a separate exec from its OPEN: %v, want Msg 15315", err)
	}

	// KEY_SOURCE and IDENTITY_VALUE recreate the same key in another
	// database: same GUID, and what one encrypts the other decrypts.
	same := SymmetricKeySpec{Name: "shared", Algorithm: SymmetricKeyAES256,
		KeySource: "gosmo key source", IdentityValue: "gosmo identity",
		Encryptions: []SymmetricKeyEncryptor{{Kind: SymmetricKeyByPassword, Password: pass}}}
	for _, x := range []*Database{d, d2} {
		if err := x.CreateSymmetricKey(ctx, same); err != nil {
			t.Fatalf("create shared in %s: %v", x.Name, err)
		}
	}
	g1, err1 := d.SymmetricKeyByName(ctx, "shared")
	g2, err2 := d2.SymmetricKeyByName(ctx, "shared")
	if err1 != nil || err2 != nil || g1.KeyGUID != g2.KeyGUID {
		t.Errorf("shared key GUIDs %v / %v (%v, %v), want equal", g1, g2, err1, err2)
	}
	var plain string
	err = d.withConn(ctx, func(c *sql.Conn) error {
		var cipher []byte
		if err := c.QueryRowContext(ctx, "OPEN SYMMETRIC KEY shared DECRYPTION BY PASSWORD = N'"+pass+"'; "+
			"SELECT ENCRYPTBYKEY(KEY_GUID('shared'), N'secret'); CLOSE SYMMETRIC KEY shared;").Scan(&cipher); err != nil {
			return err
		}
		return d2.withConn(ctx, func(c2 *sql.Conn) error {
			return c2.QueryRowContext(ctx, "OPEN SYMMETRIC KEY shared DECRYPTION BY PASSWORD = N'"+pass+"'; "+
				"SELECT CONVERT(nvarchar(20), DECRYPTBYKEY(@p1)); CLOSE SYMMETRIC KEY shared;", cipher).Scan(&plain)
		})
	})
	if err != nil || plain != "secret" {
		t.Errorf("decrypt in the second database: %q, %v", plain, err)
	}

	for _, n := range []string{"k'ey", "parent", "shared"} {
		if err := d.SymmetricKeyRef(n).Drop(ctx); err != nil {
			t.Fatalf("Drop(%s): %v", n, err)
		}
	}
	if keys, err := d.SymmetricKeys(ctx); err != nil || len(keys) != 0 {
		t.Errorf("after drops: %v, %v", keys, err)
	}
}

// TestLiveSymmetricKeyAlterUnderLoad runs AddEncryption / DropEncryption from
// many goroutines at once, beside readers, over a pool of three connections —
// so every connection is shared and re-handed out constantly. A write whose
// OPEN and ALTER could land on different connections would fail here with
// Msg 15315; one batch on one connection never does.
func TestLiveSymmetricKeyAlterUnderLoad(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	db.SetMaxOpenConns(3)

	d, drop := liveScratchDB(t, db, ctx, "gosmo_symload")
	defer drop()

	const writers, readers, rounds = 8, 4, 6
	for i := range writers {
		if err := d.CreateSymmetricKey(ctx, SymmetricKeySpec{Name: fmt.Sprintf("k%d", i), Algorithm: SymmetricKeyAES256,
			Encryptions: []SymmetricKeyEncryptor{{Kind: SymmetricKeyByPassword, Password: "Base!Pass1"}}}); err != nil {
			t.Fatal(err)
		}
	}

	loadCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	fail := func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}
	for i := range readers {
		wg.Go(func() {
			for loadCtx.Err() == nil {
				if _, err := d.SymmetricKeys(loadCtx); err != nil && loadCtx.Err() == nil {
					fail(fmt.Errorf("reader %d: %w", i, err))
					return
				}
			}
		})
	}
	var writersWG sync.WaitGroup
	for i := range writers {
		writersWG.Go(func() {
			k := d.SymmetricKeyRef(fmt.Sprintf("k%d", i))
			dec := SymmetricKeyDecryptor{Kind: SymmetricKeyByPassword, Password: "Base!Pass1"}
			for r := range rounds {
				enc := SymmetricKeyEncryptor{Kind: SymmetricKeyByPassword, Password: fmt.Sprintf("Round!%d#%d", i, r)}
				if err := k.AddEncryption(ctx, enc, dec); err != nil {
					fail(fmt.Errorf("writer %d round %d add: %w", i, r, err))
					return
				}
				if err := k.DropEncryption(ctx, enc, dec); err != nil {
					fail(fmt.Errorf("writer %d round %d drop: %w", i, r, err))
					return
				}
			}
		})
	}
	writersWG.Wait()
	cancel()
	wg.Wait()
	for _, err := range errs {
		t.Error(err)
	}

	keys, err := d.SymmetricKeys(ctx)
	if err != nil || len(keys) != writers {
		t.Fatalf("after load: %d keys, %v", len(keys), err)
	}
	for _, k := range keys {
		if len(k.Encryptions) != 1 || k.Encryptions[0].Kind != SymmetricKeyByPassword {
			t.Errorf("%s ended with %+v, want its one password", k.Name, k.Encryptions)
		}
	}
}
