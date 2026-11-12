//go:build livedb

// The full-text reads against a throwaway gossms_p5_fts database holding two
// catalogs, a stoplist, a search property list and two indexed tables
// (TestLiveFullText, on an instance with the component: win10cli and
// win10cli\SQL2017), and against an instance without it
// (TestLiveFullTextNotInstalled: win10cli\SQL2016), where every read must
// come back empty rather than fail. Where a semantic language statistics
// database is registered (win10cli's default instance), TestLiveFullText
// also creates, scripts and replays STATISTICAL_SEMANTICS columns.
//
//	go test -tags livedb . -run TestLiveFullText -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

const ftsDB = "gossms_p5_fts"

func TestLiveFullText(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 6*time.Minute)
	t.Cleanup(done) // registered first, so it runs after the drops below
	srv := liveServer(t, db, ctx)

	info, err := srv.FullTextInfo(ctx)
	if err != nil {
		t.Fatalf("FullTextInfo: %v", err)
	}
	if !info.Installed {
		t.Skip("Full-Text Search is not installed here — TestLiveFullTextNotInstalled covers this instance")
	}
	if len(info.Languages) == 0 || len(info.DocumentTypes) == 0 {
		t.Errorf("FullTextInfo: %d languages, %d document types; want some of each", len(info.Languages), len(info.DocumentTypes))
	}
	if i := slices.IndexFunc(info.Languages, func(l FullTextLanguage) bool { return l.LCID == 1033 }); i < 0 || info.Languages[i].Name != "English" {
		t.Errorf("Languages has no 1033 English: %+v", info.Languages)
	}
	if slices.IndexFunc(info.DocumentTypes, func(d FullTextDocumentType) bool { return d.Extension == ".txt" && len(d.ClassID) == 36 }) < 0 {
		t.Errorf("DocumentTypes has no .txt filter with a class id")
	}

	d, drop := liveScratchDB(t, db, ctx, ftsDB)
	t.Cleanup(drop)
	liveExecIn(t, d, ctx,
		`CREATE FULLTEXT CATALOG ftc_a WITH ACCENT_SENSITIVITY = OFF AS DEFAULT`,
		`CREATE FULLTEXT CATALOG ftc_b`,
		`CREATE FULLTEXT STOPLIST sl_a;`,
		`ALTER FULLTEXT STOPLIST sl_a ADD 'foo' LANGUAGE 1033;`,
		`ALTER FULLTEXT STOPLIST sl_a ADD 'bar' LANGUAGE 0;`,
		`CREATE SEARCH PROPERTY LIST spl_a;`,
		`ALTER SEARCH PROPERTY LIST spl_a ADD 'Title' WITH (PROPERTY_SET_GUID = 'F29F85E0-4FF9-1068-AB91-08002B27B3D9', PROPERTY_INT_ID = 2, PROPERTY_DESCRIPTION = 'System.Title');`,
		`CREATE TABLE dbo.Doc (id int NOT NULL CONSTRAINT PK_Doc PRIMARY KEY, body nvarchar(max), ext nvarchar(8), content varbinary(max))`,
		`INSERT dbo.Doc VALUES (1, N'hello full text world', N'.txt', CAST('hello' AS varbinary(max))), (2, N'another document', N'.txt', NULL)`,
		`CREATE FULLTEXT INDEX ON dbo.Doc (body LANGUAGE 1033, content TYPE COLUMN ext) KEY INDEX PK_Doc ON ftc_a
		 WITH CHANGE_TRACKING = MANUAL, STOPLIST = sl_a, SEARCH PROPERTY LIST = spl_a`,
		`CREATE TABLE dbo.Note (id int NOT NULL CONSTRAINT PK_Note PRIMARY KEY, txt nvarchar(200))`,
		`CREATE FULLTEXT INDEX ON dbo.Note (txt) KEY INDEX PK_Note ON ftc_b WITH CHANGE_TRACKING OFF, NO POPULATION, STOPLIST OFF`,
	)

	// Doc's population runs in the background; wait for it.
	doc, err := d.TableByName(ctx, "dbo", "Doc")
	if err != nil {
		t.Fatal(err)
	}
	docIdx := liveFullTextWait(t, ctx, doc, "Doc", func(i *FullTextIndex) bool {
		return i.CrawlCompleted && i.ItemCount == 2
	})

	// Catalogs, with the counters a finished population leaves.
	cats, err := d.FullText().Catalogs(ctx)
	if err != nil {
		t.Fatalf("Catalogs: %v", err)
	}
	if len(cats) != 2 || cats[0].Name != "ftc_a" || cats[1].Name != "ftc_b" {
		t.Fatalf("Catalogs = %+v", cats)
	}
	a, b := cats[0], cats[1]
	if !a.IsDefault || a.AccentSensitive || b.IsDefault || a.Owner != "dbo" || a.IndexCount != 1 || b.IndexCount != 1 {
		t.Errorf("ftc_a/ftc_b flags: %+v / %+v", a, b)
	}
	if a.ItemCount != 2 || a.UniqueKeyCount == 0 || a.PopulateStatus != FullTextCatalogIdle {
		t.Errorf("ftc_a counters: items %d, keys %d, status %v", a.ItemCount, a.UniqueKeyCount, a.PopulateStatus)
	}
	// PopulateCompletionAge and crawl_end_date are both the server's clock,
	// read two ways; they agree to the second or so.
	if a.LastPopulated.IsZero() || a.LastPopulated.Sub(docIdx.CrawlEnd).Abs() > 5*time.Second {
		t.Errorf("ftc_a LastPopulated = %v, Doc's crawl ended %v", a.LastPopulated, docIdx.CrawlEnd)
	}
	if !b.LastPopulated.IsZero() || b.ItemCount != 0 {
		t.Errorf("ftc_b, never populated: LastPopulated %v, items %d", b.LastPopulated, b.ItemCount)
	}
	if c, err := d.FullText().CatalogByName(ctx, "FTC_B"); err != nil || c.ID != b.ID {
		t.Errorf("CatalogByName(FTC_B) = %+v, %v", c, err)
	}
	if _, err := d.FullText().CatalogByName(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("CatalogByName(nope) err = %v, want ErrNotFound", err)
	}
	if idx, err := b.Indexes(ctx); err != nil || len(idx) != 1 || idx[0].Table != "Note" {
		t.Errorf("ftc_b.Indexes = %+v, %v", idx, err)
	}

	// The index as the table and the database list it.
	if docIdx.Schema != "dbo" || docIdx.Table != "Doc" || docIdx.KeyIndex != "PK_Doc" || docIdx.Catalog != "ftc_a" ||
		!docIdx.IsEnabled || docIdx.ChangeTracking != FullTextChangeTrackingManual ||
		docIdx.StoplistKind != FullTextStoplistUser || docIdx.Stoplist != "sl_a" || docIdx.SearchPropertyList != "spl_a" ||
		docIdx.CrawlType != "FULL_CRAWL" || docIdx.CrawlStart.IsZero() || docIdx.CrawlEnd.IsZero() ||
		docIdx.PopulateStatus != FullTextTableIdle || docIdx.Database() != d {
		t.Errorf("Doc's index: %+v", docIdx)
	}
	// 1 below 2025 (the substitute); 1 or 2 from it, read from the catalog.
	if docIdx.IndexVersion != 1 && docIdx.IndexVersion != 2 {
		t.Errorf("IndexVersion = %d", docIdx.IndexVersion)
	}
	if len(docIdx.Columns) != 2 {
		t.Fatalf("Doc's columns = %+v", docIdx.Columns)
	}
	if c := docIdx.Columns[0]; c.Name != "body" || c.LanguageID != 1033 || c.Language != "English" || c.TypeColumn != "" || c.StatisticalSemantics {
		t.Errorf("body column = %+v", c)
	}
	if c := docIdx.Columns[1]; c.Name != "content" || c.TypeColumn != "ext" {
		t.Errorf("content column = %+v", c)
	}
	all, err := d.FullText().Indexes(ctx)
	if err != nil || len(all) != 2 || all[0].Table != "Doc" || all[1].Table != "Note" {
		t.Fatalf("Indexes = %+v, %v", all, err)
	}
	note := all[1]
	if len(all[0].Columns) != 2 || len(note.Columns) != 1 || note.Columns[0].Name != "txt" {
		t.Errorf("columns grouped wrong: Doc %+v, Note %+v", all[0].Columns, note.Columns)
	}
	if note.ChangeTracking != FullTextChangeTrackingOff || note.StoplistKind != FullTextStoplistOff || note.Stoplist != "" ||
		note.SearchPropertyList != "" || note.CrawlCompleted || !note.CrawlEnd.IsZero() {
		t.Errorf("Note's index: %+v", note)
	}
	liveExecIn(t, d, ctx, `ALTER FULLTEXT INDEX ON dbo.Note SET STOPLIST SYSTEM`)
	if idx, err := d.FullText().Indexes(ctx); err != nil || idx[1].StoplistKind != FullTextStoplistSystem || idx[1].Stoplist != "" {
		t.Errorf("Note after STOPLIST SYSTEM: %+v, %v", idx, err)
	}

	// A table with no index is not found; a handle is refused.
	liveExecIn(t, d, ctx, `CREATE TABLE dbo.Plain (id int NOT NULL PRIMARY KEY)`)
	plain, err := d.TableByName(ctx, "dbo", "Plain")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.FullTextIndex(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("Plain.FullTextIndex err = %v, want ErrNotFound", err)
	}
	if _, err := d.TableRef("dbo", "Doc").FullTextIndex(ctx); !errors.Is(err, ErrHandleNotLoaded) {
		t.Errorf("TableRef.FullTextIndex err = %v, want ErrHandleNotLoaded", err)
	}

	// Populations: none on an idle index; one while a population runs.
	if pops, err := docIdx.Populations(ctx); err != nil || len(pops) != 0 {
		t.Errorf("idle Populations = %+v, %v", pops, err)
	}
	liveExecIn(t, d, ctx, `ALTER FULLTEXT INDEX ON dbo.Note START FULL POPULATION`)
	if pops, err := note.Populations(ctx); err != nil {
		t.Errorf("Populations during a population: %v", err)
	} else if len(pops) > 0 && (pops[0].Type == "" || pops[0].Status == "" || pops[0].StartTime.IsZero()) {
		// Note is empty, so its population can be over before the read:
		// none is a pass, a row has to be filled in.
		t.Errorf("population row = %+v", pops[0])
	}

	// Stoplists and property lists.
	lists, err := d.FullText().Stoplists(ctx)
	if err != nil || len(lists) != 1 || lists[0].Name != "sl_a" || lists[0].Owner != "dbo" || lists[0].CreateDate.IsZero() {
		t.Fatalf("Stoplists = %+v, %v", lists, err)
	}
	words, err := lists[0].Stopwords(ctx)
	if err != nil || len(words) != 2 || words[0] != (FullTextStopword{"foo", "English", 1033}) || words[1] != (FullTextStopword{"bar", "Neutral", 0}) {
		t.Errorf("Stopwords = %+v, %v", words, err)
	}
	if l, err := d.FullText().StoplistByName(ctx, "sl_a"); err != nil || l.ID != lists[0].ID {
		t.Errorf("StoplistByName = %+v, %v", l, err)
	}
	pls, err := d.FullText().SearchPropertyLists(ctx)
	if err != nil || len(pls) != 1 || pls[0].Name != "spl_a" || pls[0].Owner != "dbo" {
		t.Fatalf("SearchPropertyLists = %+v, %v", pls, err)
	}
	props, err := pls[0].Properties(ctx)
	if err != nil || len(props) != 1 || props[0].Name != "Title" || props[0].IntID != 2 ||
		props[0].SetGUID != "F29F85E0-4FF9-1068-AB91-08002B27B3D9" || props[0].Description != "System.Title" || props[0].ID == 0 {
		t.Errorf("Properties = %+v, %v", props, err)
	}
	if p, err := d.FullText().SearchPropertyListByName(ctx, "SPL_A"); err != nil || p.ID != pls[0].ID {
		t.Errorf("SearchPropertyListByName = %+v, %v", p, err)
	}

	// A user with no VIEW SERVER STATE reads the index; only the
	// population DMV is refused.
	const login, pass = "gosmo_fts_probe", "Pr0be!pass-5521"
	if _, err := db.ExecContext(ctx, "CREATE LOGIN "+quoteIdent(login)+" WITH PASSWORD = N'"+pass+"', CHECK_POLICY = OFF"); err != nil {
		t.Fatalf("create login: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(ctx, "DROP LOGIN "+quoteIdent(login)); err != nil {
			t.Errorf("drop login: %v", err)
		}
	})
	liveExecIn(t, d, ctx, "CREATE USER "+quoteIdent(login), "GRANT SELECT, VIEW DEFINITION TO "+quoteIdent(login))
	u, err := url.Parse(*liveDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(login, pass)
	other, err := sql.Open("sqlserver", u.String())
	if err != nil {
		t.Fatal(err)
	}
	// Closed before the database drop, which would otherwise wait on it.
	t.Cleanup(func() { other.Close() })
	osrv, err := NewServer(ctx, other)
	if err != nil {
		t.Fatalf("connect as %s: %v", login, err)
	}
	od := osrv.DatabaseRef(ftsDB)
	oidx, err := od.FullText().Indexes(ctx)
	if err != nil || len(oidx) != 2 || len(oidx[0].Columns) != 2 {
		t.Fatalf("Indexes as %s = %+v, %v", login, oidx, err)
	}
	if _, err := oidx[0].Populations(ctx); err == nil {
		t.Errorf("Populations as %s with no VIEW SERVER STATE: no error", login)
	}
	other.Close()

	// STATISTICAL_SEMANTICS, only where a semantic language statistics
	// database is registered (win10cli's 2025 instance): create, add a
	// column, read back, script, replay.
	var semantic int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.fulltext_semantic_language_statistics_database`).Scan(&semantic); err != nil {
		t.Fatalf("semantic database probe: %v", err)
	}
	if semantic == 0 {
		t.Log("no semantic language statistics database registered; STATISTICAL_SEMANTICS not exercised")
		return
	}
	liveExecIn(t, d, ctx, `CREATE TABLE dbo.Sem (id int NOT NULL CONSTRAINT PK_Sem PRIMARY KEY, abstract nvarchar(max), title nvarchar(200))`)
	sem, err := d.TableByName(ctx, "dbo", "Sem")
	if err != nil {
		t.Fatal(err)
	}
	semIdx, err := sem.CreateFullTextIndex(ctx, CreateFullTextIndexRequest{
		KeyIndex: "PK_Sem", Catalog: "ftc_b", ChangeTracking: FullTextChangeTrackingOff, NoPopulation: true,
		Columns: []FullTextIndexColumnSpec{{Name: "abstract", Language: "1033", StatisticalSemantics: true}},
	})
	if err != nil {
		t.Fatalf("CreateFullTextIndex with STATISTICAL_SEMANTICS: %v", err)
	}
	if err := semIdx.AddColumn(ctx, FullTextIndexColumnSpec{Name: "title", Language: "English", StatisticalSemantics: true}, FullTextNoPopulation); err != nil {
		t.Fatalf("AddColumn with STATISTICAL_SEMANTICS: %v", err)
	}
	semIdx, err = sem.FullTextIndex(ctx)
	if err != nil || len(semIdx.Columns) != 2 || !semIdx.Columns[0].StatisticalSemantics || !semIdx.Columns[1].StatisticalSemantics {
		t.Fatalf("Sem's index = %+v, %v; want both columns STATISTICAL_SEMANTICS", semIdx, err)
	}
	script, err := NewScripter(d, ScriptOptions{Verb: ScriptCreate}).ScriptFullTextIndex(ctx, "dbo", "Sem")
	if err != nil {
		t.Fatalf("ScriptFullTextIndex(Sem): %v", err)
	}
	if !strings.Contains(script, "[abstract] LANGUAGE 1033 STATISTICAL_SEMANTICS, [title] LANGUAGE 1033 STATISTICAL_SEMANTICS") {
		t.Errorf("Sem's script lost STATISTICAL_SEMANTICS:\n%s", script)
	}
	if err := semIdx.Drop(ctx); err != nil {
		t.Fatal(err)
	}
	liveRunScript(t, d, ctx, script)
	if again, err := sem.FullTextIndex(ctx); err != nil || len(again.Columns) != 2 || !again.Columns[0].StatisticalSemantics || !again.Columns[1].StatisticalSemantics {
		t.Errorf("Sem's index after replaying its script = %+v, %v", again, err)
	}
}

// liveFullTextWait reads tbl's full-text index until done holds, for at
// most liveFullTextWaitFor. A crawl is the instance's own background work,
// sharing the filter daemon with every other database's, and once outran a
// 60 s wait in a run that then passed twice; on timeout the failure carries
// what liveFullTextState reads, so the next one tells a slow crawl from a
// stuck one.
func liveFullTextWait(t *testing.T, ctx context.Context, tbl *Table, what string, done func(*FullTextIndex) bool) *FullTextIndex {
	t.Helper()
	for deadline := time.Now().Add(liveFullTextWaitFor); ; {
		i, err := tbl.FullTextIndex(ctx)
		if err != nil {
			t.Fatalf("%s: FullTextIndex: %v", what, err)
		}
		if done(i) {
			return i
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: population did not finish in %v: %+v\n%s", what, liveFullTextWaitFor, i, liveFullTextState(ctx, tbl))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

const liveFullTextWaitFor = 2 * time.Minute

// liveFullTextState is tbl's catalog's PopulateStatus and ItemCount and the
// table's sys.dm_fts_index_population rows, as one line.
func liveFullTextState(ctx context.Context, tbl *Table) string {
	var s string
	err := tbl.Database().queryRow(ctx, func(r *sql.Row) error { return r.Scan(&s) }, `
SELECT CONCAT(N'catalog ', c.name COLLATE DATABASE_DEFAULT,
       N': PopulateStatus=', FULLTEXTCATALOGPROPERTY(c.name, 'PopulateStatus'),
       N' ItemCount=', FULLTEXTCATALOGPROPERTY(c.name, 'ItemCount'),
       N'; populations: ', ISNULL((
           SELECT STRING_AGG(CONCAT(p.population_type_description COLLATE DATABASE_DEFAULT, N' ', p.status_description COLLATE DATABASE_DEFAULT,
                      N' (', p.completion_type_description COLLATE DATABASE_DEFAULT, N') since ', CONVERT(nvarchar(23), p.start_time, 121),
                      N', ranges ', p.completed_range_count, N'/', p.range_count,
                      N', outstanding batches ', p.outstanding_batch_count), N'; ')
           FROM sys.dm_fts_index_population AS p
           WHERE p.database_id = DB_ID() AND p.table_id = i.object_id), N'none'))
FROM sys.fulltext_indexes AS i
JOIN sys.fulltext_catalogs AS c ON c.fulltext_catalog_id = i.fulltext_catalog_id
WHERE i.object_id = @p1`, tbl.ObjectID)
	if err != nil {
		return "full-text state unread: " + err.Error()
	}
	return s
}

// On an instance without the component every read is empty, not an error.
func TestLiveFullTextNotInstalled(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv := liveServer(t, db, ctx)

	info, err := srv.FullTextInfo(ctx)
	if err != nil {
		t.Fatalf("FullTextInfo: %v", err)
	}
	if info.Installed {
		t.Skip("Full-Text Search is installed here — TestLiveFullText covers this instance")
	}
	d := srv.DatabaseRef("master")
	if c, err := d.FullText().Catalogs(ctx); err != nil || len(c) != 0 {
		t.Errorf("Catalogs = %+v, %v", c, err)
	}
	if l, err := d.FullText().Stoplists(ctx); err != nil || len(l) != 0 {
		t.Errorf("Stoplists = %+v, %v", l, err)
	}
	if p, err := d.FullText().SearchPropertyLists(ctx); err != nil || len(p) != 0 {
		t.Errorf("SearchPropertyLists = %+v, %v", p, err)
	}
	if i, err := d.FullText().Indexes(ctx); err != nil || len(i) != 0 {
		t.Errorf("Indexes = %+v, %v", i, err)
	}
}
