//go:build livedb

// Live parity of SameName/NameKey with the server's own `=` under a
// collation, for the Turkish dotted/dotless I above all. Read-only: plain
// SELECTs with COLLATE clauses, no object is created.
//
//	go test -tags livedb . -run TestLiveSameNameMatchesServerCollation -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
package gosmo

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// bareCollation is a collation name safe to splice into a COLLATE clause,
// which takes no parameter.
var bareCollation = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

func TestLiveSameNameMatchesServerCollation(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	// The fixed pairs under the named collations: gosmo's answer is the
	// server's, and the offline table (turkicNameCases) states it too.
	for _, c := range turkicNameCases {
		for i, p := range turkicNamePairs {
			var same int
			q := "SELECT CASE WHEN @p1 = @p2 COLLATE " + c.collation + " THEN 1 ELSE 0 END"
			if err := db.QueryRowContext(ctx, q, p.a, p.b).Scan(&same); err != nil {
				t.Fatalf("%s: %v", c.collation, err)
			}
			if got := SameName(c.collation, p.a, p.b); got != (same == 1) {
				t.Errorf("SameName(%q, %q, %q) = %v, server says %v", c.collation, p.a, p.b, got, same == 1)
			}
			if c.same[i] != (same == 1) {
				t.Errorf("turkicNameCases[%q] for %q/%q = %v, server says %v", c.collation, p.a, p.b, c.same[i], same == 1)
			}
		}
	}

	// Every case-insensitive, accent-sensitive collation the server has:
	// SameName answers as the server for `id`/`İD`, `id`/`ID` and `Ĳ`/`ĳ`.
	// An _AI collation is left out: it makes `İ` and `I` one letter by
	// accent, which SameName does not model.
	rows, err := db.QueryContext(ctx, "SELECT name FROM sys.fn_helpcollations()")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		if CollationIgnoresCase(n) && !accentInsensitive(n) {
			if !bareCollation.MatchString(n) {
				t.Fatalf("collation name %q is not a bare identifier", n)
			}
			names = append(names, n)
		}
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("sys.fn_helpcollations() listed no case-insensitive collation")
	}
	var sb strings.Builder
	for i, n := range names {
		if i > 0 {
			sb.WriteString(" UNION ALL ")
		}
		fmt.Fprintf(&sb, "SELECT %d, CASE WHEN N'id' = N'İD' COLLATE %s THEN 1 ELSE 0 END, CASE WHEN N'id' = N'ID' COLLATE %s THEN 1 ELSE 0 END, CASE WHEN N'\u0132' = N'\u0133' COLLATE %s THEN 1 ELSE 0 END", i, n, n, n)
	}
	rows, err = db.QueryContext(ctx, sb.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	turkic := 0
	for rows.Next() {
		var i, dotted, plain, ij int
		if err := rows.Scan(&i, &dotted, &plain, &ij); err != nil {
			t.Fatal(err)
		}
		n := names[i]
		if dotted == 1 {
			turkic++
		}
		if SameName(n, "id", "İD") != (dotted == 1) || SameName(n, "id", "ID") != (plain == 1) {
			t.Errorf("SameName under %q disagrees with the server", n)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d case-insensitive, accent-sensitive collations, %d with Turkic case", len(names), turkic)
}

// accentInsensitive reports whether collation has an AI token.
func accentInsensitive(collation string) bool {
	for tok := range strings.SplitSeq(strings.ToUpper(collation), "_") {
		if tok == "AI" {
			return true
		}
	}
	return false
}
