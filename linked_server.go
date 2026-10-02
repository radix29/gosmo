package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// -- Linked servers ------------------------------------------------------------

// LinkedServer represents a linked server definition.
type LinkedServer struct {
	Name       string
	Product    string
	Provider   string
	DataSource string
	IsRemote   bool
	// DataAccess is is_data_access_enabled: whether distributed queries
	// (four-part names, OPENQUERY) may use the server. Off, every read through
	// it fails Msg 7411.
	DataAccess bool
}

// LinkedServers returns all linked servers defined on this instance.
func (s *Server) LinkedServers(ctx context.Context) ([]*LinkedServer, error) {
	const q = `
	SELECT name, product, provider, data_source, is_remote_login_enabled, is_data_access_enabled
	FROM sys.servers
	WHERE is_linked = 1
	ORDER BY name`

	rows, err := s.query(ctx, q)
	return scanRows(rows, err, "list linked servers", func(scan func(...any) error) (*LinkedServer, error) {
		l := &LinkedServer{}
		var ds sql.NullString
		if err := scan(&l.Name, &l.Product, &l.Provider, &ds, &l.IsRemote, &l.DataAccess); err != nil {
			return nil, err
		}
		l.DataSource = ds.String
		return l, nil
	})
}

// -- Reads through a linked server ---------------------------------------------
//
// Both run the remote's own catalog views through OPENQUERY, so the whole
// query executes on the remote as the login the linked server maps this one
// to, and what they return is what a query through the linked server would
// see. They need only data access, not RPC Out. The remote must be SQL Server
// (any version): another product has no sys views and the read fails.
//
// sp_catalogs/sp_tables_ex/sp_columns_ex were rejected for the catalog: they
// go through the provider's schema rowsets, which report decimal as numeric
// and nchar as nvarchar and drop CLR-typed columns (geography, hierarchyid).

// LinkedServerDatabases returns the names of the ONLINE databases on linked
// server linked that the mapped remote login can open (HAS_DBACCESS, evaluated
// on the remote), sorted by name.
func (s *Server) LinkedServerDatabases(ctx context.Context, linked string) ([]string, error) {
	const remote = `SELECT name FROM sys.databases WHERE state_desc = 'ONLINE' AND HAS_DBACCESS(name) = 1`
	rows, err := s.query(ctx, openQuery(linked, remote))
	names, err := scanRows(rows, err, fmt.Sprintf("list databases on linked server %q", linked), func(scan func(...any) error) (string, error) {
		var n string
		return n, scan(&n)
	})
	slices.Sort(names)
	return names, err
}

// LinkedServerCatalog returns Database.Catalog's snapshot of database on
// linked server linked: every user table and view with its columns, sorted
// by schema then name. Functions is always empty — a four-part name cannot
// call a table-valued function.
func (s *Server) LinkedServerCatalog(ctx context.Context, linked, database string) (*Catalog, error) {
	rows, err := s.query(ctx, openQuery(linked, linkedCatalogQuery(database)))
	what := fmt.Sprintf("load catalog for %q on linked server %q", database, linked)
	if err != nil {
		return nil, fmt.Errorf("gosmo: %s: %w", what, err)
	}
	defer rows.Close()

	cat := &Catalog{}
	var cur *CatalogObject
	for rows.Next() {
		var objectID int
		var schema, name, typeCode string
		var col CatalogColumn
		if err := rows.Scan(&objectID, &schema, &name, &typeCode, &col.Name, &col.DataType,
			&col.MaxLength, &col.Precision, &col.Scale, &col.IsNullable); err != nil {
			return nil, fmt.Errorf("gosmo: %s: %w", what, err)
		}
		if cur == nil || cur.ObjectID != objectID {
			cat.Objects = append(cat.Objects, CatalogObject{
				ObjectID: objectID, Schema: schema, Name: name,
				Type: catalogObjectType(strings.TrimSpace(typeCode)),
			})
			cur = &cat.Objects[len(cat.Objects)-1]
			if !slices.Contains(cat.Schemas, schema) {
				cat.Schemas = append(cat.Schemas, schema)
			}
		}
		cur.Columns = append(cur.Columns, col)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: %s: %w", what, err)
	}
	slices.Sort(cat.Schemas)
	return cat, nil
}

// linkedCatalogQuery is the remote half of LinkedServerCatalog: objects and
// columns in one result, one row per column, grouped by object. Schema and
// type names are joined from database's own sys.schemas and sys.types —
// SCHEMA_NAME() and TYPE_NAME() would answer for the remote session's current
// database, not this one. Each column gets its own alias because OPENQUERY
// refuses a result with duplicate column names.
func linkedCatalogQuery(database string) string {
	db := quoteIdent(database)
	return `SELECT o.object_id, s.name AS schema_name, o.name AS object_name, o.type,
       c.name AS column_name, tp.name AS type_name,
       c.max_length, c.precision, c.scale, c.is_nullable
FROM   ` + db + `.sys.objects o
JOIN   ` + db + `.sys.schemas s ON s.schema_id = o.schema_id
JOIN   ` + db + `.sys.columns c ON c.object_id = o.object_id
JOIN   ` + db + `.sys.types tp ON tp.user_type_id = c.user_type_id
WHERE  o.type IN ('U','V') AND o.is_ms_shipped = 0
ORDER  BY s.name, o.name, o.object_id, c.column_id`
}

// openQuery wraps remote, a query for linked server linked, in OPENQUERY.
// OPENQUERY takes only a literal — no variable or parameter — so the remote
// text is embedded as one, and anything it names must already be quoted
// (quoteIdent) before this quotes the whole of it again.
func openQuery(linked, remote string) string {
	return "SELECT * FROM OPENQUERY(" + quoteIdent(linked) + ", " + QuoteLiteral(remote) + ")"
}
