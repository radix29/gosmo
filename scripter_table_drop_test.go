package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"testing"
)

// A table with a module schema-bound to it — every natively compiled module
// that reads a memory-optimized table is — cannot be dropped: DROP TABLE
// fails with Msg 3729 and leaves it in place, and a DROP AND CREATE's guarded
// CREATE is then skipped. The script says so, and why, ahead of the DROP.
func TestTableDropNamesWhatIsSchemaBoundToIt(t *testing.T) {
	deps := []schemaBoundDependent{
		{schema: "dbo", name: "p]nc", typeDesc: "SQL_STORED_PROCEDURE", native: true},
		{schema: "dbo", name: "v\nDROP DATABASE x", typeDesc: "VIEW"},
	}
	script := func(verb ScriptVerb, versioned bool, deps []schemaBoundDependent) string {
		opts := DefaultScriptOptions()
		opts.Verb = verb
		p := tableScriptParts{cols: []*Column{{Name: "id", DataType: DataTypeInt}}, boundBy: deps}
		p.table.SystemVersioned = versioned
		if versioned {
			p.table.HistorySchema, p.table.HistoryTable = "dbo", "H"
		}
		return buildTableScript("dbo", "T", "db", p, opts)
	}

	for _, verb := range []ScriptVerb{ScriptDrop, ScriptDropAndCreate} {
		got := script(verb, false, deps)
		note := strings.Index(got, "-- This DROP fails with Msg 3729 while these are schema-bound to [dbo].[T]:\n")
		if note != 0 {
			t.Errorf("verb %d: want the note first, before the drop guard:\n%s", verb, got)
		}
		for _, want := range []string{
			"--   [dbo].[p]]nc] (SQL_STORED_PROCEDURE, natively compiled)\n",
			"--   [dbo].[v DROP DATABASE x] (VIEW)\n",
			"-- A natively compiled module requires SCHEMABINDING: drop it and recreate it after.\n",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("verb %d: want %q:\n%s", verb, want, got)
			}
		}
		if strings.Contains(got, "\nDROP DATABASE") {
			t.Errorf("verb %d: a line break in a name escaped the comment:\n%s", verb, got)
		}
	}

	got := script(ScriptDropAndCreate, true, deps[1:])
	if note, alter := strings.Index(got, "-- This DROP fails with Msg 3729"),
		strings.Index(got, "SET (SYSTEM_VERSIONING = OFF)"); note != 0 || alter < note {
		t.Errorf("want a temporal table's note ahead of its versioning ALTER, which is refused too:\n%s", got)
	}
	if strings.Contains(got, "natively compiled") {
		t.Errorf("want no native-compilation line without a native module:\n%s", got)
	}

	if got := script(ScriptDropAndCreate, false, nil); strings.Contains(got, "Msg 3729") {
		t.Errorf("want no note with nothing schema-bound:\n%s", got)
	}

	ft := tableScriptParts{boundBy: deps[:1]}
	ft.table.IsFileTable = true
	opts := DefaultScriptOptions()
	opts.Verb = ScriptDrop
	if got := buildFileTableScript("dbo", "F", "db", ft, opts); !strings.HasPrefix(got, "-- This DROP fails with Msg 3729") {
		t.Errorf("want a FileTable's DROP to carry the note too:\n%s", got)
	}
}

// ScriptTable reads the schema-bound dependents only for a script that drops
// the table, and the note reaches the script.
func TestScriptTableReadsSchemaBoundDependentsOnlyToDrop(t *testing.T) {
	dependents := cannedRow{
		match: "sys.sql_expression_dependencies",
		cols:  []string{"schema", "name", "type", "native"},
		row:   []driver.Value{"dbo", "p_nc", "SQL_STORED_PROCEDURE", true},
	}
	for _, c := range []struct {
		verb ScriptVerb
		read bool
	}{
		{ScriptCreate, false},
		{ScriptAlter, false},
		{ScriptDrop, true},
		{ScriptDropAndCreate, true},
	} {
		db, err := sql.Open("capture", "")
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		captured.reset(tableMetadataRow("dbo", "mo"), dependents)
		opts := DefaultScriptOptions()
		opts.Verb = c.verb
		d := &Database{server: &Server{db: db}, Name: "testdb"}
		got, err := NewScripter(d, opts).ScriptTable(context.Background(), "dbo", "mo")
		db.Close()
		if err != nil {
			t.Fatalf("verb %d: ScriptTable: %v", c.verb, err)
		}
		if n := captured.count("sys.sql_expression_dependencies"); (n == 1) != c.read {
			t.Errorf("verb %d: dependents read %d times, want read=%v", c.verb, n, c.read)
		}
		if strings.Contains(got, "--   [dbo].[p_nc] (SQL_STORED_PROCEDURE, natively compiled)") != c.read {
			t.Errorf("verb %d: note present=%v, want %v:\n%s", c.verb, !c.read, c.read, got)
		}
	}
}
