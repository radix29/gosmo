package gosmo

import (
	"context"
	"strings"
	"testing"
)

// Database.SearchContext's SQL, captured off the driver — the same reach
// extended_properties_read_test.go uses, and the only one there is for a read:
// WithScript intercepts writes only.
//
// The rule under test is ObjectFilter.clause's, which this statement did not
// follow. A bare `o.name LIKE '%' + @p1 + '%'` compares under the database's
// collation, so on a case-sensitive one a search for "customer" does not find
// Customer — the search box comes up empty with nothing to say why, on the one
// kind of database where the user is least able to guess the casing.
//
// Wrapping only the column is the other half of the same mistake, and is worse:
// `LOWER(o.name) LIKE '%' + @p1 + '%'` matches nothing at all for a pattern with
// any upper-case letter in it, on every collation. Both mutants are killed here.

func TestSearchComparesCaseInsensitivelyWhateverTheCollation(t *testing.T) {
	d := captureDatabase(t)

	// The capture driver returns no rows; the statement generated on the way is
	// what is under test.
	if _, err := d.SearchContext(context.Background(), "Customer"); err != nil {
		t.Fatalf("SearchContext: %v", err)
	}
	q := captured.find("sys.objects")
	if q == "" {
		t.Fatal("no sys.objects statement was issued")
	}

	// The whole predicate, not a "contains LOWER" test: lowering one side only
	// still contains LOWER, and is the mutant that matches nothing.
	const want = `LOWER(o.name) LIKE '%' + LOWER(@p1) + '%' ESCAPE '\'`
	if !strings.Contains(q, want) {
		t.Errorf("statement:\n%s\nwant it to contain:\n%s\n"+
			"— both sides of the LIKE must be lowered, or the match follows the "+
			"database collation (bare) or fails outright (column only)", q, want)
	}
}

// The escaping the LIKE's ESCAPE clause exists for. Held here beside the
// case rule because the two are one decision — a pattern that is lowered but
// not escaped, or escaped but compared bare, is wrong in the same statement.
func TestSearchEscapesWildcardsInThePattern(t *testing.T) {
	d := captureDatabase(t)

	// _ is legal in an identifier and is LIKE's single-character wildcard, so
	// unescaped, a search for pct_1 also finds pct11.
	if _, err := d.SearchContext(context.Background(), "pct_1"); err != nil {
		t.Fatalf("SearchContext: %v", err)
	}
	if q := captured.find("sys.objects"); !strings.Contains(q, `ESCAPE '\'`) {
		t.Errorf("statement:\n%s\nhas no ESCAPE clause — the backslashes likeEscape "+
			"adds are then matched as themselves", q)
	}
}
