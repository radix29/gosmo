package gosmo

import (
	"strings"
	"testing"
)

// hostileNames each try to leave the comment a script puts them in: a line
// break ends a -- comment, "*/" ends a /* */ one early, and "/*" opens a
// nested one that swallows the rest of the script. EVIL marks the text that
// would then run as T-SQL.
var hostileNames = []string{
	"a*/EVIL/*b",
	"a*/*/EVIL",
	"a/*b",
	"a/*/EVIL",
	"a\nEVIL",
	"a\r\nEVIL --x",
}

// TestScriptCommentsContainHostileNames scripts objects with hostile names
// through every site that writes a name or a stored value into a comment, and
// checks the script still lexes as its author meant: no EVIL outside a
// comment, literal or quoted identifier, and nothing left open at the end —
// SURVIVE, appended after the script, must still be code.
func TestScriptCommentsContainHostileNames(t *testing.T) {
	headers := DefaultScriptOptions()
	headers.IncludeHeaders = true
	col := []*Column{{Name: "id", DataType: DataTypeInt}}
	sites := map[string]func(n string) string{
		"table header": func(n string) string {
			return buildTableScript(n, n, n, tableScriptParts{cols: col}, headers)
		},
		"FileTable header": func(n string) string {
			return buildFileTableScript(n, n, n, tableScriptParts{
				table: tableScriptOptions{IsFileTable: true, FileTableDirectory: "d"}}, headers)
		},
		"external table header": func(n string) string {
			return buildExternalTableScript(n, n, n, tableScriptParts{cols: col, table: tableScriptOptions{
				IsExternal: true, External: externalTableOptions{DataSource: "ds", FileFormat: "ff", Location: "x"}}}, nil, headers)
		},
		"database header": func(n string) string {
			sc := scripterOverDatabase(n)
			sc.opts.IncludeHeaders = true
			sc.db.server.info = &ServerInfo{ProductVersion: n}
			s, err := sc.scriptDatabaseFrom(sc.db, nil)
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
		"encrypted bound object": func(n string) string {
			return buildBoundObjectScript("RULE", n, n, "", DefaultScriptOptions())
		},
		"unscriptable index": func(n string) string {
			return scriptIndex(&Index{Name: n, Type: IndexTypeXML, KeyColumns: []IndexColumn{{Name: "doc"}}},
				qualifiedName(n, n), DefaultScriptOptions())
		},
		"built-in resource pool": func(n string) string {
			var sb strings.Builder
			systemRGScript(&sb, "ALTER RESOURCE POOL", "resource pool", n, &rgOptions{}, "")
			return sb.String()
		},
		"multi-server collector": func(n string) string {
			c := &ScriptCollector{entries: []ScriptEntry{{Server: n, SQL: "S1"}, {Server: "other", SQL: "S2"}}}
			return c.String()
		},
		"UPDATE with no updatable columns": func(n string) string {
			return buildUpdateScript(n, n, nil)
		},
		"external data source type": func(n string) string {
			return buildExternalDataSourceScript(&ExternalDataSource{Name: "ds", Type: n, Location: "x"}, DefaultScriptOptions())
		},
		"external file format row terminator": func(n string) string {
			return buildExternalFileFormatScript(&ExternalFileFormat{Name: "ff", FormatType: "DELIMITEDTEXT", RowTerminator: n},
				DefaultScriptOptions())
		},
		"external library scope": func(n string) string {
			return buildExternalLibraryScript(&ExternalLibrary{Name: "l", Scope: n}, DefaultScriptOptions())
		},
		"float16 parameter note": func(n string) string {
			return float16Note([]string{commentSafe(n)}) // as float16Parameters scans it
		},
	}
	for site, script := range sites {
		for _, n := range hostileNames {
			s := script(n)
			if strings.Contains(n, "EVIL") && !strings.Contains(strings.ToUpper(s), "EVIL") {
				t.Fatalf("%s: the name never reached the script:\n%s", site, s)
			}
			var code strings.Builder
			full := s + "\nSURVIVE"
			for _, sp := range scriptCodeSpans(full) {
				code.WriteString(full[sp.start:sp.end])
				code.WriteString(" ")
			}
			if c := code.String(); strings.Contains(strings.ToUpper(c), "EVIL") || !strings.Contains(c, "SURVIVE") {
				t.Errorf("%s, name %q: the comment does not contain it; code outside comments and quotes:\n%s\nscript:\n%s",
					site, n, c, s)
			}
		}
	}
}

func TestBlockCommentSafe(t *testing.T) {
	for _, s := range []string{"*/", "/*", "*/*", "/*/", "**//", "a*/*/b", "/**/", "x"} {
		got := blockCommentSafe(s)
		if strings.Contains(got, "*/") || strings.Contains(got, "/*") {
			t.Errorf("blockCommentSafe(%q) = %q, still holds a delimiter", s, got)
		}
		if strings.ReplaceAll(got, " ", "") != strings.ReplaceAll(s, " ", "") {
			t.Errorf("blockCommentSafe(%q) = %q, changed more than spacing", s, got)
		}
	}
}
