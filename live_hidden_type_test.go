//go:build livedb

// Live verification of the reads of a column or parameter whose user-defined
// type the login cannot see (Column.IsTypeHidden, Sequence.IsTypeHidden):
// kept, named by its base type, and refused by the scripter.
//
//	go test -tags livedb . -run TestLiveHiddenUserType -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops one throwaway database and one login; touches nothing
// else.
package gosmo

import (
	"database/sql"
	"errors"
	"net/url"
	"testing"
)

func TestLiveHiddenUserType(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_hidden_type_live")
	defer drop()
	liveExecIn(t, d, ctx,
		"CREATE TYPE dbo.Phone FROM nvarchar(25) NULL",
		"CREATE TYPE dbo.Num FROM int NOT NULL",
		"CREATE SEQUENCE dbo.S AS dbo.Num START WITH 1",
		"CREATE TABLE dbo.T (id int CONSTRAINT CK_T_id CHECK (id > 0), ph dbo.Phone, n int)",
		"CREATE PROCEDURE dbo.p @ph dbo.Phone, @n int AS SELECT @ph, @n",
	)
	const login, pass = "gosmo_hidden_type_lp", "Ht!4wQ#8zKr2"
	if _, err := db.ExecContext(ctx, "CREATE LOGIN "+login+" WITH PASSWORD = N'"+pass+"', CHECK_POLICY = OFF"); err != nil {
		t.Fatalf("create login: %v", err)
	}
	defer db.ExecContext(ctx, "DROP LOGIN "+login)
	// SELECT and EXECUTE show the table, the procedure and their columns and
	// parameters, but not the type, nor the check's definition.
	liveExecIn(t, d, ctx, "CREATE USER "+login+" FOR LOGIN "+login,
		"GRANT SELECT ON dbo.T TO "+login, "GRANT EXECUTE ON dbo.p TO "+login,
		"GRANT UPDATE ON dbo.S TO "+login)
	defer liveExecIn(t, d, ctx, "DROP USER "+login)

	u, err := url.Parse(*liveDSN)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	u.User = url.UserPassword(login, pass)
	ldb, err := sql.Open("sqlserver", u.String())
	if err != nil {
		t.Fatalf("open as %s: %v", login, err)
	}
	defer ldb.Close()
	ld, err := liveServer(t, ldb, ctx).DatabaseByName(ctx, d.Name)
	if err != nil {
		t.Fatalf("DatabaseByName as %s: %v", login, err)
	}

	t.Run("Columns", func(t *testing.T) {
		tbl, err := ld.TableByName(ctx, "dbo", "T")
		if err != nil {
			t.Fatalf("TableByName: %v", err)
		}
		cols, err := tbl.Columns(ctx)
		if err != nil {
			t.Fatalf("Columns: %v", err)
		}
		if len(cols) != 3 {
			t.Fatalf("Columns = %d, want 3 (the hidden-type column dropped?)", len(cols))
		}
		if ph := cols[1]; !ph.IsTypeHidden || ph.TypeString() != "nvarchar(25)" || ph.TypeSchema != "" {
			t.Errorf("ph: IsTypeHidden %v, TypeString %q, TypeSchema %q", ph.IsTypeHidden, ph.TypeString(), ph.TypeSchema)
		}
		if n := cols[2]; n.IsTypeHidden || n.TypeString() != "int" {
			t.Errorf("n: IsTypeHidden %v, TypeString %q", n.IsTypeHidden, n.TypeString())
		}
		checks, err := tbl.CheckConstraints(ctx)
		if err != nil || len(checks) != 1 || checks[0].Definition != "" {
			t.Errorf("CheckConstraints = %+v, %v; want one with Definition \"\"", checks, err)
		}
	})

	t.Run("Parameters", func(t *testing.T) {
		ps, err := ld.Parameters(ctx, "dbo", "p")
		if err != nil {
			t.Fatalf("Parameters: %v", err)
		}
		if len(ps) != 2 || !ps[0].IsTypeHidden || ps[0].TypeString() != "nvarchar(25)" || ps[1].IsTypeHidden {
			t.Errorf("Parameters = %+v", ps)
		}
	})

	t.Run("SearchColumns", func(t *testing.T) {
		got, err := ld.SearchColumns(ctx, "ph")
		if err != nil {
			t.Fatalf("SearchColumns: %v", err)
		}
		if len(got) != 1 || got[0].DataType != "nvarchar(25)" || !got[0].IsTypeHidden {
			t.Errorf("SearchColumns(ph) = %+v", got)
		}
	})

	t.Run("Catalog", func(t *testing.T) {
		cat, err := ld.Catalog(ctx)
		if err != nil {
			t.Fatalf("Catalog: %v", err)
		}
		for _, o := range cat.Objects {
			if o.Schema == "dbo" && o.Name == "T" {
				if len(o.Columns) != 3 || o.Columns[1].DataType != "nvarchar" || !o.Columns[1].IsTypeHidden {
					t.Errorf("catalog dbo.T columns = %+v", o.Columns)
				}
				return
			}
		}
		t.Errorf("dbo.T not in the catalog")
	})

	t.Run("Sequence", func(t *testing.T) {
		seqs, err := ld.Sequences(ctx)
		if err != nil {
			t.Fatalf("Sequences: %v", err)
		}
		if len(seqs) != 1 || !seqs[0].IsTypeHidden || seqs[0].DataType != "int" || seqs[0].DataTypeSchema != "" {
			t.Fatalf("Sequences = %+v, want dbo.S as a hidden int", seqs)
		}
		_, err = NewScripter(ld, DefaultScriptOptions()).ScriptSequence(ctx, "dbo", "S")
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("ScriptSequence as %s = %v, want ErrUnsupported", login, err)
		}
		seq, err := d.SequenceByName(ctx, "dbo", "S")
		if err != nil || seq.IsTypeHidden || seq.DataType != "Num" || seq.DataTypeSchema != "dbo" {
			t.Errorf("SequenceByName as sa = %+v, %v", seq, err)
		}
	})

	// The partition function read's LEFT JOIN is belt and braces: the server
	// refuses an alias type as its input, so its type is always a system one.
	t.Run("partition function refuses an alias type", func(t *testing.T) {
		if _, err := d.exec(ctx, "CREATE PARTITION FUNCTION pfNum (dbo.Num) AS RANGE RIGHT FOR VALUES (1)"); err == nil {
			d.exec(ctx, "DROP PARTITION FUNCTION pfNum")
			t.Errorf("CREATE PARTITION FUNCTION over an alias type succeeded")
		} else {
			t.Logf("refused: %v", err)
		}
	})

	t.Run("ScriptTable refused", func(t *testing.T) {
		_, err := NewScripter(ld, DefaultScriptOptions()).ScriptTable(ctx, "dbo", "T")
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("ScriptTable as %s = %v, want ErrUnsupported", login, err)
		}
	})

	t.Run("sa sees the type", func(t *testing.T) {
		tbl, err := d.TableByName(ctx, "dbo", "T")
		if err != nil {
			t.Fatalf("TableByName: %v", err)
		}
		cols, err := tbl.Columns(ctx)
		if err != nil || len(cols) != 3 || cols[1].IsTypeHidden || cols[1].TypeString() != "[dbo].[Phone]" {
			t.Errorf("Columns as sa = %+v, %v", cols, err)
		}
	})
}
