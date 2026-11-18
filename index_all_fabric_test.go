package gosmo_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/radix29/gosmo"
	"github.com/radix29/gosmo/gosmotest"
)

// SQL database in Fabric refuses ALTER INDEX ALL (a named index is
// accepted), so the …All… methods expand it there to one ALTER INDEX per
// index, and RebuildAllIndexesScript returns the same text. Every other
// engine keeps the single ALL statement and reads nothing.

// fabricIndexes is a Fake on Fabric whose table has a clustered primary key,
// a disabled nonclustered index whose name needs escaping, and a
// hypothetical one — rows of allIndexTargetsQuery.
func fabricIndexes(t *testing.T, rows ...[]any) (*gosmotest.Fake, *gosmo.Server) {
	t.Helper()
	f := gosmotest.New(t)
	f.Info.EngineEdition = gosmo.EngineFabricSQLDatabase
	srv := f.Server()
	f.On("LEFT   JOIN sys.indexes i ON i.object_id = o.object_id", gosmotest.Rows(rows...))
	return f, srv
}

var threeIndexes = [][]any{
	{int64(42), "PK_T", false, false},
	{int64(42), "IX]d", true, false},
	{int64(42), "_dta_hyp", false, true},
}

// execs is every write the Fake saw, as sent.
func execs(f *gosmotest.Fake) []string {
	var out []string
	for _, q := range f.Queries() {
		if q.Exec {
			out = append(out, q.Text)
		}
	}
	return out
}

func TestFabricRebuildAllIndexesExpandsPerIndex(t *testing.T) {
	ctx := context.Background()
	f, srv := fabricIndexes(t, threeIndexes...)
	tbl := srv.DatabaseRef("app").TableRef("dbo", "T")
	opts := gosmo.IndexRebuildOptions{Online: true, MaxDOP: 1}
	want := "ALTER INDEX [PK_T] ON [dbo].[T] REBUILD WITH (ONLINE = ON, MAXDOP = 1);\n" +
		"ALTER INDEX [IX]]d] ON [dbo].[T] REBUILD WITH (ONLINE = ON, MAXDOP = 1);"

	script, err := tbl.RebuildAllIndexesScript(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if script != want {
		t.Errorf("RebuildAllIndexesScript =\n%s\nwant\n%s", script, want)
	}
	if err := tbl.RebuildAllIndexesWith(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if got := execs(f); len(got) != 1 || !strings.HasSuffix(got[0], want) {
		t.Errorf("RebuildAllIndexesWith sent %q, want one batch ending\n%s", got, want)
	}
	// The TableRef has no object id: the read must go by name.
	for _, q := range f.Queries() {
		if strings.Contains(q.Text, "sys.indexes") && (q.Arg(0) != int64(0) || q.Arg(1) != "[dbo].[T]") {
			t.Errorf("index read args = %v, want (0, [dbo].[T])", q.Args)
		}
	}
}

func TestFabricRebuildAllIndexesFillFactor(t *testing.T) {
	f, srv := fabricIndexes(t, threeIndexes[0])
	if err := srv.DatabaseRef("app").TableRef("dbo", "T").RebuildAllIndexes(context.Background(), 80); err != nil {
		t.Fatal(err)
	}
	want := "ALTER INDEX [PK_T] ON [dbo].[T] REBUILD WITH (FILLFACTOR = 80);"
	if got := execs(f); len(got) != 1 || !strings.HasSuffix(got[0], want) {
		t.Errorf("sent %q, want %q", got, want)
	}
}

// A reorganize passes over a disabled index as ALL does: the server refuses
// one by name.
func TestFabricReorganizeAllIndexesSkipsDisabled(t *testing.T) {
	f, srv := fabricIndexes(t, threeIndexes...)
	if err := srv.DatabaseRef("app").TableRef("dbo", "T").ReorganizeAllIndexes(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "ALTER INDEX [PK_T] ON [dbo].[T] REORGANIZE;"
	if got := execs(f); len(got) != 1 || !strings.HasSuffix(got[0], want) || strings.Contains(got[0], "IX]]d") {
		t.Errorf("sent %q, want only %q", got, want)
	}
}

func TestFabricViewRebuildAllIndexes(t *testing.T) {
	f, srv := fabricIndexes(t, []any{int64(7), "CIX_V", false, false})
	if err := srv.DatabaseRef("app").ViewRef("dbo", "V").RebuildAllIndexesWith(context.Background(), gosmo.IndexRebuildOptions{}); err != nil {
		t.Fatal(err)
	}
	want := "ALTER INDEX [CIX_V] ON [dbo].[V] REBUILD;"
	if got := execs(f); len(got) != 1 || !strings.HasSuffix(got[0], want) {
		t.Errorf("sent %q, want %q", got, want)
	}
}

// A heap has no index: nothing is sent and the script is empty, as ALL on it
// changes nothing.
func TestFabricRebuildAllIndexesOnAHeapSendsNothing(t *testing.T) {
	ctx := context.Background()
	f, srv := fabricIndexes(t, []any{int64(42), nil, false, false})
	tbl := srv.DatabaseRef("app").TableRef("dbo", "Heap")
	if script, err := tbl.RebuildAllIndexesScript(ctx, gosmo.IndexRebuildOptions{}); err != nil || script != "" {
		t.Errorf("RebuildAllIndexesScript = %q, %v; want \"\", nil", script, err)
	}
	if err := tbl.RebuildAllIndexesWith(ctx, gosmo.IndexRebuildOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := tbl.ReorganizeAllIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	if got := execs(f); len(got) != 0 {
		t.Errorf("sent %q on a heap, want nothing", got)
	}
}

// A missing table is not found, as the server's ALL would report it, rather
// than a success that rebuilt nothing.
func TestFabricRebuildAllIndexesMissingTable(t *testing.T) {
	f, srv := fabricIndexes(t, []any{nil, nil, false, false})
	err := srv.DatabaseRef("app").TableRef("dbo", "Gone").RebuildAllIndexesWith(context.Background(), gosmo.IndexRebuildOptions{})
	if !errors.Is(err, gosmo.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if !strings.HasPrefix(err.Error(), "gosmo: rebuild all indexes on [dbo].[Gone]: ") {
		t.Errorf("err = %q, want the operation's prefix once", err)
	}
	if got := execs(f); len(got) != 0 {
		t.Errorf("sent %q, want nothing", got)
	}
}

// Off Fabric the single ALL statement stays, and nothing is read for it.
func TestRebuildAllIndexesScriptOnPremIsAll(t *testing.T) {
	f := gosmotest.New(t)
	srv := f.Server()
	before := len(f.Queries())
	script, err := srv.DatabaseRef("app").TableRef("dbo", "T").RebuildAllIndexesScript(context.Background(), gosmo.IndexRebuildOptions{Online: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := "ALTER INDEX ALL ON [dbo].[T] REBUILD WITH (ONLINE = ON)"; script != want {
		t.Errorf("script = %q, want %q", script, want)
	}
	if after := f.Queries()[before:]; len(after) != 0 {
		t.Errorf("read %v for an on-premises script, want nothing", after)
	}
}

// A TableRef with no schema is refused before the Fabric read, which would
// otherwise resolve the bare name through the caller's default schema.
func TestFabricRebuildAllIndexesRequiresSchema(t *testing.T) {
	f, srv := fabricIndexes(t, threeIndexes...)
	before := len(f.Queries())
	tbl := srv.DatabaseRef("app").TableRef("", "T")
	if _, err := tbl.RebuildAllIndexesScript(context.Background(), gosmo.IndexRebuildOptions{}); !errors.Is(err, gosmo.ErrSchemaRequired) {
		t.Errorf("script err = %v, want ErrSchemaRequired", err)
	}
	if err := tbl.ReorganizeAllIndexes(context.Background()); !errors.Is(err, gosmo.ErrSchemaRequired) {
		t.Errorf("reorganize err = %v, want ErrSchemaRequired", err)
	}
	if after := f.Queries()[before:]; len(after) != 0 {
		t.Errorf("sent %v, want nothing", after)
	}
}
