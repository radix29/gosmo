package gosmo

import (
	"context"
	"database/sql"
)

// -- Linked servers ------------------------------------------------------------

// LinkedServer represents a linked server definition.
type LinkedServer struct {
	Name       string
	Product    string
	Provider   string
	DataSource string
	IsRemote   bool
}

// LinkedServers returns all linked servers defined on this instance.
func (s *Server) LinkedServers(ctx context.Context) ([]*LinkedServer, error) {
	const q = `
	SELECT name, product, provider, data_source, is_remote_login_enabled
	FROM sys.servers
	WHERE is_linked = 1
	ORDER BY name`

	rows, err := s.query(ctx, q)
	return scanRows(rows, err, "list linked servers", func(scan func(...any) error) (*LinkedServer, error) {
		l := &LinkedServer{}
		var ds sql.NullString
		if err := scan(&l.Name, &l.Product, &l.Provider, &ds, &l.IsRemote); err != nil {
			return nil, err
		}
		l.DataSource = ds.String
		return l, nil
	})
}
