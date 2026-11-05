//go:build livedb

// Live verification of BulkInsert's WithScript and statement-observer
// handling (T22): an observed load reports one INSERT BULK entry carrying the
// server's row count, and a scripted one refuses and copies nothing.
//
//	go test -tags livedb . -run TestLiveBulkInsert -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own database; touches nothing else.
package gosmo

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLiveBulkInsertObservedAndScripted(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const name = "gosmo_bulkinsert_live"
	d, drop := liveScratchDB(t, db, ctx, name)
	defer drop()
	if _, err := db.ExecContext(ctx, "CREATE TABLE "+quoteIdent(name)+".dbo.t (a int, [b]]c] nvarchar(10))"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	count := func() int {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdent(name)+".dbo.t").Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	bc := BulkCopy{Schema: "dbo", Table: "t", Columns: []string{"a", "b]c"}}
	rows := SliceRows([][]any{{1, "x"}, {2, "y"}, {3, nil}})

	sctx, c := WithScript(ctx)
	if _, err := d.BulkInsert(sctx, bc, rows); !errors.Is(err, ErrUnsupported) {
		t.Errorf("BulkInsert under WithScript = %v, want an ErrUnsupported error", err)
	}
	if n := count(); n != 0 || c.Len() != 0 {
		t.Errorf("a scripted bulk insert left %d rows and collected %v, want neither", n, c.Entries())
	}

	octx, got := observed(ctx)
	n, err := d.BulkInsert(octx, bc, rows)
	if err != nil {
		t.Fatalf("BulkInsert: %v", err)
	}
	if n != 3 || count() != 3 {
		t.Errorf("BulkInsert copied %d (table has %d), want 3", n, count())
	}
	want := "INSERT BULK [dbo].[t] ([a], [b]]c]) -- 3 rows"
	if len(*got) != 1 || (*got)[0].SQL != want || (*got)[0].Database != name {
		t.Errorf("observed %+v, want one entry %q in %s", *got, want, name)
	}
}

// TestLiveBulkInsertChecksValues: what go-mssqldb alone loaded wrong (review
// plan 2026-11-04 A2) is now converted or refused. A varchar's text arrives
// in its code page, not as UTF-8 bytes read as the database's; smalldatetime
// rounds as the server does; 300 for a tinyint, and a character the
// database's code page lacks, are refused naming the row and column, and
// leave the table as it was.
func TestLiveBulkInsertChecksValues(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const name = "gosmo_bulkcheck_live"
	d, drop := liveScratchDB(t, db, ctx, name)
	defer drop()
	if _, err := db.ExecContext(ctx, "ALTER DATABASE "+quoteIdent(name)+" COLLATE Latin1_General_CI_AS"); err != nil {
		t.Fatalf("collate database: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE "+quoteIdent(name)+`.dbo.t (ti tinyint,
		vc varchar(10) COLLATE Latin1_General_CI_AS, vcc varchar(10) COLLATE Cyrillic_General_CI_AS,
		sdt smalldatetime, nc nchar(3))`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	bc := BulkCopy{Schema: "dbo", Table: "t", Columns: []string{"ti", "vc", "vcc", "sdt", "nc"}}
	when := time.Date(2024, 2, 29, 13, 45, 29, 999000000, time.UTC)
	if _, err := d.BulkInsert(ctx, bc, SliceRows([][]any{{uint8(7), "café", nil, when, "é"}})); err != nil {
		t.Fatalf("BulkInsert: %v", err)
	}
	var vc, sdt, nc string
	if err := db.QueryRowContext(ctx, "SELECT CONVERT(varchar(30), CAST(vc AS varbinary(30)), 1), "+
		"CONVERT(varchar(16), sdt, 120), nc + '|' FROM "+quoteIdent(name)+".dbo.t").Scan(&vc, &sdt, &nc); err != nil {
		t.Fatal(err)
	}
	if vc != "0x636166E9" || sdt != "2024-02-29 13:46" || nc != "é  |" {
		t.Errorf("stored vc %s, sdt %s, nc %q; want 0x636166E9, 2024-02-29 13:46, \"é  |\"", vc, sdt, nc)
	}

	for _, c := range []struct {
		row  []any
		want string
	}{
		{[]any{300, nil, nil, nil, nil}, "row 2, column [ti]: 300 is out of range (0 to 255)"},
		{[]any{1, nil, "Ж", nil, nil}, "row 2, column [vcc]: 'Ж' (U+0416) is not in the database's code page 1252"},
	} {
		_, err := d.BulkInsert(ctx, bc, SliceRows([][]any{{1, "ok", nil, nil, nil}, c.row}))
		if _, ok := errors.AsType[*BulkValueError](err); !ok || !strings.HasSuffix(err.Error(), c.want) {
			t.Errorf("BulkInsert %v = %v, want a *BulkValueError ending %q", c.row, err, c.want)
		}
	}
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdent(name)+".dbo.t").Scan(&n); err != nil || n != 1 {
		t.Errorf("table has %d rows (%v) after the refused loads, want 1", n, err)
	}
}
