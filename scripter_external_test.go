package gosmo

import (
	"strings"
	"testing"
)

func TestBuildExternalDataSourceScriptOptionsAndGuardedDrop(t *testing.T) {
	s := &ExternalDataSource{Name: "RemoteDB", Location: "sqlserver://srv:1433",
		Type: "RDBMS", Credential: "RemoteCred", DatabaseName: "Sales"}

	got := buildExternalDataSourceScript(s, DefaultScriptOptions())
	if !strings.Contains(got, "CREATE EXTERNAL DATA SOURCE [RemoteDB] WITH (") {
		t.Errorf("data source not scripted:\n%s", got)
	}
	if !strings.Contains(got, "LOCATION = N'sqlserver://srv:1433',") ||
		!strings.Contains(got, "TYPE = RDBMS,") ||
		!strings.Contains(got, "CREDENTIAL = [RemoteCred],") ||
		!strings.Contains(got, "DATABASE_NAME = N'Sales'\n") {
		t.Errorf("options wrong — the last one takes no comma:\n%s", got)
	}
	// The unset options are absent entirely; an option set to an empty
	// literal is not the same as one left unset.
	if strings.Contains(got, "SHARD_MAP_NAME") || strings.Contains(got, "PUSHDOWN") {
		t.Errorf("unset options emitted:\n%s", got)
	}

	opts := DefaultScriptOptions()
	opts.Verb = ScriptDrop
	drop := buildExternalDataSourceScript(s, opts)
	// DROP EXTERNAL DATA SOURCE has no IF EXISTS form on any supported major.
	if !strings.Contains(drop, "IF EXISTS (SELECT 1 FROM sys.external_data_sources WHERE name = N'RemoteDB')") ||
		!strings.Contains(drop, "DROP EXTERNAL DATA SOURCE [RemoteDB];") {
		t.Errorf("drop must be guarded by a catalog lookup:\n%s", drop)
	}
}

func TestBuildExternalDataSourceScriptOmitsATypeKeywordItCannotEmit(t *testing.T) {
	// type_desc reports kinds CREATE EXTERNAL DATA SOURCE has no keyword for;
	// naming one is a parse error, so the type is reported in a comment.
	s := &ExternalDataSource{Name: "Generic", Location: "abfss://c@a.dfs.core.windows.net",
		Type: "EXTERNAL_GENERICS"}
	got := buildExternalDataSourceScript(s, DefaultScriptOptions())
	if strings.Contains(got, "TYPE = EXTERNAL_GENERICS") {
		t.Errorf("unsupported TYPE keyword emitted — the script cannot parse:\n%s", got)
	}
	if !strings.Contains(got, "EXTERNAL_GENERICS") {
		t.Errorf("the reported type must still be stated:\n%s", got)
	}
}

func TestBuildExternalFileFormatScriptNestsFormatOptions(t *testing.T) {
	f := &ExternalFileFormat{Name: "CSV", FormatType: "DELIMITEDTEXT",
		FieldTerminator: ",", StringDelimiter: "\"", UseTypeDefault: true,
		FirstRow: 2, Encoding: "UTF8", DataCompression: "org.apache.hadoop.io.compress.GzipCodec"}

	got := buildExternalFileFormatScript(f, DefaultScriptOptions())
	if !strings.Contains(got, "FORMAT_TYPE = DELIMITEDTEXT,") {
		t.Errorf("format type wrong:\n%s", got)
	}
	if !strings.Contains(got, "FORMAT_OPTIONS (FIELD_TERMINATOR = N',', STRING_DELIMITER = N'\"', USE_TYPE_DEFAULT = TRUE, FIRST_ROW = 2, ENCODING = N'UTF8'),") {
		t.Errorf("format options wrong:\n%s", got)
	}
	if !strings.Contains(got, "DATA_COMPRESSION = N'org.apache.hadoop.io.compress.GzipCodec'\n") {
		t.Errorf("data compression lost:\n%s", got)
	}

	// A Parquet format has none of the delimited-text options, and an empty
	// FORMAT_OPTIONS ( ) does not parse.
	parquet := &ExternalFileFormat{Name: "P", FormatType: "PARQUET"}
	if got := buildExternalFileFormatScript(parquet, DefaultScriptOptions()); strings.Contains(got, "FORMAT_OPTIONS") {
		t.Errorf("empty FORMAT_OPTIONS emitted:\n%s", got)
	}
}

func TestBuildExternalFileFormatScriptReportsTheRowTerminator(t *testing.T) {
	f := &ExternalFileFormat{Name: "CSV", FormatType: "DELIMITEDTEXT", RowTerminator: "\\n"}
	got := buildExternalFileFormatScript(f, DefaultScriptOptions())
	if strings.Contains(got, "ROW_TERMINATOR =") {
		t.Errorf("CREATE EXTERNAL FILE FORMAT has no ROW_TERMINATOR option:\n%s", got)
	}
	if !strings.Contains(got, "Row terminator stored with this format") {
		t.Errorf("the stored row terminator must still be reported:\n%s", got)
	}
}

func TestBuildExternalLibraryScriptElidesTheContent(t *testing.T) {
	l := &ExternalLibrary{Name: "ggplot2", Owner: "dbo", Language: "R", Scope: "PUBLIC"}
	got := buildExternalLibraryScript(l, DefaultScriptOptions())

	if !strings.Contains(got, "CREATE EXTERNAL LIBRARY [ggplot2] AUTHORIZATION [dbo]") {
		t.Errorf("library not scripted with its owner:\n%s", got)
	}
	if !strings.Contains(got, externalLibraryContentPlaceholder) {
		t.Errorf("content placeholder missing:\n%s", got)
	}
	if !strings.Contains(got, "WITH (LANGUAGE = N'R');") {
		t.Errorf("language lost:\n%s", got)
	}
	// Scope is set by ownership and rights, not by this statement.
	if strings.Contains(got, "SCOPE =") {
		t.Errorf("invented SCOPE clause:\n%s", got)
	}

	opts := DefaultScriptOptions()
	opts.Verb = ScriptDrop
	if got := buildExternalLibraryScript(l, opts); !strings.Contains(got,
		"IF EXISTS (SELECT 1 FROM sys.external_libraries WHERE name = N'ggplot2')") {
		t.Errorf("drop must be guarded by a catalog lookup:\n%s", got)
	}
}
