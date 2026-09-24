//go:build livedb

package gosmo

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// Every Create* returns the object it created, read back from the catalog
// (2026-09-24; until then 20 of 33 returned only an error). A fake connection
// can only prove the read was *asked*; this proves each read-back finds the
// new row and populates it — a field only the catalog knows (an id, a SID)
// is non-zero — and that the handle fallback is not what answered.
func TestLiveCreateReturnsTheCreatedObject(t *testing.T) {
	sqldb, ctx, done := liveDB(t)
	// A cleanup, not a defer: the drops below are cleanups too, and run
	// before this one only if it was registered first. A deferred done
	// closed the pool under them and left every object behind.
	t.Cleanup(done)
	srv := liveServer(t, sqldb, ctx)

	const dbName = "gosmo_create_returns"
	_ = srv.DropDatabase(ctx, dbName, true)
	d, err := srv.CreateDatabase(ctx, CreateDatabaseRequest{Name: dbName})
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	t.Cleanup(func() { _ = srv.DropDatabase(context.Background(), dbName, true) })
	if d.ID == 0 {
		t.Fatalf("CreateDatabase returned %+v, want it read back", d)
	}

	check := func(what string, err error, populated bool) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if !populated {
			t.Errorf("%s: the returned object is not populated from the catalog", what)
		}
	}

	s, err := d.CreateSchema(ctx, CreateSchemaRequest{Name: "sales"})
	check("CreateSchema", err, err == nil && s.ID != 0)

	tbl, err := d.CreateTable(ctx, CreateTableRequest{Schema: "sales", Name: "t",
		Columns: []ColumnDefinition{{Name: "a", DataType: DataTypeInt}}})
	check("CreateTable", err, err == nil && tbl.ObjectID != 0)

	ix, err := tbl.CreateIndex(ctx, CreateIndexRequest{Name: "ix_a", Type: IndexTypeNonClustered,
		KeyColumns: []IndexColumnDef{{Name: "a"}}})
	check("CreateIndex", err, err == nil && ix.IndexID != 0)

	st, err := tbl.CreateStatistic(ctx, CreateStatisticRequest{Name: "st_a", Columns: []string{"a"}})
	check("CreateStatistic", err, err == nil && st.StatID != 0)

	seq, err := d.CreateSequence(ctx, CreateSequenceRequest{Schema: "sales", Name: "seq", StartValue: 1, Increment: 1})
	check("CreateSequence", err, err == nil && seq.ObjectID != 0)

	syn, err := d.CreateSynonym(ctx, CreateSynonymRequest{Schema: "sales", Name: "syn", BaseObject: "[sales].[t]"})
	check("CreateSynonym", err, err == nil && syn.ObjectID != 0)

	proc, err := d.CreateStoredProcedure(ctx, CreateStoredProcedureRequest{Schema: "sales", Name: "p", Body: "SELECT 1"})
	check("CreateStoredProcedure", err, err == nil && proc.ObjectID != 0)

	pf, err := d.CreatePartitionFunction(ctx, CreatePartitionFunctionRequest{Name: "pf", InputType: DataTypeInt, Boundaries: []string{"10"}})
	check("CreatePartitionFunction", err, err == nil && pf.FunctionID != 0)

	ps, err := d.CreatePartitionScheme(ctx, CreatePartitionSchemeRequest{Name: "ps", Function: "pf", FileGroups: []string{"PRIMARY", "PRIMARY"}})
	check("CreatePartitionScheme", err, err == nil && ps.SchemeID != 0)

	u, err := d.CreateUser(ctx, CreateUserRequest{Name: "u", Kind: UserWithoutLogin})
	check("CreateUser", err, err == nil && u.ID != 0)

	mk, err := d.CreateMasterKey(ctx, CreateMasterKeyRequest{Password: "Gosmo-Str0ng!pw#1"})
	check("CreateMasterKey", err, err == nil && !mk.CreateDate.IsZero())

	cert, err := d.CreateCertificate(ctx, CreateCertificateRequest{Name: "c", Subject: "gosmo create returns"})
	check("CreateCertificate", err, err == nil && cert.CertificateID != 0)

	ak, err := d.CreateAsymmetricKey(ctx, CreateAsymmetricKeyRequest{Name: "ak", Algorithm: AsymmetricKeyRSA2048})
	check("CreateAsymmetricKey", err, err == nil && ak.KeyID != 0)

	sk, err := d.CreateSymmetricKey(ctx, CreateSymmetricKeyRequest{Name: "sk", Algorithm: SymmetricKeyAES256,
		Encryptions: []SymmetricKeyEncryptor{{Kind: SymmetricKeyByCertificate, Name: "c"}}})
	check("CreateSymmetricKey", err, err == nil && sk.KeyID != 0)

	cmk, err := d.CreateColumnMasterKey(ctx, CreateColumnMasterKeyRequest{Name: "cmk",
		KeyStoreProvider: "MSSQL_CERTIFICATE_STORE", KeyPath: "CurrentUser/my/DEADBEEF"})
	check("CreateColumnMasterKey", err, err == nil && cmk.ID != 0)

	cek, err := d.CreateColumnEncryptionKey(ctx, CreateColumnEncryptionKeyRequest{Name: "cek",
		Values: []ColumnEncryptionKeyValue{{MasterKeyName: "cmk", EncryptionAlgorithm: "RSA_OAEP",
			EncryptedValue: bytes.Repeat([]byte{0x01, 0x02}, 8)}}})
	check("CreateColumnEncryptionKey", err, err == nil && cek.ID != 0)

	// Server-scoped: cleaned up whatever happens above.
	login, err := srv.CreateLogin(ctx, CreateLoginRequest{Name: dbName + "_login", Password: "Gosmo-Str0ng!pw#1"})
	if err == nil {
		t.Cleanup(func() { _ = srv.LoginRef(dbName + "_login").Drop(context.Background()) })
	}
	check("CreateLogin", err, err == nil && len(login.SID) != 0)

	cat, err := srv.CreateCategory(ctx, CreateCategoryRequest{Class: CategoryClassJob, Name: dbName + "_cat"})
	if err == nil {
		t.Cleanup(func() { _ = srv.DeleteCategory(context.Background(), CategoryClassJob, dbName+"_cat") })
	}
	check("CreateCategory", err, err == nil && cat.ID != 0)

	// The empty-schema rule reaches the server's side too: nothing was sent,
	// so the database has no object the refused call could have made.
	if _, err := d.CreateSequence(ctx, CreateSequenceRequest{Name: "unqualified"}); !errors.Is(err, ErrSchemaRequired) {
		t.Errorf("CreateSequence with no schema: err = %v, want ErrSchemaRequired", err)
	}
}
