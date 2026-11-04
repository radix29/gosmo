package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
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
	// IsTypeHidden is true when DataType is a user-defined alias type the
	// login cannot see (no REFERENCES or VIEW DEFINITION on it): DataType is
	// then its base system type and DataTypeSchema "". Without the type
	// LEFT JOINed, such a sequence was left out of Sequences altogether.
	// ScriptSequence refuses it — the script could only name the base type.
	IsTypeHidden bool
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
	return readByName(ctx, d, scanSequence, d.sequenceSelect()+`
WHERE  SCHEMA_NAME(s.schema_id) COLLATE DATABASE_DEFAULT = @p1
  AND  s.name                                            = @p2`, []any{schema, name},
		notFoundf("gosmo: sequence %s not found in %q", qualifiedName(schema, name), d.Name),
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
	// The colSince test, spelled out: the select substitutes NULL below 2017.
	seq := &Sequence{db: d, noLastUsed: d.before(SQLServer2017)}
	var lastUsed sql.NullString
	if err := scan(
		&seq.Name, &seq.Schema, &seq.ObjectID,
		&seq.DataType, &seq.DataTypeSchema, &seq.Precision, &seq.Scale,
		&seq.StartValue, &seq.Increment,
		&seq.MinValue, &seq.MaxValue,
		&seq.IsCycling, &seq.IsCached, &seq.CacheSize,
		&seq.CurrentValue, &lastUsed, &seq.IsTypeHidden,
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
       ` + typeNameSQL("s") + `, ISNULL(SCHEMA_NAME(tp.schema_id), N''), s.precision, s.scale,
       CONVERT(nvarchar(40), s.start_value),
       CONVERT(nvarchar(40), s.increment),
       CONVERT(nvarchar(40), s.minimum_value),
       CONVERT(nvarchar(40), s.maximum_value),
       s.is_cycling, s.is_cached, ISNULL(s.cache_size, 0),
       CONVERT(nvarchar(40), s.current_value),
       ` + colSince(d.serverMajorVersion(), SQLServer2017, "CONVERT(nvarchar(40), s.last_used_value)", "CAST(NULL AS nvarchar(40))") + `,
       ` + typeHiddenSQL + `
FROM   sys.sequences s
LEFT   JOIN sys.types tp ON tp.user_type_id = s.user_type_id`
}

// CreateSequenceRequest describes a new sequence.
type CreateSequenceRequest struct {
	Schema   string
	Name     string
	DataType DataType // defaults to bigint
	// Precision is a decimal or numeric sequence's precision, 1 to 38, written
	// as (Precision, 0): a sequence's scale is always 0. Zero leaves the bare
	// type name, which the server reads as (18, 0). Refused for any other type.
	Precision  int
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
		return nil, invalidf("gosmo: create sequence %q: unrecognized data type %q", req.Name, req.DataType)
	}
	schema := req.Schema
	if err := requireSchema("create sequence", schema, req.Name); err != nil {
		return nil, err
	}
	typeName := string(req.DataType)
	if req.Precision != 0 {
		if req.DataType != DataTypeDecimal && req.DataType != DataTypeNumeric {
			return nil, invalidf("gosmo: create sequence %q: a precision applies only to decimal and numeric, not %q", req.Name, req.DataType)
		}
		if req.Precision < 1 || req.Precision > 38 {
			return nil, invalidf("gosmo: create sequence %q: precision %d is outside 1 to 38", req.Name, req.Precision)
		}
		typeName = fmt.Sprintf("%s(%d, 0)", req.DataType, req.Precision)
	}

	q := fmt.Sprintf("CREATE SEQUENCE %s AS %s", qualifiedName(schema, req.Name), typeName)
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

// SequenceCache is an ALTER SEQUENCE CACHE clause.
type SequenceCache struct {
	// Enabled false is NO CACHE; Size is then ignored.
	Enabled bool
	// Size is CACHE n. Zero, with Enabled, is a bare CACHE: the server picks
	// the size, and sys.sequences reads cache_size NULL (CacheSize 0).
	Size int
}

// SequenceChanges is what ALTER SEQUENCE can change. A nil field leaves that
// setting as stored: the statement omits its clause. The data type and the
// start value have no ALTER; RestartWith moves the start value with the
// current one.
//
// The values are int64, CreateSequenceRequest's, so a decimal(38, 0)
// sequence's range past it cannot be written; an unset field is not touched,
// whatever the sequence holds.
type SequenceChanges struct {
	// RestartWith is RESTART WITH n: the next value handed out is n.
	RestartWith *int64
	// Increment is INCREMENT BY n, never 0.
	Increment *int64
	// MinValue and MaxValue are MINVALUE n and MAXVALUE n. The catalog always
	// holds a concrete bound (NO MINVALUE stores the type's), so a change
	// always names one.
	MinValue *int64
	MaxValue *int64
	// Cycle is CYCLE or NO CYCLE.
	Cycle *bool
	// Cache is CACHE [n] or NO CACHE.
	Cache *SequenceCache
}

// clauses renders ch's clauses in the statement's documented order, or
// refuses what the server would. An empty slice means nothing was set.
func (ch SequenceChanges) clauses() ([]string, error) {
	var c []string
	if ch.RestartWith != nil {
		c = append(c, fmt.Sprintf("RESTART WITH %d", *ch.RestartWith))
	}
	if ch.Increment != nil {
		if *ch.Increment == 0 {
			return nil, invalidf("the increment is 0; a sequence must move")
		}
		c = append(c, fmt.Sprintf("INCREMENT BY %d", *ch.Increment))
	}
	if ch.MinValue != nil {
		c = append(c, fmt.Sprintf("MINVALUE %d", *ch.MinValue))
	}
	if ch.MaxValue != nil {
		c = append(c, fmt.Sprintf("MAXVALUE %d", *ch.MaxValue))
	}
	if ch.MinValue != nil && ch.MaxValue != nil && *ch.MinValue >= *ch.MaxValue {
		return nil, invalidf("the minimum value %d is not below the maximum %d", *ch.MinValue, *ch.MaxValue)
	}
	if ch.Cycle != nil {
		if *ch.Cycle {
			c = append(c, "CYCLE")
		} else {
			c = append(c, "NO CYCLE")
		}
	}
	if k := ch.Cache; k != nil {
		switch {
		case !k.Enabled:
			c = append(c, "NO CACHE")
		case k.Size < 0:
			return nil, invalidf("the cache size is %d; it must be positive", k.Size)
		case k.Size == 0:
			c = append(c, "CACHE")
		default:
			c = append(c, fmt.Sprintf("CACHE %d", k.Size))
		}
	}
	return c, nil
}

// Alter applies every setting on ch in one ALTER SEQUENCE. An empty
// SequenceChanges is a no-op and issues nothing. Restart is its RestartWith
// alone.
func (seq *Sequence) Alter(ctx context.Context, ch SequenceChanges) error {
	if err := requireSchema("alter sequence", seq.Schema, seq.Name); err != nil {
		return err
	}
	clauses, err := ch.clauses()
	if err != nil {
		return fmt.Errorf("gosmo: alter sequence %s: %w", qualifiedName(seq.Schema, seq.Name), err)
	}
	if len(clauses) == 0 {
		return nil
	}
	q := "ALTER SEQUENCE " + qualifiedName(seq.Schema, seq.Name) + " " + strings.Join(clauses, " ")
	if _, err := seq.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: alter sequence %s: %w", qualifiedName(seq.Schema, seq.Name), err)
	}
	str := func(p *int64) *string {
		if p == nil {
			return nil
		}
		return new(strconv.FormatInt(*p, 10))
	}
	if v := str(ch.RestartWith); v != nil {
		// As Restart: the start value moves too, and last_used_value clears.
		setIfApplied(ctx, &seq.StartValue, *v)
		setIfApplied(ctx, &seq.CurrentValue, *v)
		setIfApplied(ctx, &seq.LastUsedValue, "")
	}
	setPtrIfApplied(ctx, &seq.Increment, str(ch.Increment))
	setPtrIfApplied(ctx, &seq.MinValue, str(ch.MinValue))
	setPtrIfApplied(ctx, &seq.MaxValue, str(ch.MaxValue))
	setPtrIfApplied(ctx, &seq.IsCycling, ch.Cycle)
	if k := ch.Cache; k != nil {
		setIfApplied(ctx, &seq.IsCached, k.Enabled)
		size := 0
		if k.Enabled {
			size = k.Size
		}
		setIfApplied(ctx, &seq.CacheSize, size)
	}
	return nil
}

// NextValue retrieves the next value from the sequence.
//
// This deliberately uses execIn, not queryRow: "NEXT VALUE FOR" advances the
// sequence as a side effect of being read, so it isn't safe to retry —
// unlike every other queryRow caller in this package, re-running it on a fresh
// connection after a transient failure could silently skip a value. execIn
// still retries the acquire step (safe, nothing server-side has happened
// yet), just not the query itself.
func (seq *Sequence) NextValue(ctx context.Context) (int64, error) {
	if err := requireSchema("next value for", seq.Schema, seq.Name); err != nil {
		return 0, err
	}
	var val int64
	err := seq.db.execIn(ctx, "SELECT NEXT VALUE FOR "+qualifiedName(seq.Schema, seq.Name), nil,
		func(ctx context.Context, conn sqlConn, q string, args []any) error {
			return conn.QueryRowContext(ctx, q, args...).Scan(&val)
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
	return readByName(ctx, d, scanSynonym, synonymSelect+`
WHERE  SCHEMA_NAME(schema_id) COLLATE DATABASE_DEFAULT = @p1
  AND  name                                            = @p2`, []any{schema, name},
		notFoundf("gosmo: synonym %s not found in %q", qualifiedName(schema, name), d.Name),
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
		return nil, invalidf("gosmo: create synonym %s: invalid base object %q", qualifiedName(schema, req.Name), req.BaseObject)
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
