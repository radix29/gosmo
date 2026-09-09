package gosmo

// Database snapshots — the read-only, point-in-time copies SSMS shows in a
// server-level Database Snapshots folder, a sibling of System Databases
// rather than a folder under the source database.
//
// A snapshot is an ordinary row in sys.databases with source_database_id set,
// so it is also returned by Server.Databases. That is deliberate: the catalog
// shows it, and a listing that silently omitted it would disagree with the
// catalog. Database.IsSnapshot is what a caller building a *tree* filters on,
// so the snapshot appears once, under its own folder.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DatabaseSnapshot describes one database snapshot and the database it was
// taken of.
type DatabaseSnapshot struct {
	server *Server

	Name       string
	DatabaseID int

	// SourceDatabase is the name of the database the snapshot was taken of,
	// empty in the rare case where the source has since been dropped — which
	// also makes the snapshot unusable, but it stays in the catalog until it
	// is dropped itself.
	SourceDatabase   string
	SourceDatabaseID int

	// State is the snapshot's own state_desc. A snapshot whose sparse files
	// have run out of disk goes SUSPECT and stays there; there is no repair
	// but dropping it.
	State string

	CreateDate time.Time
}

// DatabaseSnapshot returns a lightweight handle to a snapshot by name,
// without a query. Nothing verifies that it exists and every field but Name
// stays zero — SourceDatabase included, so Restore refuses on it and the
// caller must go through Server.RestoreFromSnapshot with both names.
//
// Like Server.Database, it is the form the name-only operations take — Drop,
// which names the snapshot in the statement and reads nothing else — and the
// only one that works under a WithScript-derived context, where no lookup can
// run at all.
func (s *Server) DatabaseSnapshot(name string) *DatabaseSnapshot {
	return &DatabaseSnapshot{server: s, Name: name}
}

// Server returns the server the snapshot is on.
func (s *DatabaseSnapshot) Server() *Server { return s.server }

// Database returns a lightweight handle for the snapshot database itself, for
// reading its (read-only) contents.
func (s *DatabaseSnapshot) Database() *Database { return s.server.Database(s.Name) }

const databaseSnapshotSelect = `
SELECT d.name, d.database_id,
       ISNULL(DB_NAME(d.source_database_id), ''), d.source_database_id,
       ISNULL(d.state_desc, ''), d.create_date
FROM   sys.databases d
WHERE  d.source_database_id IS NOT NULL`

func scanDatabaseSnapshot(srv *Server, scan func(...any) error) (*DatabaseSnapshot, error) {
	s := &DatabaseSnapshot{server: srv}
	if err := scan(&s.Name, &s.DatabaseID, &s.SourceDatabase,
		&s.SourceDatabaseID, &s.State, &s.CreateDate); err != nil {
		return nil, err
	}
	return s, nil
}

// DatabaseSnapshots returns every database snapshot on the server.
func (s *Server) DatabaseSnapshots() ([]*DatabaseSnapshot, error) {
	return s.DatabaseSnapshotsContext(context.Background())
}

// DatabaseSnapshotsContext is the context-aware variant of DatabaseSnapshots.
func (s *Server) DatabaseSnapshotsContext(ctx context.Context) ([]*DatabaseSnapshot, error) {
	const q = databaseSnapshotSelect + `
ORDER  BY d.name`

	rows, err := s.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list database snapshots: %w", err)
	}
	defer rows.Close()

	var snaps []*DatabaseSnapshot
	for rows.Next() {
		snap, err := scanDatabaseSnapshot(s, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list database snapshots: %w", err)
		}
		snaps = append(snaps, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list database snapshots: %w", err)
	}
	return snaps, nil
}

// DatabaseSnapshotByName returns one snapshot, or a not-found error
// (errors.Is ErrNotFound) when the server has no snapshot by that name. A
// database that exists but is not a snapshot is not found either — the
// predicate is part of what is being asked.
func (s *Server) DatabaseSnapshotByName(name string) (*DatabaseSnapshot, error) {
	return s.DatabaseSnapshotByNameContext(context.Background(), name)
}

// DatabaseSnapshotByNameContext is the context-aware variant of
// DatabaseSnapshotByName.
func (s *Server) DatabaseSnapshotByNameContext(ctx context.Context, name string) (*DatabaseSnapshot, error) {
	var snap *DatabaseSnapshot
	err := s.queryRow(ctx, func(row *sql.Row) error {
		var err error
		snap, err = scanDatabaseSnapshot(s, row.Scan)
		return err
	}, databaseSnapshotSelect+`
   AND d.name = @p1`, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: database snapshot %q not found", name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read database snapshot %q: %w", name, err)
	}
	return snap, nil
}

// SnapshotsOf returns the snapshots taken of one source database.
func (s *Server) SnapshotsOf(database string) ([]*DatabaseSnapshot, error) {
	return s.SnapshotsOfContext(context.Background(), database)
}

// SnapshotsOfContext is the context-aware variant of SnapshotsOf.
func (s *Server) SnapshotsOfContext(ctx context.Context, database string) ([]*DatabaseSnapshot, error) {
	const q = databaseSnapshotSelect + `
   AND d.source_database_id = DB_ID(@p1)
ORDER  BY d.name`

	rows, err := s.query(ctx, q, database)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list snapshots of %q: %w", database, err)
	}
	defer rows.Close()

	var snaps []*DatabaseSnapshot
	for rows.Next() {
		snap, err := scanDatabaseSnapshot(s, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list snapshots of %q: %w", database, err)
		}
		snaps = append(snaps, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list snapshots of %q: %w", database, err)
	}
	return snaps, nil
}

// ============================================================
// Creating a snapshot
// ============================================================

// SnapshotFileSpec is one sparse file of a new snapshot: the logical name of
// a data file in the *source* database, and the path the snapshot's sparse
// file for it goes to.
type SnapshotFileSpec struct {
	// LogicalName is the source file's logical name, which the snapshot must
	// reuse verbatim — CREATE DATABASE … AS SNAPSHOT OF matches its file
	// clauses to the source by name, not by position.
	LogicalName string

	// FileName is the path of the sparse file to create. SQL Server writes
	// it with the service account's rights, so the directory must exist and
	// be writable by the instance.
	FileName string
}

// CreateDatabaseSnapshotRequest describes a snapshot to create.
type CreateDatabaseSnapshotRequest struct {
	// Name is the new snapshot's database name.
	Name string

	// SourceDatabase is the database to snapshot.
	SourceDatabase string

	// Files is one entry per ROWS file in the source. Leave it nil to have
	// the paths defaulted from the source's own files — see
	// SnapshotFileDefaultsContext, which is what the nil case calls.
	Files []SnapshotFileSpec
}

// SnapshotFileDefaults returns one SnapshotFileSpec per data file of the
// source database, with each sparse file placed beside the source file it
// shadows and suffixed with the snapshot name.
func (s *Server) SnapshotFileDefaults(source, snapshotName string) ([]SnapshotFileSpec, error) {
	return s.SnapshotFileDefaultsContext(context.Background(), source, snapshotName)
}

// SnapshotFileDefaultsContext is the context-aware variant of
// SnapshotFileDefaults.
//
// Only ROWS files are returned. A snapshot has no transaction log and no
// FILESTREAM container, and naming either in the CREATE DATABASE is an error
// — "the file … cannot be added to a database snapshot" — which is the usual
// way a hand-built snapshot statement fails.
func (s *Server) SnapshotFileDefaultsContext(ctx context.Context, source, snapshotName string) ([]SnapshotFileSpec, error) {
	files, err := s.DatabaseFilesContext(ctx, source)
	if err != nil {
		return nil, err
	}
	var specs []SnapshotFileSpec
	for _, f := range files {
		if f.Type != "ROWS" {
			continue
		}
		specs = append(specs, SnapshotFileSpec{
			LogicalName: f.Name,
			FileName:    snapshotFilePath(f.PhysicalName, snapshotName),
		})
	}
	if len(specs) == 0 {
		return nil, notFoundf("gosmo: database %q has no data files to snapshot", source)
	}
	return specs, nil
}

// snapshotFilePath derives a sparse-file path from the source file's path:
// the same directory and stem with "_<snapshot>.ss" in place of the
// extension, which is the convention Microsoft's own examples use.
//
// The split is done on both separators regardless of the client's OS: the
// path belongs to the *server's* filesystem, and gosmo runs on Linux against
// Windows instances routinely, so filepath.Dir on the client would mangle a
// Windows path into one string.
func snapshotFilePath(physical, snapshotName string) string {
	dir := ""
	if i := strings.LastIndexAny(physical, `\/`); i >= 0 {
		dir, physical = physical[:i+1], physical[i+1:]
	}
	if i := strings.LastIndex(physical, "."); i > 0 {
		physical = physical[:i]
	}
	return dir + physical + "_" + snapshotName + ".ss"
}

// CreateDatabaseSnapshot creates a database snapshot.
func (s *Server) CreateDatabaseSnapshot(req CreateDatabaseSnapshotRequest) (*DatabaseSnapshot, error) {
	return s.CreateDatabaseSnapshotContext(context.Background(), req)
}

// CreateDatabaseSnapshotContext is the context-aware variant of
// CreateDatabaseSnapshot.
//
// Under a WithScript context it returns a name-only handle rather than
// reading the snapshot back: nothing ran, so there is nothing to read, and
// the by-name lookup would be a real query against a database that does not
// exist.
func (s *Server) CreateDatabaseSnapshotContext(ctx context.Context, req CreateDatabaseSnapshotRequest) (*DatabaseSnapshot, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("gosmo: create database snapshot: name is required")
	}
	if req.SourceDatabase == "" {
		return nil, fmt.Errorf("gosmo: create database snapshot %q: source database is required", req.Name)
	}

	files := req.Files
	if len(files) == 0 {
		var err error
		files, err = s.SnapshotFileDefaultsContext(ctx, req.SourceDatabase, req.Name)
		if err != nil {
			return nil, fmt.Errorf("gosmo: create database snapshot %q: %w", req.Name, err)
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "CREATE DATABASE %s ON", QuoteName(req.Name))
	for i, f := range files {
		if f.LogicalName == "" || f.FileName == "" {
			return nil, fmt.Errorf("gosmo: create database snapshot %q: file %d needs both a logical name and a path", req.Name, i+1)
		}
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "\n    ( NAME = %s, FILENAME = %s )",
			QuoteName(f.LogicalName), QuoteLiteral(f.FileName))
	}
	fmt.Fprintf(&sb, "\nAS SNAPSHOT OF %s", QuoteName(req.SourceDatabase))

	if err := s.execContext(ctx, sb.String()); err != nil {
		return nil, fmt.Errorf("gosmo: create database snapshot %q of %q: %w", req.Name, req.SourceDatabase, err)
	}
	if Scripting(ctx) {
		return &DatabaseSnapshot{server: s, Name: req.Name, SourceDatabase: req.SourceDatabase}, nil
	}
	return s.DatabaseSnapshotByNameContext(ctx, req.Name)
}

// ============================================================
// Reverting and dropping
// ============================================================

// RestoreFromSnapshot reverts a database to one of its snapshots.
func (s *Server) RestoreFromSnapshot(database, snapshot string) error {
	return s.RestoreFromSnapshotContext(context.Background(), database, snapshot)
}

// RestoreFromSnapshotContext is the context-aware variant of
// RestoreFromSnapshot.
//
// The server refuses the revert unless the source has exactly one snapshot —
// every other snapshot of the same database has to be dropped first — and
// unless nobody else is connected to either database. Both are the server's
// checks, reported as its error; gosmo does not pre-empt them, because either
// could change between a check here and the statement.
//
// The snapshot is named as a *string literal*, not an identifier: the
// FROM DATABASE_SNAPSHOT clause takes a name, not a bracketed reference.
func (s *Server) RestoreFromSnapshotContext(ctx context.Context, database, snapshot string) error {
	stmt := fmt.Sprintf("RESTORE DATABASE %s FROM DATABASE_SNAPSHOT = %s",
		QuoteName(database), QuoteLiteral(snapshot))
	if err := s.execContext(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: restore %q from snapshot %q: %w", database, snapshot, err)
	}
	return nil
}

// Restore reverts the snapshot's source database to it.
func (s *DatabaseSnapshot) Restore() error { return s.RestoreContext(context.Background()) }

// RestoreContext is the context-aware variant of Restore.
func (s *DatabaseSnapshot) RestoreContext(ctx context.Context) error {
	if s.SourceDatabase == "" {
		return fmt.Errorf("gosmo: restore from snapshot %q: its source database is gone", s.Name)
	}
	return s.server.RestoreFromSnapshotContext(ctx, s.SourceDatabase, s.Name)
}

// Drop drops the snapshot. Dropping a snapshot deletes its sparse files and
// leaves the source database untouched.
func (s *DatabaseSnapshot) Drop() error { return s.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (s *DatabaseSnapshot) DropContext(ctx context.Context) error {
	if err := s.server.execContext(ctx, "DROP DATABASE "+QuoteName(s.Name)); err != nil {
		return fmt.Errorf("gosmo: drop database snapshot %q: %w", s.Name, err)
	}
	return nil
}
