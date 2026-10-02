//go:build livedb

// Live verification of ScriptUserDefinedTableType's shape (T12): a table
// type's keys, checks, indexes, defaults and non-default collation survive
// a script replayed into a second database, for a disk type and a
// memory-optimized one.
//
// The recreated type is scripted again and the two scripts compared: the
// script names no generated constraint name and no database, so any lost
// element shows up as a difference.
//
//	go test -tags livedb . -run TestLiveScriptTableTypeShape -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops two throwaway databases; touches nothing else.
package gosmo

import (
	"strings"
	"testing"
	"time"
)

func TestLiveScriptTableTypeShape(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 3*time.Minute)
	defer done()

	var dataPath string
	if err := db.QueryRowContext(ctx, `SELECT CONVERT(nvarchar(260), SERVERPROPERTY('InstanceDefaultDataPath'))`).Scan(&dataPath); err != nil {
		t.Fatalf("server properties: %v", err)
	}
	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_tt_src")
	defer dropSrc()
	dst, dropDst := liveScratchDB(t, db, ctx, "gosmo_tt_dst")
	defer dropDst()
	for _, d := range []*Database{src, dst} {
		liveAddFileGroups(t, db, ctx, d, dataPath, false)
	}

	// INCLUDE in a table type's index parses on 17 and not on 13 or 14.
	include := ""
	if major := src.serverMajorVersion(); major == 0 || major >= 17 {
		include = " INCLUDE (note)"
	}
	liveExecIn(t, src, ctx,
		`CREATE TYPE dbo.tt_disk AS TABLE (
			id int NOT NULL,
			note nvarchar(50) COLLATE Latin1_General_BIN2 NULL,
			qty int NULL DEFAULT 1 CHECK (qty > 0),
			total AS qty * 2,
			PRIMARY KEY CLUSTERED (id DESC) WITH (IGNORE_DUP_KEY = ON),
			UNIQUE NONCLUSTERED (note),
			CHECK (id <> qty),
			INDEX ix_qty NONCLUSTERED (qty)`+include+` WHERE qty > 0,
			INDEX ix_note UNIQUE NONCLUSTERED (note, qty))`,
		`CREATE TYPE dbo.tt_mo AS TABLE (
			id int NOT NULL PRIMARY KEY NONCLUSTERED HASH WITH (BUCKET_COUNT = 1000),
			v nvarchar(20) NOT NULL,
			INDEX ix_v NONCLUSTERED (v DESC))
		WITH (MEMORY_OPTIMIZED = ON)`,
	)

	for _, name := range []string{"tt_disk", "tt_mo"} {
		t.Run(name, func(t *testing.T) {
			script, err := NewScripter(src, DefaultScriptOptions()).ScriptUserDefinedTableType(ctx, "dbo", name)
			if err != nil {
				t.Fatalf("ScriptUserDefinedTableType: %v", err)
			}
			liveRunScript(t, dst, ctx, script)
			again, err := NewScripter(dst, DefaultScriptOptions()).ScriptUserDefinedTableType(ctx, "dbo", name)
			if err != nil {
				t.Fatalf("script the recreated type: %v", err)
			}
			if again != script {
				t.Errorf("the recreated type differs:\n--- original\n%s\n--- recreated\n%s", script, again)
			}
			for _, want := range []string{"PRIMARY KEY", "INDEX "} {
				if !strings.Contains(script, want) {
					t.Errorf("script lost %q:\n%s", want, script)
				}
			}
		})
	}
}
