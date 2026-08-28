package gosmo

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// The read side of extended properties, which the WithScript tests in
// script_extended_properties_write_test.go cannot reach: WithScript intercepts
// writes only, so ExtendedPropertiesContext's SQL is captured off the driver.
//
// The rule under test is the one the writes already follow — an unused level
// argument is the NULL keyword, not an empty string literal. The read spelled
// level 0 with hard-coded quotes and so sent an empty N-literal where the
// writes send NULL, which fn_listextendedproperty reads as a level named by
// the empty string. A zero ExtendedPropertyLevel therefore wrote against the
// database and read back nothing, with no error on either side.

// captureDatabase returns a Database wired to the capture driver
// (identifier_quoting_test.go).
func captureDatabase(t *testing.T) *Database {
	t.Helper()
	db, err := sql.Open("capture", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	captured.reset()
	return &Database{server: &Server{db: db}, name: "testdb"}
}

func TestExtendedPropertiesReadNullsAnUnusedLevel(t *testing.T) {
	cases := []struct {
		name  string
		level ExtendedPropertyLevel
		want  []string
		bad   []string
	}{
		{
			name:  "database level names no level at all",
			level: ExtendedPropertyLevel{},
			// Every one of the six arguments is absent, so every one is NULL.
			// The empty-literal form is what this test exists to catch.
			want: []string{"NULL,\n           NULL,\n           NULL,\n           NULL,\n           NULL,\n           NULL"},
			bad:  []string{"N''"},
		},
		{
			name:  "schema level nulls levels 1 and 2",
			level: ExtendedPropertyLevel{Level0Type: "SCHEMA", Level0Name: "dbo"},
			want:  []string{"N'SCHEMA'", "N'dbo'"},
			bad:   []string{"N''"},
		},
		{
			name: "a quote in a level name is escaped, not ended",
			level: ExtendedPropertyLevel{
				Level0Type: "SCHEMA", Level0Name: "dbo",
				Level1Type: "TABLE", Level1Name: "Sales'Archive",
			},
			want: []string{"N'Sales''Archive'"},
			bad:  []string{"N''"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := captureDatabase(t)
			// The capture driver returns no rows; the statement generated on
			// the way is what is under test.
			if _, err := d.ExtendedPropertiesContext(context.Background(), tc.level); err != nil {
				t.Fatalf("ExtendedPropertiesContext: %v", err)
			}
			q := captured.find("fn_listextendedproperty")
			if q == "" {
				t.Fatal("no fn_listextendedproperty statement was issued")
			}
			for _, want := range tc.want {
				if !strings.Contains(q, want) {
					t.Errorf("statement:\n%s\nwant it to contain:\n%s", q, want)
				}
			}
			for _, bad := range tc.bad {
				if strings.Contains(q, bad) {
					t.Errorf("statement:\n%s\nmust not contain %s — an absent level is NULL, not an empty literal", q, bad)
				}
			}
		})
	}
}
