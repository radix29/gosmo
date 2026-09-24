//go:build livedb

// Live verification that catalog numerics past int64 read and script. A
// decimal(38,0) identity seed failed Table.Columns with "value out of
// range", and one decimal(38,0) sequence failed Database.Sequences for every
// sequence in the database; a scripted sequence started at its last used
// value, so the copy's first NEXT VALUE FOR re-issued it.
//
//	go test -tags livedb . -run TestLiveCatalogNumerics -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops two throwaway databases.
package gosmo

import (
	"database/sql"
	"strconv"
	"strings"
	"testing"
)

func TestLiveCatalogNumericsReadAndReplay(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_numerics_src_live")
	defer dropSrc()
	dst, dropDst := liveScratchDB(t, db, ctx, "gosmo_numerics_dst_live")
	defer dropDst()

	for _, q := range []string{
		"CREATE TABLE dbo.big_ident (id decimal(38,0) IDENTITY(100000000000000000000, 1), x int)",
		"CREATE SEQUENCE dbo.s_used AS int START WITH 1 INCREMENT BY 1",
		"CREATE SEQUENCE dbo.s_big AS decimal(38,0) START WITH 100000000000000000000 INCREMENT BY 10",
		"CREATE SEQUENCE dbo.s_num AS numeric(12,0) START WITH 5 INCREMENT BY 2 MAXVALUE 9 CYCLE",
		"CREATE SEQUENCE dbo.s_fresh AS int START WITH 7 INCREMENT BY 1",
		"CREATE SEQUENCE dbo.s_restart AS int START WITH 1 INCREMENT BY 1",
		"SELECT NEXT VALUE FOR dbo.s_used, NEXT VALUE FOR dbo.s_big, NEXT VALUE FOR dbo.s_num",
		"SELECT NEXT VALUE FOR dbo.s_used, NEXT VALUE FOR dbo.s_num",
		"SELECT NEXT VALUE FOR dbo.s_used, NEXT VALUE FOR dbo.s_num, NEXT VALUE FOR dbo.s_restart",
		"ALTER SEQUENCE dbo.s_restart RESTART WITH 50",
	} {
		if _, err := src.exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	tbl, err := src.TableByName(ctx, "dbo", "big_ident")
	if err != nil {
		t.Fatalf("TableByName: %v", err)
	}
	cols, err := tbl.Columns(ctx)
	if err != nil {
		t.Fatalf("Columns on a decimal(38,0) identity: %v", err)
	}
	if cols[0].IdentitySeed != "100000000000000000000" || cols[0].IdentityIncrement != "1" {
		t.Errorf("identity = (%s,%s), want (100000000000000000000,1)", cols[0].IdentitySeed, cols[0].IdentityIncrement)
	}
	if cols[1].IdentitySeed != "" {
		t.Errorf("a non-identity column has seed %q, want empty", cols[1].IdentitySeed)
	}

	seqs, err := src.Sequences(ctx)
	if err != nil {
		t.Fatalf("Sequences with a decimal(38,0) sequence present: %v", err)
	}
	if len(seqs) != 5 {
		t.Fatalf("Sequences returned %d, want 5", len(seqs))
	}

	sc := NewScripter(src, DefaultScriptOptions())
	var scripts []string
	tblScript, err := sc.ScriptTable(ctx, "dbo", "big_ident")
	if err != nil {
		t.Fatalf("ScriptTable: %v", err)
	}
	if !strings.Contains(tblScript, "IDENTITY(100000000000000000000,1)") {
		t.Errorf("table script lost the identity seed:\n%s", tblScript)
	}
	scripts = append(scripts, tblScript)
	for _, seq := range seqs {
		s, err := sc.ScriptSequence(ctx, seq.Schema, seq.Name)
		if err != nil {
			t.Fatalf("ScriptSequence %s: %v", seq.Name, err)
		}
		t.Logf("%s:\n%s", seq.Name, s)
		scripts = append(scripts, s)
	}
	for _, s := range scripts {
		for _, batch := range strings.Split(s, "\nGO\n") {
			if batch = strings.TrimSpace(batch); batch == "" {
				continue
			}
			if _, err := dst.exec(ctx, batch); err != nil {
				t.Fatalf("replay rejected:\n%s\n\nerror: %v", batch, err)
			}
		}
	}

	// The copy's first value must be the one the original hands out next.
	// numeric(12,0) at 9 cycles to its MINVALUE; a fresh sequence starts at
	// its START WITH; a restarted one at the restart value; a pre-2017
	// server has no last_used_value and skips one number of a fresh or
	// restarted sequence instead.
	pre2017 := src.serverMajorVersion() != 0 && src.serverMajorVersion() < int(SQLServer2017)
	for _, name := range []string{"s_used", "s_big", "s_num", "s_fresh", "s_restart"} {
		next := func(d *Database) string {
			var v sql.NullString
			if err := d.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&v) },
				"SELECT CONVERT(nvarchar(40), NEXT VALUE FOR dbo."+name+")"); err != nil {
				t.Fatalf("NEXT VALUE FOR %s: %v", name, err)
			}
			return v.String
		}
		want, got := next(src), next(dst)
		if pre2017 && (name == "s_fresh" || name == "s_restart") {
			n, _ := strconv.Atoi(want)
			want = strconv.Itoa(n + 1)
		}
		if got != want {
			t.Errorf("%s: copy's first value %s, original's next %s", name, got, want)
		}
	}

	var typ string
	if err := dst.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&typ) },
		"SELECT CONCAT(TYPE_NAME(user_type_id), '(', precision, ',', scale, ')') FROM sys.sequences WHERE name = 's_num'"); err != nil {
		t.Fatalf("read s_num type: %v", err)
	}
	if typ != "numeric(12,0)" {
		t.Errorf("replayed s_num is %s, want numeric(12,0)", typ)
	}
}
