package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
)

// The four sub-folder kinds and the plain user list have to partition
// sys.tables: a table listed under two folders is as wrong as one listed
// under none. The clauses are text, so this pins the four exclusions the
// user clause carries — dropping any one of them is the plausible edit, and
// it shows up as a filetable (or an external, or a graph table) listed twice.
func TestUserTableClauseExcludesEveryOtherKind(t *testing.T) {
	user := kindClause(int(SQLServer2017), TableKindUser)
	for _, want := range []string{
		"t.is_ms_shipped = 0",
		"t.is_filetable = 0",
		"t.is_external = 0",
		"NOT (t.is_node = 1 OR t.is_edge = 1)",
	} {
		if !strings.Contains(user, want) {
			t.Errorf("user-table clause does not exclude %s:\n%s", want, user)
		}
	}
	for kind, want := range map[TableKind]string{
		TableKindSystem:    "t.is_ms_shipped = 1",
		TableKindFileTable: "t.is_filetable = 1",
		TableKindExternal:  "t.is_external = 1",
		TableKindGraph:     "(t.is_node = 1 OR t.is_edge = 1)",
	} {
		got := kindClause(int(SQLServer2017), kind)
		if !strings.Contains(got, want) {
			t.Errorf("%s clause does not select %s:\n%s", kind, want, got)
		}
	}
}

// On an instance with no is_node/is_edge the predicate has to be false for
// every row rather than name a column that is not there — naming it fails the
// whole read, which is what the 2016 floor makes routine.
func TestGraphPredicateNamesNoColumnBefore2017(t *testing.T) {
	for _, kind := range []TableKind{TableKindUser, TableKindGraph} {
		got := kindClause(int(SQLServer2016), kind)
		if strings.Contains(got, "is_node") || strings.Contains(got, "is_edge") {
			t.Errorf("%s clause at major 13 names a 2017 column:\n%s", kind, got)
		}
	}
}

// A graph listing on a server that cannot have graph tables is refused, not
// answered with an empty list: "this database has no graph tables" and "this
// server has no such thing" are different answers, and only the refusal tells
// a tree builder to leave the folder out.
func TestGraphTableListingIsRefusedBefore2017(t *testing.T) {
	for _, c := range []struct {
		major   int
		refused bool
	}{
		{13, true},
		{14, false},
		{17, false},
		{0, false}, // never read (Azure, or a Server built without NewServer)
	} {
		err := dbAtMajor(c.major).requireGraphTables()
		if got := err != nil; got != c.refused {
			t.Errorf("major %d: refused = %v, want %v (err %v)", c.major, got, c.refused, err)
		}
		if err != nil && !errors.Is(err, ErrUnsupportedVersion) {
			t.Errorf("major %d: %v does not wrap ErrUnsupportedVersion", c.major, err)
		}
	}
}

// MAX() over no rows is NULL, which is what a database with no tables at all
// returns — a scan straight into bool fails there, and the failure would only
// ever show up on an empty database.
func TestTableKindsPresentOnADatabaseWithNoTables(t *testing.T) {
	db, err := sql.Open("capture", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	captured.reset(cannedRow{
		match: "FROM   sys.tables t",
		cols:  []string{"sys", "ft", "ext", "graph"},
		row:   []driver.Value{nil, nil, nil, nil},
	})

	d := &Database{server: &Server{db: db}, name: "testdb"}
	got, err := d.TableKindsPresentContext(context.Background())
	if err != nil {
		t.Fatalf("TableKindsPresentContext: %v", err)
	}
	if (got != TableKindPresence{}) {
		t.Errorf("presence = %+v, want every field false", got)
	}
}
