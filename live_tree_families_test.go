//go:build livedb

// Live verification of the object families added for the Object Explorer's
// missing folders: types, rules, defaults, plan guides and database
// snapshots.
//
// The version sweep already asks whether each read *runs* on the connected
// instance. This asks the two questions the sweep cannot, because both need
// a write:
//
//   - a plan guide's Enable/Disable actually flips is_disabled, and the
//     receiver ends up agreeing with the server;
//
//   - a snapshot created from SnapshotFileDefaults is accepted by the server,
//     shows up as a snapshot rather than as a user database, and can be
//     reverted to and dropped.
//
//     go test -tags livedb . -run TestLiveTreeFamilies -v \
//     -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database and snapshot; touches nothing
// else.
package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestLiveTreeFamiliesReadTheirCatalog(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_families_live")
	defer drop()

	liveExecIn(t, d, ctx,
		`CREATE TABLE dbo.fam_parent (id INT NOT NULL PRIMARY KEY, name NVARCHAR(100) NOT NULL)`,
		`CREATE TABLE dbo.fam_child (id INT NOT NULL PRIMARY KEY,
		   amount DECIMAL(18,2) NULL CONSTRAINT DF_fam_child_amount DEFAULT 0)`,
		`CREATE TYPE dbo.fam_alias FROM VARCHAR(20) NOT NULL`,
		`CREATE TYPE dbo.fam_tabletype AS TABLE (id INT NOT NULL PRIMARY KEY, note NVARCHAR(50) NULL)`,
		`CREATE XML SCHEMA COLLECTION dbo.fam_xsd AS N'<xsd:schema xmlns:xsd="http://www.w3.org/2001/XMLSchema"><xsd:element name="fam" type="xsd:string"/></xsd:schema>'`,
		`CREATE RULE dbo.fam_rule AS @value > 0`,
		`CREATE DEFAULT dbo.fam_default AS 0`,
	)

	t.Run("alias types exclude table and CLR types", func(t *testing.T) {
		aliases, err := d.UserDefinedDataTypesContext(ctx)
		if err != nil {
			t.Fatalf("UserDefinedDataTypesContext: %v", err)
		}
		var names []string
		for _, a := range aliases {
			names = append(names, a.Name)
		}
		if len(aliases) != 1 || aliases[0].Name != "fam_alias" {
			t.Fatalf("alias types = %v, want exactly [fam_alias] — the table type must not be in this list", names)
		}
		if got := aliases[0].BaseType; got != "varchar" {
			t.Errorf("base type = %q, want varchar", got)
		}
		if aliases[0].IsNullable {
			t.Error("alias declared NOT NULL reads as nullable")
		}

		one, err := d.UserDefinedDataTypeByNameContext(ctx, "dbo", "fam_alias")
		if err != nil {
			t.Fatalf("UserDefinedDataTypeByNameContext: %v", err)
		}
		if one.UserTypeID != aliases[0].UserTypeID {
			t.Errorf("finder returned user_type_id %d, listing said %d", one.UserTypeID, aliases[0].UserTypeID)
		}
	})

	t.Run("table type columns come off the internal table", func(t *testing.T) {
		tt, err := d.UserDefinedTableTypeByNameContext(ctx, "dbo", "fam_tabletype")
		if err != nil {
			t.Fatalf("UserDefinedTableTypeByNameContext: %v", err)
		}
		if tt.TypeTableObjectID == 0 {
			t.Fatal("type_table_object_id is 0 — the columns read cannot work")
		}
		cols, err := tt.ColumnsContext(ctx)
		if err != nil {
			t.Fatalf("ColumnsContext: %v", err)
		}
		if len(cols) != 2 || cols[0].Name != "id" || cols[1].Name != "note" {
			t.Fatalf("columns = %+v, want id then note", cols)
		}
		if !cols[0].IsPrimaryKey {
			t.Error("id is the table type's primary key and does not read as one")
		}
	})

	t.Run("XML schema collection excludes the sys collection", func(t *testing.T) {
		cols, err := d.XmlSchemaCollectionsContext(ctx)
		if err != nil {
			t.Fatalf("XmlSchemaCollectionsContext: %v", err)
		}
		if len(cols) != 1 || cols[0].Name != "fam_xsd" {
			t.Fatalf("collections = %+v, want exactly fam_xsd", cols)
		}
		def, err := cols[0].DefinitionContext(ctx)
		if err != nil {
			t.Fatalf("DefinitionContext: %v", err)
		}
		if !strings.Contains(def, "fam") {
			t.Errorf("definition does not mention the element it declares: %q", def)
		}
	})

	// The one that matters: sys.objects type 'D' is both standalone defaults
	// and default constraints, and DF_fam_child_amount above is a default
	// constraint. Without the parent_object_id = 0 predicate it lands in the
	// Defaults folder.
	t.Run("defaults exclude default constraints", func(t *testing.T) {
		defs, err := d.DefaultsContext(ctx)
		if err != nil {
			t.Fatalf("DefaultsContext: %v", err)
		}
		var names []string
		for _, df := range defs {
			names = append(names, df.Name)
		}
		if len(defs) != 1 || defs[0].Name != "fam_default" {
			t.Fatalf("defaults = %v, want exactly [fam_default] — DF_fam_child_amount is a default constraint, not a CREATE DEFAULT object", names)
		}
		if !strings.Contains(defs[0].Definition, "CREATE DEFAULT") {
			t.Errorf("definition = %q, want the CREATE DEFAULT text", defs[0].Definition)
		}

		if _, err := d.DefaultByNameContext(ctx, "dbo", "DF_fam_child_amount"); !errors.Is(err, ErrNotFound) {
			t.Errorf("DefaultByName on a default constraint: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("rules", func(t *testing.T) {
		rules, err := d.RulesContext(ctx)
		if err != nil {
			t.Fatalf("RulesContext: %v", err)
		}
		if len(rules) != 1 || rules[0].Name != "fam_rule" {
			t.Fatalf("rules = %+v, want exactly fam_rule", rules)
		}
		if !strings.Contains(rules[0].Definition, "CREATE RULE") {
			t.Errorf("definition = %q, want the CREATE RULE text", rules[0].Definition)
		}
	})
}

func TestLiveTreeFamiliesPlanGuideEnableDisable(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_planguide_live")
	defer drop()

	liveExecIn(t, d, ctx,
		`CREATE TABLE dbo.pg_parent (id INT NOT NULL PRIMARY KEY, name NVARCHAR(100) NOT NULL)`,
		`EXEC sp_create_plan_guide @name = N'pg_live',
		   @stmt = N'SELECT COUNT(*) FROM dbo.pg_parent WHERE name = @name',
		   @type = N'SQL', @module_or_batch = NULL,
		   @params = N'@name nvarchar(100)',
		   @hints = N'OPTION (OPTIMIZE FOR (@name = N''one''))'`,
	)

	g, err := d.PlanGuideByNameContext(ctx, "pg_live")
	if err != nil {
		t.Fatalf("PlanGuideByNameContext: %v", err)
	}
	if g.IsDisabled {
		t.Fatal("a freshly created plan guide reads as disabled")
	}
	if g.Scope != PlanGuideScopeSQL {
		t.Errorf("scope = %q, want SQL", g.Scope)
	}
	if !strings.Contains(g.Hints, "OPTIMIZE FOR") {
		t.Errorf("hints = %q, want the OPTION clause", g.Hints)
	}
	if g.ScopeObject != "" {
		t.Errorf("scope object = %q, want empty on a SQL-scoped guide", g.ScopeObject)
	}

	if err := g.DisableContext(ctx); err != nil {
		t.Fatalf("DisableContext: %v", err)
	}
	if !g.IsDisabled {
		t.Error("receiver still reads as enabled after Disable")
	}
	again, err := d.PlanGuideByNameContext(ctx, "pg_live")
	if err != nil {
		t.Fatalf("re-read after Disable: %v", err)
	}
	if !again.IsDisabled {
		t.Error("server still reports the plan guide as enabled after Disable")
	}

	if err := g.EnableContext(ctx); err != nil {
		t.Fatalf("EnableContext: %v", err)
	}
	again, err = d.PlanGuideByNameContext(ctx, "pg_live")
	if err != nil {
		t.Fatalf("re-read after Enable: %v", err)
	}
	if again.IsDisabled {
		t.Error("server still reports the plan guide as disabled after Enable")
	}

	if err := g.DropContext(ctx); err != nil {
		t.Fatalf("DropContext: %v", err)
	}
	if _, err := d.PlanGuideByNameContext(ctx, "pg_live"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after Drop: err = %v, want ErrNotFound", err)
	}
}

func TestLiveDatabaseSnapshotLifecycle(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	source, dropSource := liveScratchDB(t, db, ctx, "gosmo_snapsrc_live")
	defer dropSource()

	liveExecIn(t, source, ctx,
		`CREATE TABLE dbo.snap_rows (id INT NOT NULL PRIMARY KEY)`,
		`INSERT dbo.snap_rows (id) VALUES (1), (2)`,
	)

	const snapName = "gosmo_snapsrc_live_snap"
	// Drop a leftover from an interrupted run before creating: a snapshot
	// blocks its source from being dropped, so one left behind would fail
	// every later run of this test at liveScratchDB rather than here.
	db.ExecContext(ctx, "IF DB_ID('"+snapName+"') IS NOT NULL DROP DATABASE ["+snapName+"]")

	specs, err := srv.SnapshotFileDefaultsContext(ctx, source.Name(), snapName)
	if err != nil {
		t.Fatalf("SnapshotFileDefaultsContext: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("file defaults = %+v, want one entry — the scratch database has one data file and one log file", specs)
	}
	if strings.HasSuffix(strings.ToLower(specs[0].FileName), ".ldf") {
		t.Fatalf("file defaults name the log file %q", specs[0].FileName)
	}

	snap, err := srv.CreateDatabaseSnapshotContext(ctx, CreateDatabaseSnapshotRequest{
		Name:           snapName,
		SourceDatabase: source.Name(),
	})
	if err != nil {
		t.Fatalf("CreateDatabaseSnapshotContext: %v", err)
	}
	defer func() {
		c := context.Background()
		db.ExecContext(c, "IF DB_ID('"+snapName+"') IS NOT NULL DROP DATABASE ["+snapName+"]")
	}()

	if snap.SourceDatabase != source.Name() {
		t.Errorf("snapshot source = %q, want %q", snap.SourceDatabase, source.Name())
	}

	// A snapshot is an ordinary sys.databases row, so Databases returns it.
	// IsSnapshot is what a caller filtering a tree uses; if it does not hold
	// here, gossms's Databases folder lists every snapshot twice.
	dbs, err := srv.DatabasesContext(ctx)
	if err != nil {
		t.Fatalf("DatabasesContext: %v", err)
	}
	var found bool
	for _, cand := range dbs {
		if cand.Name() != snapName {
			continue
		}
		found = true
		if !cand.IsSnapshot() {
			t.Error("the snapshot's Database row does not report IsSnapshot")
		}
		if cand.SourceDatabaseID() != source.ID() {
			t.Errorf("source database id = %d, want %d", cand.SourceDatabaseID(), source.ID())
		}
	}
	if !found {
		t.Fatalf("%q is not in the databases listing", snapName)
	}
	for _, cand := range dbs {
		if cand.Name() == source.Name() && cand.IsSnapshot() {
			t.Error("the source database reports IsSnapshot")
		}
	}

	of, err := srv.SnapshotsOfContext(ctx, source.Name())
	if err != nil {
		t.Fatalf("SnapshotsOfContext: %v", err)
	}
	if len(of) != 1 || of[0].Name != snapName {
		t.Fatalf("SnapshotsOf(%q) = %+v, want the one snapshot", source.Name(), of)
	}

	// Revert: delete a row, restore, and check it is back. This is the whole
	// point of the folder, and the statement's shape (a string literal, not
	// a bracketed name) is what it verifies.
	liveExecIn(t, source, ctx, `DELETE dbo.snap_rows WHERE id = 2`)
	if err := snap.RestoreContext(ctx); err != nil {
		t.Fatalf("RestoreContext: %v", err)
	}
	var rows int
	if err := source.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&rows) },
		`SELECT COUNT(*) FROM dbo.snap_rows`); err != nil {
		t.Fatalf("count after restore: %v", err)
	}
	if rows != 2 {
		t.Errorf("%d rows after the revert, want 2", rows)
	}

	if err := snap.DropContext(ctx); err != nil {
		t.Fatalf("DropContext: %v", err)
	}
	if _, err := srv.DatabaseSnapshotByNameContext(ctx, snapName); !errors.Is(err, ErrNotFound) {
		t.Errorf("after Drop: err = %v, want ErrNotFound", err)
	}
}
