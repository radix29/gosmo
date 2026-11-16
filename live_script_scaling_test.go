//go:build livedb

package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestLiveScriptingManyTablesReadsPerCallNotPerTable is review step D1:
// ScriptDatabaseObjectList over a database of n tables must issue a number of
// round trips that does not grow with n (prefetchTables reads every table's
// script inputs in one batch). An Extended Events event_counter, filtered on
// the scripting pool's application name, counts the batches and RPCs at 50
// and at 500 tables. Every table, its index, a column and its foreign key
// carry an extended property, and a sample of tables must script as
// ScriptTable alone (no prefetch) scripts them.
//
//	go test -tags livedb . -run TestLiveScriptingManyTablesReadsPerCallNotPerTable -v -livedb '...'
func TestLiveScriptingManyTablesReadsPerCallNotPerTable(t *testing.T) {
	skipSlowInShort(t)
	db, ctx, done := liveDBTimeout(t, 10*time.Minute)
	defer done()

	const dbName, app, xe = "gosmo_scale_live", "gosmo_scale_probe", "gosmo_scale_probe_xe"
	_, drop := liveScratchDB(t, db, ctx, dbName)
	defer drop()

	u, err := url.Parse(*liveDSN)
	if err != nil {
		t.Fatalf("parse -livedb DSN: %v", err)
	}
	q := u.Query()
	q.Set("app name", app)
	u.RawQuery = q.Encode()
	probeDB, err := sql.Open("sqlserver", u.String())
	if err != nil {
		t.Fatalf("open probe pool: %v", err)
	}
	defer probeDB.Close()

	dropXE := func() {
		db.ExecContext(context.Background(), "IF EXISTS (SELECT 1 FROM sys.server_event_sessions WHERE name = N'"+xe+"') DROP EVENT SESSION ["+xe+"] ON SERVER")
	}
	dropXE()
	defer dropXE()
	if _, err := db.ExecContext(ctx, `CREATE EVENT SESSION [`+xe+`] ON SERVER
		ADD EVENT sqlserver.sql_batch_completed (WHERE sqlserver.client_app_name = N'`+app+`'),
		ADD EVENT sqlserver.rpc_completed (WHERE sqlserver.client_app_name = N'`+app+`')
		ADD TARGET package0.event_counter`); err != nil {
		t.Fatalf("create event session: %v", err)
	}

	addTables := func(from, to int) {
		t.Helper()
		// One batch: table i has a key, a default, a check, an index and
		// (after the first) a foreign key to table i-1, and an extended
		// property on the table, a column, the index and the key.
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`USE [%s];
DECLARE @i int = %d, @q nvarchar(max), @t sysname, @v nvarchar(50), @ix sysname, @fk sysname;
WHILE @i <= %d
BEGIN
	SET @q = N'CREATE TABLE dbo.t' + CAST(@i AS nvarchar(10)) + N' (
		id int NOT NULL CONSTRAINT pk_t' + CAST(@i AS nvarchar(10)) + N' PRIMARY KEY,
		parent int NULL,
		name nvarchar(50) NOT NULL CONSTRAINT df_t' + CAST(@i AS nvarchar(10)) + N' DEFAULT (N''x''),
		qty int NOT NULL CONSTRAINT ck_t' + CAST(@i AS nvarchar(10)) + N' CHECK (qty >= 0),
		created datetime2 NULL);
		CREATE INDEX ix_t' + CAST(@i AS nvarchar(10)) + N' ON dbo.t' + CAST(@i AS nvarchar(10)) + N' (name) INCLUDE (qty);'
	IF @i > 1
		SET @q += N'ALTER TABLE dbo.t' + CAST(@i AS nvarchar(10)) + N' ADD CONSTRAINT fk_t' + CAST(@i AS nvarchar(10)) +
			N' FOREIGN KEY (parent) REFERENCES dbo.t' + CAST(@i - 1 AS nvarchar(10)) + N' (id);'
	EXEC (@q)
	SELECT @t = N't' + CAST(@i AS nvarchar(10)), @v = N'table ' + CAST(@i AS nvarchar(10)),
	       @ix = N'ix_t' + CAST(@i AS nvarchar(10)), @fk = N'fk_t' + CAST(@i AS nvarchar(10))
	EXEC sys.sp_addextendedproperty N'MS_Description', @v, N'SCHEMA', N'dbo', N'TABLE', @t
	EXEC sys.sp_addextendedproperty N'MS_Description', N'name column', N'SCHEMA', N'dbo', N'TABLE', @t, N'COLUMN', N'name'
	EXEC sys.sp_addextendedproperty N'MS_Description', N'by name', N'SCHEMA', N'dbo', N'TABLE', @t, N'INDEX', @ix
	IF @i > 1
		EXEC sys.sp_addextendedproperty N'MS_Description', N'to parent', N'SCHEMA', N'dbo', N'TABLE', @t, N'CONSTRAINT', @fk
	SET @i += 1
END`, dbName, from, to)); err != nil {
			t.Fatalf("create tables %d..%d: %v", from, to, err)
		}
	}

	type sample struct {
		tables, objects int
		roundTrips      int64
		elapsed         time.Duration
	}
	measure := func(n int) sample {
		t.Helper()
		srv, err := NewServer(ctx, probeDB)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		d, err := srv.DatabaseByName(ctx, dbName)
		if err != nil {
			t.Fatalf("DatabaseByName: %v", err)
		}
		if _, err := db.ExecContext(ctx, "ALTER EVENT SESSION ["+xe+"] ON SERVER STATE = START"); err != nil {
			t.Fatalf("start event session: %v", err)
		}
		start := time.Now()
		res, err := NewScripter(d, DefaultScriptOptions()).ScriptDatabaseObjectList(ctx, ObjectScriptOptions{})
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("ScriptDatabaseObjectList (%d tables): %v", n, err)
		}
		var count sql.NullInt64
		if err := db.QueryRowContext(ctx, `
DECLARE @x xml = (SELECT CAST(st.target_data AS xml)
	FROM sys.dm_xe_sessions s JOIN sys.dm_xe_session_targets st ON st.event_session_address = s.address
	WHERE s.name = N'`+xe+`' AND st.target_name = N'event_counter');
SELECT SUM(c.value('@count', 'bigint')) FROM @x.nodes('//Event') AS e(c)`).Scan(&count); err != nil {
			t.Fatalf("read event counter: %v", err)
		}
		if _, err := db.ExecContext(ctx, "ALTER EVENT SESSION ["+xe+"] ON SERVER STATE = STOP"); err != nil {
			t.Fatalf("stop event session: %v", err)
		}
		tables := 0
		// ScriptTable alone reads the table the ordinary way, with no
		// prefetch; the list defers foreign keys, so it does too.
		aloneOpts := DefaultScriptOptions()
		aloneOpts.DeferReferences = true
		alone := NewScripter(d, aloneOpts)
		for _, o := range res.Objects {
			switch o.Kind {
			case ScriptedTable:
				tables++
				if o.Name != "t1" && o.Name != "t2" && o.Name != fmt.Sprintf("t%d", n) {
					continue
				}
				for _, p := range []string{"N'table ", "N'name column'", "N'by name'"} {
					if !strings.Contains(o.SQL, p) {
						t.Errorf("table %s lost extended property %s:\n%s", o.Name, p, o.SQL)
					}
				}
				want, err := alone.ScriptTable(ctx, o.Schema, o.Name)
				if err != nil {
					t.Fatalf("ScriptTable %s: %v", o.Name, err)
				}
				if o.SQL != want {
					t.Errorf("table %s from the prefetch:\n%s\nwant (ScriptTable):\n%s", o.Name, o.SQL, want)
				}
			case ScriptedForeignKey:
				if !strings.Contains(o.SQL, "N'to parent'") {
					t.Errorf("foreign key %s lost its extended property:\n%s", o.Name, o.SQL)
				}
			}
		}
		if tables != n {
			t.Fatalf("scripted %d tables, want %d", tables, n)
		}
		s := sample{tables: n, objects: len(res.Objects), roundTrips: count.Int64, elapsed: elapsed}
		t.Logf("%d tables: %d objects, %d round trips, %v (%v per table)",
			s.tables, s.objects, s.roundTrips, s.elapsed.Round(time.Millisecond), (s.elapsed / time.Duration(n)).Round(time.Microsecond))
		return s
	}

	addTables(1, 50)
	small := measure(50)
	addTables(51, 500)
	large := measure(500)

	if small.roundTrips == 0 {
		t.Fatal("the event counter saw no round trips: the probe pool's app name filter matched nothing")
	}
	// A per-table read would add at least 450 round trips.
	if grown := large.roundTrips - small.roundTrips; grown > 10 {
		t.Errorf("round trips grew by %d from 50 to 500 tables (%d -> %d): something reads per table",
			grown, small.roundTrips, large.roundTrips)
	}
}
