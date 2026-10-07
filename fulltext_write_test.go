package gosmo

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Every full-text write from its Ref handle, with quote-hostile names (see
// script_write_common_test.go): the handle is all a write needs, and each
// one is pinned whole.
func TestFullTextWriteStatements(t *testing.T) {
	d := scriptTestDB
	cat := func() *FullTextCatalog { return d().FullTextCatalogRef("ft]c") }
	sl := func() *FullTextStoplist { return d().FullTextStoplistRef("s]l") }
	pl := func() *SearchPropertyList { return d().SearchPropertyListRef("p]l") }
	ix := func() *FullTextIndex { return d().TableRef("Sales.Archive", "o'brien").FullTextIndexRef() }
	const tbl = "[Sales.Archive].[o'brien]"
	u := scriptUsePrefix
	runScriptCases(t, []scriptCase{
		{"catalog create, all options", func(c context.Context) error {
			_, err := d().CreateFullTextCatalog(c, CreateFullTextCatalogRequest{
				Name: "ft]c", AccentSensitive: Ptr(false), IsDefault: true, Owner: "o]wn"})
			return err
		}, u + "CREATE FULLTEXT CATALOG [ft]]c] WITH ACCENT_SENSITIVITY = OFF AS DEFAULT AUTHORIZATION [o]]wn]"},
		{"catalog create, bare", func(c context.Context) error {
			_, err := d().CreateFullTextCatalog(c, CreateFullTextCatalogRequest{Name: "ft]c"})
			return err
		}, u + "CREATE FULLTEXT CATALOG [ft]]c]"},
		{"catalog rebuild", func(c context.Context) error { return cat().Rebuild(c, nil) },
			u + "ALTER FULLTEXT CATALOG [ft]]c] REBUILD"},
		{"catalog rebuild accent", func(c context.Context) error { return cat().Rebuild(c, Ptr(true)) },
			u + "ALTER FULLTEXT CATALOG [ft]]c] REBUILD WITH ACCENT_SENSITIVITY = ON"},
		{"catalog reorganize", func(c context.Context) error { return cat().Reorganize(c) },
			u + "ALTER FULLTEXT CATALOG [ft]]c] REORGANIZE"},
		{"catalog default", func(c context.Context) error { return cat().SetDefault(c) },
			u + "ALTER FULLTEXT CATALOG [ft]]c] AS DEFAULT"},
		{"catalog owner", func(c context.Context) error { return cat().SetOwner(c, "a]b") },
			u + "ALTER AUTHORIZATION ON FULLTEXT CATALOG::[ft]]c] TO [a]]b]"},
		{"catalog drop", func(c context.Context) error { return cat().Drop(c) },
			u + "DROP FULLTEXT CATALOG [ft]]c]"},

		{"stoplist create empty", func(c context.Context) error {
			_, err := d().CreateFullTextStoplist(c, CreateFullTextStoplistRequest{Name: "s]l"})
			return err
		}, u + "CREATE FULLTEXT STOPLIST [s]]l];"},
		{"stoplist create from system", func(c context.Context) error {
			_, err := d().CreateFullTextStoplist(c, CreateFullTextStoplistRequest{Name: "s]l", FromSystem: true, Owner: "o]wn"})
			return err
		}, u + "CREATE FULLTEXT STOPLIST [s]]l] FROM SYSTEM STOPLIST AUTHORIZATION [o]]wn];"},
		{"stoplist create from another database", func(c context.Context) error {
			_, err := d().CreateFullTextStoplist(c, CreateFullTextStoplistRequest{Name: "s]l", From: "Sales.Archive", FromDatabase: "o'brien"})
			return err
		}, u + "CREATE FULLTEXT STOPLIST [s]]l] FROM [o'brien].[Sales.Archive];"},
		{"stoplist create from local", func(c context.Context) error {
			_, err := d().CreateFullTextStoplist(c, CreateFullTextStoplistRequest{Name: "s]l", From: "a]b"})
			return err
		}, u + "CREATE FULLTEXT STOPLIST [s]]l] FROM [a]]b];"},
		{"stopword add by lcid", func(c context.Context) error { return sl().AddStopword(c, "o'brien", "1033") },
			u + "ALTER FULLTEXT STOPLIST [s]]l] ADD N'o''brien' LANGUAGE 1033;"},
		{"stopword add by hex", func(c context.Context) error { return sl().AddStopword(c, "x", "0x0409") },
			u + "ALTER FULLTEXT STOPLIST [s]]l] ADD N'x' LANGUAGE 0x0409;"},
		{"stopword add by name", func(c context.Context) error { return sl().AddStopword(c, "x", "o'brien") },
			u + "ALTER FULLTEXT STOPLIST [s]]l] ADD N'x' LANGUAGE N'o''brien';"},
		{"stopword drop", func(c context.Context) error { return sl().DropStopword(c, "o'brien", "0") },
			u + "ALTER FULLTEXT STOPLIST [s]]l] DROP N'o''brien' LANGUAGE 0;"},
		{"stopwords drop language", func(c context.Context) error { return sl().DropLanguageStopwords(c, "English") },
			u + "ALTER FULLTEXT STOPLIST [s]]l] DROP ALL LANGUAGE N'English';"},
		{"stopwords drop all", func(c context.Context) error { return sl().DropAllStopwords(c) },
			u + "ALTER FULLTEXT STOPLIST [s]]l] DROP ALL;"},
		{"stoplist owner", func(c context.Context) error { return sl().SetOwner(c, "a]b") },
			u + "ALTER AUTHORIZATION ON FULLTEXT STOPLIST::[s]]l] TO [a]]b]"},
		{"stoplist drop", func(c context.Context) error { return sl().Drop(c) },
			u + "DROP FULLTEXT STOPLIST [s]]l];"},

		{"property list create", func(c context.Context) error {
			_, err := d().CreateSearchPropertyList(c, CreateSearchPropertyListRequest{Name: "p]l", Owner: "o]wn"})
			return err
		}, u + "CREATE SEARCH PROPERTY LIST [p]]l] AUTHORIZATION [o]]wn];"},
		{"property list create from", func(c context.Context) error {
			_, err := d().CreateSearchPropertyList(c, CreateSearchPropertyListRequest{Name: "p]l", From: "a]b", FromDatabase: "o'brien"})
			return err
		}, u + "CREATE SEARCH PROPERTY LIST [p]]l] FROM [o'brien].[a]]b];"},
		{"property add", func(c context.Context) error {
			return pl().AddProperty(c, SearchProperty{Name: "o'brien", SetGUID: "F29F85E0-4FF9-1068-AB91-08002B27B3D9", IntID: 2, Description: "it's"})
		}, u + "ALTER SEARCH PROPERTY LIST [p]]l] ADD N'o''brien' WITH (PROPERTY_SET_GUID = N'F29F85E0-4FF9-1068-AB91-08002B27B3D9', PROPERTY_INT_ID = 2, PROPERTY_DESCRIPTION = N'it''s');"},
		{"property add, no description", func(c context.Context) error {
			return pl().AddProperty(c, SearchProperty{Name: "T", SetGUID: "g", IntID: 4})
		}, u + "ALTER SEARCH PROPERTY LIST [p]]l] ADD N'T' WITH (PROPERTY_SET_GUID = N'g', PROPERTY_INT_ID = 4);"},
		{"property drop", func(c context.Context) error { return pl().DropProperty(c, "o'brien") },
			u + "ALTER SEARCH PROPERTY LIST [p]]l] DROP N'o''brien';"},
		{"property list owner", func(c context.Context) error { return pl().SetOwner(c, "a]b") },
			u + "ALTER AUTHORIZATION ON SEARCH PROPERTY LIST::[p]]l] TO [a]]b]"},
		{"property list drop", func(c context.Context) error { return pl().Drop(c) },
			u + "DROP SEARCH PROPERTY LIST [p]]l];"},

		{"index create, everything", func(c context.Context) error {
			_, err := d().TableRef("Sales.Archive", "o'brien").CreateFullTextIndex(c, CreateFullTextIndexRequest{
				Columns: []FullTextIndexColumnSpec{
					{Name: "a]b", Language: "1033", StatisticalSemantics: true},
					{Name: "doc", TypeColumn: "e]xt", Language: "English"},
				},
				KeyIndex: "PK]1", Catalog: "ft]c", FileGroup: "F]G",
				ChangeTracking: FullTextChangeTrackingOff, NoPopulation: true,
				Stoplist: "s]l", SearchPropertyList: "p]l",
			})
			return err
		}, u + "CREATE FULLTEXT INDEX ON " + tbl + " ([a]]b] LANGUAGE 1033 STATISTICAL_SEMANTICS, [doc] TYPE COLUMN [e]]xt] LANGUAGE N'English')" +
			" KEY INDEX [PK]]1] ON ([ft]]c], FILEGROUP [F]]G]) WITH (CHANGE_TRACKING = OFF, NO POPULATION, STOPLIST = [s]]l], SEARCH PROPERTY LIST = [p]]l])"},
		{"index create, defaults", func(c context.Context) error {
			_, err := d().TableRef("dbo", "t").CreateFullTextIndex(c, CreateFullTextIndexRequest{
				Columns: []FullTextIndexColumnSpec{{Name: "c"}}, KeyIndex: "pk"})
			return err
		}, u + "CREATE FULLTEXT INDEX ON [dbo].[t] ([c]) KEY INDEX [pk]"},
		{"index create, filegroup only, stoplist off", func(c context.Context) error {
			_, err := d().TableRef("dbo", "t").CreateFullTextIndex(c, CreateFullTextIndexRequest{
				Columns: []FullTextIndexColumnSpec{{Name: "c"}}, KeyIndex: "pk", FileGroup: "fg", StoplistOff: true,
				ChangeTracking: FullTextChangeTrackingManual})
			return err
		}, u + "CREATE FULLTEXT INDEX ON [dbo].[t] ([c]) KEY INDEX [pk] ON (FILEGROUP [fg]) WITH (CHANGE_TRACKING = MANUAL, STOPLIST = OFF)"},
		{"index create, catalog only", func(c context.Context) error {
			_, err := d().TableRef("dbo", "t").CreateFullTextIndex(c, CreateFullTextIndexRequest{
				Columns: []FullTextIndexColumnSpec{{Name: "c"}}, KeyIndex: "pk", Catalog: "c"})
			return err
		}, u + "CREATE FULLTEXT INDEX ON [dbo].[t] ([c]) KEY INDEX [pk] ON [c]"},
		{"index enable", func(c context.Context) error { return ix().Enable(c) },
			u + "ALTER FULLTEXT INDEX ON " + tbl + " ENABLE"},
		{"index disable", func(c context.Context) error { return ix().Disable(c) },
			u + "ALTER FULLTEXT INDEX ON " + tbl + " DISABLE"},
		{"index add column", func(c context.Context) error {
			return ix().AddColumn(c, FullTextIndexColumnSpec{Name: "a]b", Language: "0"}, true)
		}, u + "ALTER FULLTEXT INDEX ON " + tbl + " ADD ([a]]b] LANGUAGE 0) WITH NO POPULATION"},
		{"index add column, populate", func(c context.Context) error {
			return ix().AddColumn(c, FullTextIndexColumnSpec{Name: "c"}, false)
		}, u + "ALTER FULLTEXT INDEX ON " + tbl + " ADD ([c])"},
		{"index drop column", func(c context.Context) error { return ix().DropColumn(c, "a]b", false) },
			u + "ALTER FULLTEXT INDEX ON " + tbl + " DROP ([a]]b])"},
		{"index drop column, no population", func(c context.Context) error { return ix().DropColumn(c, "c", true) },
			u + "ALTER FULLTEXT INDEX ON " + tbl + " DROP ([c]) WITH NO POPULATION"},
		{"index change tracking", func(c context.Context) error { return ix().SetChangeTracking(c, FullTextChangeTrackingManual) },
			u + "ALTER FULLTEXT INDEX ON " + tbl + " SET CHANGE_TRACKING = MANUAL"},
		{"index stoplist off", func(c context.Context) error { return ix().SetStoplist(c, FullTextStoplistOff, "ignored") },
			u + "ALTER FULLTEXT INDEX ON " + tbl + " SET STOPLIST = OFF"},
		{"index stoplist system", func(c context.Context) error { return ix().SetStoplist(c, FullTextStoplistSystem, "") },
			u + "ALTER FULLTEXT INDEX ON " + tbl + " SET STOPLIST = SYSTEM"},
		{"index stoplist user", func(c context.Context) error { return ix().SetStoplist(c, FullTextStoplistUser, "s]l") },
			u + "ALTER FULLTEXT INDEX ON " + tbl + " SET STOPLIST = [s]]l]"},
		{"index property list", func(c context.Context) error { return ix().SetSearchPropertyList(c, "p]l") },
			u + "ALTER FULLTEXT INDEX ON " + tbl + " SET SEARCH PROPERTY LIST = [p]]l]"},
		{"index property list off", func(c context.Context) error { return ix().SetSearchPropertyList(c, "") },
			u + "ALTER FULLTEXT INDEX ON " + tbl + " SET SEARCH PROPERTY LIST = OFF"},
		{"index full population", func(c context.Context) error { return ix().StartPopulation(c, FullTextPopulationFull) },
			u + "ALTER FULLTEXT INDEX ON " + tbl + " START FULL POPULATION"},
		{"index incremental population", func(c context.Context) error { return ix().StartPopulation(c, FullTextPopulationIncremental) },
			u + "ALTER FULLTEXT INDEX ON " + tbl + " START INCREMENTAL POPULATION"},
		{"index apply tracked changes", func(c context.Context) error { return ix().StartPopulation(c, FullTextPopulationUpdate) },
			u + "ALTER FULLTEXT INDEX ON " + tbl + " START UPDATE POPULATION"},
		{"index stop population", func(c context.Context) error { return ix().StopPopulation(c) },
			u + "ALTER FULLTEXT INDEX ON " + tbl + " STOP POPULATION"},
		{"index pause population", func(c context.Context) error { return ix().PausePopulation(c) },
			u + "ALTER FULLTEXT INDEX ON " + tbl + " PAUSE POPULATION"},
		{"index resume population", func(c context.Context) error { return ix().ResumePopulation(c) },
			u + "ALTER FULLTEXT INDEX ON " + tbl + " RESUME POPULATION"},
		{"index drop", func(c context.Context) error { return ix().Drop(c) },
			u + "DROP FULLTEXT INDEX ON " + tbl},
	})
}

// A Create under WithScript answers with the family's handle, and a write
// under it leaves the receiver as it was (CLAUDE.md § Script mode).
func TestFullTextCreatesReturnHandlesAndScriptedWritesDoNotMirror(t *testing.T) {
	ctx, _ := WithScript(context.Background())
	d := scriptTestDB()
	if c, err := d.CreateFullTextCatalog(ctx, CreateFullTextCatalogRequest{Name: "c"}); err != nil || c.Name != "c" || c.Database() != d {
		t.Errorf("CreateFullTextCatalog = %+v, %v", c, err)
	}
	if l, err := d.CreateFullTextStoplist(ctx, CreateFullTextStoplistRequest{Name: "l"}); err != nil || l.Name != "l" {
		t.Errorf("CreateFullTextStoplist = %+v, %v", l, err)
	}
	if p, err := d.CreateSearchPropertyList(ctx, CreateSearchPropertyListRequest{Name: "p"}); err != nil || p.Name != "p" {
		t.Errorf("CreateSearchPropertyList = %+v, %v", p, err)
	}
	i, err := d.TableRef("dbo", "t").CreateFullTextIndex(ctx, CreateFullTextIndexRequest{KeyIndex: "pk"})
	if err != nil || i.Schema != "dbo" || i.Table != "t" || i.Database() != d {
		t.Fatalf("CreateFullTextIndex = %+v, %v", i, err)
	}

	i.IsEnabled, i.ChangeTracking, i.StoplistKind, i.Stoplist, i.SearchPropertyList = true, FullTextChangeTrackingAuto, FullTextStoplistUser, "s", "p"
	_ = i.Disable(ctx)
	_ = i.SetChangeTracking(ctx, FullTextChangeTrackingOff)
	_ = i.SetStoplist(ctx, FullTextStoplistOff, "")
	_ = i.SetSearchPropertyList(ctx, "")
	if !i.IsEnabled || i.ChangeTracking != FullTextChangeTrackingAuto || i.StoplistKind != FullTextStoplistUser || i.Stoplist != "s" || i.SearchPropertyList != "p" {
		t.Errorf("scripted writes mirrored onto the index: %+v", i)
	}
	c := d.FullTextCatalogRef("c")
	_ = c.SetDefault(ctx)
	_ = c.Rebuild(ctx, Ptr(true))
	if c.IsDefault || c.AccentSensitive {
		t.Errorf("scripted writes mirrored onto the catalog: %+v", c)
	}
}

// The refusals come before anything is sent.
func TestFullTextWritesRefuseBadRequests(t *testing.T) {
	ctx, script := WithScript(context.Background())
	d := scriptTestDB()
	tbl := d.TableRef("dbo", "t")
	col := []FullTextIndexColumnSpec{{Name: "c"}}
	for name, call := range map[string]func() error{
		"index without key": func() error {
			_, err := tbl.CreateFullTextIndex(ctx, CreateFullTextIndexRequest{Columns: col})
			return err
		},
		"index stoplist off and named": func() error {
			_, err := tbl.CreateFullTextIndex(ctx, CreateFullTextIndexRequest{Columns: col, KeyIndex: "pk", StoplistOff: true, Stoplist: "s"})
			return err
		},
		"index no population without tracking off": func() error {
			_, err := tbl.CreateFullTextIndex(ctx, CreateFullTextIndexRequest{Columns: col, KeyIndex: "pk", NoPopulation: true, ChangeTracking: FullTextChangeTrackingAuto})
			return err
		},
		"index unknown tracking": func() error {
			_, err := tbl.CreateFullTextIndex(ctx, CreateFullTextIndexRequest{Columns: col, KeyIndex: "pk", ChangeTracking: "SOMETIMES"})
			return err
		},
		"index without schema": func() error {
			_, err := d.TableRef("", "t").CreateFullTextIndex(ctx, CreateFullTextIndexRequest{Columns: col, KeyIndex: "pk"})
			return err
		},
		"set empty tracking":    func() error { return tbl.FullTextIndexRef().SetChangeTracking(ctx, "") },
		"user stoplist unnamed": func() error { return tbl.FullTextIndexRef().SetStoplist(ctx, FullTextStoplistUser, "") },
		"unknown stoplist kind": func() error { return tbl.FullTextIndexRef().SetStoplist(ctx, 9, "") },
		"unknown population":    func() error { return tbl.FullTextIndexRef().StartPopulation(ctx, "SOME") },
		"drop without schema":   func() error { return d.TableRef("", "t").FullTextIndexRef().Drop(ctx) },
		"alter without schema":  func() error { return d.TableRef("", "t").FullTextIndexRef().Enable(ctx) },
		"stoplist from both": func() error {
			_, err := d.CreateFullTextStoplist(ctx, CreateFullTextStoplistRequest{Name: "l", FromSystem: true, From: "x"})
			return err
		},
		"stoplist database without source": func() error {
			_, err := d.CreateFullTextStoplist(ctx, CreateFullTextStoplistRequest{Name: "l", FromDatabase: "x"})
			return err
		},
		"property list database without source": func() error {
			_, err := d.CreateSearchPropertyList(ctx, CreateSearchPropertyListRequest{Name: "p", FromDatabase: "x"})
			return err
		},
	} {
		if err := call(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if n := script.Len(); n != 0 {
		t.Errorf("refused writes sent %d statements: %v", n, script.Statements())
	}
	if err := d.TableRef("", "t").FullTextIndexRef().Drop(ctx); !errors.Is(err, ErrSchemaRequired) {
		t.Errorf("Drop without a schema: %v, want ErrSchemaRequired", err)
	}
}

func TestFullTextLanguageTerm(t *testing.T) {
	for in, want := range map[string]string{
		"1033": "1033", "0": "0", "0x0409": "0x0409", "0X40c": "0X40c",
		"English": "N'English'", "0x": "N'0x'", "0xZZ": "N'0xZZ'", "-1": "N'-1'", "Simplified Chinese": "N'Simplified Chinese'",
	} {
		if got := fullTextLanguage(in); got != want {
			t.Errorf("fullTextLanguage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildFullTextScripts(t *testing.T) {
	d := scriptTestDB()
	opts := DefaultScriptOptions()
	drop := opts
	drop.Verb = ScriptDrop
	plain := ScriptOptions{}

	cat := &FullTextCatalog{db: d, Name: "o'b]c", AccentSensitive: true, IsDefault: true, Owner: "dbo"}
	if got, want := buildFullTextCatalogScript(cat, opts),
		"IF NOT EXISTS (SELECT 1 FROM sys.fulltext_catalogs WHERE name = N'o''b]c')\n"+
			"CREATE FULLTEXT CATALOG [o'b]]c] WITH ACCENT_SENSITIVITY = ON AS DEFAULT AUTHORIZATION [dbo]\nGO\n"; got != want {
		t.Errorf("catalog CREATE:\n%s\nwant:\n%s", got, want)
	}
	if got, want := buildFullTextCatalogScript(cat, drop),
		"IF EXISTS (SELECT 1 FROM sys.fulltext_catalogs WHERE name = N'o''b]c')\nDROP FULLTEXT CATALOG [o'b]]c]\nGO\n"; got != want {
		t.Errorf("catalog DROP:\n%s\nwant:\n%s", got, want)
	}

	sl := &FullTextStoplist{db: d, Name: "s'l", Owner: "dbo"}
	words := []FullTextStopword{{Word: "o'brien", Language: "English", LanguageID: 1033}, {Word: "x", LanguageID: 0}}
	if got, want := buildFullTextStoplistScript(sl, words, plain),
		"CREATE FULLTEXT STOPLIST [s'l] AUTHORIZATION [dbo];\nGO\n"+
			"ALTER FULLTEXT STOPLIST [s'l] ADD N'o''brien' LANGUAGE 1033;\nGO\n"+
			"ALTER FULLTEXT STOPLIST [s'l] ADD N'x' LANGUAGE 0;\nGO\n"; got != want {
		t.Errorf("stoplist CREATE:\n%s\nwant:\n%s", got, want)
	}
	got := buildFullTextStoplistScript(sl, words, opts)
	if !strings.Contains(got, "WHERE l.name = N's''l' AND w.stopword = N'o''brien' AND w.language_id = 1033)\nALTER FULLTEXT STOPLIST [s'l] ADD N'o''brien'") ||
		!strings.HasPrefix(got, "IF NOT EXISTS (SELECT 1 FROM sys.fulltext_stoplists WHERE name = N's''l')\nCREATE FULLTEXT STOPLIST") {
		t.Errorf("stoplist guards:\n%s", got)
	}
	if got, want := buildFullTextStoplistScript(sl, nil, drop),
		"IF EXISTS (SELECT 1 FROM sys.fulltext_stoplists WHERE name = N's''l')\nDROP FULLTEXT STOPLIST [s'l];\nGO\n"; got != want {
		t.Errorf("stoplist DROP:\n%s\nwant:\n%s", got, want)
	}

	pl := &SearchPropertyList{db: d, Name: "p'l"}
	props := []SearchProperty{{Name: "Title", SetGUID: "F29F85E0-4FF9-1068-AB91-08002B27B3D9", IntID: 2, Description: "System.Title", ID: 7}}
	if got, want := buildSearchPropertyListScript(pl, props, plain),
		"CREATE SEARCH PROPERTY LIST [p'l];\nGO\n"+
			"ALTER SEARCH PROPERTY LIST [p'l] ADD N'Title' WITH (PROPERTY_SET_GUID = N'F29F85E0-4FF9-1068-AB91-08002B27B3D9', PROPERTY_INT_ID = 2, PROPERTY_DESCRIPTION = N'System.Title');\nGO\n"; got != want {
		t.Errorf("property list CREATE:\n%s\nwant:\n%s", got, want)
	}
	if got := buildSearchPropertyListScript(pl, props, opts); !strings.Contains(got, "WHERE l.name = N'p''l' AND sp.property_name = N'Title')\nALTER SEARCH") {
		t.Errorf("property guard:\n%s", got)
	}

	ix := &FullTextIndex{db: d, Schema: "dbo", Table: "o'b", KeyIndex: "PK", Catalog: "c", FileGroup: "PRIMARY",
		ChangeTracking: FullTextChangeTrackingManual, StoplistKind: FullTextStoplistUser, Stoplist: "s",
		SearchPropertyList: "p", Columns: []FullTextIndexColumn{
			{Name: "body", LanguageID: 1033, Language: "English"},
			{Name: "doc", TypeColumn: "ext", LanguageID: 0, StatisticalSemantics: true}}}
	if got, want := buildFullTextIndexScript(ix, opts),
		"IF NOT EXISTS (SELECT 1 FROM sys.fulltext_indexes WHERE object_id = OBJECT_ID(N'[dbo].[o''b]'))\n"+
			"CREATE FULLTEXT INDEX ON [dbo].[o'b] ([body] LANGUAGE 1033, [doc] TYPE COLUMN [ext] LANGUAGE 0 STATISTICAL_SEMANTICS)"+
			" KEY INDEX [PK] ON ([c], FILEGROUP [PRIMARY]) WITH (CHANGE_TRACKING = MANUAL, STOPLIST = [s], SEARCH PROPERTY LIST = [p])\nGO\n"+
			"ALTER FULLTEXT INDEX ON [dbo].[o'b] DISABLE\nGO\n"; got != want {
		t.Errorf("index CREATE:\n%s\nwant:\n%s", got, want)
	}
	ix.IsEnabled, ix.StoplistKind, ix.Stoplist = true, FullTextStoplistSystem, ""
	if got := buildFullTextIndexScript(ix, plain); strings.Contains(got, "STOPLIST") || strings.Contains(got, "DISABLE") {
		t.Errorf("system stoplist / enabled index scripted a STOPLIST or DISABLE:\n%s", got)
	}
	ix.StoplistKind = FullTextStoplistOff
	if got := buildFullTextIndexScript(ix, plain); !strings.Contains(got, "STOPLIST = OFF") {
		t.Errorf("stoplist off not scripted:\n%s", got)
	}
	if got, want := buildFullTextIndexScript(ix, drop),
		"IF EXISTS (SELECT 1 FROM sys.fulltext_indexes WHERE object_id = OBJECT_ID(N'[dbo].[o''b]'))\nDROP FULLTEXT INDEX ON [dbo].[o'b]\nGO\n"; got != want {
		t.Errorf("index DROP:\n%s\nwant:\n%s", got, want)
	}
}
