//go:build livedb

// Live verification of the table families — TableKind, the clauses that
// partition sys.tables into them, and TableKindsPresent.
//
// The unit tests read the clause text; only a real catalog answers the
// questions that matter: that the five kinds partition the tables (no table
// listed twice, none lost), that a graph table really is excluded from the
// plain user list, that msdb's hundred-odd ms-shipped tables come back as
// system tables and are absent from Tables(), and that the graph predicate
// runs at all on an instance that has no is_node/is_edge.
//
//	go test -tags livedb . -run TestLiveTableKinds -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway databases; touches nothing else.
package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// liveServer builds a Server over the live connection, which is what carries
// the major version every kind clause gates on.
func liveServer(t *testing.T, db *sql.DB, ctx context.Context) *Server {
	t.Helper()
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

func hasName(tables []*Table, want string) bool {
	for _, t := range tables {
		if t.Schema+"."+t.Name == want {
			return true
		}
	}
	return false
}

func TestLiveTableKinds(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	srv := liveServer(t, db, ctx)
	major := srv.serverMajorVersion()
	graphOK := major == 0 || major >= int(SQLServer2017)

	d, drop := liveScratchDB(t, db, ctx, "gosmo_tablekinds_live")
	defer drop()

	liveExecIn(t, d, ctx, `CREATE TABLE dbo.tk_plain (id INT NOT NULL PRIMARY KEY)`)
	if graphOK {
		liveExecIn(t, d, ctx,
			`CREATE TABLE dbo.tk_node (id INT NOT NULL PRIMARY KEY) AS NODE`,
			`CREATE TABLE dbo.tk_edge AS EDGE`,
		)
	}

	t.Run("the kinds partition sys.tables", func(t *testing.T) {
		all, err := d.TablesContext(ctx)
		if err != nil {
			t.Fatalf("TablesContext: %v", err)
		}
		seen := map[string]int{}
		kinds := []TableKind{TableKindUser, TableKindFileTable, TableKindExternal}
		if graphOK {
			kinds = append(kinds, TableKindGraph)
		}
		for _, k := range kinds {
			listed, err := d.TablesOfKindContext(ctx, k)
			if err != nil {
				t.Fatalf("TablesOfKindContext(%s): %v", k, err)
			}
			for _, name := range tableNames(listed) {
				seen[name]++
			}
		}
		for _, name := range tableNames(all) {
			switch seen[name] {
			case 1:
			case 0:
				t.Errorf("%s is in Tables() but in no kind — a table the tree would lose", name)
			default:
				t.Errorf("%s is listed by %d kinds — the tree would show it that many times", name, seen[name])
			}
			delete(seen, name)
		}
		for name := range seen {
			t.Errorf("%s is listed by a kind but not by Tables()", name)
		}
	})

	t.Run("graph tables are not in the plain user list", func(t *testing.T) {
		if !graphOK {
			t.Skipf("major %d has no is_node/is_edge", major)
		}
		users, err := d.TablesOfKindContext(ctx, TableKindUser)
		if err != nil {
			t.Fatalf("TablesOfKindContext(user): %v", err)
		}
		if !hasName(users, "dbo.tk_plain") {
			t.Errorf("user tables %v do not include dbo.tk_plain", tableNames(users))
		}
		for _, name := range []string{"dbo.tk_node", "dbo.tk_edge"} {
			if hasName(users, name) {
				t.Errorf("%s is a graph table and is listed as a plain user table too: %v", name, tableNames(users))
			}
		}

		graph, err := d.TablesOfKindContext(ctx, TableKindGraph)
		if err != nil {
			t.Fatalf("TablesOfKindContext(graph): %v", err)
		}
		for _, name := range []string{"dbo.tk_node", "dbo.tk_edge"} {
			if !hasName(graph, name) {
				t.Errorf("graph tables %v do not include %s", tableNames(graph), name)
			}
		}
		for _, tb := range graph {
			if !tb.IsNode && !tb.IsEdge {
				t.Errorf("%s.%s came back from the graph listing with both flags false", tb.Schema, tb.Name)
			}
		}
	})

	t.Run("a graph listing is refused where the columns do not exist", func(t *testing.T) {
		if graphOK {
			t.Skipf("major %d has the graph columns", major)
		}
		_, err := d.TablesOfKindContext(ctx, TableKindGraph)
		if !errors.Is(err, ErrUnsupportedVersion) {
			t.Errorf("TablesOfKindContext(graph) on major %d = %v, want ErrUnsupportedVersion", major, err)
		}
	})

	t.Run("presence of the scratch database's kinds", func(t *testing.T) {
		p, err := d.TableKindsPresentContext(ctx)
		if err != nil {
			t.Fatalf("TableKindsPresentContext: %v", err)
		}
		want := TableKindPresence{Graph: graphOK}
		if p != want {
			t.Errorf("presence = %+v, want %+v", p, want)
		}
	})

	// msdb is the instance's own worked example of the System Tables folder:
	// its tables are ordinary rows in sys.tables with is_ms_shipped = 1, and
	// the reason the user listing has to exclude them at all.
	t.Run("msdb's own tables are system tables", func(t *testing.T) {
		msdb, err := srv.DatabaseByNameContext(ctx, "msdb")
		if err != nil {
			t.Fatalf("DatabaseByNameContext(msdb): %v", err)
		}
		system, err := msdb.TablesOfKindContext(ctx, TableKindSystem)
		if err != nil {
			t.Fatalf("TablesOfKindContext(system): %v", err)
		}
		if len(system) < 50 {
			t.Fatalf("msdb reports %d system tables, want the catalog's own hundred-odd", len(system))
		}
		for _, tb := range system {
			if !tb.IsSystem {
				t.Errorf("%s.%s came back from the system listing with IsSystem false", tb.Schema, tb.Name)
			}
		}
		users, err := msdb.TablesContext(ctx)
		if err != nil {
			t.Fatalf("TablesContext(msdb): %v", err)
		}
		for _, tb := range system {
			if hasName(users, tb.Schema+"."+tb.Name) {
				t.Fatalf("%s.%s is listed both as a system table and by Tables()", tb.Schema, tb.Name)
			}
		}
		// The by-name lookup deliberately does find one — see TableByName.
		if _, err := msdb.TableByNameContext(ctx, system[0].Schema, system[0].Name); err != nil {
			t.Errorf("TableByNameContext(%s.%s): %v", system[0].Schema, system[0].Name, err)
		}
		p, err := msdb.TableKindsPresentContext(ctx)
		if err != nil {
			t.Fatalf("TableKindsPresentContext(msdb): %v", err)
		}
		if !p.System {
			t.Errorf("msdb presence = %+v, want System true", p)
		}
	})

	t.Run("a filetable is its own kind", func(t *testing.T) {
		fdb, dropFS := liveFileTableDB(t, db, ctx, srv)
		if fdb == nil {
			return // skipped: no FILESTREAM on this instance
		}
		defer dropFS()

		fts, err := fdb.TablesOfKindContext(ctx, TableKindFileTable)
		if err != nil {
			t.Fatalf("TablesOfKindContext(filetable): %v", err)
		}
		if !hasName(fts, "dbo.tk_files") {
			t.Fatalf("filetables %v do not include dbo.tk_files", tableNames(fts))
		}
		if !fts[0].IsFileTable {
			t.Errorf("dbo.tk_files came back with IsFileTable false")
		}
		users, err := fdb.TablesOfKindContext(ctx, TableKindUser)
		if err != nil {
			t.Fatalf("TablesOfKindContext(user): %v", err)
		}
		if hasName(users, "dbo.tk_files") {
			t.Errorf("the filetable is listed as a plain user table too: %v", tableNames(users))
		}
		p, err := fdb.TableKindsPresentContext(ctx)
		if err != nil {
			t.Fatalf("TableKindsPresentContext: %v", err)
		}
		if !p.FileTable {
			t.Errorf("presence = %+v, want FileTable true", p)
		}
	})
}

// liveFileTableDB creates a throwaway FILESTREAM database holding one
// FileTable, or returns nil after skipping when the instance has FILESTREAM
// turned off — which no SQL connection can change (the filter driver and the
// share are the OS administrator's).
func liveFileTableDB(t *testing.T, db *sql.DB, ctx context.Context, srv *Server) (*Database, func()) {
	t.Helper()

	var level int
	if err := db.QueryRowContext(ctx, "SELECT CAST(SERVERPROPERTY('FilestreamEffectiveLevel') AS int)").Scan(&level); err != nil {
		t.Fatalf("reading FilestreamEffectiveLevel: %v", err)
	}
	if level == 0 {
		t.Skip("FILESTREAM is not enabled on this instance")
		return nil, nil
	}
	dataPath := srv.Info().DefaultDataPath
	if dataPath == "" {
		t.Skip("the instance reports no default data path")
		return nil, nil
	}

	const name = "gosmo_filetable_live"
	drop := func() {
		c := context.Background()
		db.ExecContext(c, "IF DB_ID('"+name+"') IS NOT NULL ALTER DATABASE ["+name+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE")
		db.ExecContext(c, "IF DB_ID('"+name+"') IS NOT NULL DROP DATABASE ["+name+"]")
	}
	drop()

	// The FILESTREAM file's FILENAME is a directory SQL Server creates and
	// DROP DATABASE removes; its parent has to exist, which is why the
	// instance's own default data path is read rather than assumed.
	create := `CREATE DATABASE [` + name + `]
ON PRIMARY ( NAME = ` + name + `_data, FILENAME = ` + QuoteLiteral(dataPath+name+`.mdf`) + ` ),
FILEGROUP ` + name + `_fs CONTAINS FILESTREAM
    ( NAME = ` + name + `_stream, FILENAME = ` + QuoteLiteral(dataPath+name+`_stream`) + ` )
LOG ON ( NAME = ` + name + `_log, FILENAME = ` + QuoteLiteral(dataPath+name+`.ldf`) + ` )
WITH FILESTREAM ( NON_TRANSACTED_ACCESS = FULL, DIRECTORY_NAME = N'` + name + `' )`
	if _, err := db.ExecContext(ctx, create); err != nil {
		drop()
		t.Skipf("cannot create a FILESTREAM database here: %v", err)
		return nil, nil
	}

	d, err := srv.DatabaseByNameContext(ctx, name)
	if err != nil {
		drop()
		t.Fatalf("DatabaseByNameContext(%s): %v", name, err)
	}
	liveExecIn(t, d, ctx, `CREATE TABLE dbo.tk_files AS FileTable`)
	return d, drop
}
