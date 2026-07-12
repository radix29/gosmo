package gosmo

import "testing"

func TestBulkOptionsDriverMapping(t *testing.T) {
	got := BulkOptions{
		CheckConstraints:  true,
		FireTriggers:      true,
		KeepNulls:         true,
		TableLock:         true,
		RowsPerBatch:      100,
		KilobytesPerBatch: 64,
		Order:             []string{"id ASC"},
	}.driverOptions()

	if !got.CheckConstraints || !got.FireTriggers || !got.KeepNulls {
		t.Errorf("constraint/trigger/nulls flags not mapped: %+v", got)
	}
	if !got.Tablock {
		t.Error("TableLock should map to driver Tablock")
	}
	if got.RowsPerBatch != 100 || got.KilobytesPerBatch != 64 {
		t.Errorf("batch sizes = %d/%d, want 100/64", got.RowsPerBatch, got.KilobytesPerBatch)
	}
	if len(got.Order) != 1 || got.Order[0] != "id ASC" {
		t.Errorf("Order = %v, want [id ASC]", got.Order)
	}
}

func TestBulkOptionsZeroValue(t *testing.T) {
	got := BulkOptions{}.driverOptions()
	if got.CheckConstraints || got.FireTriggers || got.KeepNulls || got.Tablock {
		t.Errorf("zero BulkOptions should map to all-false, got %+v", got)
	}
	if got.RowsPerBatch != 0 || got.KilobytesPerBatch != 0 || got.Order != nil {
		t.Errorf("zero BulkOptions should leave batch/order unset, got %+v", got)
	}
}

func TestSliceRows(t *testing.T) {
	in := [][]any{{1, "a"}, {2, "b"}, {3, "c"}}
	var out [][]any
	for row, err := range SliceRows(in) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out = append(out, row)
	}
	if len(out) != 3 {
		t.Fatalf("got %d rows, want 3", len(out))
	}
	if out[1][1] != "b" {
		t.Errorf("out[1][1] = %v, want b", out[1][1])
	}
}

func TestSliceRowsEarlyStop(t *testing.T) {
	in := [][]any{{1}, {2}, {3}}
	count := 0
	for range SliceRows(in) {
		count++
		break // consumer stops early; the adapter must honour it
	}
	if count != 1 {
		t.Errorf("iterated %d times, want 1 (early break)", count)
	}
}

// The destination guards run before any connection is acquired, so they are
// reachable on a zero-value Database.

func TestBulkInsertRejectsEmptyTable(t *testing.T) {
	d := &Database{}
	if _, err := d.BulkInsert(BulkCopy{Columns: []string{"a"}}, SliceRows(nil)); err == nil {
		t.Fatal("want error when Table is empty, got nil")
	}
}

func TestBulkInsertRejectsNoColumns(t *testing.T) {
	d := &Database{}
	if _, err := d.BulkInsert(BulkCopy{Table: "t"}, SliceRows(nil)); err == nil {
		t.Fatal("want error when Columns is empty, got nil")
	}
}
