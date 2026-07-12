package gosmo

import (
	"context"
	"fmt"
	"iter"

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
	// Schema is the destination schema; empty defaults to "dbo".
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
func (d *Database) BulkInsert(bc BulkCopy, rows iter.Seq2[[]any, error]) (int64, error) {
	return d.BulkInsertContext(context.Background(), bc, rows)
}

// BulkInsertContext is the context-aware variant of BulkInsert. Cancelling
// ctx stops the load; the count of rows copied before cancellation is
// returned alongside the error.
func (d *Database) BulkInsertContext(ctx context.Context, bc BulkCopy, rows iter.Seq2[[]any, error]) (int64, error) {
	if bc.Table == "" {
		return 0, fmt.Errorf("gosmo: bulk insert: no destination table")
	}
	if len(bc.Columns) == 0 {
		return 0, fmt.Errorf("gosmo: bulk insert into %q: no columns specified", bc.Table)
	}
	schema := bc.Schema
	if schema == "" {
		schema = "dbo"
	}
	target := qualifiedName(schema, bc.Table)

	conn, err := d.server.db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("gosmo: acquire connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE "+quoteIdent(d.name)); err != nil {
		return 0, fmt.Errorf("gosmo: USE %s: %w", d.name, err)
	}

	// mssql.CopyIn encodes the target and options into an "INSERTBULK ..."
	// statement that the driver turns into a streaming bulk-copy session; the
	// statement must live on this one pinned connection for the load to work.
	stmt, err := conn.PrepareContext(ctx, mssql.CopyIn(target, bc.Options.driverOptions(), bc.Columns...))
	if err != nil {
		return 0, fmt.Errorf("gosmo: prepare bulk insert into %s: %w", target, err)
	}
	defer stmt.Close()

	var n int64
	for row, rerr := range rows {
		if rerr != nil {
			return n, fmt.Errorf("gosmo: bulk insert into %s: reading row %d: %w", target, n, rerr)
		}
		if len(row) != len(bc.Columns) {
			return n, fmt.Errorf("gosmo: bulk insert into %s: row %d has %d values, want %d",
				target, n, len(row), len(bc.Columns))
		}
		if _, err := stmt.ExecContext(ctx, row...); err != nil {
			return n, fmt.Errorf("gosmo: bulk insert into %s: row %d: %w", target, n, err)
		}
		n++
	}

	// A final Exec with no arguments flushes the buffered rows and reports the
	// authoritative server-side row count.
	res, err := stmt.ExecContext(ctx)
	if err != nil {
		return n, fmt.Errorf("gosmo: bulk insert into %s: flush: %w", target, err)
	}
	if copied, err := res.RowsAffected(); err == nil {
		n = copied
	}
	return n, nil
}
