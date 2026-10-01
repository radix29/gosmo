package gosmo

import (
	"context"
	"fmt"
	"strings"
)

// ============================================================
// Selective XML indexes — their promoted paths and namespaces
// ============================================================

// SelectiveXMLPath is one promoted path of a selective XML index — a row of
// sys.selective_xml_index_paths, and one item of CREATE SELECTIVE XML
// INDEX's FOR (…).
//
// A path is mapped either AS XQUERY or AS SQL. An XQUERY path carries the
// XSD type, MAXLENGTH and SINGLETON it was declared with — each zero when
// the declaration left it out, including when the server inferred it from
// the column's XML schema collection, which refuses an explicit XSD type
// (Msg 6368) — or the 'node()' hint. An SQL path carries its SQL type.
type SelectiveXMLPath struct {
	// Name is the path's name, which a secondary selective XML index's
	// FOR (…) refers to it by.
	Name string
	// Path is the XPath expression, as declared.
	Path string
	// IsSQL marks the AS SQL form, whose type is SQLType ("nvarchar(30)").
	IsSQL   bool
	SQLType string
	// XQueryType is the declared XSD type ("xs:string"), "" for none.
	XQueryType string
	// MaxLength is an XQUERY path's declared MAXLENGTH, 0 for none.
	MaxLength int
	// IsNode is the 'node()' hint: the path's existence alone is indexed.
	// It takes SINGLETON too, but no type or MAXLENGTH.
	IsNode      bool
	IsSingleton bool
}

// XMLNamespace is one entry of a WITH XMLNAMESPACES clause: a URI and the
// prefix it binds, or the default namespace when Prefix is "".
type XMLNamespace struct {
	Prefix string
	URI    string
}

// attachSelectiveXML reads the paths and namespaces of every selective XML
// index in indexes, two queries for all of them — and none when there is
// no such index, which is nearly always.
func (t *Table) attachSelectiveXML(ctx context.Context, indexes []*Index) error {
	byID := make(map[int]*Index)
	for _, idx := range indexes {
		if idx.IsSelectiveXML {
			byID[idx.IndexID] = idx
		}
	}
	if len(byID) == 0 {
		return nil
	}

	what := fmt.Sprintf("selective xml index paths on %s", t.FullName())
	rows, err := t.db.query(ctx, `
SELECT p.index_id, p.name, p.path, CAST(CASE WHEN p.path_type = 1 THEN 1 ELSE 0 END AS bit),
       ISNULL(TYPE_NAME(p.user_type_id), ''), ISNULL(p.max_length, 0),
       ISNULL(p.precision, 0), ISNULL(p.scale, 0),
       CASE WHEN p.is_xquery_type_inferred = 0 THEN ISNULL(p.xquery_type_description, '') ELSE '' END,
       CASE WHEN p.is_xquery_max_length_inferred = 0 AND p.xquery_max_length > 0
            THEN CAST(p.xquery_max_length AS int) ELSE 0 END,
       p.is_node, p.is_singleton
FROM   sys.selective_xml_index_paths p
WHERE  p.object_id = @p1
ORDER  BY p.index_id, p.path_id`, t.ObjectID)
	type pathRow struct {
		indexID int
		path    SelectiveXMLPath
	}
	paths, err := scanRows(rows, err, what, func(scan func(...any) error) (pathRow, error) {
		var r pathRow
		var sqlType string
		var maxLength, precision, scale int
		if err := scan(&r.indexID, &r.path.Name, &r.path.Path, &r.path.IsSQL,
			&sqlType, &maxLength, &precision, &scale,
			&r.path.XQueryType, &r.path.MaxLength, &r.path.IsNode, &r.path.IsSingleton); err != nil {
			return r, err
		}
		if r.path.IsSQL {
			r.path.SQLType = TypeString(DataType(sqlType), maxLength, precision, scale)
		}
		return r, nil
	})
	if err != nil {
		return err
	}
	for _, r := range paths {
		if idx := byID[r.indexID]; idx != nil {
			idx.SelectiveXMLPaths = append(idx.SelectiveXMLPaths, r.path)
		}
	}

	what = fmt.Sprintf("selective xml index namespaces on %s", t.FullName())
	rows, err = t.db.query(ctx, `
SELECT n.index_id, CASE WHEN n.is_default_uri = 1 THEN '' ELSE ISNULL(n.prefix, '') END, n.uri
FROM   sys.selective_xml_index_namespaces n
WHERE  n.object_id = @p1
ORDER  BY n.index_id, n.is_default_uri DESC, n.prefix`, t.ObjectID)
	type nsRow struct {
		indexID int
		ns      XMLNamespace
	}
	namespaces, err := scanRows(rows, err, what, func(scan func(...any) error) (nsRow, error) {
		var r nsRow
		return r, scan(&r.indexID, &r.ns.Prefix, &r.ns.URI)
	})
	if err != nil {
		return err
	}
	for _, r := range namespaces {
		if idx := byID[r.indexID]; idx != nil {
			idx.SelectiveXMLNamespaces = append(idx.SelectiveXMLNamespaces, r.ns)
		}
	}
	return nil
}

// selectiveXMLIndexCreate renders a selective XML index's CREATE, or a
// secondary selective one's, up to the statement terminator.
//
// The path and URI literals are N'…': a path names elements, and an
// element name outside the code page would otherwise come back as '?'.
// Neither form takes an ON clause — like every XML index, it lives with the
// table — and the WITH list is the one an XML index takes.
func selectiveXMLIndexCreate(idx *Index, tableName string) string {
	var sb strings.Builder
	col := ""
	if len(idx.KeyColumns) > 0 {
		col = idx.KeyColumns[0].ref()
	}
	if idx.IsSelectiveXML {
		fmt.Fprintf(&sb, "CREATE SELECTIVE XML INDEX %s\n    ON %s (%s)", quoteIdent(idx.Name), tableName, col)
		if len(idx.SelectiveXMLNamespaces) > 0 {
			ns := make([]string, len(idx.SelectiveXMLNamespaces))
			for i, n := range idx.SelectiveXMLNamespaces {
				if n.Prefix == "" {
					ns[i] = "DEFAULT " + QuoteLiteral(n.URI)
				} else {
					ns[i] = QuoteLiteral(n.URI) + " AS " + quoteIdent(n.Prefix)
				}
			}
			fmt.Fprintf(&sb, "\n    WITH XMLNAMESPACES (%s)", strings.Join(ns, ", "))
		}
		paths := make([]string, len(idx.SelectiveXMLPaths))
		for i, p := range idx.SelectiveXMLPaths {
			paths[i] = p.clause()
		}
		fmt.Fprintf(&sb, "\n    FOR (\n        %s\n    )", strings.Join(paths, ",\n        "))
	} else {
		fmt.Fprintf(&sb, "CREATE XML INDEX %s\n    ON %s (%s)\n    USING XML INDEX %s FOR (%s)",
			quoteIdent(idx.Name), tableName, col, quoteIdent(idx.PrimaryXMLIndex), quoteIdent(idx.SelectiveXMLPath))
	}
	sb.WriteString(xmlIndexWithClause(idx))
	return sb.String()
}

// clause renders the path as one FOR (…) item. An XQUERY path with nothing
// declared takes no AS clause at all.
func (p SelectiveXMLPath) clause() string {
	s := quoteIdent(p.Name) + " = " + QuoteLiteral(p.Path)
	if p.IsSQL {
		s += " AS SQL " + p.SQLType
		if p.IsSingleton {
			s += " SINGLETON"
		}
		return s
	}
	// SINGLETON goes with the 'node()' hint as with a type: the server
	// insists paths sharing an expression all have it or all lack it
	// (Msg 9538), so a hint scripted without it does not replay.
	var hint []string
	switch {
	case p.IsNode:
		hint = append(hint, "'node()'")
	case p.XQueryType != "":
		hint = append(hint, "'"+escapeSingle(p.XQueryType)+"'")
	}
	if p.MaxLength > 0 {
		hint = append(hint, fmt.Sprintf("MAXLENGTH(%d)", p.MaxLength))
	}
	if p.IsSingleton {
		hint = append(hint, "SINGLETON")
	}
	if len(hint) > 0 {
		s += " AS XQUERY " + strings.Join(hint, " ")
	}
	return s
}
