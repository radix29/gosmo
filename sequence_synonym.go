package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
)

// ============================================================
// Sequences  (SQL Server 2012+)
// ============================================================

// Sequence mirrors sys.sequences.
type Sequence struct {
	db       *Database
	Name     string
	Schema   string
	ObjectID int
	// DataType is the sequence's underlying type name, unqualified. A
	// sequence declared over a user-defined alias type carries that type's
	// name here, which is outside the DataType vocabulary CreateSequence
	// accepts; DataTypeSchema is the half that disambiguates it.
	DataType DataType
	// DataTypeSchema is the schema DataType lives in — "sys" for the
	// built-in types, the owning schema for an alias type. Scripting has to
	// qualify with it, or a re-run resolves the alias against the executing
	// principal's default schema instead.
	DataTypeSchema string
	// Precision and Scale are the declared type's, from sys.sequences; they
	// matter for a decimal/numeric sequence, whose bare type name means
	// (18,0).
	Precision int
	Scale     int
	// StartValue, Increment, MinValue, MaxValue and CurrentValue are the
	// catalog's values as decimal digits. They are strings because a
	// decimal(38,0) sequence ranges far past int64, and reading one as an
	// integer failed the whole listing with an arithmetic overflow. ALTER
	// SEQUENCE … RESTART moves StartValue to the restart value.
	StartValue string
	Increment  string
	MinValue   string
	MaxValue   string
	IsCycling  bool
	IsCached   bool
	CacheSize  int
	// CurrentValue is sys.sequences.current_value: the last value handed
	// out, or StartValue when none has been since the sequence was created
	// or restarted.
	CurrentValue string
	// LastUsedValue is sys.sequences.last_used_value (SQL Server 2017+):
	// the last value handed out, "" when none has been since the sequence
	// was created or restarted. Unlike CurrentValue it tells those two
	// cases apart. Always "" before 2017, which has no such column.
	LastUsedValue string
	// noLastUsed records that the server predates last_used_value, so an
	// empty LastUsedValue says nothing about whether a value was handed out.
	noLastUsed bool
}

// Database returns the database the sequence belongs to.
func (seq *Sequence) Database() *Database { return seq.db }

// Sequences returns all sequences in the database.
func (d *Database) Sequences(ctx context.Context) ([]*Sequence, error) {
	rows, err := d.query(ctx, d.sequenceSelect()+`
ORDER  BY SCHEMA_NAME(s.schema_id), s.name`)
	return scanRows(rows, err, "list sequences", func(scan func(...any) error) (*Sequence, error) {
		return scanSequence(d, scan)
	})
}

// SequenceByName returns one sequence by schema and name. The names compare
// under the database's collation, so a case-sensitive database with both
// [Sales].[seq] and [sales].[seq] returns the one asked for. An empty schema
// means dbo.
//
// It returns an error satisfying errors.Is(err, ErrNotFound) when the database
// has no such sequence.
func (d *Database) SequenceByName(ctx context.Context, schema, name string) (*Sequence, error) {
	if err := requireSchema("sequence by name", schema, name); err != nil {
		return nil, err
	}
	var seq *Sequence
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		seq, err = scanSequence(d, row.Scan)
		return err
	}, d.sequenceSelect()+`
WHERE  SCHEMA_NAME(s.schema_id) = @p1
  AND  s.name                   = @p2`, schema, name)
	return foundRow(seq, err, notFoundf("gosmo: sequence %s not found in %q", qualifiedName(schema, name), d.Name),
		fmt.Sprintf("find sequence %s in %q", qualifiedName(schema, name), d.Name))
}

// SequenceRef returns a lightweight handle for a sequence by name, without
// querying the catalog — the counterpart of Server.DatabaseRef. Every field
// but the schema and name stays at its zero value; SequenceByName is what populates them.
//
// Every write on *Sequence addresses it by name, so this handle is enough to
// drop one the caller already knows exists — and is the form to use when
// there is nothing to read yet, such as a script of a CREATE that was only
// collected.
//
// schema is taken as given: an empty one is refused by the handle's writes
// (ErrSchemaRequired), never defaulted.
func (d *Database) SequenceRef(schema, name string) *Sequence {
	return &Sequence{db: d, Schema: schema, Name: name}
}

func scanSequence(d *Database, scan func(...any) error) (*Sequence, error) {
	// The colSince test, spelled out: hasColumnSince is reserved for
	// GROUP BY gates, and version_gate_inventory_test counts it.
	major := d.serverMajorVersion()
	seq := &Sequence{db: d, noLastUsed: major != 0 && major < int(SQLServer2017)}
	var lastUsed sql.NullString
	if err := scan(
		&seq.Name, &seq.Schema, &seq.ObjectID,
		&seq.DataType, &seq.DataTypeSchema, &seq.Precision, &seq.Scale,
		&seq.StartValue, &seq.Increment,
		&seq.MinValue, &seq.MaxValue,
		&seq.IsCycling, &seq.IsCached, &seq.CacheSize,
		&seq.CurrentValue, &lastUsed,
	); err != nil {
		return nil, err
	}
	seq.LastUsedValue = lastUsed.String
	return seq, nil
}

// sequenceSelect is the query Sequences and SequenceByName share, without
// its WHERE or ORDER BY. Every value column is read as text: see the
// Sequence field comments.
func (d *Database) sequenceSelect() string {
	return `
SELECT s.name, SCHEMA_NAME(s.schema_id), s.object_id,
       tp.name, SCHEMA_NAME(tp.schema_id), s.precision, s.scale,
       CONVERT(nvarchar(40), s.start_value),
       CONVERT(nvarchar(40), s.increment),
       CONVERT(nvarchar(40), s.minimum_value),
       CONVERT(nvarchar(40), s.maximum_value),
       s.is_cycling, s.is_cached, ISNULL(s.cache_size, 0),
       CONVERT(nvarchar(40), s.current_value),
       ` + colSince(d.serverMajorVersion(), SQLServer2017, "CONVERT(nvarchar(40), s.last_used_value)", "CAST(NULL AS nvarchar(40))") + `
FROM   sys.sequences s
JOIN   sys.types tp ON tp.user_type_id = s.user_type_id`
}

// CreateSequenceRequest describes a new sequence.
type CreateSequenceRequest struct {
	Schema     string
	Name       string
	DataType   DataType // defaults to bigint
	StartValue int64
	Increment  int64
	MinValue   *int64
	MaxValue   *int64
	Cycle      bool
	Cache      *int // nil = no cache; 0 = NO CACHE; >0 = cache size
}

// CreateSequence creates a new sequence in the database.
func (d *Database) CreateSequence(ctx context.Context, req CreateSequenceRequest) (*Sequence, error) {
	if req.DataType == "" {
		req.DataType = DataTypeBigInt
	}
	if !validDataType(req.DataType) {
		return nil, fmt.Errorf("gosmo: create sequence %q: unrecognized data type %q", req.Name, req.DataType)
	}
	schema := req.Schema
	if err := requireSchema("create sequence", schema, req.Name); err != nil {
		return nil, err
	}

	q := fmt.Sprintf("CREATE SEQUENCE %s AS %s", qualifiedName(schema, req.Name), req.DataType)
	q += fmt.Sprintf(" START WITH %d INCREMENT BY %d", req.StartValue, req.Increment)
	if req.MinValue != nil {
		q += fmt.Sprintf(" MINVALUE %d", *req.MinValue)
	} else {
		q += " NO MINVALUE"
	}
	if req.MaxValue != nil {
		q += fmt.Sprintf(" MAXVALUE %d", *req.MaxValue)
	} else {
		q += " NO MAXVALUE"
	}
	if req.Cycle {
		q += " CYCLE"
	} else {
		q += " NO CYCLE"
	}
	if req.Cache == nil {
		q += " CACHE"
	} else if *req.Cache == 0 {
		q += " NO CACHE"
	} else {
		q += fmt.Sprintf(" CACHE %d", *req.Cache)
	}

	_, err := d.exec(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: create sequence %s: %w", qualifiedName(schema, req.Name), err)
	}
	return createdObject(ctx, d.SequenceRef(schema, req.Name), func() (*Sequence, error) {
		return d.SequenceByName(ctx, schema, req.Name)
	})
}

// Drop drops the sequence.
func (seq *Sequence) Drop(ctx context.Context) error {
	if err := requireSchema("drop sequence", seq.Schema, seq.Name); err != nil {
		return err
	}
	_, err := seq.db.exec(ctx, fmt.Sprintf("DROP SEQUENCE %s", qualifiedName(seq.Schema, seq.Name)))
	if err != nil {
		return fmt.Errorf("gosmo: drop sequence %s: %w", qualifiedName(seq.Schema, seq.Name), err)
	}
	return nil
}

// Restart restarts the sequence at the given value.
func (seq *Sequence) Restart(ctx context.Context, value int64) error {
	if err := requireSchema("restart sequence", seq.Schema, seq.Name); err != nil {
		return err
	}
	_, err := seq.db.exec(ctx,
		fmt.Sprintf("ALTER SEQUENCE %s RESTART WITH %d",
			qualifiedName(seq.Schema, seq.Name), value))
	if err != nil {
		return fmt.Errorf("gosmo: restart sequence %s: %w", qualifiedName(seq.Schema, seq.Name), err)
	}
	// RESTART moves start_value too and clears last_used_value, so the
	// handle mirrors all three.
	v := strconv.FormatInt(value, 10)
	setIfApplied(ctx, &seq.StartValue, v)
	setIfApplied(ctx, &seq.CurrentValue, v)
	setIfApplied(ctx, &seq.LastUsedValue, "")
	return nil
}

// NextValue retrieves the next value from the sequence.
//
// This deliberately uses withConn, not queryRow: "NEXT VALUE FOR" advances the
// sequence as a side effect of being read, so it isn't safe to retry —
// unlike every other queryRow caller in this package, re-running it on a fresh
// connection after a transient failure could silently skip a value. withConn
// still retries the acquire+USE step (safe, nothing server-side has happened
// yet), just not the query itself.
func (seq *Sequence) NextValue(ctx context.Context) (int64, error) {
	if err := requireSchema("next value for", seq.Schema, seq.Name); err != nil {
		return 0, err
	}
	var val int64
	err := seq.db.withConn(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx,
			fmt.Sprintf("SELECT NEXT VALUE FOR %s", qualifiedName(seq.Schema, seq.Name)),
		).Scan(&val)
	})
	if err != nil {
		return 0, fmt.Errorf("gosmo: next value for %s: %w", qualifiedName(seq.Schema, seq.Name), err)
	}
	// Assigned directly, not via setIfApplied: this is the value the server
	// just returned, and reads run against the server under WithScript too.
	seq.CurrentValue = strconv.FormatInt(val, 10)
	seq.LastUsedValue = seq.CurrentValue
	return val, nil
}

// ============================================================
// Synonyms
// ============================================================

// Synonym mirrors sys.synonyms.
type Synonym struct {
	db         *Database
	Name       string
	Schema     string
	ObjectID   int
	BaseObject string // fully qualified base object name
}

// Database returns the database the synonym belongs to.
func (syn *Synonym) Database() *Database { return syn.db }

// Synonyms returns all synonyms in the database.
func (d *Database) Synonyms(ctx context.Context) ([]*Synonym, error) {
	rows, err := d.query(ctx, synonymSelect+`
ORDER  BY SCHEMA_NAME(schema_id), name`)
	return scanRows(rows, err, "list synonyms", func(scan func(...any) error) (*Synonym, error) {
		return scanSynonym(d, scan)
	})
}

// SynonymByName returns one synonym by schema and name, compared under the
// database's collation. An empty schema is refused (ErrSchemaRequired).
//
// It returns an error satisfying errors.Is(err, ErrNotFound) when the database
// has no such synonym.
func (d *Database) SynonymByName(ctx context.Context, schema, name string) (*Synonym, error) {
	if err := requireSchema("synonym by name", schema, name); err != nil {
		return nil, err
	}
	var syn *Synonym
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		syn, err = scanSynonym(d, row.Scan)
		return err
	}, synonymSelect+`
WHERE  SCHEMA_NAME(schema_id) = @p1
  AND  name                   = @p2`, schema, name)
	return foundRow(syn, err, notFoundf("gosmo: synonym %s not found in %q", qualifiedName(schema, name), d.Name),
		fmt.Sprintf("find synonym %s in %q", qualifiedName(schema, name), d.Name))
}

// SynonymRef returns a lightweight handle for a synonym by name, without
// querying the catalog — the counterpart of Server.DatabaseRef. Every field
// but the schema and name stays at its zero value; SynonymByName is what populates them.
//
// Every write on *Synonym addresses it by name, so this handle is enough to
// drop one the caller already knows exists — and is the form to use when
// there is nothing to read yet, such as a script of a CREATE that was only
// collected.
//
// schema is taken as given: an empty one is refused by the handle's writes
// (ErrSchemaRequired), never defaulted.
func (d *Database) SynonymRef(schema, name string) *Synonym {
	return &Synonym{db: d, Schema: schema, Name: name}
}

const synonymSelect = `
SELECT name, SCHEMA_NAME(schema_id), object_id, base_object_name
FROM   sys.synonyms`

func scanSynonym(d *Database, scan func(...any) error) (*Synonym, error) {
	s := &Synonym{db: d}
	var baseObj sql.NullString
	if err := scan(&s.Name, &s.Schema, &s.ObjectID, &baseObj); err != nil {
		return nil, err
	}
	s.BaseObject = baseObj.String
	return s, nil
}

// qualifiedObjectNamePart matches one part of a dot-separated multi-part
// T-SQL object name: either a bracket-quoted identifier (embedded ']'
// doubled, embedded '.' allowed same as SQL Server itself allows inside
// brackets) or a bare identifier.
const qualifiedObjectNamePart = `(?:\[(?:[^\]]|\]\])+\]|[A-Za-z_][A-Za-z0-9_]*)`

// qualifiedObjectNamePattern matches a one- to four-part T-SQL object name
// (linked_server.database.schema.object, or any suffix of it) end to end —
// the shape CreateSynonym's baseObject documents callers must
// already have built, e.g. "[OtherDB].[dbo].[MyTable]".
var qualifiedObjectNamePattern = regexp.MustCompile(`^` + qualifiedObjectNamePart + `(?:\.` + qualifiedObjectNamePart + `){0,3}$`)

// validQualifiedObjectName reports whether s is a well-formed multi-part
// object name with no stray T-SQL syntax (a ';', '--', unmatched bracket,
// ...) outside of dot-separated, individually-quoted-or-bare parts.
// baseObject can span server/database/schema/object, so it can't be quoted
// as a single identifier the way qualifiedName's schema+name pair is —
// this validation is CreateSynonym's injection defense in its
// place.
func validQualifiedObjectName(s string) bool {
	return qualifiedObjectNamePattern.MatchString(s)
}

// CreateSynonymRequest describes a new synonym.
type CreateSynonymRequest struct {
	Schema string // required; see ErrSchemaRequired
	Name   string
	// BaseObject is the fully qualified name of what the synonym stands
	// for, already bracket-quoted: "[OtherDB].[dbo].[MyTable]".
	BaseObject string
}

// CreateSynonym creates a synonym for a base object, and returns it read
// back from the catalog — or, under Scripting(ctx), the SynonymRef handle,
// since nothing ran.
func (d *Database) CreateSynonym(ctx context.Context, req CreateSynonymRequest) (*Synonym, error) {
	schema := req.Schema
	if err := requireSchema("create synonym", schema, req.Name); err != nil {
		return nil, err
	}
	if !validQualifiedObjectName(req.BaseObject) {
		return nil, fmt.Errorf("gosmo: create synonym %s: invalid base object %q", qualifiedName(schema, req.Name), req.BaseObject)
	}
	_, err := d.exec(ctx,
		fmt.Sprintf("CREATE SYNONYM %s FOR %s", qualifiedName(schema, req.Name), req.BaseObject))
	if err != nil {
		return nil, fmt.Errorf("gosmo: create synonym %s: %w", qualifiedName(schema, req.Name), err)
	}
	return createdObject(ctx, d.SynonymRef(schema, req.Name), func() (*Synonym, error) {
		return d.SynonymByName(ctx, schema, req.Name)
	})
}

// Drop drops the synonym. A synonym that isn't there is the server's error,
// not a silent success — see the note on Table.Drop.
func (syn *Synonym) Drop(ctx context.Context) error {
	if err := requireSchema("drop synonym", syn.Schema, syn.Name); err != nil {
		return err
	}
	_, err := syn.db.exec(ctx, fmt.Sprintf("DROP SYNONYM %s", qualifiedName(syn.Schema, syn.Name)))
	if err != nil {
		return fmt.Errorf("gosmo: drop synonym %s: %w", qualifiedName(syn.Schema, syn.Name), err)
	}
	return nil
}

// Rename renames the sequence (sp_rename's 'OBJECT' class). newName is a bare
// name; a rename never moves the sequence between schemas — see Transfer.
func (seq *Sequence) Rename(ctx context.Context, newName string) error {
	if err := seq.db.renameSchemaObject(ctx, "sequence", renameObjectClass, seq.Schema, seq.Name, newName); err != nil {
		return err
	}
	setIfApplied(ctx, &seq.Name, newName)
	return nil
}

// Transfer moves the sequence into another schema (ALTER SCHEMA ... TRANSFER).
// It keeps its name and object_id; permissions granted on it directly are
// dropped by the server.
func (seq *Sequence) Transfer(ctx context.Context, targetSchema string) error {
	if err := seq.db.transferSchemaObject(ctx, "sequence", transferObjectClass, targetSchema, seq.Schema, seq.Name); err != nil {
		return err
	}
	setIfApplied(ctx, &seq.Schema, targetSchema)
	return nil
}

// Rename renames the synonym (sp_rename's 'OBJECT' class). newName is a bare
// name; a rename never moves the synonym between schemas — see Transfer.
func (syn *Synonym) Rename(ctx context.Context, newName string) error {
	if err := syn.db.renameSchemaObject(ctx, "synonym", renameObjectClass, syn.Schema, syn.Name, newName); err != nil {
		return err
	}
	setIfApplied(ctx, &syn.Name, newName)
	return nil
}

// Transfer moves the synonym into another schema (ALTER SCHEMA ... TRANSFER).
// It keeps its name and object_id; permissions granted on it directly are
// dropped by the server.
func (syn *Synonym) Transfer(ctx context.Context, targetSchema string) error {
	if err := syn.db.transferSchemaObject(ctx, "synonym", transferObjectClass, targetSchema, syn.Schema, syn.Name); err != nil {
		return err
	}
	setIfApplied(ctx, &syn.Schema, targetSchema)
	return nil
}
