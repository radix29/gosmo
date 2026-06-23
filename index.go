package gosmo

import (
	"context"
	"fmt"
	"strings"
)

// ── Index management ──────────────────────────────────────────────────────────

// Rebuild rebuilds the index (equivalent to ALTER INDEX … REBUILD).
// Pass fillFactor=0 to use the existing fill factor.
func (idx *Index) Rebuild(t *Table, fillFactor int) error {
	q := fmt.Sprintf("ALTER INDEX [%s] ON %s REBUILD", idx.Name, t.FullName())
	if fillFactor > 0 {
		q += fmt.Sprintf(" WITH (FILLFACTOR = %d)", fillFactor)
	}
	_, err := t.db.exec(context.Background(), q)
	if err != nil {
		return fmt.Errorf("gosmo: rebuild index [%s]: %w", idx.Name, err)
	}
	return nil
}

// Reorganize reorganizes the index (ALTER INDEX … REORGANIZE).
func (idx *Index) Reorganize(t *Table) error {
	q := fmt.Sprintf("ALTER INDEX [%s] ON %s REORGANIZE", idx.Name, t.FullName())
	_, err := t.db.exec(context.Background(), q)
	if err != nil {
		return fmt.Errorf("gosmo: reorganize index [%s]: %w", idx.Name, err)
	}
	return nil
}

// Disable disables the index (ALTER INDEX … DISABLE).
func (idx *Index) Disable(t *Table) error {
	q := fmt.Sprintf("ALTER INDEX [%s] ON %s DISABLE", idx.Name, t.FullName())
	_, err := t.db.exec(context.Background(), q)
	if err != nil {
		return fmt.Errorf("gosmo: disable index [%s]: %w", idx.Name, err)
	}
	idx.IsDisabled = true
	return nil
}

// Enable re-enables a disabled index by rebuilding it.
func (idx *Index) Enable(t *Table) error {
	return idx.Rebuild(t, 0)
}

// Drop drops the index from the table.
func (idx *Index) Drop(t *Table) error {
	q := fmt.Sprintf("DROP INDEX [%s] ON %s", idx.Name, t.FullName())
	_, err := t.db.exec(context.Background(), q)
	if err != nil {
		return fmt.Errorf("gosmo: drop index [%s]: %w", idx.Name, err)
	}
	return nil
}

// RebuildAllIndexes rebuilds all indexes on the table.
func (t *Table) RebuildAllIndexes(fillFactor int) error {
	q := fmt.Sprintf("ALTER INDEX ALL ON %s REBUILD", t.FullName())
	if fillFactor > 0 {
		q += fmt.Sprintf(" WITH (FILLFACTOR = %d)", fillFactor)
	}
	_, err := t.db.exec(context.Background(), q)
	if err != nil {
		return fmt.Errorf("gosmo: rebuild all indexes on %s: %w", t.FullName(), err)
	}
	return nil
}

// CreateIndexRequest describes a new index to create.
type CreateIndexRequest struct {
	Name             string
	Type             IndexType
	IsUnique         bool
	KeyColumns       []IndexColumnDef
	IncludedColumns  []string
	FilterDefinition string
	FillFactor       int
	Online           bool // WITH (ONLINE = ON)
	SortInTempDB     bool
}

// IndexColumnDef describes a key column in a new index.
type IndexColumnDef struct {
	Name       string
	Descending bool
}

// CreateIndex creates a new index on the table.
func (t *Table) CreateIndex(req CreateIndexRequest) error {
	if len(req.KeyColumns) == 0 {
		return fmt.Errorf("gosmo: create index: at least one key column required")
	}

	var sb strings.Builder
	sb.WriteString("CREATE ")
	if req.IsUnique {
		sb.WriteString("UNIQUE ")
	}
	switch req.Type {
	case IndexTypeClustered:
		sb.WriteString("CLUSTERED ")
	case IndexTypeColumnStore:
		sb.WriteString("NONCLUSTERED COLUMNSTORE ")
	default:
		sb.WriteString("NONCLUSTERED ")
	}
	fmt.Fprintf(&sb, "INDEX [%s] ON %s (", req.Name, t.FullName())

	keyCols := make([]string, len(req.KeyColumns))
	for i, c := range req.KeyColumns {
		dir := "ASC"
		if c.Descending {
			dir = "DESC"
		}
		keyCols[i] = fmt.Sprintf("[%s] %s", c.Name, dir)
	}
	sb.WriteString(strings.Join(keyCols, ", "))
	sb.WriteString(")")

	if len(req.IncludedColumns) > 0 {
		inc := make([]string, len(req.IncludedColumns))
		for i, c := range req.IncludedColumns {
			inc[i] = fmt.Sprintf("[%s]", c)
		}
		fmt.Fprintf(&sb, " INCLUDE (%s)", strings.Join(inc, ", "))
	}

	if req.FilterDefinition != "" {
		fmt.Fprintf(&sb, " WHERE %s", req.FilterDefinition)
	}

	var withs []string
	if req.FillFactor > 0 {
		withs = append(withs, fmt.Sprintf("FILLFACTOR = %d", req.FillFactor))
	}
	if req.Online {
		withs = append(withs, "ONLINE = ON")
	}
	if req.SortInTempDB {
		withs = append(withs, "SORT_IN_TEMPDB = ON")
	}
	if len(withs) > 0 {
		fmt.Fprintf(&sb, " WITH (%s)", strings.Join(withs, ", "))
	}

	_, err := t.db.exec(context.Background(), sb.String())
	if err != nil {
		return fmt.Errorf("gosmo: create index [%s] on %s: %w", req.Name, t.FullName(), err)
	}
	return nil
}

// IndexFragmentation holds fragmentation statistics for an index.
type IndexFragmentation struct {
	IndexName           string
	IndexID             int
	AvgFragmentationPct float64
	PageCount           int64
	FragmentCount       int64
}

// FragmentationStats returns fragmentation info for all indexes on the table.
// mode: "LIMITED" (fast, default), "SAMPLED", or "DETAILED".
func (t *Table) FragmentationStats(mode string) ([]*IndexFragmentation, error) {
	if mode == "" {
		mode = "LIMITED"
	}
	q := fmt.Sprintf(`
SELECT i.name, s.index_id,
       s.avg_fragmentation_in_percent,
       s.page_count,
       s.fragment_count
FROM   sys.dm_db_index_physical_stats(DB_ID(), OBJECT_ID(N'%s.%s'), NULL, NULL, N'%s') s
JOIN   sys.indexes i ON i.object_id = s.object_id AND i.index_id = s.index_id
WHERE  s.index_id > 0
ORDER  BY s.avg_fragmentation_in_percent DESC`,
		escapeSingle(t.Schema), escapeSingle(t.Name), mode)

	rows, err := t.db.query(context.Background(), q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: fragmentation stats for %s: %w", t.FullName(), err)
	}
	defer rows.Close()

	var results []*IndexFragmentation
	for rows.Next() {
		f := &IndexFragmentation{}
		if err := rows.Scan(&f.IndexName, &f.IndexID,
			&f.AvgFragmentationPct, &f.PageCount, &f.FragmentCount); err != nil {
			return nil, err
		}
		results = append(results, f)
	}
	return results, rows.Err()
}
