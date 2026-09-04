//go:build livedb

// Live column-existence check: ask the connected instance whether every
// version-gated column gosmo names actually exists there, and compare that
// with what the gate decided for the instance's major.
//
// This is the layer that generalises to the majors nobody owns. It needs no
// new code the day a 2016, 2019 or 2022 instance appears — point it at one and
// it checks the gates for that major against that server's own catalog.
//
//	go test -tags livedb . -run TestLiveGatedColumns -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// It catches both directions. A column named on an instance that lacks it is
// the failure the whole plan exists for — it kills the entire read. A column
// gated away on an instance that *has* it is the quieter one: the field reads
// as its zero value on a server that could have answered, and nothing errors.
//
// Reads only; it creates nothing.
package gosmo

import (
	"context"
	"database/sql"
	"testing"
)

// systemColumnExists reports whether the instance's catalog has view.column in
// the sys schema. sys.system_columns/sys.system_objects describe the catalog
// views themselves, so this is the server's own answer rather than gosmo's.
func systemColumnExists(t *testing.T, db *sql.DB, ctx context.Context, view, column string) bool {
	t.Helper()
	const q = `
SELECT COUNT(*)
FROM   sys.system_columns c
JOIN   sys.system_objects o ON o.object_id = c.object_id
WHERE  SCHEMA_NAME(o.schema_id) = 'sys'
  AND  o.name = @p1
  AND  c.name = @p2`
	var n int
	if err := db.QueryRowContext(ctx, q, view, column).Scan(&n); err != nil {
		t.Fatalf("sys.system_columns for %s.%s: %v", view, column, err)
	}
	return n > 0
}

func TestLiveGatedColumnsMatchTheCatalog(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	major := srv.serverMajorVersion()
	if major == 0 {
		t.Fatalf("the instance's major version was never read; every gate would answer as newest")
	}
	t.Logf("instance major %d (%s)", major, srv.Info().ProductVersion)

	for _, g := range gatedColumns {
		t.Run(g.view+"."+g.column, func(t *testing.T) {
			onServer := systemColumnExists(t, db, ctx, g.view, g.column)
			gateNames := hasColumnSince(major, g.since)
			switch {
			case gateNames && !onServer:
				t.Errorf("gate names %s.%s on major %d but the instance has no such column — the whole read fails there; %s is too old a floor",
					g.view, g.column, major, versionConstName(g.since))
			case !gateNames && onServer && g.backported != "":
				// Deliberate: the column exists here only because the
				// instance is serviced, and the gate has to stay safe for one
				// that is not.
				t.Logf("over-gated by design on major %d: the instance has %s.%s and gosmo reads it as zero — %s",
					major, g.view, g.column, g.backported)
			case !gateNames && onServer:
				t.Errorf("gate substitutes %s.%s away on major %d but the instance has it — the field reads zero on a server that could answer; %s is too new a floor",
					g.view, g.column, major, versionConstName(g.since))
			}
		})
	}
}
