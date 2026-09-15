package gosmo

import (
	"context"
	"database/sql"
	"fmt"
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
func (s *Server) LinkedServers() ([]*LinkedServer, error) {
	return s.LinkedServersContext(context.Background())
}

// LinkedServersContext is the context-aware variant of LinkedServers.
func (s *Server) LinkedServersContext(ctx context.Context) ([]*LinkedServer, error) {
	const q = `
	SELECT name, product, provider, data_source, is_remote_login_enabled
	FROM sys.servers
	WHERE is_linked = 1
	ORDER BY name`

	rows, err := s.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list linked servers: %w", err)
	}
	defer rows.Close()

	var ls []*LinkedServer
	for rows.Next() {
		l := &LinkedServer{}
		var ds sql.NullString
		if err := rows.Scan(&l.Name, &l.Product, &l.Provider, &ds, &l.IsRemote); err != nil {
			return nil, fmt.Errorf("gosmo: list linked servers: %w", err)
		}
		l.DataSource = ds.String
		ls = append(ls, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list linked servers: %w", err)
	}
	return ls, nil
}
