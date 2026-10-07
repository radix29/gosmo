package gosmo

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// ============================================================
// Full-Text Search scripts
// ============================================================
//
// CREATE/DROP scripts for the four full-text families. A stoplist and a
// property list are a CREATE followed by one ALTER … ADD per word or
// property; under IncludeIfNotExists each ADD gets its own guard as well as
// the CREATE — adding a word the stoplist has fails (Msg 30033), so a
// re-run of the script against an existing object would otherwise stop at
// the first one.
//
// None of the families has DROP … IF EXISTS, so the DROP is guarded by a
// catalog-view test instead.

// ScriptFullTextCatalog generates the CREATE (or DROP) script for one
// full-text catalog.
func (sc *Scripter) ScriptFullTextCatalog(ctx context.Context, name string) (string, error) {
	c, err := sc.db.FullTextCatalogByName(ctx, name)
	if err != nil {
		return "", err
	}
	return buildFullTextCatalogScript(c, sc.opts), nil
}

func buildFullTextCatalogScript(c *FullTextCatalog, opts ScriptOptions) string {
	exists := fmt.Sprintf("EXISTS (SELECT 1 FROM sys.fulltext_catalogs WHERE name = N'%s')", escapeSingle(c.Name))
	drop := fmt.Sprintf("IF %s\nDROP FULLTEXT CATALOG %s\nGO\n", exists, quoteIdent(c.Name))
	return opts.envelope(drop, "IF NOT "+exists+"\n", func(sb *strings.Builder) {
		sb.WriteString(createFullTextCatalogStatement(CreateFullTextCatalogRequest{
			Name: c.Name, AccentSensitive: &c.AccentSensitive, IsDefault: c.IsDefault, Owner: c.Owner,
		}))
		sb.WriteString("\nGO\n")
	})
}

// ScriptFullTextStoplist generates the CREATE (or DROP) script for one
// stoplist: the CREATE, then an ADD for each of its words.
func (sc *Scripter) ScriptFullTextStoplist(ctx context.Context, name string) (string, error) {
	l, err := sc.db.FullTextStoplistByName(ctx, name)
	if err != nil {
		return "", err
	}
	var words []FullTextStopword
	if sc.opts.verb() != ScriptDrop {
		if words, err = l.Stopwords(ctx); err != nil {
			return "", err
		}
	}
	return buildFullTextStoplistScript(l, words, sc.opts), nil
}

func buildFullTextStoplistScript(l *FullTextStoplist, words []FullTextStopword, opts ScriptOptions) string {
	exists := fmt.Sprintf("EXISTS (SELECT 1 FROM sys.fulltext_stoplists WHERE name = N'%s')", escapeSingle(l.Name))
	drop := fmt.Sprintf("IF %s\nDROP FULLTEXT STOPLIST %s;\nGO\n", exists, quoteIdent(l.Name))
	return opts.envelope(drop, "IF NOT "+exists+"\n", func(sb *strings.Builder) {
		// Cannot fail: the request names no source.
		create, _ := createFullTextStoplistStatement(CreateFullTextStoplistRequest{Name: l.Name, Owner: l.Owner})
		sb.WriteString(create + "\nGO\n")
		for _, w := range words {
			if opts.IncludeIfNotExists {
				fmt.Fprintf(sb, "IF NOT EXISTS (SELECT 1 FROM sys.fulltext_stopwords w JOIN sys.fulltext_stoplists l ON l.stoplist_id = w.stoplist_id\n"+
					"               WHERE l.name = N'%s' AND w.stopword = N'%s' AND w.language_id = %d)\n",
					escapeSingle(l.Name), escapeSingle(w.Word), w.LanguageID)
			}
			fmt.Fprintf(sb, "ALTER FULLTEXT STOPLIST %s ADD %s LANGUAGE %d;\nGO\n",
				quoteIdent(l.Name), QuoteLiteral(w.Word), w.LanguageID)
		}
	})
}

// ScriptSearchPropertyList generates the CREATE (or DROP) script for one
// search property list: the CREATE, then an ADD for each property.
func (sc *Scripter) ScriptSearchPropertyList(ctx context.Context, name string) (string, error) {
	p, err := sc.db.SearchPropertyListByName(ctx, name)
	if err != nil {
		return "", err
	}
	var props []SearchProperty
	if sc.opts.verb() != ScriptDrop {
		if props, err = p.Properties(ctx); err != nil {
			return "", err
		}
	}
	return buildSearchPropertyListScript(p, props, sc.opts), nil
}

func buildSearchPropertyListScript(p *SearchPropertyList, props []SearchProperty, opts ScriptOptions) string {
	exists := fmt.Sprintf("EXISTS (SELECT 1 FROM sys.registered_search_property_lists WHERE name = N'%s')", escapeSingle(p.Name))
	drop := fmt.Sprintf("IF %s\nDROP SEARCH PROPERTY LIST %s;\nGO\n", exists, quoteIdent(p.Name))
	return opts.envelope(drop, "IF NOT "+exists+"\n", func(sb *strings.Builder) {
		// Cannot fail: the request names no source.
		create, _ := createSearchPropertyListStatement(CreateSearchPropertyListRequest{Name: p.Name, Owner: p.Owner})
		sb.WriteString(create + "\nGO\n")
		for _, prop := range props {
			if opts.IncludeIfNotExists {
				fmt.Fprintf(sb, "IF NOT EXISTS (SELECT 1 FROM sys.registered_search_properties sp JOIN sys.registered_search_property_lists l ON l.property_list_id = sp.property_list_id\n"+
					"               WHERE l.name = N'%s' AND sp.property_name = N'%s')\n",
					escapeSingle(p.Name), escapeSingle(prop.Name))
			}
			sb.WriteString(addSearchPropertyStatement(p.Name, prop) + "\nGO\n")
		}
	})
}

// ScriptFullTextIndex generates the CREATE (or DROP) script for a table's
// full-text index, followed by a DISABLE when the index is disabled.
func (sc *Scripter) ScriptFullTextIndex(ctx context.Context, schema, table string) (string, error) {
	if err := requireSchema("script full-text index on", schema, table); err != nil {
		return "", err
	}
	name := qualifiedName(schema, table)
	idx, err := sc.db.fullTextIndexesWhere(ctx, fmt.Sprintf("read the full-text index of %s", name),
		"WHERE fi.object_id = OBJECT_ID(@p1)", name)
	if err != nil {
		return "", err
	}
	if len(idx) == 0 {
		return "", notFoundf("gosmo: table %s in database %q has no full-text index", name, sc.db.Name)
	}
	return buildFullTextIndexScript(idx[0], sc.opts), nil
}

func buildFullTextIndexScript(i *FullTextIndex, opts ScriptOptions) string {
	exists := fmt.Sprintf("EXISTS (SELECT 1 FROM sys.fulltext_indexes WHERE object_id = OBJECT_ID(N'%s'))", escapeSingle(i.FullName()))
	drop := fmt.Sprintf("IF %s\nDROP FULLTEXT INDEX ON %s\nGO\n", exists, i.FullName())
	return opts.envelope(drop, "IF NOT "+exists+"\n", func(sb *strings.Builder) {
		req := CreateFullTextIndexRequest{
			KeyIndex:           i.KeyIndex,
			Catalog:            i.Catalog,
			FileGroup:          i.FileGroup,
			ChangeTracking:     i.ChangeTracking,
			StoplistOff:        i.StoplistKind == FullTextStoplistOff,
			SearchPropertyList: i.SearchPropertyList,
		}
		if i.StoplistKind == FullTextStoplistUser {
			req.Stoplist = i.Stoplist
		}
		for _, c := range i.Columns {
			req.Columns = append(req.Columns, FullTextIndexColumnSpec{
				Name: c.Name, TypeColumn: c.TypeColumn, Language: strconv.Itoa(c.LanguageID),
				StatisticalSemantics: c.StatisticalSemantics,
			})
		}
		sb.WriteString(createFullTextIndexStatement(i.FullName(), req) + "\nGO\n")
		if !i.IsEnabled {
			fmt.Fprintf(sb, "ALTER FULLTEXT INDEX ON %s DISABLE\nGO\n", i.FullName())
		}
	})
}
