package gosmo

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
)

// A hypothetical index (sys.indexes.is_hypothetical = 1) is the Database
// Engine Tuning Advisor's what-if index: statistics, no data. Read as an
// ordinary index it was scripted as CREATE INDEX and built for real on the
// replay's target, so the index list has to say which rows are hypothetical.
func TestIndexesReadsIsHypothetical(t *testing.T) {
	tbl := captureTable(t)
	captured.reset(
		cannedRow{
			match: "FROM   sys.indexes i",
			cols: []string{"name", "index_id", "type_desc", "is_unique", "is_primary_key",
				"is_unique_constraint", "is_disabled", "is_hypothetical", "fill_factor", "filter_definition",
				"is_padded", "ignore_dup_key", "allow_row_locks", "allow_page_locks",
				"data_compression_desc",
				"data_space", "is_partition_scheme", "is_default_filegroup", "partition_column",
				"no_recompute", "is_incremental", "optimize_for_sequential_key", "bucket_count", "compression_delay",
				"is_primary_xml", "primary_xml_index", "secondary_type_desc", "is_selective_xml", "selective_xml_path",
				"tessellation_scheme",
				"xmin", "ymin", "xmax", "ymax", "level_1", "level_2", "level_3", "level_4", "cells_per_object"},
			rows: [][]driver.Value{
				withNoXMLOrSpatial("ix_real", int64(2), "NONCLUSTERED", false, false, false, false, false, int64(80), "", false, false, true, true, nil, "PRIMARY", false, true, "", false, false, false, int64(0), int64(0)),
				withNoXMLOrSpatial("ix_hypo", int64(3), "NONCLUSTERED", false, false, false, false, true, int64(0), "", false, false, true, true, nil, "", false, false, "", false, false, false, int64(0), int64(0)),
			},
		},
		cannedRow{
			match: "FROM   sys.index_columns ic",
			cols:  []string{"index_id", "name", "is_descending_key", "is_included_column", "column_store_order_ordinal"},
		},
	)
	idxs, err := tbl.Indexes(context.Background())
	if err != nil {
		t.Fatalf("Indexes: %v", err)
	}
	if len(idxs) != 2 {
		t.Fatalf("got %d indexes, want 2", len(idxs))
	}
	if idxs[0].IsHypothetical || !idxs[1].IsHypothetical {
		t.Errorf("IsHypothetical = %v, %v; want false for ix_real, true for ix_hypo", idxs[0].IsHypothetical, idxs[1].IsHypothetical)
	}
	if idxs[0].FillFactor != 80 {
		t.Errorf("FillFactor = %d, want 80 (columns after is_hypothetical misaligned)", idxs[0].FillFactor)
	}
}

// A hypothetical index is written as a comment, never as CREATE INDEX, and
// is not disabled either — whether scripted with its table or on its own.
func TestScriptSkipsHypotheticalIndex(t *testing.T) {
	hypo := &Index{Name: "ix_hypo", Type: IndexTypeNonClustered, IsHypothetical: true,
		KeyColumns: []IndexColumn{{Name: "c"}}}
	real := &Index{Name: "ix_real", Type: IndexTypeNonClustered, KeyColumns: []IndexColumn{{Name: "c"}}}
	parts := tableScriptParts{cols: []*Column{{Name: "c", DataType: DataTypeInt}}, indexes: []*Index{real, hypo}}

	for name, script := range map[string]string{
		"table":       buildTableScript("dbo", "t", "d", parts, DefaultScriptOptions()),
		"index alone": buildIndexScript(hypo, "[dbo].[t]", DataSpace{}, DefaultScriptOptions(), false),
	} {
		if strings.Contains(script, "[ix_hypo] ON") || strings.Contains(script, "INDEX [ix_hypo]") {
			t.Errorf("%s script creates or alters the hypothetical index:\n%s", name, script)
		}
		if !strings.Contains(script, "-- hypothetical index [ix_hypo] on [dbo].[t] (tuning advisor) not scripted") {
			t.Errorf("%s script has no comment for the skipped hypothetical index:\n%s", name, script)
		}
	}
	if s := buildTableScript("dbo", "t", "d", parts, DefaultScriptOptions()); !strings.Contains(s, "CREATE NONCLUSTERED INDEX [ix_real]") {
		t.Errorf("table script lost the real index:\n%s", s)
	}

	hypo.IsDisabled = true
	if s := indexDisableStatement(hypo, "[dbo].[t]"); s != "" {
		t.Errorf("indexDisableStatement(hypothetical) = %q, want \"\"", s)
	}
}

// ALTER INDEX on a hypothetical index succeeds on the server and changes
// nothing, so a read one is refused before anything is sent. DROP INDEX is
// the cleanup and must still go through.
func TestAlterIndexRefusesHypothetical(t *testing.T) {
	ctx := context.Background()
	idx := &Index{Name: "ix_hypo", IsHypothetical: true, table: &Table{Schema: "dbo", Name: "t"}}
	for name, call := range map[string]func() error{
		"Rebuild":    func() error { return idx.Rebuild(ctx, IndexRebuildOptions{}) },
		"Enable":     func() error { return idx.Enable(ctx) },
		"Reorganize": func() error { return idx.Reorganize(ctx) },
		"Disable":    func() error { return idx.Disable(ctx) },
		"SetOptions": func() error { return idx.SetOptions(ctx, IndexSetOptions{AllowRowLocks: new(true)}) },
	} {
		if err := call(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s on a hypothetical index: err = %v, want ErrInvalidRequest", name, err)
		}
	}
	if idx.IsDisabled {
		t.Error("Disable marked a hypothetical index disabled")
	}
}
