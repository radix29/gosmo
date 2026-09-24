//go:build livedb

// Live verification that Parameter.TypeString qualifies a user-defined type,
// so the fragment resolves to the parameter's own type under any default
// schema.
//
//	go test -tags livedb . -run TestLiveParameterTypeString -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
package gosmo

import "testing"

// TestLiveParameterTypeStringQualifiesAliasTypes (G6): app.Phone is a varchar
// alias and dbo.Phone an int alias, so the bare "Phone" the parameter used to
// render as resolves, for a dbo-default user, to the int — and a DECLARE
// built from it rejects a string that the parameter itself accepts.
func TestLiveParameterTypeStringQualifiesAliasTypes(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_paramtype_live")
	defer drop()

	liveExecIn(t, d, ctx,
		`CREATE SCHEMA app`,
		`CREATE TYPE app.Phone FROM varchar(20) NOT NULL`,
		`CREATE TYPE dbo.Phone FROM int NOT NULL`,
		`CREATE PROCEDURE dbo.usp_phone @p app.Phone, @n nvarchar(10), @o app.Phone OUTPUT AS SET @o = @p`,
	)

	params, err := d.Parameters(ctx, "dbo", "usp_phone")
	if err != nil {
		t.Fatalf("Parameters: %v", err)
	}
	want := []string{"[app].[Phone]", "nvarchar(10)", "[app].[Phone]"}
	if len(params) != len(want) {
		t.Fatalf("Parameters returned %d rows, want %d", len(params), len(want))
	}
	for i, p := range params {
		if got := p.TypeString(); got != want[i] {
			t.Errorf("%s TypeString = %q, want %q (TypeSchema %q, IsUserDefinedType %v)",
				p.Name, got, want[i], p.TypeSchema, p.IsUserDefinedType)
		}
	}

	// The fragment must bind as the parameter's type on the server, not only
	// read right: 'abc' is a valid app.Phone and not a valid dbo.Phone.
	stmt := "DECLARE @o " + params[2].TypeString() + "; EXEC dbo.usp_phone @p = 'abc', @n = N'x', @o = @o OUTPUT; IF @o <> 'abc' THROW 50000, 'wrong value', 1"
	if _, err := d.exec(ctx, stmt); err != nil {
		t.Errorf("EXEC built from TypeString failed: %v\n%s", err, stmt)
	}
}
