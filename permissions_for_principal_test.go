package gosmo

import (
	"database/sql/driver"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// PermissionsForPrincipal reports each row under the class ApplyPermission
// takes back, so a Securables page can list a grant and revoke it. Before
// review plan 2026-10-15 § P3 it read tables and views only, and a role's
// EXECUTE on a procedure was invisible.
func TestPermissionsForPrincipalReportsEveryClass(t *testing.T) {
	d := captureDatabase(t)
	cols := []string{"class", "perm", "state", "schema", "name", "type"}
	row := func(class, schema, name, typ string) []driver.Value {
		return []driver.Value{class, "EXECUTE", "GRANT", schema, name, typ}
	}
	captured.reset(cannedRow{match: "FROM   sys.database_permissions dp", cols: cols, rows: [][]driver.Value{
		row("DATABASE", "", "", ""),
		row("SCHEMA", "sales", "", ""),
		row("OBJECT_OR_COLUMN", "dbo", "t", "U"),
		row("OBJECT_OR_COLUMN", "dbo", "v", "V"),
		row("OBJECT_OR_COLUMN", "dbo", "p", "P"),
		row("OBJECT_OR_COLUMN", "dbo", "clr_p", "PC"),
		row("OBJECT_OR_COLUMN", "dbo", "sf", "FN"),
		row("OBJECT_OR_COLUMN", "dbo", "clr_sf", "FS"),
		row("OBJECT_OR_COLUMN", "dbo", "if", "IF"),
		row("OBJECT_OR_COLUMN", "dbo", "tf", "TF"),
		row("OBJECT_OR_COLUMN", "dbo", "clr_tf", "FT"),
		row("OBJECT_OR_COLUMN", "dbo", "sq", "SO"),
		row("OBJECT_OR_COLUMN", "dbo", "syn", "SN"),
		row("TYPE", "dbo", "ty", ""),
		row("CERTIFICATE", "", "ce", ""),
	}})

	got, err := d.PermissionsForPrincipal(t.Context(), "app_role")
	if err != nil {
		t.Fatal(err)
	}
	want := []PrincipalSecurable{
		{SecurableDatabase, "", "", "EXECUTE", "GRANT"},
		{SecurableSchema, "", "sales", "EXECUTE", "GRANT"},
		{SecurableTable, "dbo", "t", "EXECUTE", "GRANT"},
		{SecurableView, "dbo", "v", "EXECUTE", "GRANT"},
		{SecurableProcedure, "dbo", "p", "EXECUTE", "GRANT"},
		{SecurableProcedure, "dbo", "clr_p", "EXECUTE", "GRANT"},
		{SecurableScalarFunction, "dbo", "sf", "EXECUTE", "GRANT"},
		{SecurableScalarFunction, "dbo", "clr_sf", "EXECUTE", "GRANT"},
		{SecurableInlineFunction, "dbo", "if", "EXECUTE", "GRANT"},
		{SecurableTableFunction, "dbo", "tf", "EXECUTE", "GRANT"},
		{SecurableTableFunction, "dbo", "clr_tf", "EXECUTE", "GRANT"},
		{SecurableSequence, "dbo", "sq", "EXECUTE", "GRANT"},
		{SecurableSynonym, "dbo", "syn", "EXECUTE", "GRANT"},
		{SecurableUserType, "dbo", "ty", "EXECUTE", "GRANT"},
		{SecurableCertificate, "", "ce", "EXECUTE", "GRANT"},
	}
	if len(got) != len(want) {
		t.Fatalf("%d entries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if *got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, *got[i], want[i])
		}
	}

	// The type codes the query filters on are the ones the scan maps, and
	// every class below the database that ApplyPermission takes is read.
	q := captured.find("FROM   sys.database_permissions dp")
	for code := range objectSecurableClasses {
		if !strings.Contains(q, "'"+code+"'") {
			t.Errorf("the query does not read type %s:\n%s", code, q)
		}
	}
	for _, class := range []string{"'TYPE'", "'CERTIFICATE'", "sys.types", "sys.certificates"} {
		if !strings.Contains(q, class) {
			t.Errorf("the query does not read %s:\n%s", class, q)
		}
	}
}

// Every class ApplyPermission takes below the database is one a read reports
// and a search finds: a class missing from objectSecurableClasses or
// securableSearchOrder is a grant the Securables page cannot see or add.
func TestEverySecurableClassIsReadAndSearched(t *testing.T) {
	read := map[SecurableClass]bool{SecurableSchema: true, SecurableUserType: true, SecurableCertificate: true}
	for _, c := range objectSecurableClasses {
		read[c] = true
	}
	for _, c := range []SecurableClass{
		SecurableSchema, SecurableTable, SecurableView, SecurableProcedure,
		SecurableScalarFunction, SecurableInlineFunction, SecurableTableFunction,
		SecurableSequence, SecurableSynonym, SecurableUserType, SecurableCertificate,
	} {
		if !read[c] {
			t.Errorf("PermissionsForPrincipal never reports %s", c)
		}
		if !slices.Contains(securableSearchOrder, c) {
			t.Errorf("FindSecurables never lists %s", c)
		}
	}
}

// TestSchemaScopedAgreesWithOnClause pins SchemaScoped to the ON clause:
// a class is schema-scoped exactly when on() renders its name two-part, so
// a new class cannot be qualified in one and bare in the other.
func TestSchemaScopedAgreesWithOnClause(t *testing.T) {
	for _, c := range []SecurableClass{
		SecurableServer, SecurableDatabase, SecurableSchema, SecurableTable, SecurableView,
		SecurableProcedure, SecurableScalarFunction, SecurableInlineFunction, SecurableTableFunction,
		SecurableSequence, SecurableSynonym, SecurableUserType, SecurableCertificate,
	} {
		qualified := strings.Contains(Securable{Class: c, Schema: "s", Name: "n"}.on(), "[s].[n]")
		if c.SchemaScoped() != qualified {
			t.Errorf("%s: SchemaScoped() = %v, but on() qualifies the name: %v", c, c.SchemaScoped(), qualified)
		}
	}
}

func TestFindSecurablesSearchesEveryClass(t *testing.T) {
	d := captureDatabase(t)
	captured.reset(cannedRow{match: "TOP (@p2)", cols: []string{"kind", "schema", "name"}, rows: [][]driver.Value{
		{"SCHEMA", "", "sales"},
		{"PROCEDURE", "dbo", "p"},
		{"CERTIFICATE", "", "ce"},
	}})
	got, err := d.FindSecurables(t.Context(), SecurableSearch{Name: "x", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	want := []Securable{{Class: SecurableSchema, Name: "sales"}, {Class: SecurableProcedure, Schema: "dbo", Name: "p"}, {Class: SecurableCertificate, Name: "ce"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindSecurables = %+v, want %+v", got, want)
	}

	q := captured.find("TOP (@p2)")
	for code, class := range objectSecurableClasses {
		if !strings.Contains(q, "WHEN '"+code+"' THEN '"+string(class)+"'") {
			t.Errorf("the search does not map type %s to %s:\n%s", code, class, q)
		}
	}
	for i, class := range securableSearchOrder {
		if want := fmt.Sprintf("WHEN '%s' THEN %d ", class, i); !strings.Contains(q, want) {
			t.Errorf("the search does not rank %s at %d:\n%s", class, i, q)
		}
	}
	for _, src := range []string{"sys.schemas", "sys.objects", "sys.types", "sys.certificates"} {
		if !strings.Contains(q, src) {
			t.Errorf("the search does not read %s:\n%s", src, q)
		}
	}
}
