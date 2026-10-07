package gosmo

import (
	"context"
	"fmt"
	"strings"
)

// ============================================================
// Full-Text Search (writes)
// ============================================================
//
// The four full-text families — catalog, stoplist, search property list and
// a table's full-text index — each with its Ref handle, Create, the ALTER
// forms SSMS offers, and Drop. Every write addresses its object by name only
// (the index by its table), so a handle is enough for all of them.
//
// None of these statements may run inside a user transaction (CREATE/ALTER/
// DROP FULLTEXT CATALOG and INDEX refuse one outright), so they do not belong
// in a Server.Transaction batch.
//
// The stoplist and search-property-list statements end in ';': SQL Server
// refuses them unterminated (Msg 10736), unlike every other DDL statement
// gosmo emits.

// fullTextLanguage renders a LANGUAGE term: an LCID (decimal or 0x hex) as
// it is, anything else as a string literal naming the language ('English').
func fullTextLanguage(term string) string {
	if isDecimal(term) || isHexLiteral(term) {
		return term
	}
	return QuoteLiteral(term)
}

func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isHexLiteral(s string) bool {
	if len(s) < 3 || (s[:2] != "0x" && s[:2] != "0X") {
		return false
	}
	for _, r := range s[2:] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

// -- Catalogs -----------------------------------------------------------------

// FullTextCatalogRef returns a lightweight handle for a full-text catalog by
// name, without querying the catalog — the counterpart of
// Server.DatabaseRef. Every field but the name stays at its zero value;
// FullTextCatalogByName is what populates them. Every write on
// *FullTextCatalog addresses it by name, so the handle is enough for them.
func (d *Database) FullTextCatalogRef(name string) *FullTextCatalog {
	return &FullTextCatalog{db: d, Name: name}
}

// CreateFullTextCatalogRequest describes a full-text catalog to create.
type CreateFullTextCatalogRequest struct {
	Name string
	// AccentSensitive sets WITH ACCENT_SENSITIVITY; nil takes the
	// database's collation.
	AccentSensitive *bool
	// IsDefault makes it the database's default catalog (AS DEFAULT), the
	// one a CREATE FULLTEXT INDEX naming none goes into.
	IsDefault bool
	// Owner is the AUTHORIZATION user or role; empty is the caller.
	Owner string
}

// CreateFullTextCatalog creates a full-text catalog (CREATE FULLTEXT
// CATALOG). It works without the full-text component installed.
func (d *Database) CreateFullTextCatalog(ctx context.Context, req CreateFullTextCatalogRequest) (*FullTextCatalog, error) {
	if _, err := d.exec(ctx, createFullTextCatalogStatement(req)); err != nil {
		return nil, fmt.Errorf("gosmo: create full-text catalog %q: %w", req.Name, err)
	}
	return createdObject(ctx, d.FullTextCatalogRef(req.Name), func() (*FullTextCatalog, error) {
		return d.FullTextCatalogByName(ctx, req.Name)
	})
}

// createFullTextCatalogStatement is CreateFullTextCatalog's statement, which
// the scripter's CREATE shares.
func createFullTextCatalogStatement(req CreateFullTextCatalogRequest) string {
	var sb strings.Builder
	sb.WriteString("CREATE FULLTEXT CATALOG " + quoteIdent(req.Name))
	if req.AccentSensitive != nil {
		sb.WriteString(" WITH ACCENT_SENSITIVITY = " + onOff(*req.AccentSensitive))
	}
	if req.IsDefault {
		sb.WriteString(" AS DEFAULT")
	}
	if req.Owner != "" {
		sb.WriteString(" AUTHORIZATION " + quoteIdent(req.Owner))
	}
	return sb.String()
}

// Rebuild empties and repopulates the whole catalog (ALTER FULLTEXT CATALOG
// … REBUILD), SSMS's Rebuild. accentSensitive changes the catalog's accent
// sensitivity on the way — a rebuild is the only way to — and nil keeps it.
func (c *FullTextCatalog) Rebuild(ctx context.Context, accentSensitive *bool) error {
	q := "ALTER FULLTEXT CATALOG " + quoteIdent(c.Name) + " REBUILD"
	if accentSensitive != nil {
		q += " WITH ACCENT_SENSITIVITY = " + onOff(*accentSensitive)
	}
	if _, err := c.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: rebuild full-text catalog %q: %w", c.Name, err)
	}
	if accentSensitive != nil {
		setIfApplied(ctx, &c.AccentSensitive, *accentSensitive)
	}
	return nil
}

// Reorganize merges the catalog's index fragments into one (ALTER FULLTEXT
// CATALOG … REORGANIZE), SSMS's Optimize.
func (c *FullTextCatalog) Reorganize(ctx context.Context) error {
	if _, err := c.db.exec(ctx, "ALTER FULLTEXT CATALOG "+quoteIdent(c.Name)+" REORGANIZE"); err != nil {
		return fmt.Errorf("gosmo: reorganize full-text catalog %q: %w", c.Name, err)
	}
	return nil
}

// SetDefault makes the catalog the database's default (ALTER FULLTEXT
// CATALOG … AS DEFAULT). There is no statement to unset it: another catalog
// becomes the default instead.
func (c *FullTextCatalog) SetDefault(ctx context.Context) error {
	if _, err := c.db.exec(ctx, "ALTER FULLTEXT CATALOG "+quoteIdent(c.Name)+" AS DEFAULT"); err != nil {
		return fmt.Errorf("gosmo: make full-text catalog %q the default: %w", c.Name, err)
	}
	setIfApplied(ctx, &c.IsDefault, true)
	return nil
}

// SetOwner transfers ownership of the catalog (ALTER AUTHORIZATION).
func (c *FullTextCatalog) SetOwner(ctx context.Context, owner string) error {
	q := "ALTER AUTHORIZATION ON FULLTEXT CATALOG::" + quoteIdent(c.Name) + " TO " + quoteIdent(owner)
	if _, err := c.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set owner of full-text catalog %q: %w", c.Name, err)
	}
	setIfApplied(ctx, &c.Owner, owner)
	return nil
}

// Drop drops the catalog. The server refuses while an index is in it.
func (c *FullTextCatalog) Drop(ctx context.Context) error {
	if _, err := c.db.exec(ctx, "DROP FULLTEXT CATALOG "+quoteIdent(c.Name)); err != nil {
		return fmt.Errorf("gosmo: drop full-text catalog %q: %w", c.Name, err)
	}
	return nil
}

// -- Stoplists ----------------------------------------------------------------

// FullTextStoplistRef returns a lightweight handle for a full-text stoplist
// by name, without querying the catalog — the counterpart of
// Server.DatabaseRef. Every field but the name stays at its zero value;
// FullTextStoplistByName is what populates them. Every write on
// *FullTextStoplist addresses it by name, so the handle is enough for them.
func (d *Database) FullTextStoplistRef(name string) *FullTextStoplist {
	return &FullTextStoplist{db: d, Name: name}
}

// CreateFullTextStoplistRequest describes a stoplist to create. It starts
// empty, as a copy of the system stoplist (FromSystem), or as a copy of
// another user stoplist (From, in FromDatabase or this one) — not more than
// one of those.
type CreateFullTextStoplistRequest struct {
	Name         string
	FromSystem   bool
	From         string
	FromDatabase string
	// Owner is the AUTHORIZATION user or role; empty is the caller.
	Owner string
}

// CreateFullTextStoplist creates a full-text stoplist (CREATE FULLTEXT
// STOPLIST).
func (d *Database) CreateFullTextStoplist(ctx context.Context, req CreateFullTextStoplistRequest) (*FullTextStoplist, error) {
	q, err := createFullTextStoplistStatement(req)
	if err != nil {
		return nil, err
	}
	if _, err := d.exec(ctx, q); err != nil {
		return nil, fmt.Errorf("gosmo: create full-text stoplist %q: %w", req.Name, err)
	}
	return createdObject(ctx, d.FullTextStoplistRef(req.Name), func() (*FullTextStoplist, error) {
		return d.FullTextStoplistByName(ctx, req.Name)
	})
}

func createFullTextStoplistStatement(req CreateFullTextStoplistRequest) (string, error) {
	if req.FromSystem && req.From != "" {
		return "", fmt.Errorf("gosmo: create full-text stoplist %q: FromSystem and From are exclusive", req.Name)
	}
	if req.FromDatabase != "" && req.From == "" {
		return "", fmt.Errorf("gosmo: create full-text stoplist %q: FromDatabase needs From", req.Name)
	}
	var sb strings.Builder
	sb.WriteString("CREATE FULLTEXT STOPLIST " + quoteIdent(req.Name))
	switch {
	case req.FromSystem:
		sb.WriteString(" FROM SYSTEM STOPLIST")
	case req.FromDatabase != "":
		sb.WriteString(" FROM " + quoteIdent(req.FromDatabase) + "." + quoteIdent(req.From))
	case req.From != "":
		sb.WriteString(" FROM " + quoteIdent(req.From))
	}
	if req.Owner != "" {
		sb.WriteString(" AUTHORIZATION " + quoteIdent(req.Owner))
	}
	sb.WriteString(";")
	return sb.String(), nil
}

// AddStopword adds word to the stoplist in language — an LCID ("1033",
// "0x0409"; "0" is the neutral language) or a language name ("English").
// Adding a word the stoplist already has in that language is an error
// (Msg 30033).
func (l *FullTextStoplist) AddStopword(ctx context.Context, word, language string) error {
	q := "ALTER FULLTEXT STOPLIST " + quoteIdent(l.Name) + " ADD " + QuoteLiteral(word) +
		" LANGUAGE " + fullTextLanguage(language) + ";"
	if _, err := l.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: add stopword %q to full-text stoplist %q: %w", word, l.Name, err)
	}
	return nil
}

// DropStopword removes word in language (as AddStopword takes it) from the
// stoplist.
func (l *FullTextStoplist) DropStopword(ctx context.Context, word, language string) error {
	q := "ALTER FULLTEXT STOPLIST " + quoteIdent(l.Name) + " DROP " + QuoteLiteral(word) +
		" LANGUAGE " + fullTextLanguage(language) + ";"
	if _, err := l.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: drop stopword %q from full-text stoplist %q: %w", word, l.Name, err)
	}
	return nil
}

// DropLanguageStopwords removes every word of one language from the
// stoplist (DROP ALL LANGUAGE).
func (l *FullTextStoplist) DropLanguageStopwords(ctx context.Context, language string) error {
	q := "ALTER FULLTEXT STOPLIST " + quoteIdent(l.Name) + " DROP ALL LANGUAGE " + fullTextLanguage(language) + ";"
	if _, err := l.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: drop %s stopwords from full-text stoplist %q: %w", language, l.Name, err)
	}
	return nil
}

// DropAllStopwords empties the stoplist (DROP ALL).
func (l *FullTextStoplist) DropAllStopwords(ctx context.Context) error {
	if _, err := l.db.exec(ctx, "ALTER FULLTEXT STOPLIST "+quoteIdent(l.Name)+" DROP ALL;"); err != nil {
		return fmt.Errorf("gosmo: drop all stopwords from full-text stoplist %q: %w", l.Name, err)
	}
	return nil
}

// SetOwner transfers ownership of the stoplist (ALTER AUTHORIZATION).
func (l *FullTextStoplist) SetOwner(ctx context.Context, owner string) error {
	q := "ALTER AUTHORIZATION ON FULLTEXT STOPLIST::" + quoteIdent(l.Name) + " TO " + quoteIdent(owner)
	if _, err := l.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set owner of full-text stoplist %q: %w", l.Name, err)
	}
	setIfApplied(ctx, &l.Owner, owner)
	return nil
}

// Drop drops the stoplist. The server refuses while an index uses it.
func (l *FullTextStoplist) Drop(ctx context.Context) error {
	if _, err := l.db.exec(ctx, "DROP FULLTEXT STOPLIST "+quoteIdent(l.Name)+";"); err != nil {
		return fmt.Errorf("gosmo: drop full-text stoplist %q: %w", l.Name, err)
	}
	return nil
}

// -- Search property lists ----------------------------------------------------

// SearchPropertyListRef returns a lightweight handle for a search property
// list by name, without querying the catalog — the counterpart of
// Server.DatabaseRef. Every field but the name stays at its zero value;
// SearchPropertyListByName is what populates them. Every write on
// *SearchPropertyList addresses it by name, so the handle is enough for them.
func (d *Database) SearchPropertyListRef(name string) *SearchPropertyList {
	return &SearchPropertyList{db: d, Name: name}
}

// CreateSearchPropertyListRequest describes a search property list to
// create: empty, or a copy of another one (From, in FromDatabase or this
// one).
type CreateSearchPropertyListRequest struct {
	Name         string
	From         string
	FromDatabase string
	// Owner is the AUTHORIZATION user or role; empty is the caller.
	Owner string
}

// CreateSearchPropertyList creates a search property list (CREATE SEARCH
// PROPERTY LIST).
func (d *Database) CreateSearchPropertyList(ctx context.Context, req CreateSearchPropertyListRequest) (*SearchPropertyList, error) {
	q, err := createSearchPropertyListStatement(req)
	if err != nil {
		return nil, err
	}
	if _, err := d.exec(ctx, q); err != nil {
		return nil, fmt.Errorf("gosmo: create search property list %q: %w", req.Name, err)
	}
	return createdObject(ctx, d.SearchPropertyListRef(req.Name), func() (*SearchPropertyList, error) {
		return d.SearchPropertyListByName(ctx, req.Name)
	})
}

func createSearchPropertyListStatement(req CreateSearchPropertyListRequest) (string, error) {
	if req.FromDatabase != "" && req.From == "" {
		return "", fmt.Errorf("gosmo: create search property list %q: FromDatabase needs From", req.Name)
	}
	var sb strings.Builder
	sb.WriteString("CREATE SEARCH PROPERTY LIST " + quoteIdent(req.Name))
	switch {
	case req.FromDatabase != "":
		sb.WriteString(" FROM " + quoteIdent(req.FromDatabase) + "." + quoteIdent(req.From))
	case req.From != "":
		sb.WriteString(" FROM " + quoteIdent(req.From))
	}
	if req.Owner != "" {
		sb.WriteString(" AUTHORIZATION " + quoteIdent(req.Owner))
	}
	sb.WriteString(";")
	return sb.String(), nil
}

// AddProperty registers a property in the list. Name, SetGUID and IntID
// are required, Description optional; ID is the server's and ignored.
func (p *SearchPropertyList) AddProperty(ctx context.Context, prop SearchProperty) error {
	if _, err := p.db.exec(ctx, addSearchPropertyStatement(p.Name, prop)); err != nil {
		return fmt.Errorf("gosmo: add property %q to search property list %q: %w", prop.Name, p.Name, err)
	}
	return nil
}

func addSearchPropertyStatement(list string, prop SearchProperty) string {
	q := fmt.Sprintf("ALTER SEARCH PROPERTY LIST %s ADD %s WITH (PROPERTY_SET_GUID = %s, PROPERTY_INT_ID = %d",
		quoteIdent(list), QuoteLiteral(prop.Name), QuoteLiteral(prop.SetGUID), prop.IntID)
	if prop.Description != "" {
		q += ", PROPERTY_DESCRIPTION = " + QuoteLiteral(prop.Description)
	}
	return q + ");"
}

// DropProperty removes the property called name from the list.
func (p *SearchPropertyList) DropProperty(ctx context.Context, name string) error {
	q := "ALTER SEARCH PROPERTY LIST " + quoteIdent(p.Name) + " DROP " + QuoteLiteral(name) + ";"
	if _, err := p.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: drop property %q from search property list %q: %w", name, p.Name, err)
	}
	return nil
}

// SetOwner transfers ownership of the list (ALTER AUTHORIZATION).
func (p *SearchPropertyList) SetOwner(ctx context.Context, owner string) error {
	q := "ALTER AUTHORIZATION ON SEARCH PROPERTY LIST::" + quoteIdent(p.Name) + " TO " + quoteIdent(owner)
	if _, err := p.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set owner of search property list %q: %w", p.Name, err)
	}
	setIfApplied(ctx, &p.Owner, owner)
	return nil
}

// Drop drops the list. The server refuses while an index uses it.
func (p *SearchPropertyList) Drop(ctx context.Context) error {
	if _, err := p.db.exec(ctx, "DROP SEARCH PROPERTY LIST "+quoteIdent(p.Name)+";"); err != nil {
		return fmt.Errorf("gosmo: drop search property list %q: %w", p.Name, err)
	}
	return nil
}

// -- Indexes ------------------------------------------------------------------

// FullTextIndexRef returns a lightweight handle for the table's full-text
// index without querying the server — a table has at most one, so the
// table's name is the index's. Every field but Schema and Table stays at its
// zero value; Table.FullTextIndex is what populates them. Every write on
// *FullTextIndex addresses it by its table, so the handle is enough for
// them, and works from a TableRef.
func (t *Table) FullTextIndexRef() *FullTextIndex {
	return &FullTextIndex{db: t.db, Schema: t.Schema, Table: t.Name}
}

// FullTextIndexColumnSpec is one column to full-text index.
type FullTextIndexColumnSpec struct {
	Name string
	// TypeColumn names the column holding each row's document type
	// (".docx"), for a varbinary or image column of documents.
	TypeColumn string
	// Language is the word breaker's language, an LCID ("1033", "0x0409";
	// "0" is neutral) or a name ("English"); empty is the instance's
	// default full-text language.
	Language string
	// StatisticalSemantics also extracts key phrases and similarity. It
	// needs the semantic language statistics database.
	StatisticalSemantics bool
}

func (c FullTextIndexColumnSpec) clause() string {
	s := quoteIdent(c.Name)
	if c.TypeColumn != "" {
		s += " TYPE COLUMN " + quoteIdent(c.TypeColumn)
	}
	if c.Language != "" {
		s += " LANGUAGE " + fullTextLanguage(c.Language)
	}
	if c.StatisticalSemantics {
		s += " STATISTICAL_SEMANTICS"
	}
	return s
}

// CreateFullTextIndexRequest describes a table's full-text index.
type CreateFullTextIndexRequest struct {
	Columns []FullTextIndexColumnSpec
	// KeyIndex is the unique, single-column, non-nullable index whose key
	// identifies each row. Required.
	KeyIndex string
	// Catalog is the full-text catalog; empty is the database's default
	// catalog. FileGroup places the index; empty is the default
	// filegroup.
	Catalog   string
	FileGroup string
	// ChangeTracking is empty for the server's default, AUTO.
	ChangeTracking FullTextChangeTracking
	// NoPopulation skips the initial population. The server allows it only
	// with ChangeTracking OFF.
	NoPopulation bool
	// StoplistOff indexes every word (STOPLIST OFF). Otherwise Stoplist
	// names a user stoplist, and empty is the system stoplist — the
	// server's default.
	StoplistOff bool
	Stoplist    string
	// SearchPropertyList is empty for none.
	SearchPropertyList string
}

func (req CreateFullTextIndexRequest) validate(table string) error {
	switch {
	case req.KeyIndex == "":
		return fmt.Errorf("gosmo: create full-text index on %s: a key index is required", table)
	case req.StoplistOff && req.Stoplist != "":
		return fmt.Errorf("gosmo: create full-text index on %s: StoplistOff and Stoplist are exclusive", table)
	case req.NoPopulation && req.ChangeTracking != FullTextChangeTrackingOff:
		return fmt.Errorf("gosmo: create full-text index on %s: NoPopulation needs ChangeTracking OFF", table)
	}
	if err := validChangeTracking(req.ChangeTracking, true); err != nil {
		return fmt.Errorf("gosmo: create full-text index on %s: %w", table, err)
	}
	return nil
}

func validChangeTracking(ct FullTextChangeTracking, emptyOK bool) error {
	switch ct {
	case FullTextChangeTrackingAuto, FullTextChangeTrackingManual, FullTextChangeTrackingOff:
		return nil
	case "":
		if emptyOK {
			return nil
		}
	}
	return fmt.Errorf("unknown change tracking %q", ct)
}

// CreateFullTextIndex creates the table's full-text index (CREATE FULLTEXT
// INDEX). Unless NoPopulation is set the server starts a full population
// in the background; the call does not wait for it.
func (t *Table) CreateFullTextIndex(ctx context.Context, req CreateFullTextIndexRequest) (*FullTextIndex, error) {
	if err := requireSchema("create full-text index on", t.Schema, t.Name); err != nil {
		return nil, err
	}
	if err := req.validate(t.FullName()); err != nil {
		return nil, err
	}
	if _, err := t.exec(ctx, createFullTextIndexStatement(t.FullName(), req)); err != nil {
		return nil, fmt.Errorf("gosmo: create full-text index on %s: %w", t.FullName(), err)
	}
	return createdObject(ctx, t.FullTextIndexRef(), func() (*FullTextIndex, error) {
		return t.FullTextIndex(ctx)
	})
}

func createFullTextIndexStatement(table string, req CreateFullTextIndexRequest) string {
	var sb strings.Builder
	sb.WriteString("CREATE FULLTEXT INDEX ON " + table)
	if len(req.Columns) > 0 {
		cols := make([]string, len(req.Columns))
		for i, c := range req.Columns {
			cols[i] = c.clause()
		}
		sb.WriteString(" (" + strings.Join(cols, ", ") + ")")
	}
	sb.WriteString(" KEY INDEX " + quoteIdent(req.KeyIndex))
	switch {
	case req.Catalog != "" && req.FileGroup != "":
		sb.WriteString(" ON (" + quoteIdent(req.Catalog) + ", FILEGROUP " + quoteIdent(req.FileGroup) + ")")
	case req.Catalog != "":
		sb.WriteString(" ON " + quoteIdent(req.Catalog))
	case req.FileGroup != "":
		sb.WriteString(" ON (FILEGROUP " + quoteIdent(req.FileGroup) + ")")
	}
	var with []string
	if req.ChangeTracking != "" {
		ct := "CHANGE_TRACKING = " + string(req.ChangeTracking)
		if req.NoPopulation {
			ct += ", NO POPULATION"
		}
		with = append(with, ct)
	}
	switch {
	case req.StoplistOff:
		with = append(with, "STOPLIST = OFF")
	case req.Stoplist != "":
		with = append(with, "STOPLIST = "+quoteIdent(req.Stoplist))
	}
	if req.SearchPropertyList != "" {
		with = append(with, "SEARCH PROPERTY LIST = "+quoteIdent(req.SearchPropertyList))
	}
	if len(with) > 0 {
		sb.WriteString(" WITH (" + strings.Join(with, ", ") + ")")
	}
	return sb.String()
}

// alter runs ALTER FULLTEXT INDEX ON <table> <action>.
func (i *FullTextIndex) alter(ctx context.Context, what, action string) error {
	if err := requireSchema(what, i.Schema, i.Table); err != nil {
		return err
	}
	if _, err := i.db.exec(ctx, "ALTER FULLTEXT INDEX ON "+i.FullName()+" "+action); err != nil {
		return fmt.Errorf("gosmo: %s %s: %w", what, i.FullName(), err)
	}
	return nil
}

func withNoPopulation(noPopulation bool) string {
	if noPopulation {
		return " WITH NO POPULATION"
	}
	return ""
}

// Enable turns the index back on; with change tracking it catches up on
// what changed while it was off.
func (i *FullTextIndex) Enable(ctx context.Context) error {
	if err := i.alter(ctx, "enable the full-text index on", "ENABLE"); err != nil {
		return err
	}
	setIfApplied(ctx, &i.IsEnabled, true)
	return nil
}

// Disable stops the index being maintained or used by queries; it keeps
// its data.
func (i *FullTextIndex) Disable(ctx context.Context) error {
	if err := i.alter(ctx, "disable the full-text index on", "DISABLE"); err != nil {
		return err
	}
	setIfApplied(ctx, &i.IsEnabled, false)
	return nil
}

// AddColumn adds a column to the index. The server repopulates unless
// noPopulation is set.
func (i *FullTextIndex) AddColumn(ctx context.Context, col FullTextIndexColumnSpec, noPopulation bool) error {
	return i.alter(ctx, fmt.Sprintf("add column %q to the full-text index on", col.Name),
		"ADD ("+col.clause()+")"+withNoPopulation(noPopulation))
}

// DropColumn removes a column from the index. The server repopulates unless
// noPopulation is set.
func (i *FullTextIndex) DropColumn(ctx context.Context, name string, noPopulation bool) error {
	return i.alter(ctx, fmt.Sprintf("drop column %q from the full-text index on", name),
		"DROP ("+quoteIdent(name)+")"+withNoPopulation(noPopulation))
}

// SetChangeTracking sets how changes reach the index: AUTO as they happen,
// MANUAL when StartPopulation(FullTextPopulationUpdate) applies them, OFF
// not at all.
func (i *FullTextIndex) SetChangeTracking(ctx context.Context, ct FullTextChangeTracking) error {
	if err := validChangeTracking(ct, false); err != nil {
		return fmt.Errorf("gosmo: set change tracking of the full-text index on %s: %w", i.FullName(), err)
	}
	if err := i.alter(ctx, "set change tracking of the full-text index on", "SET CHANGE_TRACKING = "+string(ct)); err != nil {
		return err
	}
	setIfApplied(ctx, &i.ChangeTracking, ct)
	return nil
}

// SetStoplist sets the index's stoplist: kind Off or System, or User with
// name naming it (name is ignored otherwise). The server repopulates.
func (i *FullTextIndex) SetStoplist(ctx context.Context, kind FullTextStoplistKind, name string) error {
	var term string
	switch kind {
	case FullTextStoplistOff:
		term, name = "OFF", ""
	case FullTextStoplistSystem:
		term, name = "SYSTEM", ""
	case FullTextStoplistUser:
		if name == "" {
			return fmt.Errorf("gosmo: set stoplist of the full-text index on %s: a user stoplist needs a name", i.FullName())
		}
		term = quoteIdent(name)
	default:
		return fmt.Errorf("gosmo: set stoplist of the full-text index on %s: unknown kind %v", i.FullName(), kind)
	}
	if err := i.alter(ctx, "set stoplist of the full-text index on", "SET STOPLIST = "+term); err != nil {
		return err
	}
	setIfApplied(ctx, &i.StoplistKind, kind)
	setIfApplied(ctx, &i.Stoplist, name)
	return nil
}

// SetSearchPropertyList sets the index's search property list; empty name
// removes it (OFF). The server repopulates.
func (i *FullTextIndex) SetSearchPropertyList(ctx context.Context, name string) error {
	term := "OFF"
	if name != "" {
		term = quoteIdent(name)
	}
	if err := i.alter(ctx, "set search property list of the full-text index on", "SET SEARCH PROPERTY LIST = "+term); err != nil {
		return err
	}
	setIfApplied(ctx, &i.SearchPropertyList, name)
	return nil
}

// FullTextPopulationKind is which population StartPopulation starts.
type FullTextPopulationKind string

const (
	// FullTextPopulationFull reindexes every row.
	FullTextPopulationFull FullTextPopulationKind = "FULL"
	// FullTextPopulationIncremental reindexes the rows whose timestamp
	// column changed; the table needs one.
	FullTextPopulationIncremental FullTextPopulationKind = "INCREMENTAL"
	// FullTextPopulationUpdate applies the changes tracked so far — SSMS's
	// Apply Tracked Changes, for MANUAL change tracking.
	FullTextPopulationUpdate FullTextPopulationKind = "UPDATE"
)

// StartPopulation starts a population and returns; it runs in the
// background (Populations and the index's PopulateStatus follow it).
// While a population is running the server ignores the request with an
// informational message and no error ("is ignored because a population is
// currently active"), so a nil error does not mean one started — compare
// CrawlStart before and after, which the server stamps before the
// statement returns (seen on 17, 2026-10-07). Under AUTO change tracking
// that is always the answer to FULL and INCREMENTAL: the DMV keeps an AUTO
// population listed while PopulateStatus reads idle (14 and 17,
// 2026-10-08); MANUAL and OFF start one.
func (i *FullTextIndex) StartPopulation(ctx context.Context, kind FullTextPopulationKind) error {
	switch kind {
	case FullTextPopulationFull, FullTextPopulationIncremental, FullTextPopulationUpdate:
	default:
		return fmt.Errorf("gosmo: start population of the full-text index on %s: unknown kind %q", i.FullName(), kind)
	}
	return i.alter(ctx, "start "+strings.ToLower(string(kind))+" population of the full-text index on",
		"START "+string(kind)+" POPULATION")
}

// StopPopulation stops the running population. Only under OFF change
// tracking: under AUTO or MANUAL the server ignores it with an
// informational message and no error (AUTO: "Stop crawl request is
// ignored"), and the population runs on (seen on 17, 2026-10-07).
func (i *FullTextIndex) StopPopulation(ctx context.Context) error {
	return i.alter(ctx, "stop population of the full-text index on", "STOP POPULATION")
}

// PausePopulation pauses the running full population.
func (i *FullTextIndex) PausePopulation(ctx context.Context) error {
	return i.alter(ctx, "pause population of the full-text index on", "PAUSE POPULATION")
}

// ResumePopulation resumes a paused full population.
func (i *FullTextIndex) ResumePopulation(ctx context.Context) error {
	return i.alter(ctx, "resume population of the full-text index on", "RESUME POPULATION")
}

// Drop drops the full-text index.
func (i *FullTextIndex) Drop(ctx context.Context) error {
	if err := requireSchema("drop the full-text index on", i.Schema, i.Table); err != nil {
		return err
	}
	if _, err := i.db.exec(ctx, "DROP FULLTEXT INDEX ON "+i.FullName()); err != nil {
		return fmt.Errorf("gosmo: drop the full-text index on %s: %w", i.FullName(), err)
	}
	return nil
}
