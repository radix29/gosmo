//go:build livedb

// Live verification that Schema.ObjectCountsByTypeContext agrees with the six
// listings it replaced in gossms's Schema Properties > General page.
//
// The page used to fetch every view, procedure, function, synonym and
// sequence in the database and count the ones whose Schema matched. The whole
// point of the single query is that it returns the same six numbers, so the
// only test worth writing compares it against those listings on a schema
// built to have something of each kind — including the shapes where the
// listings' predicates differ from a naive type-code GROUP BY.
//
//	go test -tags livedb . -run TestLiveSchemaObjectCounts -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
package gosmo

import (
	"context"
	"testing"
)

func TestLiveSchemaObjectCountsMatchTheListings(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_schemacounts_live")
	defer drop()

	// Two schemas with objects in both: a count that ignored the schema
	// filter, or applied it to the wrong column, still looks plausible
	// against one schema alone.
	liveExecIn(t, d, ctx,
		`CREATE SCHEMA app`,
		`CREATE SCHEMA other`,
		`CREATE TABLE app.t1 (id INT NOT NULL PRIMARY KEY)`,
		`CREATE TABLE app.t2 (id INT NOT NULL PRIMARY KEY)`,
		`CREATE TABLE other.t3 (id INT NOT NULL PRIMARY KEY)`,
		`CREATE VIEW app.v1 AS SELECT id FROM app.t1`,
		`CREATE VIEW other.v2 AS SELECT id FROM other.t3`,
		`CREATE PROCEDURE app.p1 AS SELECT 1`,
		`CREATE PROCEDURE app.p2 WITH ENCRYPTION AS SELECT 2`,
		`CREATE FUNCTION app.fn_scalar (@a INT) RETURNS INT AS BEGIN RETURN @a END`,
		`CREATE FUNCTION app.fn_inline () RETURNS TABLE AS RETURN SELECT 1 AS n`,
		`CREATE FUNCTION app.fn_table () RETURNS @r TABLE (n INT)
		 AS BEGIN INSERT @r VALUES (1); RETURN END`,
		`CREATE SYNONYM app.syn1 FOR app.t1`,
		`CREATE SYNONYM other.syn2 FOR other.t3`,
		`CREATE SEQUENCE app.seq1 AS INT START WITH 1`,
	)

	// countsFromListings is what the page did before: fetch everything, keep
	// what matches the schema.
	countsFromListings := func(t *testing.T, schema string) SchemaObjectCounts {
		t.Helper()
		var c SchemaObjectCounts
		tables, err := d.TablesBySchemaContext(ctx, schema)
		if err != nil {
			t.Fatalf("tables: %v", err)
		}
		c.Tables = len(tables)
		views, err := d.ViewsContext(ctx)
		if err != nil {
			t.Fatalf("views: %v", err)
		}
		for _, v := range views {
			if v.Schema == schema {
				c.Views++
			}
		}
		procs, err := d.StoredProceduresContext(ctx)
		if err != nil {
			t.Fatalf("procedures: %v", err)
		}
		for _, p := range procs {
			if p.Schema == schema {
				c.StoredProcedures++
			}
		}
		funcs, err := d.UserDefinedFunctionsContext(ctx)
		if err != nil {
			t.Fatalf("functions: %v", err)
		}
		for _, fn := range funcs {
			if fn.Schema == schema {
				c.Functions++
			}
		}
		syns, err := d.SynonymsContext(ctx)
		if err != nil {
			t.Fatalf("synonyms: %v", err)
		}
		for _, s := range syns {
			if s.Schema == schema {
				c.Synonyms++
			}
		}
		seqs, err := d.SequencesContext(ctx)
		if err != nil {
			t.Fatalf("sequences: %v", err)
		}
		for _, s := range seqs {
			if s.Schema == schema {
				c.Sequences++
			}
		}
		return c
	}

	for _, schema := range []string{"app", "other", "dbo"} {
		t.Run(schema, func(t *testing.T) {
			sc, err := d.SchemaByNameContext(ctx, schema)
			if err != nil {
				t.Fatalf("SchemaByNameContext %s: %v", schema, err)
			}
			got, err := sc.ObjectCountsByTypeContext(ctx)
			if err != nil {
				t.Fatalf("ObjectCountsByTypeContext: %v", err)
			}
			if want := countsFromListings(t, schema); got != want {
				t.Errorf("counts = %+v, listings = %+v", got, want)
			}
		})
	}

	// The numbers themselves, so a bug that broke both paths the same way
	// still fails. app has an encrypted procedure — sys.sql_modules keeps a
	// row for it, so it counts, and the listing returns it too.
	t.Run("expected", func(t *testing.T) {
		sc, err := d.SchemaByNameContext(ctx, "app")
		if err != nil {
			t.Fatalf("SchemaByNameContext: %v", err)
		}
		got, err := sc.ObjectCountsByTypeContext(ctx)
		if err != nil {
			t.Fatalf("ObjectCountsByTypeContext: %v", err)
		}
		want := SchemaObjectCounts{
			Tables: 2, Views: 1, StoredProcedures: 2,
			Functions: 3, Synonyms: 1, Sequences: 1,
		}
		if got != want {
			t.Errorf("app counts = %+v, want %+v", got, want)
		}
	})

	// ObjectCount is the same schema's total from one COUNT over sys.objects.
	// It is deliberately not the sum of the six — synonyms and sequences are
	// in sys.objects, but so are constraints and the tables' primary keys —
	// and the two methods staying independent is the point.
	t.Run("ObjectCountIsUnaffected", func(t *testing.T) {
		sc, err := d.SchemaByNameContext(ctx, "app")
		if err != nil {
			t.Fatalf("SchemaByNameContext: %v", err)
		}
		n, err := sc.ObjectCountContext(ctx)
		if err != nil {
			t.Fatalf("ObjectCountContext: %v", err)
		}
		if n == 0 {
			t.Error("ObjectCount = 0 for a schema with ten objects")
		}
	})

	// A schema that does not exist yields zeros rather than an error:
	// SCHEMA_ID returns NULL and every subquery counts nothing.
	t.Run("MissingSchema", func(t *testing.T) {
		missing := &Schema{db: d, Name: "nope"}
		got, err := missing.ObjectCountsByTypeContext(context.Background())
		if err != nil {
			t.Fatalf("missing schema: %v", err)
		}
		if (got != SchemaObjectCounts{}) {
			t.Errorf("missing schema counts = %+v, want all zero", got)
		}
	})
}
