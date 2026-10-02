//go:build livedb

package gosmo

import (
	"strings"
	"testing"
)

// TestLiveReservedKeywords pins IsReservedKeyword and QuoteNameIfNeeded
// against the server's parser:
//
//   - every listed keyword, run through QuoteNameIfNeeded, names the column
//     and not a function (a bare User reads the USER function's 'dbo');
//
//   - every listed keyword but the five documented ones the parser tolerates
//     is refused bare as an alias, so the list has no stray entry;
//
//   - every ODBC or future keyword left off the list, and every keyword the
//     parser reads bare as something else without an error (USER and the
//     like — the dangerous omissions), is either listed or accepted bare as
//     the column.
//
//     go test -tags livedb . -run TestLiveReservedKeywords -v -livedb '...'
func TestLiveReservedKeywords(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	tolerated := map[string]bool{"DISK": true, "DUMP": true, "LOAD": true, "PRECISION": true, "SECURITYAUDIT": true}
	for w := range reservedKeywords {
		var got string
		q := "SELECT " + QuoteNameIfNeeded(w) + " FROM (SELECT N'col' AS " + QuoteName(w) + ") d"
		if err := db.QueryRowContext(ctx, q).Scan(&got); err != nil || got != "col" {
			t.Errorf("%s: got %q, %v", q, got, err)
		}
		_, err := db.ExecContext(ctx, "SELECT 1 AS "+w)
		if refused := err != nil; refused == tolerated[w] {
			t.Errorf("SELECT 1 AS %s: err %v; tolerated %v", w, err, tolerated[w])
		}
	}

	candidates := strings.Fields(`ABSOLUTE ACTION ADA ALLOCATE ARE ASSERTION AT AVG
BIT BIT_LENGTH BOTH CASCADED CAST CATALOG CHAR CHAR_LENGTH CHARACTER
CHARACTER_LENGTH COLLATION CONNECT CONNECTION CONSTRAINTS CORRESPONDING COUNT
DATE DAY DEC DECIMAL DEFERRABLE DEFERRED DESCRIBE DESCRIPTOR DIAGNOSTICS
DISCONNECT DOMAIN EXCEPTION EXTRACT FALSE FIRST FLOAT FORTRAN FOUND GET GLOBAL
GO HOUR IMMEDIATE INCLUDE INDICATOR INITIALLY INPUT INSENSITIVE INT INTEGER
INTERVAL ISOLATION LANGUAGE LAST LEADING LEVEL LOCAL LOWER MATCH MAX MIN MINUTE
MODULE MONTH NAMES NATURAL NCHAR NEXT NO NONE NUMERIC OCTET_LENGTH ONLY OUTPUT
OVERLAPS PAD PARTIAL PASCAL POSITION PREPARE PRESERVE PRIOR PRIVILEGES REAL
RELATIVE ROWS SCROLL SECOND SECTION SESSION SIZE SMALLINT SPACE SQL SQLCA
SQLCODE SQLERROR SQLSTATE SQLWARNING SUBSTRING SUM TEMPORARY TIME TIMESTAMP
TIMEZONE_HOUR TIMEZONE_MINUTE TRAILING TRANSLATE TRANSLATION TRIM TRUE UNKNOWN
UPPER USAGE USING VALUE VARCHAR WHENEVER WORK WRITE YEAR ZONE APPLY OFFSET TIES
PARTITION RANGE RETURNS ROW WITHIN NAME TYPE STATUS MATCHED TARGET SOURCE
USER CURRENT_USER SESSION_USER SYSTEM_USER CURRENT_TIMESTAMP CURRENT_DATE NULL`)
	for _, w := range candidates {
		if IsReservedKeyword(w) {
			continue
		}
		var got string
		q := "SELECT " + w + " FROM (SELECT N'col' AS " + QuoteName(w) + ") d"
		if err := db.QueryRowContext(ctx, q).Scan(&got); err != nil || got != "col" {
			t.Errorf("%s: got %q, %v — bare, it is not the column", q, got, err)
		}
	}
}
