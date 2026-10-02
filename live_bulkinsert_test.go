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
	"testing"
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
	if n := count(); n != 0 || len(c.Entries) != 0 {
		t.Errorf("a scripted bulk insert left %d rows and collected %v, want neither", n, c.Entries)
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
