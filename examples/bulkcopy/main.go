// Command bulkcopy demonstrates Database.BulkInsert — gosmo's bcp-equivalent
// load path. Rows arrive as an iter.Seq2[[]any, error], so the source can be
// a slice already in memory, a generator, or a streaming file reader that
// never holds the whole set at once.
//
//	MSSQL_SERVER=localhost:1433 MSSQL_USER=sa MSSQL_PASSWORD=YourPw go run ./examples/bulkcopy
package main

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"iter"
	"strconv"
	"strings"
	"time"

	gosmo "github.com/radix29/gosmo"
	"github.com/radix29/gosmo/examples/internal/demo"
)

const dbName = "GoSMOBulkDemo"

func main() {
	// First, so it runs after the cleanup deferred below it.
	defer demo.Exit()

	srv := demo.Connect()
	defer srv.Close()

	db, drop := demo.TempDatabase(srv, dbName)
	defer drop()

	demo.Must(db.CreateTable(gosmo.CreateTableRequest{
		Schema: "dbo",
		Name:   "Reading",
		Columns: []gosmo.ColumnDefinition{
			{Name: "ReadingID", DataType: gosmo.DataTypeInt, IsIdentity: true, IdentitySeed: 1, IdentityIncr: 1, IsPrimaryKey: true},
			{Name: "SensorID", DataType: gosmo.DataTypeInt, IsNullable: false},
			{Name: "TakenAt", DataType: gosmo.DataTypeDatetime2, Scale: 3, IsNullable: false},
			{Name: "Celsius", DataType: gosmo.DataTypeDecimal, Precision: 6, Scale: 2, IsNullable: false},
			{Name: "Note", DataType: gosmo.DataTypeNVarChar, MaxLength: 100, IsNullable: true, DefaultValue: "'(none)'"},
		},
	}))
	tbl := demo.Value(db.TableByName("dbo", "Reading"))

	// -- A slice you already have ------------------------------------------
	//
	// SliceRows adapts [][]any to the iterator BulkInsert wants. The identity
	// column is not listed in Columns, so the server assigns it.
	demo.Section("SliceRows")
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	n := demo.Value(db.BulkInsert(gosmo.BulkCopy{
		Schema:  "dbo",
		Table:   "Reading",
		Columns: []string{"SensorID", "TakenAt", "Celsius", "Note"},
	}, gosmo.SliceRows([][]any{
		{1, base, 21.5, "calibration"},
		{1, base.Add(time.Minute), 21.7, nil},
		{2, base, -3.25, "outdoor"},
	})))
	fmt.Printf("  %d rows\n", n)

	// -- A generator, never materialized -----------------------------------
	//
	// TableLock takes one BU lock for the whole load instead of row/page
	// locks, and RowsPerBatch tells the server how to size its batches.
	// Neither matters for three rows; both matter for a million.
	demo.Section("Generated rows")
	const generated = 100_000
	n = demo.Value(db.BulkInsert(gosmo.BulkCopy{
		Schema:  "dbo",
		Table:   "Reading",
		Columns: []string{"SensorID", "TakenAt", "Celsius"},
		Options: gosmo.BulkOptions{
			TableLock:    true,
			RowsPerBatch: 20_000,
			// Order names the sort the source rows are already in. When it
			// matches the destination's clustered index the server can skip
			// its own sort — a lie here costs correctness, not just speed.
			Order: []string{"SensorID", "TakenAt"},
		},
	}, sensorReadings(base, generated)))
	fmt.Printf("  %d rows\n", n)

	// -- Streaming from a file ---------------------------------------------
	//
	// csvRows yields one row at a time and stops on the first malformed
	// line by yielding an error — BulkInsert returns it and abandons the
	// load, so a bad source file cannot half-succeed silently.
	demo.Section("Streaming a CSV")
	const goodCSV = "3,2026-02-01T08:00:00Z,18.5,morning\n3,2026-02-01T12:00:00Z,24.0,noon\n"
	n = demo.Value(db.BulkInsert(gosmo.BulkCopy{
		Schema:  "dbo",
		Table:   "Reading",
		Columns: []string{"SensorID", "TakenAt", "Celsius", "Note"},
	}, csvRows(strings.NewReader(goodCSV))))
	fmt.Printf("  %d rows\n", n)

	demo.Section("A source that fails mid-stream")
	const badCSV = "4,2026-02-01T08:00:00Z,18.5,ok\n4,not-a-timestamp,24.0,bad\n"
	if _, err := db.BulkInsert(gosmo.BulkCopy{
		Schema:  "dbo",
		Table:   "Reading",
		Columns: []string{"SensorID", "TakenAt", "Celsius", "Note"},
	}, csvRows(strings.NewReader(badCSV))); err != nil {
		fmt.Printf("  load rejected: %v\n", err)
	}

	// -- Defaults, constraints and triggers --------------------------------
	//
	// KeepNulls decides what an explicit nil means: with it off (the
	// default) the column's DEFAULT applies, with it on the NULL is stored.
	// CheckConstraints and FireTriggers are off by default too — the same
	// bcp defaults, which is why a bulk load can admit rows a plain INSERT
	// would reject.
	demo.Section("KeepNulls")
	_ = demo.Value(db.BulkInsert(gosmo.BulkCopy{
		Schema:  "dbo",
		Table:   "Reading",
		Columns: []string{"SensorID", "TakenAt", "Celsius", "Note"},
		Options: gosmo.BulkOptions{KeepNulls: true, CheckConstraints: true, FireTriggers: true},
	}, gosmo.SliceRows([][]any{{5, base, 0.0, nil}})))
	fmt.Println("  1 row with an explicit NULL Note (default not applied)")

	demo.Section("Result")
	fmt.Printf("  dbo.Reading holds %d rows\n", demo.Value(tbl.RowCount()))
	fmt.Printf("  %d of them are sensor 1\n", demo.Value(tbl.CountWhere("SensorID = 1")))
	fmt.Printf("  %d have no note\n", demo.Value(tbl.CountWhere("Note IS NULL")))
}

// sensorReadings yields count synthetic readings without building a slice.
func sensorReadings(start time.Time, count int) iter.Seq2[[]any, error] {
	return func(yield func([]any, error) bool) {
		for i := range count {
			row := []any{
				i%16 + 1,
				start.Add(time.Duration(i) * time.Second),
				20 + float64(i%400)/100,
			}
			if !yield(row, nil) {
				return
			}
		}
	}
}

// csvRows streams sensorID,timestamp,celsius,note records. A malformed
// record is yielded as an error, which aborts the load.
func csvRows(r io.Reader) iter.Seq2[[]any, error] {
	return func(yield func([]any, error) bool) {
		cr := csv.NewReader(r)
		for line := 1; ; line++ {
			rec, err := cr.Read()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				yield(nil, fmt.Errorf("line %d: %w", line, err))
				return
			}
			row, err := parseReading(rec)
			if err != nil {
				yield(nil, fmt.Errorf("line %d: %w", line, err))
				return
			}
			if !yield(row, nil) {
				return
			}
		}
	}
}

func parseReading(rec []string) ([]any, error) {
	if len(rec) != 4 {
		return nil, fmt.Errorf("want 4 fields, got %d", len(rec))
	}
	sensor, err := strconv.Atoi(rec[0])
	if err != nil {
		return nil, fmt.Errorf("sensor id: %w", err)
	}
	takenAt, err := time.Parse(time.RFC3339, rec[1])
	if err != nil {
		return nil, fmt.Errorf("timestamp: %w", err)
	}
	celsius, err := strconv.ParseFloat(rec[2], 64)
	if err != nil {
		return nil, fmt.Errorf("celsius: %w", err)
	}
	return []any{sensor, takenAt, celsius, rec[3]}, nil
}
