package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"

	mssql "github.com/microsoft/go-mssqldb"
)

// ============================================================
// Bulk copy (fast data import, mirrors bcp / SSMS "Import Data")
// ============================================================

// BulkOptions tunes a bulk-copy load, mirroring the WITH options of T-SQL's
// INSERT BULK. The zero value performs a plain load with server defaults.
type BulkOptions struct {
	// CheckConstraints enforces CHECK and FOREIGN KEY constraints on the
	// incoming rows. Off by default (as with bcp), which is faster but can
	// admit rows a normal INSERT would reject.
	CheckConstraints bool

	// FireTriggers fires AFTER INSERT triggers on the destination table.
	// Off by default, so triggers do not run during the load.
	FireTriggers bool

	// KeepNulls keeps NULLs supplied by the source instead of substituting
	// the destination column's DEFAULT.
	KeepNulls bool

	// TableLock takes a bulk-update (BU) lock on the table for the duration
	// of the load rather than acquiring row/page locks — faster for a
	// dedicated import.
	TableLock bool

	// RowsPerBatch hints the number of rows per batch sent to the server.
	// Zero lets the server decide.
	RowsPerBatch int

	// KilobytesPerBatch hints the batch size in kilobytes. Zero lets the
	// server decide.
	KilobytesPerBatch int

	// Order names the columns the source rows are already sorted by (each
	// entry a column optionally followed by " ASC"/" DESC"). When it matches
	// the destination's clustered index the server can skip an internal sort.
	Order []string
}

func (o BulkOptions) driverOptions() mssql.BulkOptions {
	return mssql.BulkOptions{
		CheckConstraints:  o.CheckConstraints,
		FireTriggers:      o.FireTriggers,
		KeepNulls:         o.KeepNulls,
		Tablock:           o.TableLock,
		RowsPerBatch:      o.RowsPerBatch,
		KilobytesPerBatch: o.KilobytesPerBatch,
		Order:             o.Order,
	}
}

// BulkCopy describes the destination of a bulk-insert load.
type BulkCopy struct {
	// Schema is the destination schema; required (see ErrSchemaRequired).
	Schema string

	// Table is the destination table name (unquoted).
	Table string

	// Columns are the destination columns, in the order each row supplies
	// its values. Required.
	Columns []string

	// Options tunes the load; the zero value is fine.
	Options BulkOptions
}

// SliceRows adapts an in-memory slice of rows to the sequence BulkInsert
// consumes, for callers that already hold every row in memory.
func SliceRows(rows [][]any) iter.Seq2[[]any, error] {
	return func(yield func([]any, error) bool) {
		for _, r := range rows {
			if !yield(r, nil) {
				return
			}
		}
	}
}

// BulkInsert streams rows into a table using SQL Server's TDS bulk-copy
// protocol — the same fast path bcp and SSMS "Import Data" use, far faster
// than row-by-row INSERTs. It returns the number of rows copied.
//
// rows yields one []any per row, its values ordered to match bc.Columns; a
// nil element becomes SQL NULL. Yielding a non-nil error aborts the load and
// that error is returned (wrapped), so a streaming source such as a CSV
// reader can surface a read failure. Use SliceRows for an in-memory slice.
//
// Each value is checked and converted for its column before it is sent
// (BulkColumn.Converter): an int out of its column's range, a string too long
// or with a character the code page lacks, a datetime before 1753 and the
// rest that go-mssqldb would load wrong are refused, wrapping a
// *BulkValueError that names the row (1-based) and column, rather than
// wrapped, truncated or stored as '?'. A varchar's text is encoded to the
// database's code page, which is what the server reads it in. The columns
// are read from the catalog first, in the same session; a column it does not
// list (the table cannot be seen, or has no such column) is left unchecked,
// for the server to refuse. A refused value aborts the load like any other
// error: a load with no RowsPerBatch is one batch, so nothing is copied.
//
// Cancelling ctx stops the load; the count of rows copied before cancellation
// is returned alongside the error.
//
// Under WithScript it refuses with an ErrUnsupported error before touching
// the server: the rows travel as a TDS bulk-load stream, not T-SQL, so there
// is no statement to collect, and loading them anyway would make a scripted
// call write. A statement observer (WithStatementObserver) is told of a
// completed load as an "INSERT BULK … -- n rows" entry — a record of what
// happened, not a runnable statement.
func (d *Database) BulkInsert(ctx context.Context, bc BulkCopy, rows iter.Seq2[[]any, error]) (int64, error) {
	if bc.Table == "" {
		return 0, invalidf("gosmo: bulk insert: no destination table")
	}
	if len(bc.Columns) == 0 {
		return 0, invalidf("gosmo: bulk insert into %q: no columns specified", bc.Table)
	}
	if err := requireSchema("bulk insert into", bc.Schema, bc.Table); err != nil {
		return 0, err
	}
	target := qualifiedName(bc.Schema, bc.Table)
	if Scripting(ctx) {
		return 0, unsupportedf("gosmo: bulk insert into %s: a bulk copy cannot be scripted — its rows are a TDS stream, not T-SQL", target)
	}

	if err := refuseInTx(ctx, d.server, "bulk insert into "+target); err != nil {
		return 0, err
	}
	// INSERT BULK names the table in the session's database, so the session
	// is switched to d for the load and back after (withSessionIn).
	var n int64
	err := d.withSessionIn(ctx, func(ctx context.Context, conn *sql.Conn) error {
		var err error
		n, err = bc.load(ctx, conn, target, rows)
		return err
	})
	if err != nil {
		// load prefixes its own errors; withSessionIn's failure to enter d
		// is left to its caller.
		return n, prefixEntry(err, "bulk insert into "+target)
	}
	cols := make([]string, len(bc.Columns))
	for i, c := range bc.Columns {
		cols[i] = quoteIdent(c)
	}
	observe(ctx, d.server, ScriptEntry{
		Server:   scriptServerName(ctx, d.server),
		Database: d.Name,
		SQL:      fmt.Sprintf("INSERT BULK %s (%s) -- %d rows", target, strings.Join(cols, ", "), n),
	})
	return n, nil
}

// load is BulkInsert's copy on conn, a session switched to the target's
// database. It returns the rows copied, the server's count once flushed.
func (bc *BulkCopy) load(ctx context.Context, conn *sql.Conn, target string, rows iter.Seq2[[]any, error]) (int64, error) {
	convs, err := bc.converters(ctx, conn, target)
	if err != nil {
		return 0, err
	}
	// mssql.CopyIn encodes the target and options into an "INSERTBULK ..."
	// statement that the driver turns into a streaming bulk-copy session; the
	// statement must live on this one pinned connection for the load to work.
	stmt, err := conn.PrepareContext(ctx, mssql.CopyIn(target, bc.Options.driverOptions(), bc.Columns...))
	if err != nil {
		return 0, fmt.Errorf("gosmo: prepare bulk insert into %s: %w", target, err)
	}
	defer stmt.Close()

	// Abandoning the load without that final flush leaves the connection
	// mid-bulk-copy: the server is still waiting for rows, so the next
	// statement run on it fails with "Bulk load data was expected but not
	// sent". Returning it to the pool hands that failure to an unrelated
	// caller, so poison it instead — sql.Conn.Raw marks the connection bad
	// when f returns driver.ErrBadConn, and the pool discards it.
	discard := func() {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}

	var n int64
	for row, rerr := range rows {
		if rerr != nil {
			discard()
			return n, fmt.Errorf("gosmo: bulk insert into %s: reading row %d: %w", target, n+1, rerr)
		}
		if len(row) != len(bc.Columns) {
			discard()
			return n, invalidf("gosmo: bulk insert into %s: row %d has %d values, want %d",
				target, n+1, len(row), len(bc.Columns))
		}
		vals := make([]any, len(row))
		for i, v := range row {
			if v == nil || convs[i] == nil {
				vals[i] = v
				continue
			}
			cv, err := convs[i](v)
			if err != nil {
				discard()
				if ve, ok := errors.AsType[*BulkValueError](err); ok {
					ve.Row = n + 1
				}
				return n, fmt.Errorf("gosmo: bulk insert into %s: %w", target, err)
			}
			vals[i] = cv
		}
		if _, err := stmt.ExecContext(ctx, vals...); err != nil {
			discard()
			return n, fmt.Errorf("gosmo: bulk insert into %s: row %d: %w", target, n+1, err)
		}
		n++
	}

	// A final Exec with no arguments flushes the buffered rows and reports the
	// authoritative server-side row count.
	res, err := stmt.ExecContext(ctx)
	if err != nil {
		discard()
		return n, fmt.Errorf("gosmo: bulk insert into %s: flush: %w", target, err)
	}
	if copied, err := res.RowsAffected(); err == nil {
		n = copied
	}
	return n, nil
}

// bulkColumnsQuery reads the target's columns as BulkColumn takes them, in
// the session's database (the load's). TYPE_NAME of system_type_id is an
// alias type's base type; the CLR types share system_type_id 240, which
// names no type, so they read as "" and go unchecked. COLLATIONPROPERTY is
// NULL, read as 0, for a column with no collation.
const bulkColumnsQuery = `SELECT c.name, ISNULL(TYPE_NAME(NULLIF(c.system_type_id, 240)), ''),
       CASE WHEN c.max_length = -1 THEN -1
            WHEN c.system_type_id IN (231, 239) THEN c.max_length / 2
            ELSE c.max_length END,
       c.precision, c.scale,
       ISNULL(CAST(COLLATIONPROPERTY(c.collation_name, 'CodePage') AS int), 0),
       ISNULL(CAST(COLLATIONPROPERTY(CAST(DATABASEPROPERTYEX(DB_NAME(), 'Collation') AS nvarchar(128)), 'CodePage') AS int), 0)
FROM sys.columns c
WHERE c.object_id = OBJECT_ID(@p1)`

// converters is each of bc.Columns' BulkColumn.Converter, read on conn; nil
// for a column the catalog does not list. A name matches exactly, as
// go-mssqldb matches it: a load naming a column in another case fails in the
// driver (live on 17 and 13), so there is nothing for a looser match to check.
func (bc *BulkCopy) converters(ctx context.Context, conn *sql.Conn, target string) ([]func(any) (any, error), error) {
	wrap := func(err error) error {
		return fmt.Errorf("gosmo: bulk insert into %s: read its columns: %w", target, err)
	}
	rows, err := conn.QueryContext(ctx, bulkColumnsQuery, target)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()
	var cols []BulkColumn
	for rows.Next() {
		var c BulkColumn
		if err := rows.Scan(&c.Name, &c.Type, &c.Length, &c.Precision, &c.Scale,
			&c.CodePage, &c.DatabaseCodePage); err != nil {
			return nil, wrap(err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap(err)
	}
	convs := make([]func(any) (any, error), len(bc.Columns))
	for i, name := range bc.Columns {
		j := slices.IndexFunc(cols, func(c BulkColumn) bool { return c.Name == name })
		if j < 0 {
			continue
		}
		conv, err := cols[j].Converter()
		if err != nil {
			return nil, err
		}
		convs[i] = conv
	}
	return convs, nil
}
