//go:build livedb

// Live verification of the scripter's refusals for a definition the login
// cannot read (ErrDefinitionHidden): a module's text, and a table's default,
// computed column and check, each NULL without VIEW DEFINITION — told apart
// from an encrypted module, which no permission reveals.
//
//	go test -tags livedb . -run TestLiveHiddenDefinition -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops one throwaway database and one login; touches nothing
// else.
package gosmo

import (
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestLiveHiddenDefinition(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_hidden_def_live")
	defer drop()
	liveExecIn(t, d, ctx,
		"CREATE TABLE dbo.D (id int NOT NULL, v int NULL CONSTRAINT DF_D_v DEFAULT 1)",
		"CREATE TABLE dbo.C (id int NOT NULL, c AS id * 2)",
		"CREATE TABLE dbo.K (id int NOT NULL CONSTRAINT CK_K_id CHECK (id > 0))",
		"CREATE PROCEDURE dbo.p_plain AS SELECT 1",
		"CREATE PROCEDURE dbo.p_enc WITH ENCRYPTION AS SELECT 1",
		"CREATE VIEW dbo.v AS SELECT id FROM dbo.D",
		"CREATE TABLE dbo.F (id int NOT NULL, v int NULL)",
		"CREATE INDEX IX_F_v ON dbo.F (v) WHERE v IS NOT NULL",
		"CREATE STATISTICS ST_F_id ON dbo.F (id) WHERE id > 0",
	)
	const login, pass = "gosmo_hidden_def_lp", "Hd!7wQ#2zKr9"
	if _, err := db.ExecContext(ctx, "CREATE LOGIN "+login+" WITH PASSWORD = N'"+pass+"', CHECK_POLICY = OFF"); err != nil {
		t.Fatalf("create login: %v", err)
	}
	defer db.ExecContext(ctx, "DROP LOGIN "+login)
	// SELECT and EXECUTE make each object visible, its definitions not.
	liveExecIn(t, d, ctx, "CREATE USER "+login+" FOR LOGIN "+login,
		"GRANT SELECT ON dbo.D TO "+login, "GRANT SELECT ON dbo.C TO "+login, "GRANT SELECT ON dbo.K TO "+login, "GRANT SELECT ON dbo.F TO "+login,
		"GRANT SELECT ON dbo.v TO "+login,
		"GRANT EXECUTE ON dbo.p_plain TO "+login, "GRANT EXECUTE ON dbo.p_enc TO "+login)
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
	scripter := func(db *Database, v ScriptVerb) *Scripter {
		o := DefaultScriptOptions()
		o.Verb = v
		return NewScripter(db, o)
	}

	for _, tc := range []struct{ table, names string }{
		{"D", "default [DF_D_v]"}, {"C", "computed column [c]"}, {"K", "check constraint [CK_K_id]"},
		{"F", "index [IX_F_v]'s filter"},
	} {
		t.Run("table "+tc.table, func(t *testing.T) {
			_, err := scripter(ld, ScriptCreate).ScriptTable(ctx, "dbo", tc.table)
			if !errors.Is(err, ErrDefinitionHidden) || !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), tc.names) {
				t.Errorf("CREATE as %s = %v, want ErrDefinitionHidden naming %s", login, err, tc.names)
			}
			// DROP reads the schema-bound dependents first, which
			// sys.sql_expression_dependencies refuses this login outright
			// (Msg 229): a permission error, never a broken script.
			for _, v := range []ScriptVerb{ScriptDrop, ScriptDropAndCreate} {
				if _, err := scripter(ld, v).ScriptTable(ctx, "dbo", tc.table); !IsPermissionDenied(err) {
					t.Errorf("verb %v as %s = %v, want a permission error", v, login, err)
				}
			}
			if _, err := scripter(d, ScriptCreate).ScriptTable(ctx, "dbo", tc.table); err != nil {
				t.Errorf("CREATE as sa = %v", err)
			}
		})
	}

	// The filter is NULL to the login while has_filter is set: scripted
	// alone, the index or statistic would come back unfiltered.
	t.Run("filtered index and statistic", func(t *testing.T) {
		_, err := scripter(ld, ScriptCreate).ScriptIndex(ctx, "dbo", "F", "IX_F_v")
		if !errors.Is(err, ErrDefinitionHidden) {
			t.Errorf("ScriptIndex as %s = %v, want ErrDefinitionHidden", login, err)
		}
		if _, err := scripter(ld, ScriptDrop).ScriptIndex(ctx, "dbo", "F", "IX_F_v"); err != nil {
			t.Errorf("ScriptIndex DROP as %s = %v", login, err)
		}
		_, err = scripter(ld, ScriptCreate).ScriptStatistic(ctx, "dbo", "F", "ST_F_id")
		if !errors.Is(err, ErrDefinitionHidden) {
			t.Errorf("ScriptStatistic as %s = %v, want ErrDefinitionHidden", login, err)
		}
		tbl, err := d.TableByName(ctx, "dbo", "F")
		if err != nil {
			t.Fatalf("TableByName as sa: %v", err)
		}
		idx, err := tbl.IndexByName(ctx, "IX_F_v")
		if err != nil || idx.IsFilterHidden || idx.FilterDefinition != "([v] IS NOT NULL)" {
			t.Errorf("IndexByName as sa = %+v, %v", idx, err)
		}
		if s, err := scripter(d, ScriptCreate).ScriptIndex(ctx, "dbo", "F", "IX_F_v"); err != nil || !strings.Contains(s, "WHERE ([v] IS NOT NULL)") {
			t.Errorf("ScriptIndex as sa = %q, %v", s, err)
		}
	})

	t.Run("module", func(t *testing.T) {
		_, err := scripter(ld, ScriptCreate).ScriptStoredProcedure(ctx, "dbo", "p_plain")
		if !errors.Is(err, ErrDefinitionHidden) || !errors.Is(err, ErrUnsupported) {
			t.Errorf("p_plain as %s = %v, want ErrDefinitionHidden", login, err)
		}
		_, err = scripter(ld, ScriptAlter).ScriptView(ctx, "dbo", "v")
		if !errors.Is(err, ErrDefinitionHidden) {
			t.Errorf("view as %s = %v, want ErrDefinitionHidden", login, err)
		}
		for _, sd := range []*Database{ld, d} {
			_, err = scripter(sd, ScriptCreate).ScriptStoredProcedure(ctx, "dbo", "p_enc")
			if !errors.Is(err, ErrUnsupported) || errors.Is(err, ErrDefinitionHidden) || !strings.Contains(err.Error(), "encrypted") {
				t.Errorf("p_enc = %v, want ErrUnsupported (encrypted), not ErrDefinitionHidden", err)
			}
		}
		if _, err := scripter(d, ScriptCreate).ScriptStoredProcedure(ctx, "dbo", "p_plain"); err != nil {
			t.Errorf("p_plain as sa = %v", err)
		}
	})
}
