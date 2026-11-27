package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"sync"
)

type databasePoolCache struct {
	mu     sync.Mutex
	pools  map[string]*sql.DB
	closed bool
}

// Azure SQL Database and Fabric SQL Database cannot enter another database
// through USE or cross-database RPC. Keep the Server's identity and lifetime,
// but acquire database-scoped work from a pool that logs in there directly.
func (s *Server) databasePool(name string) (*sql.DB, error) {
	if s.info == nil || !s.info.IsDatabaseScoped() || s.login == nil {
		return s.db, nil
	}
	s.databases.mu.Lock()
	defer s.databases.mu.Unlock()
	if s.databases.closed {
		return nil, context.Canceled
	}

	s.login.mu.Lock()
	opts, build := s.login.opts, s.login.build
	s.login.mu.Unlock()
	if SameName(s.info.Collation, opts.Database, name) {
		return s.db, nil
	}
	key := NameKey(s.info.Collation, name)
	if pool := s.databases.pools[key]; pool != nil {
		return pool, nil
	}
	opts.Database = name
	connector, err := build(opts)
	if err != nil {
		return nil, err
	}
	pool := sql.OpenDB(connector)
	configurePool(pool, opts)
	if s.databases.pools == nil {
		s.databases.pools = make(map[string]*sql.DB)
	}
	s.databases.pools[key] = pool
	return pool, nil
}

func configurePool(pool *sql.DB, opts ConnectionOptions) {
	if opts.MaxOpenConns > 0 {
		pool.SetMaxOpenConns(opts.MaxOpenConns)
	}
	pool.SetMaxIdleConns(opts.MaxIdleConns)
	if opts.ConnMaxLifetime > 0 {
		pool.SetConnMaxLifetime(opts.ConnMaxLifetime)
	}
	pool.SetConnMaxIdleTime(opts.ConnMaxIdleTime)
}

func (s *Server) closeDatabasePools() error {
	s.databases.mu.Lock()
	s.databases.closed = true
	pools := s.databases.pools
	s.databases.pools = nil
	s.databases.mu.Unlock()
	var err error
	for _, pool := range pools {
		err = errors.Join(err, pool.Close())
	}
	return err
}
