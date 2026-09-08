//go:build livedb

// Live verification of the database-scope DDL trigger family — sys.triggers
// with parent_class = 0, the third trigger family. It pins the two things
// only a real server settles: that the catalog shape database_trigger.go
// scans is right (the LEFT JOIN onto sys.sql_modules and the commaList
// aggregate over sys.trigger_events), and that the DML family is untouched by
// it — Database.Triggers must still list exactly what it listed before, and
// must not list a DDL trigger.
//
//	go test -tags livedb . -run TestLiveDatabaseTrigger -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
package gosmo

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

const liveDatabaseTriggerDB = "gossms_ddl_trig_db"

func TestLiveDatabaseTriggerReadEnableDisableDrop(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	d, dropDB := liveScratchDB(t, db, ctx, liveDatabaseTriggerDB)
	defer dropDB()

	const ddlName = "gossms_ddl_trig"
	liveExecIn(t, d, ctx,
		`CREATE TRIGGER [`+ddlName+`] ON DATABASE
FOR CREATE_TABLE, ALTER_TABLE
AS
    PRINT 'gossms ddl trigger';`)

	// The DML half of the comparison: a table with its own trigger, which
	// must stay in Database.Triggers and out of Database.DatabaseTriggers.
	liveExecIn(t, d, ctx,
		"CREATE TABLE dbo.T (id int NOT NULL)",
		"CREATE VIEW dbo.V AS SELECT id FROM dbo.T")
	liveExecIn(t, d, ctx,
		"CREATE TRIGGER dbo.tr_T ON dbo.T AFTER INSERT AS PRINT 'dml';")
	liveExecIn(t, d, ctx,
		"CREATE TRIGGER dbo.tr_V ON dbo.V INSTEAD OF INSERT AS PRINT 'view dml';")

	tr, err := d.DatabaseTriggerByNameContext(ctx, ddlName)
	if err != nil {
		t.Fatalf("DatabaseTriggerByNameContext: %v", err)
	}
	if !tr.IsEnabled {
		t.Error("a freshly created trigger read back as disabled")
	}
	if tr.CreateDate.IsZero() {
		t.Error("CreateDate is zero")
	}
	if !strings.Contains(tr.Definition, "gossms ddl trigger") {
		t.Errorf("definition not read back: %q", tr.Definition)
	}
	slices.Sort(tr.Events)
	if want := []string{"ALTER_TABLE", "CREATE_TABLE"}; !slices.Equal(tr.Events, want) {
		t.Errorf("Events = %v, want %v", tr.Events, want)
	}

	list, err := d.DatabaseTriggersContext(ctx)
	if err != nil {
		t.Fatalf("DatabaseTriggersContext: %v", err)
	}
	if len(list) != 1 || list[0].Name != ddlName {
		t.Fatalf("DatabaseTriggers listed %d rows, want just %s: %+v", len(list), ddlName, list)
	}

	// The DML folder must be byte-for-byte what it was: the table's trigger
	// and the view's, and neither the DDL one nor anything else.
	dml, err := d.TriggersContext(ctx)
	if err != nil {
		t.Fatalf("TriggersContext: %v", err)
	}
	var dmlNames []string
	for _, x := range dml {
		dmlNames = append(dmlNames, x.Name)
	}
	slices.Sort(dmlNames)
	if want := []string{"tr_T", "tr_V"}; !slices.Equal(dmlNames, want) {
		t.Errorf("Database.Triggers = %v, want %v — the DML family changed", dmlNames, want)
	}

	viewTrigs, err := d.ObjectTriggersContext(ctx, "dbo", "V")
	if err != nil {
		t.Fatalf("ObjectTriggersContext: %v", err)
	}
	if len(viewTrigs) != 1 || viewTrigs[0].Name != "tr_V" {
		t.Errorf("ObjectTriggers(dbo.V) = %+v, want just tr_V", viewTrigs)
	}

	if err := d.DatabaseTrigger(ddlName).DisableContext(ctx); err != nil {
		t.Fatalf("DisableContext: %v", err)
	}
	after, err := d.DatabaseTriggerByNameContext(ctx, ddlName)
	if err != nil {
		t.Fatalf("re-read after disable: %v", err)
	}
	if after.IsEnabled {
		t.Error("trigger still reads as enabled after DISABLE")
	}

	// A disabled trigger's script must carry the DISABLE that puts it back
	// the way it was found.
	script, err := NewScripter(d, ScriptOptions{Verb: ScriptDropAndCreate}).ScriptDatabaseTriggerContext(ctx, ddlName)
	if err != nil {
		t.Fatalf("ScriptDatabaseTriggerContext: %v", err)
	}
	for _, want := range []string{
		"DROP TRIGGER IF EXISTS [" + ddlName + "] ON DATABASE;",
		"gossms ddl trigger",
		"DISABLE TRIGGER [" + ddlName + "] ON DATABASE;",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script is missing %q:\n%s", want, script)
		}
	}

	if err := d.DatabaseTrigger(ddlName).EnableContext(ctx); err != nil {
		t.Fatalf("EnableContext: %v", err)
	}
	if back, err := d.DatabaseTriggerByNameContext(ctx, ddlName); err != nil || !back.IsEnabled {
		t.Fatalf("re-read after enable: %v (enabled=%v)", err, back != nil && back.IsEnabled)
	}

	if err := d.DatabaseTrigger(ddlName).DropContext(ctx); err != nil {
		t.Fatalf("DropContext: %v", err)
	}
	if _, err := d.DatabaseTriggerByNameContext(ctx, ddlName); !errors.Is(err, ErrNotFound) {
		t.Errorf("after drop, read gave %v, want ErrNotFound", err)
	}
}
