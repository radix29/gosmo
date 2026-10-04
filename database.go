package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

// Database mirrors Microsoft.SqlServer.Management.Smo.Database.
//
// Every field here is populated from one sys.databases row, so a Database
// from Server.DatabaseRef carries only Name and leaves the rest at its zero
// value — see Server.DatabaseRef. IsSystem and IsSnapshot are derived from
// ID and SourceDatabaseID rather than stored, so they answer false on such a
// handle, master included.
type Database struct {
	server *Server

	Name               string
	ID                 int
	State              string // state_desc: ONLINE, OFFLINE, RESTORING, ...
	RecoveryModel      RecoveryModel
	CompatibilityLevel CompatibilityLevel
	Collation          string
	IsReadOnly         bool
	CreateDate         time.Time

	// CatalogCollation is the collation the database compares its object
	// names under — Collation, except in a partially contained database
	// (always Latin1_General_100_CI_AS_KS_WS_SC, case-insensitive) and an
	// Azure SQL Database created WITH CATALOG_COLLATION. A caller deciding
	// whether two names inside the database are the same wants this, not
	// Collation: on a contained _CS_ database "dbo.ORDERS" resolves to
	// dbo.Orders. Empty on a DatabaseRef handle, like Collation.
	CatalogCollation string

	// SourceDatabaseID is sys.databases.source_database_id: the database a
	// snapshot was taken of, and 0 on every database that is not one. It
	// backs IsSnapshot, which is how a caller building a tree keeps a
	// snapshot out of the user-database list — Server.Databases returns it
	// like any other row, because the catalog does.
	SourceDatabaseID int
}

// systemDatabaseMaxID is the highest database_id SQL Server permanently
// reserves for its own system databases: master=1, tempdb=2, model=3,
// msdb=4. Every user database gets an id above this range.
const systemDatabaseMaxID = 4

// IsSystem reports whether this is one of SQL Server's four built-in
// system databases (master, tempdb, model, msdb), identified by their
// permanently reserved database_id (1-4) rather than by name. It is derived
// from ID, so it answers false on a Server.DatabaseRef handle, master
// included.
func (d *Database) IsSystem() bool { return d.ID > 0 && d.ID <= systemDatabaseMaxID }

// IsSnapshot reports whether this database is a database snapshot. Like
// IsSystem it is derived — from SourceDatabaseID — so it answers false on a
// Server.DatabaseRef handle.
func (d *Database) IsSnapshot() bool { return d.SourceDatabaseID != 0 }

// Server returns the parent Server. It is a back-pointer, not catalog state,
// so it stays a method — as Table.Database does.
func (d *Database) Server() *Server { return d.server }

// -- Connection helpers --------------------------------------------------------
// These acquire a dedicated connection from the pool, switch to the correct
// database via USE, run the statement, then return the connection to the pool.
// This is safe under connection pooling because we hold the *sql.Conn for the
// entire duration of the call.

// withConn acquires a connection and switches it to d's database (USE) —
// both idempotent and safe to retry against a fresh connection on a
// transient failure (a dropped pooled connection, etc.), same as
// query/queryRow's own acquire step below — before handing it to fn, which
// is not retried: fn is the caller's actual write, and blindly re-running
// it on a fresh connection after a partial failure could re-apply side
// effects that already took hold.
//
// Inside InTransaction it is the transaction's session, switched the same way,
// and nothing is retried: a fresh connection is outside the transaction.
func (d *Database) withConn(ctx context.Context, fn func(context.Context, sqlConn) error) error {
	ctx, release := d.server.bound(ctx)
	defer release()
	if t := txFrom(ctx, d.server); t != nil {
		if !t.in(d.Name) {
			if err := d.use(ctx, t.tx); err != nil {
				t.lost()
				return err
			}
			t.at(d.Name)
		}
		err := withAllMessages(fn(ctx, t.tx))
		if err != nil {
			t.lost()
		}
		return err
	}
	conn, err := withRetry(ctx, func() (*sql.Conn, error) {
		conn, err := d.server.db.Conn(ctx)
		if err != nil {
			return nil, fmt.Errorf("gosmo: acquire connection: %w", err)
		}
		if err := d.use(ctx, conn); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	})
	if err != nil {
		return err
	}
	defer conn.Close()
	return withAllMessages(fn(ctx, conn))
}

// scriptResult is the sql.Result stand-in returned to callers of exec when
// a ScriptCollector is capturing statements instead of running them — its
// value is never inspected by exec's own callers, which either discard the
// result or (as with the extended-property writers) issue a single
// unambiguous statement per call rather than branching on RowsAffected.
type scriptResult struct{}

func (scriptResult) LastInsertId() (int64, error) { return 0, nil }
func (scriptResult) RowsAffected() (int64, error) { return 0, nil }

func (d *Database) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return d.execShown(ctx, q, q, args...)
}

// execSecret is Server.execSecret for a database-scoped statement: stmt runs,
// shown is what an observer sees and a WithScript collector captures.
func (d *Database) execSecret(ctx context.Context, stmt, shown string) (sql.Result, error) {
	return d.execShown(ctx, stmt, shown)
}

// execPasswords is execSecret for a statement whose secrets are all
// passwords.
func (d *Database) execPasswords(ctx context.Context, stmt string, passwords ...string) (sql.Result, error) {
	return d.execSecret(ctx, stmt, redactSecrets(stmt, PasswordPlaceholder, passwords...))
}

func (d *Database) execShown(ctx context.Context, q, shown string, args ...any) (sql.Result, error) {
	if c, ok := scriptFrom(ctx); ok {
		// Parameters are substituted into the text, not dropped: a captured
		// statement is run by hand in a query editor, where nothing binds
		// @p1 — see bindScriptArgs.
		bound, err := bindScriptArgs(capturedForm(ctx, q, shown), args)
		if err != nil {
			return nil, err
		}
		// The real path below always runs q after a USE — captured
		// statements need that made explicit, since the script may be
		// handed to a session scoped to a different database (or none).
		// The entry records the database rather than prefixing a USE:
		// ScriptCollector renders it, with the GO a statement that must
		// open its batch needs after it.
		c.append(ScriptEntry{Server: scriptServerName(ctx, d.server), Database: d.Name, SQL: bound})
		return scriptResult{}, nil
	}
	var res sql.Result
	err := d.withConn(ctx, func(ctx context.Context, c sqlConn) error {
		var e error
		res, e = c.ExecContext(ctx, q, args...)
		return e
	})
	if err == nil && observerFrom(ctx) != nil {
		sent, berr := bindScriptArgs(shown, args)
		if berr != nil {
			sent = shown
		}
		observe(ctx, d.server, ScriptEntry{Server: scriptServerName(ctx, d.server), Database: d.Name, SQL: sent})
	}
	return res, err
}

// dbRows is a read's *sql.Rows plus what closing them must also let go of:
// the *sql.Conn pinned for the read (see Database.query), which *sql.Rows.Close
// alone leaves checked out of the pool, and the read's link to the Server's
// lifetime (see Server.bound). Either may be nil; a server-scoped read off the
// pool pins no connection.
type dbRows struct {
	*sql.Rows
	conn    *sql.Conn
	release func()
}

func (r *dbRows) Close() error {
	err := r.Rows.Close()
	if r.conn != nil {
		if cerr := r.conn.Close(); err == nil {
			err = cerr
		}
	}
	if r.release != nil {
		r.release()
		r.release = nil
	}
	return err
}

// use switches conn to d's database on its own, with the error every
// database-scoped call has always reported for a database it cannot enter.
func (d *Database) use(ctx context.Context, conn sqlConn) error {
	if _, err := conn.ExecContext(ctx, "USE "+quoteIdent(d.Name)); err != nil {
		return fmt.Errorf("gosmo: USE %s: %w", d.Name, err)
	}
	return nil
}

// useBatch is q preceded by the switch to d's database, as one batch — a read
// then costs one round trip rather than two, which is 41 ms per read against
// an Azure SQL Managed Instance and 1.3 ms on a LAN.
//
// The guard is what makes one batch safe. A USE that fails (a missing,
// offline or inaccessible database: 911, 942, 916) raises its error and the
// batch goes on, so without it q would run in whatever database the pooled
// session was in and return that database's rows as this one's. @@ERROR
// rather than a DB_NAME() comparison, which would depend on the collation of
// the database compared in and on how the caller cased the name.
//
// The prefix shares q's first line, so every line number an error in q
// reports is the one q alone reported. Statements after a USE are compiled in
// the database it switched to; verified on 2016, 2017, 2025 and Managed
// Instance, parameterised and not. The parameters are the exception: they
// take the collation of the database the batch started in, so where that
// differs from d's, a parameter compared with a built-in's result
// (SCHEMA_NAME(x) = @p1, both collation-coercible) fails Msg 468 in the batch
// and works alone. So such a comparison carries COLLATE DATABASE_DEFAULT on
// the built-in's side (a catalog column, implicit, needs nothing), which
// TestNoNameBuiltinComparesWithAParameterUncollated enforces; recheckUse is
// the safety net for one it misses, at three round trips.
func (d *Database) useBatch(q string) string {
	return "USE " + quoteIdent(d.Name) + "; IF @@ERROR <> 0 RETURN; " + q
}

// msgCollationConflict is Msg 468, "Cannot resolve the collation conflict".
const msgCollationConflict = 468

// recheckUse tells the two halves of a useBatch read that failed with err
// apart. A USE failure is reported as one, so the USE runs again alone: if it
// fails, useErr is its error and is the read's. If it succeeds, the query
// half failed and err is already the query's own, with its own line numbers
// (see useBatch) — the query is not run a second time, since a lock timeout
// or a late overflow would cost twice, and only a failing read pays the one
// extra round trip.
//
// The exception is rerun: a collation conflict may be the batch's own doing
// (see useBatch), and the session is now in d, so q must run again alone —
// what every failing read did before 2026-10-04, which hid it. Every
// comparison gosmo's own queries make is collated (see useBatch), so this is
// only the net for one written otherwise.
func (d *Database) recheckUse(ctx context.Context, conn sqlConn, err error) (useErr error, rerun bool) {
	if ctx.Err() != nil {
		return nil, false
	}
	if uerr := d.use(ctx, conn); uerr != nil {
		return uerr, false
	}
	me, ok := errors.AsType[mssql.Error](err)
	return nil, ok && me.Number == msgCollationConflict
}

func (d *Database) query(ctx context.Context, q string, args ...any) (*dbRows, error) {
	// For queries that return rows we cannot use withConn (the conn would be
	// released before the caller finishes iterating). Instead we acquire a
	// dedicated conn, switch DB, run the query, and return the rows wrapped
	// with that conn — the caller's defer rows.Close() releases both.
	// A single read is idempotent, so a transient failure (dropped pooled
	// connection, etc.) is retried on a fresh connection.
	ctx, release := d.server.bound(ctx)
	if t := txFrom(ctx, d.server); t != nil {
		rows, err := d.queryOn(ctx, t, q, args...)
		if err != nil {
			release()
			return nil, err
		}
		return &dbRows{Rows: rows, release: release}, nil
	}
	rows, err := withRetry(ctx, func() (*dbRows, error) {
		conn, err := d.server.db.Conn(ctx)
		if err != nil {
			return nil, fmt.Errorf("gosmo: acquire connection: %w", err)
		}
		rows, err := conn.QueryContext(ctx, d.useBatch(q), args...)
		if err != nil {
			if uerr, rerun := d.recheckUse(ctx, conn, err); uerr != nil {
				err = uerr
			} else if rerun {
				rows, err = conn.QueryContext(ctx, q, args...)
			}
		}
		if err != nil {
			conn.Close()
			return nil, err
		}
		return &dbRows{Rows: rows, conn: conn, release: release}, nil
	})
	if err != nil {
		release()
	}
	return rows, err
}

// queryOn is query inside InTransaction: on the transaction's session, with
// the same error report (recheckUse), and never retried. The batch's USE is
// free, so it is sent even when the session is already in d.
func (d *Database) queryOn(ctx context.Context, t *serverTx, q string, args ...any) (*sql.Rows, error) {
	rows, err := t.tx.QueryContext(ctx, d.useBatch(q), args...)
	if err != nil {
		t.lost()
		uerr, rerun := d.recheckUse(ctx, t.tx, err)
		if uerr != nil {
			return nil, withAllMessages(uerr)
		}
		if !rerun {
			return nil, withAllMessages(err)
		}
		if rows, err = t.tx.QueryContext(ctx, q, args...); err != nil {
			return nil, withAllMessages(err)
		}
	}
	t.at(d.Name)
	return rows, nil
}

// queryRow acquires a connection, switches it to d's database (USE), runs
// q, and hands the resulting row to scan — retrying the whole acquire+USE+
// scan sequence as one unit on a transient connection failure, same as
// Server.queryRow and for the same reason: QueryRowContext itself never
// returns an error, only Scan does, so scan has to run inside the retried
// closure to be covered by it at all. Handing the caller a live *sql.Row
// to scan later would let withRetry see a nil error and return before the
// failure that only surfaces at Scan time, silently skipping the retry.
//
// The USE and q go as one batch (see useBatch). A failed batch surfaces in
// Row.Err before scan sees the row, and that is where recheckUse tells the two
// halves apart, as query does — scan never sees a USE failure, which it could
// otherwise wrap or map to something else, and sees the batch's own failed
// row for a failure in q.
func (d *Database) queryRow(ctx context.Context, scan func(*sql.Row) error, q string, args ...any) error {
	ctx, release := d.server.bound(ctx)
	defer release()
	if t := txFrom(ctx, d.server); t != nil {
		row := t.tx.QueryRowContext(ctx, d.useBatch(q), args...)
		if err := row.Err(); err != nil {
			t.lost()
			uerr, rerun := d.recheckUse(ctx, t.tx, err)
			if uerr != nil {
				return uerr
			}
			if rerun {
				row = t.tx.QueryRowContext(ctx, q, args...)
			}
		}
		if row.Err() == nil {
			t.at(d.Name)
		}
		return withAllMessages(scan(row))
	}
	_, err := withRetry(ctx, func() (struct{}, error) {
		conn, err := d.server.db.Conn(ctx)
		if err != nil {
			return struct{}{}, fmt.Errorf("gosmo: acquire connection: %w", err)
		}
		defer conn.Close()
		row := conn.QueryRowContext(ctx, d.useBatch(q), args...)
		if err := row.Err(); err != nil {
			uerr, rerun := d.recheckUse(ctx, conn, err)
			if uerr != nil {
				return struct{}{}, uerr
			}
			if rerun {
				row = conn.QueryRowContext(ctx, q, args...)
			}
		}
		return struct{}{}, scan(row)
	})
	return err
}

// -- Size / space --------------------------------------------------------------

// SpaceInfo holds space usage information for a database.
type SpaceInfo struct {
	TotalMB float64
	DataMB  float64
	LogMB   float64
	// UnallocatedMB is free space within the database's already-allocated
	// data files (SSMS's Database Properties > General "Space available"),
	// not free disk space — it can only shrink the database's on-disk
	// footprint, not grow it, without a file autogrowth event.
	UnallocatedMB float64
	// AvailLogMB is the same free-space measure as UnallocatedMB, but for
	// the log file(s) rather than the data file(s).
	AvailLogMB float64
}

// SpaceUsed returns space usage for the database.
func (d *Database) SpaceUsed(ctx context.Context) (SpaceInfo, error) {
	const q = `
SELECT
    SUM(size) * 8.0 / 1024                                                   AS total_mb,
    SUM(CASE WHEN type_desc <> 'LOG' THEN size ELSE 0 END) * 8.0 / 1024     AS data_mb,
    SUM(CASE WHEN type_desc =  'LOG' THEN size ELSE 0 END) * 8.0 / 1024     AS log_mb,
    SUM(CASE WHEN type_desc <> 'LOG'
             THEN size - CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT)
             ELSE 0 END) * 8.0 / 1024                                       AS unallocated_mb,
    SUM(CASE WHEN type_desc = 'LOG'
             THEN size - CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT)
             ELSE 0 END) * 8.0 / 1024                                       AS avail_log_mb
FROM sys.database_files`

	var si SpaceInfo
	if err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&si.TotalMB, &si.DataMB, &si.LogMB, &si.UnallocatedMB, &si.AvailLogMB)
	}, q); err != nil {
		return SpaceInfo{}, fmt.Errorf("gosmo: space used: %w", err)
	}
	return si, nil
}

// DiskUsage is a database's disk-usage breakdown — the numbers behind
// SSMS's "Disk Usage" report, in MB.
//
// The two halves are read separately, one per file kind, and each is a
// composition of the space its files hold:
//
//	data files: DataMB + IndexMB + UnusedMB + UnallocatedMB
//	log files:  LogUsedMB + LogUnusedMB
//
// UnallocatedMB and LogUnusedMB are file space not yet handed to any
// object, the same free-space measure SpaceInfo reports; UnusedMB is space
// already allocated to an object in extents whose pages it has not filled
// yet, so shrinking a file reclaims the former and rebuilding an index the
// latter.
//
// The data-file parts are counted from allocated pages and the file totals
// from the files themselves, so the four data parts sum to slightly less
// than DataFilesMB: the difference is the database's own internal pages
// (IAM, boot page, allocation bitmaps), which belong to no allocation unit.
// Read the parts against each other, not against DataFilesMB — that is how
// the SSMS report presents them too.
type DiskUsage struct {
	// DataFilesMB and LogFilesMB are the on-disk sizes of the ROWS and LOG
	// files respectively — DataFilesMB + LogFilesMB is the database's
	// footprint.
	DataFilesMB float64
	LogFilesMB  float64

	// DataMB is row data in heaps and clustered indexes, IndexMB is row
	// data in every other index, and both include the LOB and row-overflow
	// pages belonging to them, so no used page is counted twice or missed.
	DataMB  float64
	IndexMB float64
	// UnusedMB is reserved-but-unused space inside extents already
	// allocated to an object.
	UnusedMB float64
	// UnallocatedMB is data-file space not yet allocated to anything.
	UnallocatedMB float64

	// LogUsedMB and LogUnusedMB split the log files the same way — the
	// active portion against what a shrink could give back.
	LogUsedMB   float64
	LogUnusedMB float64
}

// DiskUsage returns the database's disk-usage breakdown.
//
// It is one round trip: the file figures and the allocation figures are
// unrelated aggregates over unrelated tables, so they are cross-joined rather
// than queried one after the other.
func (d *Database) DiskUsage(ctx context.Context) (DiskUsage, error) {
	// The allocation half deliberately spans every object, system tables
	// included: this describes the file, not the user's schema, and pages
	// left out of the sum would show up as unallocated space that a shrink
	// cannot reclaim. index_id 0/1 is the heap or clustered index, so its
	// LOB and row-overflow units (type 2 and 3) are the table's own data;
	// everything above is a nonclustered index.
	const q = `
SELECT
    f.data_files_mb, f.log_files_mb, f.unallocated_mb, f.avail_log_mb,
    a.data_mb, a.index_mb, a.unused_mb
FROM (
    SELECT
        SUM(CASE WHEN type_desc <> 'LOG' THEN size ELSE 0 END) * 8.0 / 1024 AS data_files_mb,
        SUM(CASE WHEN type_desc =  'LOG' THEN size ELSE 0 END) * 8.0 / 1024 AS log_files_mb,
        SUM(CASE WHEN type_desc <> 'LOG'
                 THEN size - CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT)
                 ELSE 0 END) * 8.0 / 1024                                   AS unallocated_mb,
        SUM(CASE WHEN type_desc = 'LOG'
                 THEN size - CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT)
                 ELSE 0 END) * 8.0 / 1024                                   AS avail_log_mb
    FROM sys.database_files
) f
CROSS JOIN (
    SELECT
        ISNULL(SUM(CASE WHEN i.index_id IN (0,1) THEN a.used_pages ELSE 0 END), 0) * 8.0 / 1024 AS data_mb,
        ISNULL(SUM(CASE WHEN i.index_id  > 1     THEN a.used_pages ELSE 0 END), 0) * 8.0 / 1024 AS index_mb,
        ISNULL(SUM(a.total_pages - a.used_pages), 0) * 8.0 / 1024                               AS unused_mb
    FROM sys.partitions p
    JOIN sys.allocation_units a ON a.container_id = CASE WHEN a.type = 2
                                                        THEN p.partition_id
                                                        ELSE p.hobt_id END
    JOIN sys.indexes i ON i.object_id = p.object_id AND i.index_id = p.index_id
) a`

	var du DiskUsage
	if err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&du.DataFilesMB, &du.LogFilesMB, &du.UnallocatedMB, &du.LogUnusedMB,
			&du.DataMB, &du.IndexMB, &du.UnusedMB)
	}, q); err != nil {
		return DiskUsage{}, fmt.Errorf("gosmo: disk usage: %w", err)
	}
	du.LogUsedMB = du.LogFilesMB - du.LogUnusedMB
	return du, nil
}

// -- Schemas -------------------------------------------------------------------

// schemaSelect is shared by Schemas and SchemaByName so a
// schema carries the same fields however it was fetched.
const schemaSelect = `
SELECT s.name, s.schema_id, p.name AS owner
FROM   sys.schemas s
JOIN   sys.database_principals p ON p.principal_id = s.principal_id`

// Schemas returns all schemas in the database.
func (d *Database) Schemas(ctx context.Context) ([]*Schema, error) {
	rows, err := d.query(ctx, schemaSelect+`
ORDER  BY s.name`)
	return scanRows(rows, err, fmt.Sprintf("list schemas in %q", d.Name), func(scan func(...any) error) (*Schema, error) {
		return scanSchema(d, scan)
	})
}

// SchemaByName returns one schema by name.
//
// It returns an error satisfying errors.Is(err, ErrNotFound) when the database
// has no such schema.
func (d *Database) SchemaByName(ctx context.Context, name string) (*Schema, error) {
	return readByName(ctx, d, scanSchema, schemaSelect+`
WHERE  s.name = @p1`, []any{name},
		notFoundf("gosmo: schema %q not found in %q", name, d.Name), fmt.Sprintf("find schema %q in %q", name, d.Name))
}

// SchemaRef returns a lightweight handle for a schema by name, without
// querying the catalog — the counterpart of Server.DatabaseRef. Every field
// but the name stays at its zero value; SchemaByName is what populates them.
//
// Every write on *Schema addresses it by name, so this handle is enough to
// drop one the caller already knows exists — and is the form to use when
// there is nothing to read yet, such as a script of a CREATE that was only
// collected.
func (d *Database) SchemaRef(name string) *Schema {
	return &Schema{db: d, Name: name}
}

func scanSchema(d *Database, scan func(...any) error) (*Schema, error) {
	sc := &Schema{db: d}
	if err := scan(&sc.Name, &sc.ID, &sc.Owner); err != nil {
		return nil, err
	}
	return sc, nil
}

// CreateSchemaRequest describes a new schema.
type CreateSchemaRequest struct {
	Name string
	// Owner is the AUTHORIZATION principal; empty leaves it to the server
	// (the caller's user).
	Owner string
}

// CreateSchema creates a new schema in the database, and returns it read
// back from the catalog — or, under Scripting(ctx), the SchemaRef handle,
// since nothing ran.
func (d *Database) CreateSchema(ctx context.Context, req CreateSchemaRequest) (*Schema, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("gosmo: create schema: name is required")
	}
	q := "CREATE SCHEMA " + quoteIdent(req.Name)
	if req.Owner != "" {
		q += " AUTHORIZATION " + quoteIdent(req.Owner)
	}
	if _, err := d.exec(ctx, q); err != nil {
		return nil, fmt.Errorf("gosmo: create schema %q: %w", req.Name, err)
	}
	return createdObject(ctx, d.SchemaRef(req.Name), func() (*Schema, error) {
		return d.SchemaByName(ctx, req.Name)
	})
}

// -- Tables --------------------------------------------------------------------

// Tables returns all user tables in the database.
func (d *Database) Tables(ctx context.Context) ([]*Table, error) {
	return d.tablesWhere(ctx, userTablesClause, nil)
}

// userTablesClause is the "not SQL Server's own" test every listing that is
// not asked for a specific TableKind applies. The kind listings supply their
// own is_ms_shipped test instead — see kindClause.
const userTablesClause = "AND t.is_ms_shipped = 0"

// TablesFiltered returns the user tables an ObjectFilter matches, narrowed by
// the server rather than by the caller. An empty filter is Tables.
func (d *Database) TablesFiltered(ctx context.Context, filter ObjectFilter) ([]*Table, error) {
	where, args := filter.clause(tableFilterColumns, 1)
	return d.tablesWhere(ctx, userTablesClause+" "+where, args)
}

// tableFilterColumns maps an ObjectFilter onto sys.tables as tablesWhere
// aliases it.
var tableFilterColumns = filterColumns{
	name:            "t.name",
	schema:          "SCHEMA_NAME(t.schema_id) COLLATE DATABASE_DEFAULT",
	created:         "t.create_date",
	memoryOptimized: "t.is_memory_optimized",
}

// TablesBySchema returns all tables in a specific schema.
func (d *Database) TablesBySchema(ctx context.Context, schema string) ([]*Table, error) {
	if err := requireSchema("tables by schema", schema, schema); err != nil {
		return nil, err
	}
	return d.tablesWhere(ctx, userTablesClause+" AND SCHEMA_NAME(t.schema_id) COLLATE DATABASE_DEFAULT = @p1", []any{schema})
}

// tableSelect is the SELECT list every Table listing shares, version-gated:
// is_node and is_edge are 2017 columns (see graphPredicate), substituted with
// a zero bit below that so the scan destinations stay the same at every
// major.
func (d *Database) tableSelect() string {
	major := d.serverMajorVersion()
	return `
SELECT t.object_id, SCHEMA_NAME(t.schema_id), t.name,
       t.create_date, t.modify_date,
       t.has_replication_filter, t.is_memory_optimized,
       t.is_ms_shipped, t.is_filetable, t.is_external,
       ` + colSince(major, SQLServer2017, "t.is_node", "CAST(0 AS bit)") + `,
       ` + colSince(major, SQLServer2017, "t.is_edge", "CAST(0 AS bit)") + `
FROM   sys.tables t`
}

// scanTable reads one row of tableSelect.
func scanTable(d *Database, scan func(...any) error) (*Table, error) {
	t := &Table{db: d}
	if err := scan(&t.ObjectID, &t.Schema, &t.Name,
		&t.CreateDate, &t.ModifyDate,
		&t.HasReplicationFilter, &t.IsMemoryOptimized,
		&t.IsSystem, &t.IsFileTable, &t.IsExternal, &t.IsNode, &t.IsEdge); err != nil {
		return nil, err
	}
	return t, nil
}

func (d *Database) tablesWhere(ctx context.Context, where string, args []any) ([]*Table, error) {
	q := d.tableSelect() + `
WHERE  1 = 1 ` + where + `
ORDER  BY SCHEMA_NAME(t.schema_id), t.name`

	rows, err := d.query(ctx, q, args...)
	return scanRows(rows, err, fmt.Sprintf("list tables in %q", d.Name), func(scan func(...any) error) (*Table, error) {
		return scanTable(d, scan)
	})
}

// TableByName returns a single table by schema and name using a direct query.
//
// It finds a system table (is_ms_shipped = 1) as readily as a user one: the
// caller asked for a table by name, and msdb's own tables — which is most of
// what msdb has — are the ones a by-name lookup would otherwise never reach.
// Tables() still lists only the user tables; the predicate belongs to the
// listing, not to the lookup.
func (d *Database) TableByName(ctx context.Context, schema, name string) (*Table, error) {
	if err := requireSchema("table by name", schema, name); err != nil {
		return nil, err
	}
	q := d.tableSelect() + `
WHERE  SCHEMA_NAME(t.schema_id) COLLATE DATABASE_DEFAULT = @p1
  AND  t.name                                           = @p2`

	return readByName(ctx, d, scanTable, q, []any{schema, name},
		notFoundf("gosmo: table %s not found in %q", qualifiedName(schema, name), d.Name),
		fmt.Sprintf("find table %s in %q", qualifiedName(schema, name), d.Name))
}

// -- Drop and rename -----------------------------------------------------------

// Drop drops the database.
// When force is true, active connections are terminated first — by SET
// SINGLE_USER WITH ROLLBACK IMMEDIATE, or on a Managed Instance, which refuses
// that statement, by killing the database's sessions (see killDatabaseSessions).
// This Server's own idle sessions are released first either way (see
// ReleaseIdleConnections).
func (d *Database) Drop(ctx context.Context, force bool) error {
	s, name := d.server, d.Name
	if name == "" {
		return fmt.Errorf("gosmo: drop database: name is required")
	}
	s.releaseIdle(ctx)
	drop := fmt.Sprintf("DROP DATABASE %s", quoteIdent(name))
	switch {
	case !force:
		// Nothing here set the access mode, so nothing is repaired: a
		// MULTI_USER on the way out would silently undo a RESTRICTED_USER or
		// SINGLE_USER the database was deliberately left in.
		if err := s.exec(ctx, drop); err != nil {
			return fmt.Errorf("gosmo: drop database %q: %w", name, err)
		}
	case s.refusesSingleUser():
		// One batch for the same reason as exclusiveBatch: a session that
		// reconnects between the KILLs and the DROP makes the DROP fail.
		if err := s.exec(ctx, killDatabaseSessionsBatch(name)+";\n"+drop+";"); err != nil {
			return fmt.Errorf("gosmo: drop database %q: %w", name, err)
		}
	default:
		if err := s.exec(ctx, exclusiveBatch(name, drop)); err != nil {
			// The drop can genuinely fail after the alter succeeded — the
			// database belongs to an availability group, the login may set
			// state but not drop — and the batch puts it back to MULTI_USER
			// itself. Only a batch cut short needs the repair from here.
			// Best effort: the drop's own error is what the caller is told.
			if batchCutShort(err) {
				_ = s.restoreMultiUser(ctx, name)
			}
			return fmt.Errorf("gosmo: drop database %q: %w", name, err)
		}
	}
	return nil
}

// Rename renames the database (ALTER DATABASE ... MODIFY NAME). The
// server needs exclusive access to it, so any other connection to the
// database fails the statement outright rather than waiting.
//
// When force is true the database is put into SINGLE_USER WITH ROLLBACK
// IMMEDIATE first — terminating those connections and rolling back their
// transactions — and back to MULTI_USER afterwards, including when the
// rename itself fails, so a refused rename never leaves the database
// single-user. A Managed Instance refuses SET SINGLE_USER, so there force
// kills the database's sessions instead and changes no access mode. This
// Server's own idle sessions are released first either way (see
// ReleaseIdleConnections).
//
// The new name is mirrored onto the receiver (through setIfApplied, so not
// under WithScript).
func (d *Database) Rename(ctx context.Context, newName string, force bool) error {
	if err := d.rename(ctx, newName, force); err != nil {
		return err
	}
	setIfApplied(ctx, &d.Name, newName)
	return nil
}

func (d *Database) rename(ctx context.Context, newName string, force bool) error {
	s, oldName := d.server, d.Name
	if oldName == "" || newName == "" {
		return fmt.Errorf("gosmo: rename database: both names are required")
	}
	q := fmt.Sprintf("ALTER DATABASE %s MODIFY NAME = %s", quoteIdent(oldName), quoteIdent(newName))
	s.releaseIdle(ctx)
	switch {
	case !force:
	case s.refusesSingleUser():
		// Nothing to release afterwards: no access mode was changed. One
		// batch, as in Drop.
		q = killDatabaseSessionsBatch(oldName) + ";\n" + q + ";"
	default:
		if err := s.exec(ctx, renameExclusiveBatch(oldName, newName)); err != nil {
			if batchCutShort(err) {
				_ = s.restoreMultiUserAfterRename(ctx, oldName, newName)
			}
			return fmt.Errorf("gosmo: rename database %q to %q: %w", oldName, newName, err)
		}
		return nil
	}
	if err := s.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: rename database %q to %q: %w", oldName, newName, err)
	}
	return nil
}
