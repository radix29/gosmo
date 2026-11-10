package gosmo_test

import (
	"context"
	"testing"
	"time"

	"github.com/radix29/gosmo"
	"github.com/radix29/gosmo/gosmotest"
)

// A database DDL trigger comes after the permissions: GRANT, DENY and REVOKE
// are database DDL events, so a replay that created it first fired it on each
// of the script's own GRANTs (a "no DDL" ROLLBACK guard failed the first,
// Msg 3609).
func TestScriptDatabaseObjectListDatabaseTriggersAfterPermissions(t *testing.T) {
	f := gosmotest.New(t)
	when := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	f.On("tr.parent_class = 0", gosmotest.Rows([]any{"no_ddl", false, when, when,
		`[{"v":"DDL_DATABASE_LEVEL_EVENTS"}]`, "CREATE TRIGGER no_ddl ON DATABASE FOR DDL_DATABASE_LEVEL_EVENTS AS ROLLBACK",
		true, true, false, int64(1)}))
	f.On("SUSER_SNAME(owner_sid), owner_sid FROM sys.databases", gosmotest.Rows([]any{"sa", []byte{1}}))
	f.On("HAS_PERMS_BY_NAME(DB_NAME(), 'DATABASE'", gosmotest.Rows([]any{true}))
	srv := f.Server()          // connects first, on the default server-info rules
	f.On("", gosmotest.Rows()) // every other read: an empty database
	sc := gosmo.NewScripter(srv.DatabaseRef("app"), gosmo.DefaultScriptOptions())
	res, err := sc.ScriptDatabaseObjectList(context.Background(), gosmo.ObjectScriptOptions{IncludePermissions: true})
	if err != nil {
		t.Fatal(err)
	}
	perms, trig := -1, -1
	for i, o := range res.Objects {
		switch o.Kind {
		case gosmo.ScriptedPermissions:
			perms = i
		case gosmo.ScriptedDatabaseTrigger:
			trig = i
		}
	}
	if perms < 0 || trig < 0 || trig < perms {
		var kinds []gosmo.ScriptedKind
		for _, o := range res.Objects {
			kinds = append(kinds, o.Kind)
		}
		t.Errorf("objects = %v, want the database trigger after the permissions", kinds)
	}
}
