//go:build livedb

// Live verification that a table script with a vector(n, float16) column
// names the PREVIEW_FEATURES setting its replay needs, and does not change it
// itself (gossms fix plan 2026-10-01 item 15). SQL Server 2025 only; skipped
// below.
//
//	go test -tags livedb . -run TestLiveFloat16VectorScript -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops two throwaway databases; touches nothing else.
package gosmo

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
)

func TestLiveFloat16VectorScript(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	if major := liveServer(t, db, ctx).serverMajorVersion(); !hasColumnSince(major, SQLServer2025) {
		t.Skipf("major %d has no vector type", major)
	}

	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_f16_src")
	t.Cleanup(dropSrc)
	dst, dropDst := liveScratchDB(t, db, ctx, "gosmo_f16_dst")
	t.Cleanup(dropDst)
	liveExecIn(t, src, ctx,
		`ALTER DATABASE SCOPED CONFIGURATION SET PREVIEW_FEATURES = ON`,
		`CREATE TABLE dbo.Vectors (id int NOT NULL PRIMARY KEY, v vector(3) NULL, h vector(4, float16) NULL)`,
	)

	opts := DefaultScriptOptions()
	opts.IncludeHeaders = false
	script, err := NewScripter(src, opts).ScriptTable(ctx, "dbo", "Vectors")
	if err != nil {
		t.Fatalf("ScriptTable: %v", err)
	}
	const setting = "ALTER DATABASE SCOPED CONFIGURATION SET PREVIEW_FEATURES = ON;"
	if !strings.Contains(script, "-- [h]: vector(n, float16)") || !strings.Contains(script, "--   "+setting) {
		t.Fatalf("the script does not name the setting:\n%s", script)
	}

	previewOn := func() string {
		return strings.Join(liveRowsAsStrings(t, dst, ctx,
			`SELECT CAST(value AS int) AS v FROM sys.database_scoped_configurations WHERE name = N'PREVIEW_FEATURES'`), "")
	}
	if got := previewOn(); got != "v=0" {
		t.Fatalf("PREVIEW_FEATURES in a new database = %q, want 0", got)
	}

	// Off in the target, the replay fails where the note says it will, and
	// leaves the setting as it was.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin a connection: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "USE "+quoteIdent(dst.Name)); err != nil {
		t.Fatalf("USE %s: %v", dst.Name, err)
	}
	var runErr error
	for _, batch := range splitGoBatches(script) {
		if _, runErr = conn.ExecContext(ctx, batch); runErr != nil {
			break
		}
	}
	conn.Close()
	msErr, ok := errors.AsType[mssql.Error](runErr)
	if !ok || !slices.ContainsFunc(msErr.All, func(e mssql.Error) bool { return e.Number == 195 }) {
		t.Fatalf("replay with PREVIEW_FEATURES off: got %v, want Msg 195", runErr)
	}
	if got := previewOn(); got != "v=0" {
		t.Errorf("the replay changed PREVIEW_FEATURES to %q", got)
	}

	// With the setting the note names, the same script replays.
	liveExecIn(t, dst, ctx, setting)
	livePinnedRun(t, db, ctx, dst.Name, script)
	want := []string{
		"name=h type=vector dims=4 base=float16",
		"name=id type=int dims=0 base=",
		"name=v type=vector dims=3 base=float32",
	}
	q := `SELECT c.name, TYPE_NAME(c.user_type_id) AS type, ISNULL(c.vector_dimensions, 0) AS dims,
       ISNULL(c.vector_base_type_desc, '') AS base
FROM sys.columns c WHERE c.object_id = OBJECT_ID(N'dbo.Vectors') ORDER BY c.name`
	if got := liveRowsAsStrings(t, dst, ctx, q); !slices.Equal(got, want) {
		t.Errorf("replayed columns = %q, want %q", got, want)
	}
}

// A procedure's or function's float16 parameter gets the same note, by
// parameter name, and the replay fails without the setting exactly as the
// table's does. A float32 parameter and a parameterless module get none.
func TestLiveFloat16ParameterScript(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	if major := liveServer(t, db, ctx).serverMajorVersion(); major != 0 && major < int(SQLServer2025) {
		t.Skipf("major %d has no vector type", major)
	}

	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_f16p_src")
	t.Cleanup(dropSrc)
	dst, dropDst := liveScratchDB(t, db, ctx, "gosmo_f16p_dst")
	t.Cleanup(dropDst)
	liveExecIn(t, src, ctx,
		`ALTER DATABASE SCOPED CONFIGURATION SET PREVIEW_FEATURES = ON`,
		`CREATE PROCEDURE dbo.P @h vector(4, float16), @v vector(3), @q vector(2, float16) = NULL AS SELECT 1`,
		`CREATE FUNCTION dbo.F (@h vector(4, float16)) RETURNS int AS BEGIN RETURN 1 END`,
		`CREATE PROCEDURE dbo.Plain @v vector(3) AS SELECT 1`,
	)

	opts := DefaultScriptOptions()
	opts.IncludeHeaders = false
	sc := NewScripter(src, opts)
	const setting = "ALTER DATABASE SCOPED CONFIGURATION SET PREVIEW_FEATURES = ON;"
	for _, c := range []struct {
		name, note string
		script     func(context.Context, string, string) (string, error)
	}{
		{"P", "-- @h, @q: vector(n, float16)", sc.ScriptStoredProcedure},
		{"F", "-- @h: vector(n, float16)", sc.ScriptFunction},
		{"Plain", "", sc.ScriptStoredProcedure},
	} {
		script, err := c.script(ctx, "dbo", c.name)
		if err != nil {
			t.Fatalf("script %s: %v", c.name, err)
		}
		if c.note == "" {
			if strings.Contains(script, "PREVIEW_FEATURES") {
				t.Errorf("%s has no float16 parameter but names the setting:\n%s", c.name, script)
			}
			continue
		}
		if !strings.HasPrefix(script, c.note) || !strings.Contains(script, "--   "+setting) {
			t.Fatalf("%s: the script does not open with %q and the setting:\n%s", c.name, c.note, script)
		}

		// Off in the target, the replay fails with Msg 195.
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("pin a connection: %v", err)
		}
		if _, err := conn.ExecContext(ctx, "USE "+quoteIdent(dst.Name)); err != nil {
			t.Fatalf("USE %s: %v", dst.Name, err)
		}
		var runErr error
		for _, batch := range splitGoBatches(script) {
			if _, runErr = conn.ExecContext(ctx, batch); runErr != nil {
				break
			}
		}
		conn.Close()
		msErr, ok := errors.AsType[mssql.Error](runErr)
		if !ok || !slices.ContainsFunc(msErr.All, func(e mssql.Error) bool { return e.Number == 195 }) {
			t.Fatalf("%s: replay with PREVIEW_FEATURES off: got %v, want Msg 195", c.name, runErr)
		}

		// With it on, the script replays.
		liveExecIn(t, dst, ctx, `ALTER DATABASE SCOPED CONFIGURATION SET PREVIEW_FEATURES = ON`)
		livePinnedRun(t, db, ctx, dst.Name, script)
		liveExecIn(t, dst, ctx, `ALTER DATABASE SCOPED CONFIGURATION SET PREVIEW_FEATURES = OFF`)
	}
	got := liveRowsAsStrings(t, dst, ctx, `SELECT OBJECT_NAME(object_id) AS o, name, vector_base_type_desc AS base
FROM sys.parameters WHERE object_id IN (OBJECT_ID(N'dbo.P'), OBJECT_ID(N'dbo.F')) AND parameter_id > 0 ORDER BY o, parameter_id`)
	want := []string{"o=F name=@h base=float16", "o=P name=@h base=float16", "o=P name=@q base=float16", "o=P name=@v base=float32"}
	if !slices.Equal(got, want) {
		t.Errorf("replayed parameters = %q, want %q", got, want)
	}
}
