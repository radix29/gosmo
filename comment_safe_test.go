package gosmo

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/radix29/gosmo/tsql"
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
		"database scoped configuration": func(n string) string {
			sc := scripterOverDatabase("d")
			s, err := sc.scriptDatabaseFrom(sc.db, &databaseLayout{major: 17,
				scoped: []*DatabaseScopedConfig{{Name: n, Value: "x y"}}})
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
				qualifiedName(n, n), DataSpace{}, DefaultScriptOptions())
		},
		"hypothetical index": func(n string) string {
			return scriptIndex(&Index{Name: n, Type: IndexTypeNonClustered, IsHypothetical: true,
				KeyColumns: []IndexColumn{{Name: "c"}}}, qualifiedName(n, n), DataSpace{}, DefaultScriptOptions())
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
			return float16Note([]string{tsql.CommentSafe(n)}) // as float16Parameters scans it
		},
		"extended property with no literal form": func(n string) string {
			return extendedPropertyStatement(scriptedExtProp{name: n, baseType: n, value: sql.NullString{String: "v", Valid: true},
				level: ExtendedPropertyLevel{Level0Name: n, Level1Type: "TABLE", Level1Name: n, Level2Type: "COLUMN", Level2Name: n}}, false)
		},
		"orphaned user note": func(n string) string {
			return buildUserScript(&User{Name: n, AuthType: "INSTANCE"}, DefaultScriptOptions())
		},
		"database security header": func(n string) string {
			return buildDatabaseSecurityScript(n, &databaseSecurity{})
		},
		"database security unscriptable permission": func(n string) string {
			return buildDatabaseSecurityScript("d", &databaseSecurity{grants: []securityGrant{
				{grantee: n, state: "G", permission: n, class: 99, classDesc: n}}})
		},
	}
	for site, script := range sites {
		for _, n := range hostileNames {
			s := script(n)
			if strings.Contains(n, "EVIL") && !strings.Contains(strings.ToUpper(s), "EVIL") {
				t.Fatalf("%s: the name never reached the script:\n%s", site, s)
			}
			full := s + "\nSURVIVE"
			if c := codeOnly(full); strings.Contains(strings.ToUpper(c), "EVIL") || !strings.Contains(c, "SURVIVE") {
				t.Errorf("%s, name %q: the comment does not contain it; code outside comments and quotes:\n%s\nscript:\n%s",
					site, n, c, s)
			}
		}
	}
}
