package gosmo

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// A statement that must open its batch — CREATE SCHEMA, CREATE PROCEDURE —
// fails with Msg 111 when it shares one with the USE in front of it. The
// capture used to be "USE [db];\n" + stmt, one entry, so neither could ever
// run; a GO after every USE and after every statement is the fix.
func TestScriptedCreateSchemaAndProcedureEachOpenABatch(t *testing.T) {
	ctx, script := WithScript(context.Background())
	d := scriptTestDB()
	if err := d.CreateSchema(ctx, "sales", ""); err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}
	if err := d.CreateStoredProcedure(ctx, "sales", "usp_x", "AS\nSELECT 1"); err != nil {
		t.Fatalf("CreateStoredProcedure: %v", err)
	}
	if len(script.Entries) != 2 {
		t.Fatalf("Entries = %d, want 2", len(script.Entries))
	}
	want := "USE [App'DB];\nGO\n" +
		script.Entries[0].SQL + "\nGO\n\n" +
		script.Entries[1].SQL + "\nGO\n"
	if got := script.String(); got != want {
		t.Errorf("String() =\n%s\nwant\n%s", got, want)
	}
	for _, stmt := range script.Statements() {
		if !strings.HasPrefix(stmt, "USE [App'DB];\nGO\n") {
			t.Errorf("Statements() entry does not open its own batch after the USE:\n%s", stmt)
		}
	}
}

func TestScriptEntriesRecordWhereEachStatementRuns(t *testing.T) {
	ctx, script := WithScript(context.Background())
	s := &Server{info: &ServerInfo{Name: "SQL1"}}
	d := &Database{server: s, Name: "app"}
	if err := s.exec(ctx, "CREATE LOGIN [x]"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.exec(ctx, "CREATE USER [x]"); err != nil {
		t.Fatal(err)
	}
	if err := s.exec(WithScriptServer(ctx, "SQL2"), "ALTER AVAILABILITY GROUP [ag] JOIN"); err != nil {
		t.Fatal(err)
	}
	want := []ScriptEntry{
		{Server: "SQL1", SQL: "CREATE LOGIN [x]"},
		{Server: "SQL1", Database: "app", SQL: "CREATE USER [x]"},
		{Server: "SQL2", SQL: "ALTER AVAILABILITY GROUP [ag] JOIN"},
	}
	if !slices.Equal(script.Entries, want) {
		t.Errorf("Entries =\n%#v\nwant\n%#v", script.Entries, want)
	}
}

func TestScriptCollectorString(t *testing.T) {
	tests := []struct {
		name    string
		entries []ScriptEntry
		want    string
	}{
		{"empty", nil, ""},
		{
			"one database, USE once",
			[]ScriptEntry{
				{Database: "a", SQL: "S1"},
				{Database: "a", SQL: "S2"},
			},
			"USE [a];\nGO\nS1\nGO\n\nS2\nGO\n",
		},
		{
			"database changes",
			[]ScriptEntry{
				{Database: "a", SQL: "S1"},
				{Database: "b]c", SQL: "S2"},
			},
			"USE [a];\nGO\nS1\nGO\n\nUSE [b]]c];\nGO\nS2\nGO\n",
		},
		{
			// A leading server-scoped entry runs wherever the session is;
			// one after a database-scoped entry must not run inside it.
			"server-scoped after database-scoped leaves the database",
			[]ScriptEntry{
				{SQL: "S0"},
				{Database: "a", SQL: "S1"},
				{SQL: "DROP DATABASE [a]"},
			},
			"S0\nGO\n\nUSE [a];\nGO\nS1\nGO\n\nUSE [master];\nGO\nDROP DATABASE [a]\nGO\n",
		},
		{
			"one instance gets no header",
			[]ScriptEntry{{Server: "SQL1", SQL: "S1"}, {Server: "SQL1", SQL: "S2"}},
			"S1\nGO\n\nS2\nGO\n",
		},
		{
			// Each instance's run is a different session, so the database is
			// stated again even though the previous run was already in it.
			"instances labelled, database restated per instance",
			[]ScriptEntry{
				{Server: "SQL1", Database: "a", SQL: "S1"},
				{Server: "SQL2", Database: "a", SQL: "S2"},
				{Server: "SQL2", SQL: "S3"},
				{Server: "", SQL: "S4"},
			},
			"-- on SQL1\nUSE [a];\nGO\nS1\nGO\n\n" +
				"-- on SQL2\nUSE [a];\nGO\nS2\nGO\n\nUSE [master];\nGO\nS3\nGO\n\n" +
				"-- on (unknown instance)\nS4\nGO\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &ScriptCollector{Entries: tc.entries}
			if got := c.String(); got != tc.want {
				t.Errorf("String() =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}
