//go:build livedb

// Live verification that MODIFY FILE takes a size of 2 TB and up: the
// server parses the number as an int, so 2147483648KB is Msg 102 and the
// statement has to switch to MB.
//
//	go test -tags livedb . -run TestLiveAlterFile -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
package gosmo

import "testing"

func TestLiveAlterFileMaxSizeOfTwoTerabytes(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	const name = "gosmo_alterfile_2tb"
	d, drop := liveScratchDB(t, db, ctx, name)
	defer drop()

	const twoTB = int64(2) << 30 // KB
	if err := d.FileRef(name).Alter(ctx, FileModify{MaxSizeKB: twoTB}); err != nil {
		t.Fatalf("Alter MaxSizeKB 2 TB: %v", err)
	}
	var pages int64
	if err := db.QueryRowContext(ctx,
		`SELECT max_size FROM sys.master_files WHERE database_id = DB_ID(@p1) AND name = @p1`, name).Scan(&pages); err != nil {
		t.Fatalf("read max_size: %v", err)
	}
	if want := twoTB / 8; pages != want {
		t.Errorf("max_size = %d pages, want %d", pages, want)
	}
}
