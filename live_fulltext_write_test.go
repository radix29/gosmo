//go:build livedb

// The full-text writes and scripts against two throwaway databases, on an
// instance with the component (win10cli, win10cli\SQL2017): every family
// created, altered and dropped through gosmo, then scripted from one
// database and the script run in the other — twice, since the guards make
// it re-runnable — and its DROP script run after.
//
//	go test -tags livedb . -run TestLiveFullTextWrites -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
package gosmo

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func TestLiveFullTextWrites(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 4*time.Minute)
	t.Cleanup(done) // registered first, so it runs after the drops below
	srv := liveServer(t, db, ctx)
	if info, err := srv.FullTextInfo(ctx); err != nil {
		t.Fatalf("FullTextInfo: %v", err)
	} else if !info.Installed {
		t.Skip("Full-Text Search is not installed here")
	}
	src, dropSrc := liveScratchDB(t, db, ctx, "gossms_p5_fts_w")
	t.Cleanup(dropSrc)
	dst, dropDst := liveScratchDB(t, db, ctx, "gossms_p5_fts_w2")
	t.Cleanup(dropDst)

	// Names that need their quoting: a bracket and an apostrophe.
	const catA, catB, list, plist = "ftc'a]", "ftc_b", "sl'a]", "spl'a]"

	// -- catalogs --
	a, err := src.CreateFullTextCatalog(ctx, CreateFullTextCatalogRequest{Name: catA, AccentSensitive: Ptr(false), IsDefault: true, Owner: "dbo"})
	if err != nil {
		t.Fatalf("CreateFullTextCatalog: %v", err)
	}
	if a.ID == 0 || !a.IsDefault || a.AccentSensitive || a.Owner != "dbo" {
		t.Errorf("created catalog read back as %+v", a)
	}
	b, err := src.CreateFullTextCatalog(ctx, CreateFullTextCatalogRequest{Name: catB})
	if err != nil {
		t.Fatalf("CreateFullTextCatalog(%s): %v", catB, err)
	}
	if err := b.SetDefault(ctx); err != nil || !b.IsDefault {
		t.Errorf("SetDefault: %v (IsDefault %v)", err, b.IsDefault)
	}
	if err := src.FullTextCatalogRef(catA).Rebuild(ctx, Ptr(true)); err != nil {
		t.Errorf("Rebuild: %v", err)
	}
	if err := src.FullTextCatalogRef(catA).Reorganize(ctx); err != nil {
		t.Errorf("Reorganize: %v", err)
	}
	if err := src.FullTextCatalogRef(catA).SetOwner(ctx, "dbo"); err != nil {
		t.Errorf("SetOwner: %v", err)
	}
	if a, err = src.FullTextCatalogByName(ctx, catA); err != nil || a.IsDefault || !a.AccentSensitive {
		t.Errorf("after Rebuild and the other default: %+v, %v", a, err)
	}
	if err := src.FullTextCatalogRef(catA).SetDefault(ctx); err != nil {
		t.Errorf("SetDefault back: %v", err)
	}

	// -- stoplists --
	sl, err := src.CreateFullTextStoplist(ctx, CreateFullTextStoplistRequest{Name: list, FromSystem: true, Owner: "dbo"})
	if err != nil {
		t.Fatalf("CreateFullTextStoplist: %v", err)
	}
	if sl.ID == 0 || sl.Owner != "dbo" {
		t.Errorf("created stoplist read back as %+v", sl)
	}
	words := func(l *FullTextStoplist) []FullTextStopword {
		t.Helper()
		w, err := l.Stopwords(ctx)
		if err != nil {
			t.Fatalf("Stopwords: %v", err)
		}
		return w
	}
	if n := len(words(sl)); n < 100 {
		t.Errorf("a copy of the system stoplist has %d words", n)
	}
	if err := sl.DropAllStopwords(ctx); err != nil || len(words(sl)) != 0 {
		t.Errorf("DropAllStopwords: %v", err)
	}
	for _, w := range [][2]string{{"o'brien", "1033"}, {"hereby", "English"}, {"zut", "0x040c"}, {"neutral", "0"}} {
		if err := sl.AddStopword(ctx, w[0], w[1]); err != nil {
			t.Errorf("AddStopword(%q, %q): %v", w[0], w[1], err)
		}
	}
	if err := sl.AddStopword(ctx, "hereby", "1033"); err == nil {
		t.Error("adding a word twice: no error")
	}
	if err := sl.DropStopword(ctx, "neutral", "0"); err != nil {
		t.Errorf("DropStopword: %v", err)
	}
	if err := sl.DropLanguageStopwords(ctx, "French"); err != nil {
		t.Errorf("DropLanguageStopwords: %v", err)
	}
	if got := words(sl); len(got) != 2 || got[0] != (FullTextStopword{"hereby", "English", 1033}) || got[1] != (FullTextStopword{"o'brien", "English", 1033}) {
		t.Errorf("stopwords = %+v", got)
	}
	if err := sl.SetOwner(ctx, "dbo"); err != nil {
		t.Errorf("stoplist SetOwner: %v", err)
	}
	cp, err := src.CreateFullTextStoplist(ctx, CreateFullTextStoplistRequest{Name: "copy", From: list})
	if err != nil || len(words(cp)) != 2 {
		t.Errorf("a stoplist copied from %s: %+v, %v", list, cp, err)
	}
	if cp2, err := dst.CreateFullTextStoplist(ctx, CreateFullTextStoplistRequest{Name: "copy", From: list, FromDatabase: src.Name}); err != nil || len(words(cp2)) != 2 {
		t.Errorf("a stoplist copied across databases: %+v, %v", cp2, err)
	} else if err := cp2.Drop(ctx); err != nil {
		t.Errorf("drop the cross-database copy: %v", err)
	}
	if err := src.FullTextStoplistRef("copy").Drop(ctx); err != nil {
		t.Errorf("stoplist Drop: %v", err)
	}

	// -- search property lists --
	pl, err := src.CreateSearchPropertyList(ctx, CreateSearchPropertyListRequest{Name: plist, Owner: "dbo"})
	if err != nil {
		t.Fatalf("CreateSearchPropertyList: %v", err)
	}
	title := SearchProperty{Name: "Title", SetGUID: "F29F85E0-4FF9-1068-AB91-08002B27B3D9", IntID: 2, Description: "it's the title"}
	author := SearchProperty{Name: "Author", SetGUID: "F29F85E0-4FF9-1068-AB91-08002B27B3D9", IntID: 4}
	for _, p := range []SearchProperty{title, author} {
		if err := pl.AddProperty(ctx, p); err != nil {
			t.Errorf("AddProperty(%s): %v", p.Name, err)
		}
	}
	if err := pl.DropProperty(ctx, "Author"); err != nil {
		t.Errorf("DropProperty: %v", err)
	}
	props, err := pl.Properties(ctx)
	if err != nil || len(props) != 1 || props[0].Name != "Title" || props[0].Description != "it's the title" || props[0].IntID != 2 {
		t.Errorf("Properties = %+v, %v", props, err)
	}
	if err := pl.SetOwner(ctx, "dbo"); err != nil {
		t.Errorf("property list SetOwner: %v", err)
	}
	if c, err := src.CreateSearchPropertyList(ctx, CreateSearchPropertyListRequest{Name: "copy", From: plist}); err != nil {
		t.Errorf("a property list copied: %v", err)
	} else if p, err := c.Properties(ctx); err != nil || len(p) != 1 {
		t.Errorf("the copy's properties = %+v, %v", p, err)
	} else if err := c.Drop(ctx); err != nil {
		t.Errorf("property list Drop: %v", err)
	}

	// -- the index --
	const docDDL = `CREATE TABLE dbo.[Do'c] (id int NOT NULL CONSTRAINT PK_Doc PRIMARY KEY, body nvarchar(max), extra nvarchar(200), ext nvarchar(8), content varbinary(max))`
	liveExecIn(t, src, ctx, docDDL,
		`INSERT dbo.[Do'c] VALUES (1, N'hello full text world', N'x', N'.txt', CAST('hello' AS varbinary(max))), (2, N'another document', N'y', N'.txt', NULL)`)
	doc := src.TableRef("dbo", "Do'c")
	idx, err := doc.CreateFullTextIndex(ctx, CreateFullTextIndexRequest{
		Columns: []FullTextIndexColumnSpec{
			{Name: "body", Language: "English"},
			{Name: "content", TypeColumn: "ext", Language: "1033"},
		},
		KeyIndex: "PK_Doc", Catalog: catA, ChangeTracking: FullTextChangeTrackingManual,
		Stoplist: list, SearchPropertyList: plist,
	})
	if err != nil {
		t.Fatalf("CreateFullTextIndex: %v", err)
	}
	// Read back through the TableRef: a handle, ErrHandleNotLoaded on the
	// read-back is what createdObject turns into the handle itself.
	if idx.Schema != "dbo" || idx.Table != "Do'c" {
		t.Errorf("CreateFullTextIndex returned %+v", idx)
	}
	loaded, err := src.TableByName(ctx, "dbo", "Do'c")
	if err != nil {
		t.Fatal(err)
	}
	waitIdle := func(what string) *FullTextIndex {
		t.Helper()
		for deadline := time.Now().Add(60 * time.Second); ; {
			i, err := loaded.FullTextIndex(ctx)
			if err != nil {
				t.Fatalf("%s: FullTextIndex: %v", what, err)
			}
			if i.CrawlCompleted && i.PopulateStatus == FullTextTableIdle {
				return i
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: population did not finish: %+v", what, i)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	got := waitIdle("create")
	if got.KeyIndex != "PK_Doc" || got.Catalog != catA || got.ChangeTracking != FullTextChangeTrackingManual ||
		got.StoplistKind != FullTextStoplistUser || got.Stoplist != list || got.SearchPropertyList != plist ||
		len(got.Columns) != 2 || got.Columns[1].TypeColumn != "ext" || got.Columns[0].LanguageID != 1033 || got.ItemCount != 2 {
		t.Errorf("created index read back as %+v", got)
	}
	if err := got.Drop(ctx); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if _, err := loaded.FullTextIndex(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("after Drop: %v, want ErrNotFound", err)
	}
	idx, err = loaded.CreateFullTextIndex(ctx, CreateFullTextIndexRequest{
		Columns:  []FullTextIndexColumnSpec{{Name: "body", Language: "English"}, {Name: "content", TypeColumn: "ext", Language: "1033"}},
		KeyIndex: "PK_Doc", Catalog: catA, FileGroup: "PRIMARY", ChangeTracking: FullTextChangeTrackingOff, NoPopulation: true,
		Stoplist: list, SearchPropertyList: plist,
	})
	if err != nil {
		t.Fatalf("CreateFullTextIndex from a loaded table: %v", err)
	}
	// From a loaded table the read-back is the index itself; NO POPULATION
	// left it uncrawled.
	if idx.ObjectID != loaded.ObjectID || idx.ChangeTracking != FullTextChangeTrackingOff || idx.FileGroup != "PRIMARY" || idx.ItemCount != 0 {
		t.Errorf("read-back from a loaded table: %+v", idx)
	}

	h := doc.FullTextIndexRef()
	steps := []struct {
		name string
		do   func() error
	}{
		{"AddColumn", func() error {
			return h.AddColumn(ctx, FullTextIndexColumnSpec{Name: "extra", Language: "0"}, true)
		}},
		{"DropColumn", func() error { return h.DropColumn(ctx, "extra", true) }},
		{"SetChangeTracking MANUAL", func() error { return h.SetChangeTracking(ctx, FullTextChangeTrackingManual) }},
		{"StartPopulation FULL", func() error { return h.StartPopulation(ctx, FullTextPopulationFull) }},
	}
	for _, s := range steps {
		if err := s.do(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
	}
	waitIdle("full population")
	// MANUAL tracking holds an insert back until an UPDATE population.
	liveExecIn(t, src, ctx, `INSERT dbo.[Do'c] VALUES (3, N'a third', NULL, N'.txt', NULL)`)
	if err := h.StartPopulation(ctx, FullTextPopulationUpdate); err != nil {
		t.Errorf("StartPopulation UPDATE: %v", err)
	}
	for deadline := time.Now().Add(60 * time.Second); ; {
		i := waitIdle("update population")
		if i.ItemCount == 3 && i.PendingChanges == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the tracked insert was not applied: %+v", i)
		}
		time.Sleep(500 * time.Millisecond)
	}
	// A FULL started and stopped at once: STOP can find it finished already,
	// which the server may refuse, so its result is not asserted.
	if err := h.StartPopulation(ctx, FullTextPopulationFull); err != nil {
		t.Errorf("StartPopulation FULL again: %v", err)
	}
	_ = h.StopPopulation(ctx)
	waitIdle("stop")
	for _, s := range []struct {
		name string
		do   func() error
	}{
		{"SetChangeTracking AUTO", func() error { return h.SetChangeTracking(ctx, FullTextChangeTrackingAuto) }},
		{"SetStoplist SYSTEM", func() error { return h.SetStoplist(ctx, FullTextStoplistSystem, "") }},
		{"SetSearchPropertyList OFF", func() error { return h.SetSearchPropertyList(ctx, "") }},
		{"Disable", func() error { return h.Disable(ctx) }},
	} {
		if err := s.do(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
	}
	got, err = loaded.FullTextIndex(ctx)
	if err != nil || got.IsEnabled || got.ChangeTracking != FullTextChangeTrackingAuto || got.StoplistKind != FullTextStoplistSystem ||
		got.SearchPropertyList != "" || len(got.Columns) != 2 {
		t.Errorf("after the alters: %+v, %v", got, err)
	}
	for _, s := range []struct {
		name string
		do   func() error
	}{
		{"Enable", func() error { return h.Enable(ctx) }},
		{"SetStoplist user", func() error { return h.SetStoplist(ctx, FullTextStoplistUser, list) }},
		{"SetSearchPropertyList", func() error { return h.SetSearchPropertyList(ctx, plist) }},
		{"SetChangeTracking MANUAL", func() error { return h.SetChangeTracking(ctx, FullTextChangeTrackingManual) }},
		{"Disable", func() error { return h.Disable(ctx) }},
	} {
		if err := s.do(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
	}
	// PAUSE and RESUME need a full population running; on an idle index the
	// server refuses them, which is the server's statement accepted and
	// answered — the syntax is the part that can be gosmo's fault.
	if err := h.PausePopulation(ctx); err == nil {
		t.Log("PAUSE on an idle, disabled index accepted")
	}
	if err := h.ResumePopulation(ctx); err == nil {
		t.Log("RESUME on an idle, disabled index accepted")
	}
	srcIdx, err := loaded.FullTextIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// -- scripts: run in dst twice, compared, then the DROP scripts --
	sc := NewScripter(src, DefaultScriptOptions())
	scripts := map[string]func() (string, error){
		"catalog":  func() (string, error) { return sc.ScriptFullTextCatalog(ctx, catA) },
		"stoplist": func() (string, error) { return sc.ScriptFullTextStoplist(ctx, list) },
		"plist":    func() (string, error) { return sc.ScriptSearchPropertyList(ctx, plist) },
		"index":    func() (string, error) { return sc.ScriptFullTextIndex(ctx, "dbo", "Do'c") },
	}
	order := []string{"catalog", "stoplist", "plist", "index"}
	liveExecIn(t, dst, ctx, docDDL)
	for pass := range 2 {
		for _, k := range order {
			s, err := scripts[k]()
			if err != nil {
				t.Fatalf("script %s: %v", k, err)
			}
			if pass == 0 {
				t.Logf("%s script:\n%s", k, s)
			}
			liveRunScript(t, dst, ctx, s)
		}
	}
	if c, err := dst.FullTextCatalogByName(ctx, catA); err != nil || !c.IsDefault || !c.AccentSensitive || c.Owner != "dbo" {
		t.Errorf("scripted catalog: %+v, %v", c, err)
	}
	if l, err := dst.FullTextStoplistByName(ctx, list); err != nil {
		t.Errorf("scripted stoplist: %v", err)
	} else if w := words(l); !slices.Equal(w, words(sl)) {
		t.Errorf("scripted stopwords %+v, want %+v", w, words(sl))
	}
	if p, err := dst.SearchPropertyListByName(ctx, plist); err != nil {
		t.Errorf("scripted property list: %v", err)
	} else if got, err := p.Properties(ctx); err != nil || len(got) != 1 || got[0].Name != "Title" || got[0].Description != title.Description {
		t.Errorf("scripted properties: %+v, %v", got, err)
	}
	dstDoc, err := dst.TableByName(ctx, "dbo", "Do'c")
	if err != nil {
		t.Fatal(err)
	}
	di, err := dstDoc.FullTextIndex(ctx)
	if err != nil {
		t.Fatalf("scripted index: %v", err)
	}
	if di.KeyIndex != srcIdx.KeyIndex || di.Catalog != srcIdx.Catalog || di.FileGroup != srcIdx.FileGroup ||
		di.IsEnabled != srcIdx.IsEnabled || di.ChangeTracking != srcIdx.ChangeTracking ||
		di.StoplistKind != srcIdx.StoplistKind || di.Stoplist != srcIdx.Stoplist || di.SearchPropertyList != srcIdx.SearchPropertyList ||
		!slices.Equal(di.Columns, srcIdx.Columns) {
		t.Errorf("scripted index differs:\n got %+v\nwant %+v", di, srcIdx)
	}

	drops := NewScripter(src, ScriptOptions{Verb: ScriptDrop, IncludeIfNotExists: true})
	for _, k := range slices.Backward(order) {
		var s string
		switch k {
		case "catalog":
			s, err = drops.ScriptFullTextCatalog(ctx, catA)
		case "stoplist":
			s, err = drops.ScriptFullTextStoplist(ctx, list)
		case "plist":
			s, err = drops.ScriptSearchPropertyList(ctx, plist)
		case "index":
			s, err = drops.ScriptFullTextIndex(ctx, "dbo", "Do'c")
		}
		if err != nil {
			t.Fatalf("DROP script %s: %v", k, err)
		}
		liveRunScript(t, dst, ctx, s)
		liveRunScript(t, dst, ctx, s) // guarded: a second run is a no-op
	}
	if c, err := dst.FullTextCatalogs(ctx); err != nil || len(c) != 0 {
		t.Errorf("catalogs after the DROP scripts: %+v, %v", c, err)
	}
	if l, err := dst.FullTextStoplists(ctx); err != nil || len(l) != 0 {
		t.Errorf("stoplists after the DROP scripts: %+v, %v", l, err)
	}
	if p, err := dst.SearchPropertyLists(ctx); err != nil || len(p) != 0 {
		t.Errorf("property lists after the DROP scripts: %+v, %v", p, err)
	}

	// -- drops through the handles, in dependency order --
	for _, s := range []struct {
		name string
		do   func() error
	}{
		{"index", func() error { return h.Drop(ctx) }},
		{"stoplist", func() error { return src.FullTextStoplistRef(list).Drop(ctx) }},
		{"property list", func() error { return src.SearchPropertyListRef(plist).Drop(ctx) }},
		{"catalog a", func() error { return src.FullTextCatalogRef(catA).Drop(ctx) }},
		{"catalog b", func() error { return src.FullTextCatalogRef(catB).Drop(ctx) }},
	} {
		if err := s.do(); err != nil {
			t.Errorf("drop %s: %v", s.name, err)
		}
	}
	if c, err := src.FullTextCatalogs(ctx); err != nil || len(c) != 0 {
		t.Errorf("catalogs after the drops: %+v, %v", c, err)
	}
	if i, err := src.FullTextIndexes(ctx); err != nil || len(i) != 0 {
		t.Errorf("indexes after the drops: %+v, %v", i, err)
	}
}
