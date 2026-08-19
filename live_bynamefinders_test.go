//go:build livedb

// Live verification of the five by-name finders added alongside the bulk
// listings: PartitionFunctionByNameContext, PartitionSchemeByNameContext,
// SecurityPolicyByNameContext, ColumnMasterKeyByNameContext and
// ColumnEncryptionKeyByNameContext.
//
// Only the server can say whether these queries run at all, and whether each
// returns the same object its listing does — the point of adding them was to
// replace a scan of the listing, so any drift between the two is the bug.
//
//	go test -tags livedb . -run TestLiveByNameFinders -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// liveScratchDB creates a throwaway database and returns a *Database for it
// plus a dropper.
func liveScratchDB(t *testing.T, db *sql.DB, ctx context.Context, name string) (*Database, func()) {
	t.Helper()
	if _, err := db.ExecContext(ctx, "IF DB_ID('"+name+"') IS NOT NULL ALTER DATABASE ["+name+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE"); err != nil {
		t.Fatalf("pre-drop prep %s: %v", name, err)
	}
	if _, err := db.ExecContext(ctx, "IF DB_ID('"+name+"') IS NOT NULL DROP DATABASE ["+name+"]"); err != nil {
		t.Fatalf("pre-drop %s: %v", name, err)
	}
	if _, err := db.ExecContext(ctx, "CREATE DATABASE ["+name+"]"); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	srv := &Server{db: db}
	d, err := srv.DatabaseByNameContext(ctx, name)
	if err != nil {
		t.Fatalf("DatabaseByNameContext %s: %v", name, err)
	}
	return d, func() {
		c := context.Background()
		db.ExecContext(c, "ALTER DATABASE ["+name+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE")
		db.ExecContext(c, "DROP DATABASE ["+name+"]")
	}
}

func liveExecIn(t *testing.T, d *Database, ctx context.Context, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := d.exec(ctx, s); err != nil {
			t.Fatalf("exec %.60q: %v", s, err)
		}
	}
}

func TestLiveByNameFindersMatchTheirListings(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_byname_live")
	defer drop()

	liveExecIn(t, d, ctx,
		`CREATE PARTITION FUNCTION gosmo_pf (INT) AS RANGE RIGHT FOR VALUES (100, 200)`,
		`CREATE PARTITION SCHEME gosmo_ps AS PARTITION gosmo_pf ALL TO ([PRIMARY])`,
		`CREATE COLUMN MASTER KEY gosmo_cmk WITH (KEY_STORE_PROVIDER_NAME = 'MSSQL_CERTIFICATE_STORE', KEY_PATH = 'CurrentUser/My/DEADBEEF')`,
		`CREATE COLUMN ENCRYPTION KEY gosmo_cek WITH VALUES (COLUMN_MASTER_KEY = gosmo_cmk, ALGORITHM = 'RSA_OAEP', ENCRYPTED_VALUE = 0x0123456789ABCDEF)`,
		`CREATE SCHEMA rls`,
		`CREATE TABLE dbo.gosmo_rls_target (id INT NOT NULL, owner_name SYSNAME NOT NULL)`,
		`CREATE FUNCTION rls.gosmo_pred(@owner AS SYSNAME) RETURNS TABLE WITH SCHEMABINDING
		 AS RETURN SELECT 1 AS ok WHERE @owner = USER_NAME()`,
		`CREATE SECURITY POLICY rls.gosmo_policy
		 ADD FILTER PREDICATE rls.gosmo_pred(owner_name) ON dbo.gosmo_rls_target
		 WITH (STATE = ON)`,
	)

	t.Run("PartitionFunction", func(t *testing.T) {
		list, err := d.PartitionFunctionsContext(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("listing returned %d partition functions, want 1", len(list))
		}
		got, err := d.PartitionFunctionByNameContext(ctx, "gosmo_pf")
		if err != nil {
			t.Fatalf("by name: %v", err)
		}
		want := list[0]
		if got.Name != want.Name || got.FunctionID != want.FunctionID ||
			got.BoundaryCount != want.BoundaryCount || got.InputType != want.InputType ||
			got.IsRight != want.IsRight {
			t.Errorf("by name = %+v, listing = %+v", got, want)
		}
		if len(got.Boundaries) != 2 || got.Boundaries[0] != "100" || got.Boundaries[1] != "200" {
			t.Errorf("boundaries = %v, want [100 200]", got.Boundaries)
		}
	})

	t.Run("PartitionScheme", func(t *testing.T) {
		list, err := d.PartitionSchemesContext(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("listing returned %d partition schemes, want 1", len(list))
		}
		got, err := d.PartitionSchemeByNameContext(ctx, "gosmo_ps")
		if err != nil {
			t.Fatalf("by name: %v", err)
		}
		want := list[0]
		if got.Name != want.Name || got.SchemeID != want.SchemeID || got.FunctionName != want.FunctionName {
			t.Errorf("by name = %+v, listing = %+v", got, want)
		}
		if len(got.FileGroups) != len(want.FileGroups) {
			t.Errorf("filegroups = %v, listing = %v", got.FileGroups, want.FileGroups)
		}
	})

	t.Run("ColumnMasterKey", func(t *testing.T) {
		list, err := d.ColumnMasterKeysContext(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("listing returned %d column master keys, want 1", len(list))
		}
		got, err := d.ColumnMasterKeyByNameContext(ctx, "gosmo_cmk")
		if err != nil {
			t.Fatalf("by name: %v", err)
		}
		want := list[0]
		if got.Name != want.Name || got.ID != want.ID ||
			got.KeyStoreProviderName != want.KeyStoreProviderName || got.KeyPath != want.KeyPath ||
			got.AllowEnclaveComputations != want.AllowEnclaveComputations {
			t.Errorf("by name = %+v, listing = %+v", got, want)
		}
	})

	t.Run("ColumnEncryptionKey", func(t *testing.T) {
		list, err := d.ColumnEncryptionKeysContext(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("listing returned %d column encryption keys, want 1", len(list))
		}
		got, err := d.ColumnEncryptionKeyByNameContext(ctx, "gosmo_cek")
		if err != nil {
			t.Fatalf("by name: %v", err)
		}
		want := list[0]
		if got.Name != want.Name || got.ID != want.ID ||
			got.MasterKeyName != want.MasterKeyName || got.EncryptionAlgorithm != want.EncryptionAlgorithm {
			t.Errorf("by name = %+v, listing = %+v", got, want)
		}
		// The encrypted values are the part nothing can regenerate, and the
		// fold that produces them is the only shared code here.
		if len(got.Values) != len(want.Values) {
			t.Fatalf("by name has %d values, listing has %d", len(got.Values), len(want.Values))
		}
		for i := range got.Values {
			if string(got.Values[i].EncryptedValue) != string(want.Values[i].EncryptedValue) {
				t.Errorf("value %d differs from the listing's", i)
			}
		}
	})

	t.Run("SecurityPolicy", func(t *testing.T) {
		list, err := d.SecurityPoliciesContext(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("listing returned %d security policies, want 1", len(list))
		}
		got, err := d.SecurityPolicyByNameContext(ctx, "rls", "gosmo_policy")
		if err != nil {
			t.Fatalf("by name: %v", err)
		}
		want := list[0]
		if got.Name != want.Name || got.Schema != want.Schema || got.ObjectID != want.ObjectID ||
			got.IsEnabled != want.IsEnabled || got.IsSchemaBound != want.IsSchemaBound {
			t.Errorf("by name = %+v, listing = %+v", got, want)
		}
		// Predicates come from a second query, which the by-name path has to
		// run for itself — a policy loaded without them scripts as an empty
		// CREATE SECURITY POLICY.
		if len(got.Predicates) != 1 {
			t.Fatalf("by name loaded %d predicates, want 1", len(got.Predicates))
		}
		if got.Predicates[0].TargetTable != "gosmo_rls_target" || got.Predicates[0].PredicateType != "FILTER" {
			t.Errorf("predicate = %+v", got.Predicates[0])
		}
	})

	// Every finder answers a missing name with ErrNotFound, the way the
	// thirteen that predate them do — gossms branches on it.
	t.Run("NotFound", func(t *testing.T) {
		cases := map[string]func() error{
			"PartitionFunction": func() error {
				_, err := d.PartitionFunctionByNameContext(ctx, "nope")
				return err
			},
			"PartitionScheme": func() error {
				_, err := d.PartitionSchemeByNameContext(ctx, "nope")
				return err
			},
			"ColumnMasterKey": func() error {
				_, err := d.ColumnMasterKeyByNameContext(ctx, "nope")
				return err
			},
			"ColumnEncryptionKey": func() error {
				_, err := d.ColumnEncryptionKeyByNameContext(ctx, "nope")
				return err
			},
			"SecurityPolicy": func() error {
				_, err := d.SecurityPolicyByNameContext(ctx, "rls", "nope")
				return err
			},
		}
		for name, call := range cases {
			err := call()
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("%s missing name: err = %v, want ErrNotFound", name, err)
			}
		}
	})
}
