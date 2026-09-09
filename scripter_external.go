package gosmo

// Scripter — the external resources SSMS shows beside Views: external data
// sources, external file formats and external libraries.
//
// All three are database-scoped, have no ALTER form that restates them, and
// are dropped by a statement with no IF EXISTS clause on any instance gosmo
// supports — so each drop is guarded with a lookup in the catalog view the
// object comes from, the way buildCredentialScript's is.
//
// Several catalog columns here have no place in the statement that creates
// the object: sys.external_file_formats stores a row terminator the CREATE
// takes no option for, and sys.external_libraries stores a scope CREATE
// EXTERNAL LIBRARY does not set. Those are emitted as comments rather than
// as clauses — a script has to run, and a clause invented to carry a value
// back is a parse error at the reader's expense.

import (
	"context"
	"fmt"
	"strings"
)

// ============================================================
// External data sources
// ============================================================

// ScriptExternalDataSource generates the CREATE (or DROP) script for one
// external data source.
func (sc *Scripter) ScriptExternalDataSource(name string) (string, error) {
	return sc.ScriptExternalDataSourceContext(context.Background(), name)
}

// ScriptExternalDataSourceContext is the context-aware variant of
// ScriptExternalDataSource.
func (sc *Scripter) ScriptExternalDataSourceContext(ctx context.Context, name string) (string, error) {
	s, err := sc.db.ExternalDataSourceByNameContext(ctx, name)
	if err != nil {
		return "", err
	}
	return buildExternalDataSourceScript(s, sc.opts), nil
}

// externalDataSourceTypes are the TYPE keywords CREATE EXTERNAL DATA SOURCE
// accepts. type_desc reports others — EXTERNAL_GENERICS on a 2022+ source is
// the common one — and naming those in a TYPE clause is a parse error, so an
// unrecognized kind is left off the statement and stated in a comment
// instead. The clause is optional; a source that omits it defaults by
// location prefix, which is how the 2022 forms are created in the first
// place.
var externalDataSourceTypes = map[string]bool{
	"HADOOP": true, "RDBMS": true, "SHARD_MAP_MANAGER": true, "BLOB_STORAGE": true,
}

// buildExternalDataSourceScript assembles one external data source's script.
func buildExternalDataSourceScript(s *ExternalDataSource, opts ScriptOptions) string {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "IF EXISTS (SELECT 1 FROM sys.external_data_sources WHERE name = N'%s')\n"+
			"    DROP EXTERNAL DATA SOURCE %s;\nGO\n", escapeSingle(s.Name), quoteIdent(s.Name))
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}
	kind := strings.ToUpper(s.Type)
	if kind != "" && !externalDataSourceTypes[kind] {
		fmt.Fprintf(&sb, "/* Reported type: %s — CREATE EXTERNAL DATA SOURCE has no TYPE keyword\n"+
			"   for it, so the type is left to the location prefix. */\n", kind)
	}
	if opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF NOT EXISTS (SELECT 1 FROM sys.external_data_sources WHERE name = N'%s')\n",
			escapeSingle(s.Name))
	}
	fmt.Fprintf(&sb, "CREATE EXTERNAL DATA SOURCE %s WITH (\n", quoteIdent(s.Name))

	opt := newClauseList()
	opt.addLiteral("LOCATION", s.Location)
	if externalDataSourceTypes[kind] {
		opt.addKeyword("TYPE", kind)
	}
	if s.Credential != "" {
		opt.addKeyword("CREDENTIAL", quoteIdent(s.Credential))
	}
	opt.addLiteral("RESOURCE_MANAGER_LOCATION", s.ResourceManagerLocation)
	opt.addLiteral("DATABASE_NAME", s.DatabaseName)
	opt.addLiteral("SHARD_MAP_NAME", s.ShardMapName)
	opt.addLiteral("CONNECTION_OPTIONS", s.ConnectionOptions)
	if s.PushdownEnabled {
		opt.addKeyword("PUSHDOWN", "ON")
	}
	sb.WriteString(opt.render("    "))

	sb.WriteString(");\nGO\n")
	return sb.String()
}

// ============================================================
// External file formats
// ============================================================

// ScriptExternalFileFormat generates the CREATE (or DROP) script for one
// external file format.
func (sc *Scripter) ScriptExternalFileFormat(name string) (string, error) {
	return sc.ScriptExternalFileFormatContext(context.Background(), name)
}

// ScriptExternalFileFormatContext is the context-aware variant of
// ScriptExternalFileFormat.
func (sc *Scripter) ScriptExternalFileFormatContext(ctx context.Context, name string) (string, error) {
	f, err := sc.db.ExternalFileFormatByNameContext(ctx, name)
	if err != nil {
		return "", err
	}
	return buildExternalFileFormatScript(f, sc.opts), nil
}

// buildExternalFileFormatScript assembles one external file format's script.
//
// FORMAT_OPTIONS is emitted only when the format has any: the clause is not
// valid empty, and every option in it belongs to DELIMITEDTEXT — a PARQUET
// or ORC format carries none.
func buildExternalFileFormatScript(f *ExternalFileFormat, opts ScriptOptions) string {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "IF EXISTS (SELECT 1 FROM sys.external_file_formats WHERE name = N'%s')\n"+
			"    DROP EXTERNAL FILE FORMAT %s;\nGO\n", escapeSingle(f.Name), quoteIdent(f.Name))
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}
	if f.RowTerminator != "" {
		// sys.external_file_formats stores it; CREATE EXTERNAL FILE FORMAT
		// takes no option for it, so it is reported rather than emitted.
		fmt.Fprintf(&sb, "/* Row terminator stored with this format: N'%s' — CREATE EXTERNAL FILE\n"+
			"   FORMAT has no option for it. */\n", escapeSingle(f.RowTerminator))
	}
	if opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF NOT EXISTS (SELECT 1 FROM sys.external_file_formats WHERE name = N'%s')\n",
			escapeSingle(f.Name))
	}
	fmt.Fprintf(&sb, "CREATE EXTERNAL FILE FORMAT %s WITH (\n", quoteIdent(f.Name))

	outer := newClauseList()
	outer.addKeyword("FORMAT_TYPE", strings.ToUpper(f.FormatType))

	inner := newClauseList()
	inner.addLiteral("FIELD_TERMINATOR", f.FieldTerminator)
	inner.addLiteral("STRING_DELIMITER", f.StringDelimiter)
	inner.addLiteral("DATE_FORMAT", f.DateFormat)
	if f.UseTypeDefault {
		inner.addKeyword("USE_TYPE_DEFAULT", "TRUE")
	}
	if f.FirstRow > 0 {
		inner.addKeyword("FIRST_ROW", fmt.Sprintf("%d", f.FirstRow))
	}
	inner.addLiteral("ENCODING", f.Encoding)
	inner.addLiteral("PARSER_VERSION", f.ParserVersion)
	if !inner.empty() {
		// FORMAT_OPTIONS takes its list directly, with no "=" before it —
		// the one option here that is not a NAME = value pair.
		outer.addRaw("FORMAT_OPTIONS (" + inner.inline() + ")")
	}

	outer.addLiteral("SERDE_METHOD", f.SerDeMethod)
	outer.addLiteral("DATA_COMPRESSION", f.DataCompression)
	sb.WriteString(outer.render("    "))

	sb.WriteString(");\nGO\n")
	return sb.String()
}

// ============================================================
// External libraries
// ============================================================

// externalLibraryContentPlaceholder stands in for a library's package bytes,
// for the same reason assemblyBinaryPlaceholder stands in for an assembly's:
// the content is a binary package, not text, and gosmo does not read it back
// with the listing.
const externalLibraryContentPlaceholder = "0x00 /* <replace with the package bytes, or a path literal> */"

// ScriptExternalLibrary generates the CREATE (or DROP) script for one
// external library.
func (sc *Scripter) ScriptExternalLibrary(name string) (string, error) {
	return sc.ScriptExternalLibraryContext(context.Background(), name)
}

// ScriptExternalLibraryContext is the context-aware variant of
// ScriptExternalLibrary.
func (sc *Scripter) ScriptExternalLibraryContext(ctx context.Context, name string) (string, error) {
	l, err := sc.db.ExternalLibraryByNameContext(ctx, name)
	if err != nil {
		return "", err
	}
	return buildExternalLibraryScript(l, sc.opts), nil
}

// buildExternalLibraryScript assembles one external library's script.
//
// The scope is reported in a comment rather than emitted: a library is
// PRIVATE or PUBLIC by who owns it and who was granted the package rights,
// and CREATE EXTERNAL LIBRARY has no clause that sets it.
func buildExternalLibraryScript(l *ExternalLibrary, opts ScriptOptions) string {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "IF EXISTS (SELECT 1 FROM sys.external_libraries WHERE name = N'%s')\n"+
			"    DROP EXTERNAL LIBRARY %s;\nGO\n", escapeSingle(l.Name), quoteIdent(l.Name))
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}
	sb.WriteString("/* The library's package content cannot be scripted — it is not text.\n" +
		"   Replace the placeholder below with the package bytes or its path. */\n")
	if l.Scope != "" {
		fmt.Fprintf(&sb, "/* Scope: %s — set by ownership and package rights, not by this statement. */\n",
			strings.ToUpper(l.Scope))
	}
	fmt.Fprintf(&sb, "CREATE EXTERNAL LIBRARY %s", quoteIdent(l.Name))
	if l.Owner != "" {
		fmt.Fprintf(&sb, " AUTHORIZATION %s", quoteIdent(l.Owner))
	}
	fmt.Fprintf(&sb, "\nFROM (CONTENT = %s)", externalLibraryContentPlaceholder)
	if l.Language != "" {
		fmt.Fprintf(&sb, "\nWITH (LANGUAGE = N'%s')", escapeSingle(l.Language))
	}
	sb.WriteString(";\nGO\n")
	return sb.String()
}

// ============================================================
// Clause lists
// ============================================================

// clauseList collects the NAME = value pairs of a WITH clause, so that the
// comma separators fall between the options that are actually present. The
// three CREATE EXTERNAL statements each have a different subset set on any
// given object — a delimited-text format has half of FORMAT_OPTIONS and a
// Parquet one none — and hand-placed commas are where a trailing one gets
// left behind on the last omitted option.
type clauseList struct{ parts []string }

func newClauseList() *clauseList { return &clauseList{} }

// addLiteral appends NAME = N'value', skipping the option entirely when the
// value is empty — every string field here is ISNULL'd to ” at the catalog,
// and an option set to an empty literal is not the same as one left unset.
func (c *clauseList) addLiteral(name, value string) {
	if value == "" {
		return
	}
	c.parts = append(c.parts, fmt.Sprintf("%s = N'%s'", name, escapeSingle(value)))
}

// addKeyword appends NAME = value with the value emitted as written — a
// keyword, an identifier, or a number, none of which are quoted as strings.
func (c *clauseList) addKeyword(name, value string) {
	c.parts = append(c.parts, name+" = "+value)
}

// addRaw appends an option that is not a NAME = value pair at all.
func (c *clauseList) addRaw(text string) { c.parts = append(c.parts, text) }

func (c *clauseList) empty() bool { return len(c.parts) == 0 }

// render lays the options out one per line at the given indent, comma
// separated, with a trailing newline.
func (c *clauseList) render(indent string) string {
	if c.empty() {
		return ""
	}
	return indent + strings.Join(c.parts, ",\n"+indent) + "\n"
}

// inline lays the options out on one line, for a nested option list.
func (c *clauseList) inline() string { return strings.Join(c.parts, ", ") }
