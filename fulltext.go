package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

// ============================================================
// Full-Text Search (reads)
// ============================================================
//
// What SSMS shows of full-text search: whether the instance has the
// component and how it is set up (Server.FullTextInfo), a database's
// full-text catalogs, stoplists and search property lists (Storage in
// Object Explorer), and a table's full-text index with its columns and
// population state.
//
// The catalog views read here are the same on 2016 as on 2025 but for one
// column, sys.fulltext_indexes.index_version (2025's version-2 word
// breakers), which is gated. They exist whether or not the component is
// installed: an instance without it (win10cli\SQL2016, a Linux instance
// without mssql-server-fts) reads empty listings, not errors —
// FullTextInfo.Installed is how a caller tells "none" from "can't have any".
//
// Rights: the catalog views follow metadata visibility, so a login sees the
// catalogs, stoplists, lists and indexes it has some permission on. The
// population DMV (FullTextIndex.Populations) needs VIEW SERVER STATE (VIEW
// SERVER PERFORMANCE STATE from 2022, VIEW DATABASE STATE on Azure SQL
// Database) and is a separate read for that reason, like
// ServerAudit.Status: folded into the index read it would fail the index
// for everyone else. The FULLTEXTCATALOGPROPERTY and OBJECTPROPERTYEX
// counters need nothing beyond seeing the object.

// fullTextEpoch is what FULLTEXTCATALOGPROPERTY's PopulateCompletionAge
// counts seconds from. The server's clock has no zone, so UTC, as everywhere
// else in gosmo.
var fullTextEpoch = time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)

// -- Server: the component ----------------------------------------------------

// FullTextUpgradeOption is FULLTEXTSERVICEPROPERTY('UpgradeOption'): what
// happens to full-text indexes when a database is upgraded, attached or
// restored from an older version (sp_fulltext_service 'upgrade_option').
type FullTextUpgradeOption int

const (
	FullTextUpgradeRebuild FullTextUpgradeOption = 0 // rebuild with the new word breakers
	FullTextUpgradeReset   FullTextUpgradeOption = 1 // empty the catalogs; repopulate by hand
	FullTextUpgradeImport  FullTextUpgradeOption = 2 // import them as they are
)

func (o FullTextUpgradeOption) String() string {
	switch o {
	case FullTextUpgradeRebuild:
		return "Rebuild"
	case FullTextUpgradeReset:
		return "Reset"
	case FullTextUpgradeImport:
		return "Import"
	}
	return "FullTextUpgradeOption(" + strconv.Itoa(int(o)) + ")"
}

// FullTextInfo is the instance's full-text component: whether it is
// installed, its sp_fulltext_service settings, and the languages and
// document types (filters) it has.
type FullTextInfo struct {
	// Installed is FULLTEXTSERVICEPROPERTY('IsFullTextInstalled'). Without
	// it nothing can be indexed, though catalogs and stoplists can still be
	// created and read.
	Installed bool
	// LoadOSResources is whether the operating system's word breakers,
	// stemmers and filters are used besides SQL Server's own.
	LoadOSResources bool
	// VerifySignature is whether only signed binaries are loaded.
	VerifySignature bool
	UpgradeOption   FullTextUpgradeOption

	// Languages are sys.fulltext_languages, in name order: the languages a
	// column can be indexed in.
	Languages []FullTextLanguage
	// DocumentTypes are sys.fulltext_document_types, in extension order: the
	// file types a varbinary column with a TYPE COLUMN can hold.
	DocumentTypes []FullTextDocumentType
}

// FullTextLanguage is one language the instance has a word breaker for.
type FullTextLanguage struct {
	LCID int
	Name string
}

// FullTextDocumentType is one registered IFilter.
type FullTextDocumentType struct {
	// Extension is the file extension with its dot, ".docx".
	Extension    string
	ClassID      string // the filter's COM class id
	Path         string // the DLL
	Version      string
	Manufacturer string
}

// FullTextInfo reads the full-text component's configuration. It needs no
// permission beyond connecting.
func (s *Server) FullTextInfo(ctx context.Context) (*FullTextInfo, error) {
	const what = "read full-text configuration"
	info := &FullTextInfo{}
	var installed, loadOS, verify, upgrade sql.NullInt64
	err := s.queryRowScan(ctx, `
SELECT CAST(FULLTEXTSERVICEPROPERTY('IsFullTextInstalled') AS int),
       CAST(FULLTEXTSERVICEPROPERTY('LoadOSResources') AS int),
       CAST(FULLTEXTSERVICEPROPERTY('VerifySignature') AS int),
       CAST(FULLTEXTSERVICEPROPERTY('UpgradeOption') AS int)`, nil,
		&installed, &loadOS, &verify, &upgrade)
	if err != nil {
		return nil, fmt.Errorf("gosmo: %s: %w", what, err)
	}
	info.Installed = installed.Int64 == 1
	info.LoadOSResources = loadOS.Int64 == 1
	info.VerifySignature = verify.Int64 == 1
	info.UpgradeOption = FullTextUpgradeOption(upgrade.Int64)

	rows, err := s.query(ctx, `SELECT lcid, name FROM sys.fulltext_languages ORDER BY name`)
	info.Languages, err = scanRows(rows, err, what, func(scan func(...any) error) (FullTextLanguage, error) {
		var l FullTextLanguage
		return l, scan(&l.LCID, &l.Name)
	})
	if err != nil {
		return nil, err
	}

	rows, err = s.query(ctx, `
SELECT document_type, CONVERT(nchar(36), class_id), ISNULL(path, N''), ISNULL(version, N''), ISNULL(manufacturer, N'')
FROM   sys.fulltext_document_types
ORDER  BY document_type`)
	info.DocumentTypes, err = scanRows(rows, err, what, func(scan func(...any) error) (FullTextDocumentType, error) {
		var d FullTextDocumentType
		return d, scan(&d.Extension, &d.ClassID, &d.Path, &d.Version, &d.Manufacturer)
	})
	if err != nil {
		return nil, err
	}
	return info, nil
}

// -- Catalogs -----------------------------------------------------------------

// FullTextCatalogPopulateStatus is FULLTEXTCATALOGPROPERTY's PopulateStatus.
type FullTextCatalogPopulateStatus int

const (
	FullTextCatalogIdle                  FullTextCatalogPopulateStatus = 0
	FullTextCatalogFullPopulation        FullTextCatalogPopulateStatus = 1
	FullTextCatalogPaused                FullTextCatalogPopulateStatus = 2
	FullTextCatalogThrottled             FullTextCatalogPopulateStatus = 3
	FullTextCatalogRecovering            FullTextCatalogPopulateStatus = 4
	FullTextCatalogShutdown              FullTextCatalogPopulateStatus = 5
	FullTextCatalogIncrementalPopulation FullTextCatalogPopulateStatus = 6
	FullTextCatalogBuildingIndex         FullTextCatalogPopulateStatus = 7
	FullTextCatalogDiskFullPaused        FullTextCatalogPopulateStatus = 8
	FullTextCatalogChangeTracking        FullTextCatalogPopulateStatus = 9
)

var fullTextCatalogStatusNames = map[FullTextCatalogPopulateStatus]string{
	FullTextCatalogIdle:                  "Idle",
	FullTextCatalogFullPopulation:        "Full population in progress",
	FullTextCatalogPaused:                "Paused",
	FullTextCatalogThrottled:             "Throttled",
	FullTextCatalogRecovering:            "Recovering",
	FullTextCatalogShutdown:              "Shutdown",
	FullTextCatalogIncrementalPopulation: "Incremental population in progress",
	FullTextCatalogBuildingIndex:         "Building index",
	FullTextCatalogDiskFullPaused:        "Disk is full; paused",
	FullTextCatalogChangeTracking:        "Change tracking",
}

func (s FullTextCatalogPopulateStatus) String() string {
	if n, ok := fullTextCatalogStatusNames[s]; ok {
		return n
	}
	return "FullTextCatalogPopulateStatus(" + strconv.Itoa(int(s)) + ")"
}

// FullTextCatalog is one sys.fulltext_catalogs row with its
// FULLTEXTCATALOGPROPERTY counters.
type FullTextCatalog struct {
	db *Database

	ID              int
	Name            string
	IsDefault       bool
	AccentSensitive bool
	Owner           string
	IsImporting     bool

	// IndexCount is how many full-text indexes the catalog holds.
	IndexCount int
	// ItemCount is the number of indexed rows across the catalog's indexes,
	// UniqueKeyCount the number of distinct keys (words) in it, SizeMB its
	// size in megabytes. All three are 0 on an instance without the
	// component.
	ItemCount      int
	UniqueKeyCount int
	SizeMB         int
	PopulateStatus FullTextCatalogPopulateStatus
	// MergeInProgress is whether a master merge is running.
	MergeInProgress bool
	// LastPopulated is when the last full or incremental population
	// finished, zero if none has.
	LastPopulated time.Time
}

// Database returns the database the catalog belongs to.
func (c *FullTextCatalog) Database() *Database { return c.db }

// FullTextCatalogs returns d's full-text catalogs in name order.
func (d *Database) FullTextCatalogs(ctx context.Context) ([]*FullTextCatalog, error) {
	what := fmt.Sprintf("read full-text catalogs of database %q", d.Name)
	// FULLTEXTCATALOGPROPERTY returns NULL where the component is missing;
	// PopulateCompletionAge is 0 for a catalog never populated.
	rows, err := d.query(ctx, `
SELECT c.fulltext_catalog_id, c.name, c.is_default, c.is_accent_sensitivity_on,
       ISNULL(USER_NAME(c.principal_id), N''), c.is_importing,
       (SELECT COUNT(*) FROM sys.fulltext_indexes i WHERE i.fulltext_catalog_id = c.fulltext_catalog_id),
       ISNULL(CAST(FULLTEXTCATALOGPROPERTY(c.name, 'ItemCount') AS int), 0),
       ISNULL(CAST(FULLTEXTCATALOGPROPERTY(c.name, 'UniqueKeyCount') AS int), 0),
       ISNULL(CAST(FULLTEXTCATALOGPROPERTY(c.name, 'IndexSize') AS int), 0),
       ISNULL(CAST(FULLTEXTCATALOGPROPERTY(c.name, 'PopulateStatus') AS int), 0),
       ISNULL(CAST(FULLTEXTCATALOGPROPERTY(c.name, 'MergeStatus') AS int), 0),
       ISNULL(CAST(FULLTEXTCATALOGPROPERTY(c.name, 'PopulateCompletionAge') AS bigint), 0)
FROM   sys.fulltext_catalogs c
ORDER  BY c.name`)
	return scanRows(rows, err, what, func(scan func(...any) error) (*FullTextCatalog, error) {
		c := &FullTextCatalog{db: d}
		var status, merge int
		var age int64
		if err := scan(&c.ID, &c.Name, &c.IsDefault, &c.AccentSensitive, &c.Owner, &c.IsImporting,
			&c.IndexCount, &c.ItemCount, &c.UniqueKeyCount, &c.SizeMB, &status, &merge, &age); err != nil {
			return nil, err
		}
		c.PopulateStatus = FullTextCatalogPopulateStatus(status)
		c.MergeInProgress = merge == 1
		if age > 0 {
			c.LastPopulated = fullTextEpoch.Add(time.Duration(age) * time.Second)
		}
		return c, nil
	})
}

// FullTextCatalogByName returns d's full-text catalog called name.
func (d *Database) FullTextCatalogByName(ctx context.Context, name string) (*FullTextCatalog, error) {
	cats, err := d.FullTextCatalogs(ctx)
	if err != nil {
		return nil, err
	}
	c, err := matchName(cats, name, func(c *FullTextCatalog) string { return c.Name })
	if err != nil {
		return nil, fmt.Errorf("gosmo: full-text catalog %q in database %q: %w", name, d.Name, err)
	}
	return c, nil
}

// Indexes returns the full-text indexes the catalog holds — SSMS's catalog
// Properties › Tables/Views — in table order.
func (c *FullTextCatalog) Indexes(ctx context.Context) ([]*FullTextIndex, error) {
	return c.db.fullTextIndexesWhere(ctx,
		fmt.Sprintf("read full-text indexes of catalog %q in database %q", c.Name, c.db.Name),
		"WHERE fi.fulltext_catalog_id = @p1", c.ID)
}

// -- Stoplists ----------------------------------------------------------------

// FullTextStoplist is one user-defined stoplist (sys.fulltext_stoplists).
// The system stoplist is not among them: it has no row, and an index names
// it as FullTextStoplistSystem.
type FullTextStoplist struct {
	db *Database

	ID         int
	Name       string
	Owner      string
	CreateDate time.Time
	ModifyDate time.Time
}

// Database returns the database the stoplist belongs to.
func (l *FullTextStoplist) Database() *Database { return l.db }

// FullTextStopword is one word of a stoplist, in one language.
type FullTextStopword struct {
	Word       string
	Language   string // the language's name, as sys.fulltext_stopwords holds it
	LanguageID int    // its LCID; 0 is the neutral language
}

// FullTextStoplists returns d's user-defined stoplists in name order.
func (d *Database) FullTextStoplists(ctx context.Context) ([]*FullTextStoplist, error) {
	what := fmt.Sprintf("read full-text stoplists of database %q", d.Name)
	rows, err := d.query(ctx, `
SELECT stoplist_id, name, ISNULL(USER_NAME(principal_id), N''), create_date, modify_date
FROM   sys.fulltext_stoplists
ORDER  BY name`)
	return scanRows(rows, err, what, func(scan func(...any) error) (*FullTextStoplist, error) {
		l := &FullTextStoplist{db: d}
		return l, scan(&l.ID, &l.Name, &l.Owner, &l.CreateDate, &l.ModifyDate)
	})
}

// FullTextStoplistByName returns d's stoplist called name.
func (d *Database) FullTextStoplistByName(ctx context.Context, name string) (*FullTextStoplist, error) {
	lists, err := d.FullTextStoplists(ctx)
	if err != nil {
		return nil, err
	}
	l, err := matchName(lists, name, func(l *FullTextStoplist) string { return l.Name })
	if err != nil {
		return nil, fmt.Errorf("gosmo: full-text stoplist %q in database %q: %w", name, d.Name, err)
	}
	return l, nil
}

// Stopwords returns the stoplist's words, by language then word.
func (l *FullTextStoplist) Stopwords(ctx context.Context) ([]FullTextStopword, error) {
	what := fmt.Sprintf("read stopwords of full-text stoplist %q in database %q", l.Name, l.db.Name)
	rows, err := l.db.query(ctx, `
SELECT stopword, language, language_id
FROM   sys.fulltext_stopwords
WHERE  stoplist_id = @p1
ORDER  BY language, stopword`, l.ID)
	return scanRows(rows, err, what, func(scan func(...any) error) (FullTextStopword, error) {
		var w FullTextStopword
		return w, scan(&w.Word, &w.Language, &w.LanguageID)
	})
}

// -- Search property lists ----------------------------------------------------

// SearchPropertyList is one sys.registered_search_property_lists row: the
// document properties (Author, Title, …) an index searches besides the text.
type SearchPropertyList struct {
	db *Database

	ID         int
	Name       string
	Owner      string
	CreateDate time.Time
	ModifyDate time.Time
}

// Database returns the database the property list belongs to.
func (p *SearchPropertyList) Database() *Database { return p.db }

// SearchProperty is one property registered in a search property list.
type SearchProperty struct {
	Name string
	// SetGUID and IntID identify the property to the filters: the
	// property set's GUID and the property's integer id within it.
	SetGUID     string
	IntID       int
	Description string
	// ID is the internal id full-text queries name it by
	// (sys.registered_search_properties.property_id).
	ID int
}

// SearchPropertyLists returns d's search property lists in name order.
func (d *Database) SearchPropertyLists(ctx context.Context) ([]*SearchPropertyList, error) {
	what := fmt.Sprintf("read search property lists of database %q", d.Name)
	rows, err := d.query(ctx, `
SELECT property_list_id, name, ISNULL(USER_NAME(principal_id), N''), create_date, modify_date
FROM   sys.registered_search_property_lists
ORDER  BY name`)
	return scanRows(rows, err, what, func(scan func(...any) error) (*SearchPropertyList, error) {
		p := &SearchPropertyList{db: d}
		return p, scan(&p.ID, &p.Name, &p.Owner, &p.CreateDate, &p.ModifyDate)
	})
}

// SearchPropertyListByName returns d's search property list called name.
func (d *Database) SearchPropertyListByName(ctx context.Context, name string) (*SearchPropertyList, error) {
	lists, err := d.SearchPropertyLists(ctx)
	if err != nil {
		return nil, err
	}
	p, err := matchName(lists, name, func(p *SearchPropertyList) string { return p.Name })
	if err != nil {
		return nil, fmt.Errorf("gosmo: search property list %q in database %q: %w", name, d.Name, err)
	}
	return p, nil
}

// Properties returns the list's properties in name order.
func (p *SearchPropertyList) Properties(ctx context.Context) ([]SearchProperty, error) {
	what := fmt.Sprintf("read properties of search property list %q in database %q", p.Name, p.db.Name)
	rows, err := p.db.query(ctx, `
SELECT property_name, CONVERT(nchar(36), property_set_guid), property_int_id,
       ISNULL(property_description, N''), property_id
FROM   sys.registered_search_properties
WHERE  property_list_id = @p1
ORDER  BY property_name`, p.ID)
	return scanRows(rows, err, what, func(scan func(...any) error) (SearchProperty, error) {
		var sp SearchProperty
		return sp, scan(&sp.Name, &sp.SetGUID, &sp.IntID, &sp.Description, &sp.ID)
	})
}

// -- Indexes ------------------------------------------------------------------

// FullTextChangeTracking is an index's CHANGE_TRACKING setting, spelled as
// the DDL spells it.
type FullTextChangeTracking string

const (
	FullTextChangeTrackingAuto   FullTextChangeTracking = "AUTO"
	FullTextChangeTrackingManual FullTextChangeTracking = "MANUAL"
	FullTextChangeTrackingOff    FullTextChangeTracking = "OFF"
)

// FullTextStoplistKind is which stoplist an index uses.
type FullTextStoplistKind int

const (
	FullTextStoplistOff    FullTextStoplistKind = iota // STOPLIST OFF: every word is indexed
	FullTextStoplistSystem                             // STOPLIST SYSTEM, the built-in one
	FullTextStoplistUser                               // a FullTextStoplist, named by FullTextIndex.Stoplist
)

func (k FullTextStoplistKind) String() string {
	switch k {
	case FullTextStoplistOff:
		return "Off"
	case FullTextStoplistSystem:
		return "System"
	case FullTextStoplistUser:
		return "User"
	}
	return "FullTextStoplistKind(" + strconv.Itoa(int(k)) + ")"
}

// FullTextTablePopulateStatus is OBJECTPROPERTYEX's
// TableFulltextPopulateStatus.
type FullTextTablePopulateStatus int

const (
	FullTextTableIdle                  FullTextTablePopulateStatus = 0
	FullTextTableFullPopulation        FullTextTablePopulateStatus = 1
	FullTextTableIncrementalPopulation FullTextTablePopulateStatus = 2
	FullTextTablePropagatingChanges    FullTextTablePopulateStatus = 3
	FullTextTableBackgroundUpdate      FullTextTablePopulateStatus = 4
	FullTextTableThrottledOrPaused     FullTextTablePopulateStatus = 5
)

var fullTextTableStatusNames = map[FullTextTablePopulateStatus]string{
	FullTextTableIdle:                  "Idle",
	FullTextTableFullPopulation:        "Full population in progress",
	FullTextTableIncrementalPopulation: "Incremental population in progress",
	FullTextTablePropagatingChanges:    "Propagating tracked changes",
	FullTextTableBackgroundUpdate:      "Background update in progress",
	FullTextTableThrottledOrPaused:     "Throttled or paused",
}

func (s FullTextTablePopulateStatus) String() string {
	if n, ok := fullTextTableStatusNames[s]; ok {
		return n
	}
	return "FullTextTablePopulateStatus(" + strconv.Itoa(int(s)) + ")"
}

// FullTextIndex is a table's (or indexed view's) full-text index: one
// sys.fulltext_indexes row, its columns, and the OBJECTPROPERTYEX counters.
// The running population, if any, is Populations.
type FullTextIndex struct {
	db *Database

	// The indexed table.
	ObjectID int
	Schema   string
	Table    string

	// KeyIndex is the unique index whose key identifies each row.
	KeyIndex  string
	Catalog   string
	FileGroup string // empty where the catalog's default applies
	IsEnabled bool

	ChangeTracking FullTextChangeTracking
	StoplistKind   FullTextStoplistKind
	// Stoplist names the user stoplist, empty for OFF and SYSTEM.
	Stoplist string
	// SearchPropertyList is empty when none is set.
	SearchPropertyList string
	// IndexVersion is 1 or 2 (2025's word breakers), 0 below 2025, where
	// there is only one.
	IndexVersion int

	// The last crawl (population): its type as sys.fulltext_indexes names
	// it (FULL_CRAWL, INCREMENTAL_CRAWL, UPDATE_CRAWL, PAUSED_FULL_CRAWL),
	// whether it finished, and when it ran; End is zero while it runs.
	CrawlType      string
	CrawlCompleted bool
	CrawlStart     time.Time
	CrawlEnd       time.Time

	// The OBJECTPROPERTYEX TableFulltext* counters: rows indexed, rows
	// processed by the current or last population, rows that failed, and
	// changes tracked but not yet applied.
	ItemCount      int
	DocsProcessed  int
	FailCount      int
	PendingChanges int
	PopulateStatus FullTextTablePopulateStatus

	Columns []FullTextIndexColumn
}

// Database returns the database the index belongs to.
func (i *FullTextIndex) Database() *Database { return i.db }

// FullName returns the indexed table's [Schema].[Name].
func (i *FullTextIndex) FullName() string { return qualifiedName(i.Schema, i.Table) }

// FullTextIndexColumn is one column of a full-text index.
type FullTextIndexColumn struct {
	Name string
	// TypeColumn names the column holding each row's document type, for a
	// varbinary column of documents; empty otherwise.
	TypeColumn string
	LanguageID int // the LCID the column is word-broken in
	Language   string
	// StatisticalSemantics is whether semantic key phrases and similarity
	// are extracted too.
	StatisticalSemantics bool
}

// FullTextPopulation is one sys.dm_fts_index_population row: a population
// running (or queued behind another) on an index.
type FullTextPopulation struct {
	Type          string // FULL, AUTO, MANUAL, …: population_type_description
	Status        string // status_description
	Completion    string // completion_type_description, empty while it runs
	QueuedType    string // the population waiting behind this one, if any
	StartTime     time.Time
	RangeCount    int
	RangesDone    int
	Outstanding   int // batches outstanding
	WorkerCount   int
	ClusteredScan bool
}

// fullTextIndexVersionExpr reads sys.fulltext_indexes.index_version, which
// 2025 added with its version-2 word breakers (absent from 14.0.2130.4,
// present on 17.0.1135.8). Below, there is one version and 1 is it.
func fullTextIndexVersionExpr(major int) string {
	return colSince(major, SQLServer2025, "fi.index_version", "CAST(1 AS int)")
}

// fullTextIndexSelect is the index read, filtered by where.
func (d *Database) fullTextIndexSelect(where string) string {
	return `
SELECT fi.object_id, OBJECT_SCHEMA_NAME(fi.object_id), OBJECT_NAME(fi.object_id),
       ISNULL(ki.name, N''), ISNULL(c.name, N''), ISNULL(FILEGROUP_NAME(fi.data_space_id), N''), fi.is_enabled,
       fi.change_tracking_state_desc, fi.stoplist_id, ISNULL(sl.name, N''), ISNULL(pl.name, N''),
       ` + fullTextIndexVersionExpr(d.server.serverMajorVersion()) + `,
       ISNULL(fi.crawl_type_desc, N''), fi.has_crawl_completed, fi.crawl_start_date, fi.crawl_end_date,
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextItemCount') AS int), 0),
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextDocsProcessed') AS int), 0),
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextFailCount') AS int), 0),
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextPendingChanges') AS int), 0),
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextPopulateStatus') AS int), 0)
FROM   sys.fulltext_indexes fi
LEFT   JOIN sys.indexes ki ON ki.object_id = fi.object_id AND ki.index_id = fi.unique_index_id
LEFT   JOIN sys.fulltext_catalogs c ON c.fulltext_catalog_id = fi.fulltext_catalog_id
LEFT   JOIN sys.fulltext_stoplists sl ON sl.stoplist_id = fi.stoplist_id
LEFT   JOIN sys.registered_search_property_lists pl ON pl.property_list_id = fi.property_list_id
` + where + `
ORDER  BY OBJECT_SCHEMA_NAME(fi.object_id), OBJECT_NAME(fi.object_id)`
}

// fullTextIndexesWhere reads the indexes where selects, then all their
// columns in one more query, grouped in Go.
func (d *Database) fullTextIndexesWhere(ctx context.Context, what, where string, args ...any) ([]*FullTextIndex, error) {
	rows, err := d.query(ctx, d.fullTextIndexSelect(where), args...)
	idx, err := scanRows(rows, err, what, func(scan func(...any) error) (*FullTextIndex, error) {
		i := &FullTextIndex{db: d}
		var tracking string
		var stoplistID sql.NullInt64
		var start, end sql.NullTime
		var status int
		if err := scan(&i.ObjectID, &i.Schema, &i.Table, &i.KeyIndex, &i.Catalog, &i.FileGroup, &i.IsEnabled,
			&tracking, &stoplistID, &i.Stoplist, &i.SearchPropertyList, &i.IndexVersion,
			&i.CrawlType, &i.CrawlCompleted, &start, &end,
			&i.ItemCount, &i.DocsProcessed, &i.FailCount, &i.PendingChanges, &status); err != nil {
			return nil, err
		}
		i.ChangeTracking = FullTextChangeTracking(tracking)
		switch {
		case !stoplistID.Valid:
			i.StoplistKind = FullTextStoplistOff
		case stoplistID.Int64 == 0:
			i.StoplistKind = FullTextStoplistSystem
		default:
			i.StoplistKind = FullTextStoplistUser
		}
		i.CrawlStart, i.CrawlEnd = start.Time, end.Time
		i.PopulateStatus = FullTextTablePopulateStatus(status)
		return i, nil
	})
	if err != nil || len(idx) == 0 {
		return idx, err
	}

	// The columns of the indexes the first read found, grouped by object id:
	// one index's alone, or every indexed column in the database — repeating
	// where's joins to narrow a set that small would cost more than it saves.
	type col struct {
		objectID int
		c        FullTextIndexColumn
	}
	colWhere, colArgs := "", []any(nil)
	if len(idx) == 1 {
		colWhere, colArgs = "WHERE  ic.object_id = @p1", []any{idx[0].ObjectID}
	}
	rows, err = d.query(ctx, `
SELECT ic.object_id, COL_NAME(ic.object_id, ic.column_id), ISNULL(COL_NAME(ic.object_id, ic.type_column_id), N''),
       ic.language_id, ISNULL(l.name, N''), ic.statistical_semantics
FROM   sys.fulltext_index_columns ic
LEFT   JOIN sys.fulltext_languages l ON l.lcid = ic.language_id
`+colWhere+`
ORDER  BY ic.object_id, ic.column_id`, colArgs...)
	cols, err := scanRows(rows, err, what, func(scan func(...any) error) (col, error) {
		var c col
		return c, scan(&c.objectID, &c.c.Name, &c.c.TypeColumn, &c.c.LanguageID, &c.c.Language, &c.c.StatisticalSemantics)
	})
	if err != nil {
		return nil, err
	}
	byObject := make(map[int]*FullTextIndex, len(idx))
	for _, i := range idx {
		byObject[i.ObjectID] = i
	}
	for _, c := range cols {
		if i := byObject[c.objectID]; i != nil {
			i.Columns = append(i.Columns, c.c)
		}
	}
	return idx, nil
}

// FullTextIndexes returns every full-text index in d, in table order.
func (d *Database) FullTextIndexes(ctx context.Context) ([]*FullTextIndex, error) {
	return d.fullTextIndexesWhere(ctx, fmt.Sprintf("read full-text indexes of database %q", d.Name), "")
}

// FullTextIndex returns the table's full-text index, or a not-found error
// when it has none. It needs a loaded Table (a TableRef has no ObjectID).
func (t *Table) FullTextIndex(ctx context.Context) (*FullTextIndex, error) {
	if err := t.requireLoaded("read the full-text index of"); err != nil {
		return nil, err
	}
	what := fmt.Sprintf("read the full-text index of %s", t.FullName())
	idx, err := t.db.fullTextIndexesWhere(ctx, what, "WHERE fi.object_id = @p1", t.ObjectID)
	if err != nil {
		return nil, err
	}
	if len(idx) == 0 {
		return nil, notFoundf("gosmo: table %s in database %q has no full-text index", t.FullName(), t.db.Name)
	}
	return idx[0], nil
}

// Populations returns the populations running or queued on the index, none
// when it is idle. It reads sys.dm_fts_index_population and so needs VIEW
// SERVER STATE (VIEW SERVER PERFORMANCE STATE from 2022, VIEW DATABASE STATE
// on Azure SQL Database); without it the server's permission error is
// returned. The index's own read does not depend on this one.
func (i *FullTextIndex) Populations(ctx context.Context) ([]FullTextPopulation, error) {
	what := fmt.Sprintf("read full-text populations of %s in database %q", i.FullName(), i.db.Name)
	rows, err := i.db.query(ctx, `
SELECT population_type_description, status_description, ISNULL(completion_type_description, N''),
       ISNULL(queued_population_type_description, N''), start_time,
       range_count, completed_range_count, outstanding_batch_count, worker_count, is_clustered_index_scan
FROM   sys.dm_fts_index_population
WHERE  database_id = DB_ID() AND table_id = @p1
ORDER  BY start_time`, i.ObjectID)
	return scanRows(rows, err, what, func(scan func(...any) error) (FullTextPopulation, error) {
		var p FullTextPopulation
		var completion string
		err := scan(&p.Type, &p.Status, &completion, &p.QueuedType, &p.StartTime,
			&p.RangeCount, &p.RangesDone, &p.Outstanding, &p.WorkerCount, &p.ClusteredScan)
		// NONE is the DMV's word for a population still running.
		if completion != "NONE" {
			p.Completion = completion
		}
		return p, err
	})
}
