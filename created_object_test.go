package gosmo

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// createdObject is every Create*'s tail, so its three arms are pinned here
// once rather than per family.
func TestCreatedObject(t *testing.T) {
	handle := &Schema{Name: "handle"}
	read := &Schema{Name: "read", ID: 7}
	boom := errors.New("boom")

	t.Run("scripting returns the handle without reading", func(t *testing.T) {
		ctx, _ := WithScript(context.Background())
		got, err := createdObject(ctx, handle, func() (*Schema, error) {
			t.Fatal("read ran under Scripting")
			return nil, nil
		})
		if err != nil || got != handle {
			t.Errorf("got (%v, %v), want the handle", got, err)
		}
	})
	t.Run("a read that finds it returns what it read", func(t *testing.T) {
		got, err := createdObject(context.Background(), handle, func() (*Schema, error) { return read, nil })
		if err != nil || got != read {
			t.Errorf("got (%v, %v), want the read object", got, err)
		}
	})
	// The create succeeded; a row the caller cannot see is not a failure of
	// it, and reporting one would say a create that happened had not.
	t.Run("a read that finds nothing returns the handle", func(t *testing.T) {
		got, err := createdObject(context.Background(), handle, func() (*Schema, error) {
			return nil, notFoundf("gosmo: schema %q not found", "x")
		})
		if err != nil || got != handle {
			t.Errorf("got (%v, %v), want the handle", got, err)
		}
	})
	t.Run("any other read error is returned", func(t *testing.T) {
		_, err := createdObject(context.Background(), handle, func() (*Schema, error) { return nil, boom })
		if !errors.Is(err, boom) {
			t.Errorf("err = %v, want boom", err)
		}
	})
}

// Against a connection that executes but returns no rows, a Create* runs its
// statement, reads back, finds nothing — and still hands back an object
// addressing what it created, rather than an error.
func TestCreateReadsBackAndFallsBackToTheHandle(t *testing.T) {
	tbl := captureTable(t)
	d := tbl.db
	s, err := d.CreateSchema(context.Background(), CreateSchemaRequest{Name: "sa]les", Owner: "dbo"})
	if err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}
	if s == nil || s.Name != "sa]les" || s.Database() != d {
		t.Fatalf("CreateSchema returned %+v, want a handle on [sa]]les] in %q", s, d.Name)
	}
	if q := captured.find("CREATE SCHEMA"); !strings.Contains(q, "CREATE SCHEMA [sa]]les] AUTHORIZATION [dbo]") {
		t.Errorf("statement = %q", q)
	}
	if q := captured.find("FROM   sys.schemas"); q == "" {
		t.Errorf("no read-back of the schema was issued")
	}
}

// The two lookups added for Create*'s read-back refuse an empty schema or an
// unknown class before any query, and otherwise ask for exactly one object.
func TestNewByNameLookups(t *testing.T) {
	tbl := captureTable(t)
	if _, err := tbl.db.StoredProcedureByName(context.Background(), "dbo", "usp_x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("StoredProcedureByName on no rows: err = %v, want ErrNotFound", err)
	}
	if q := captured.find("FROM   sys.procedures"); !strings.Contains(q, "SCHEMA_NAME(p.schema_id) = @p1 AND p.name = @p2") {
		t.Errorf("StoredProcedureByName query = %q, want it narrowed by schema and name", q)
	}

	srv := tbl.db.server
	if _, err := srv.CategoryByName(context.Background(), CategoryClassJob, "Nightly"); !errors.Is(err, ErrNotFound) {
		t.Errorf("CategoryByName on no rows: err = %v, want ErrNotFound", err)
	}
	if q := captured.find("msdb.dbo.syscategories"); !strings.Contains(q, "category_class = @p1 AND name = @p2") {
		t.Errorf("CategoryByName query = %q, want it narrowed by class and name", q)
	}
	if _, err := srv.CategoryByName(context.Background(), "NOPE", "x"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("CategoryByName with an unknown class: err = %v, want a refusal", err)
	}
}
