package gosmo

import (
	"context"
	"database/sql"
	"fmt"
)

// ============================================================
// Sequences  (SQL Server 2012+)
// ============================================================

// Sequence mirrors sys.sequences.
type Sequence struct {
	db           *Database
	Name         string
	Schema       string
	ObjectID     int
	DataType     DataType
	StartValue   int64
	Increment    int64
	MinValue     int64
	MaxValue     int64
	IsCycling    bool
	IsCached     bool
	CacheSize    int
	CurrentValue int64
}

// Sequences returns all sequences in the database.
func (d *Database) Sequences() ([]*Sequence, error) {
	return d.SequencesContext(context.Background())
}

// SequencesContext is the context-aware variant of Sequences.
func (d *Database) SequencesContext(ctx context.Context) ([]*Sequence, error) {
	const q = `
SELECT s.name, SCHEMA_NAME(s.schema_id), s.object_id,
       tp.name,
       CAST(s.start_value AS BIGINT),
       CAST(s.increment AS BIGINT),
       CAST(s.minimum_value AS BIGINT),
       CAST(s.maximum_value AS BIGINT),
       s.is_cycling, s.is_cached, ISNULL(s.cache_size, 0),
       CAST(s.current_value AS BIGINT)
FROM   sys.sequences s
JOIN   sys.types tp ON tp.user_type_id = s.user_type_id
ORDER  BY SCHEMA_NAME(s.schema_id), s.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list sequences: %w", err)
	}
	defer rows.Close()

	var seqs []*Sequence
	for rows.Next() {
		seq := &Sequence{db: d}
		if err := rows.Scan(
			&seq.Name, &seq.Schema, &seq.ObjectID,
			&seq.DataType,
			&seq.StartValue, &seq.Increment,
			&seq.MinValue, &seq.MaxValue,
			&seq.IsCycling, &seq.IsCached, &seq.CacheSize,
			&seq.CurrentValue,
		); err != nil {
			return nil, err
		}
		seqs = append(seqs, seq)
	}
	return seqs, rows.Err()
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
func (d *Database) CreateSequence(req CreateSequenceRequest) error {
	if req.DataType == "" {
		req.DataType = DataTypeBigInt
	}
	schema := req.Schema
	if schema == "" {
		schema = "dbo"
	}

	q := fmt.Sprintf("CREATE SEQUENCE [%s].[%s] AS %s", schema, req.Name, req.DataType)
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

	_, err := d.exec(context.Background(), q)
	if err != nil {
		return fmt.Errorf("gosmo: create sequence [%s].[%s]: %w", schema, req.Name, err)
	}
	return nil
}

// Drop drops the sequence.
func (seq *Sequence) Drop() error {
	_, err := seq.db.exec(context.Background(),
		fmt.Sprintf("DROP SEQUENCE [%s].[%s]", seq.Schema, seq.Name))
	if err != nil {
		return fmt.Errorf("gosmo: drop sequence [%s].[%s]: %w", seq.Schema, seq.Name, err)
	}
	return nil
}

// Restart restarts the sequence at the given value.
func (seq *Sequence) Restart(value int64) error {
	_, err := seq.db.exec(context.Background(),
		fmt.Sprintf("ALTER SEQUENCE [%s].[%s] RESTART WITH %d",
			seq.Schema, seq.Name, value))
	if err != nil {
		return fmt.Errorf("gosmo: restart sequence [%s].[%s]: %w", seq.Schema, seq.Name, err)
	}
	seq.CurrentValue = value
	return nil
}

// NextValue retrieves the next value from the sequence.
func (seq *Sequence) NextValue() (int64, error) {
	row, _, _ := seq.db.queryRow(context.Background(),
		fmt.Sprintf("SELECT NEXT VALUE FOR [%s].[%s]", seq.Schema, seq.Name))
	var val int64
	if err := row.Scan(&val); err != nil {
		return 0, fmt.Errorf("gosmo: next value for [%s].[%s]: %w", seq.Schema, seq.Name, err)
	}
	seq.CurrentValue = val
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

// Synonyms returns all synonyms in the database.
func (d *Database) Synonyms() ([]*Synonym, error) {
	return d.SynonymsContext(context.Background())
}

// SynonymsContext is the context-aware variant of Synonyms.
func (d *Database) SynonymsContext(ctx context.Context) ([]*Synonym, error) {
	const q = `
SELECT name, SCHEMA_NAME(schema_id), object_id, base_object_name
FROM   sys.synonyms
ORDER  BY SCHEMA_NAME(schema_id), name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list synonyms: %w", err)
	}
	defer rows.Close()

	var syns []*Synonym
	for rows.Next() {
		s := &Synonym{db: d}
		var baseObj sql.NullString
		if err := rows.Scan(&s.Name, &s.Schema, &s.ObjectID, &baseObj); err != nil {
			return nil, err
		}
		s.BaseObject = baseObj.String
		syns = append(syns, s)
	}
	return syns, rows.Err()
}

// CreateSynonym creates a synonym for a base object.
// baseObject should be the fully qualified name, e.g. "[OtherDB].[dbo].[MyTable]".
func (d *Database) CreateSynonym(schema, name, baseObject string) error {
	if schema == "" {
		schema = "dbo"
	}
	_, err := d.exec(context.Background(),
		fmt.Sprintf("CREATE SYNONYM [%s].[%s] FOR %s", schema, name, baseObject))
	if err != nil {
		return fmt.Errorf("gosmo: create synonym [%s].[%s]: %w", schema, name, err)
	}
	return nil
}

// Drop drops the synonym.
func (syn *Synonym) Drop() error {
	_, err := syn.db.exec(context.Background(),
		fmt.Sprintf("DROP SYNONYM IF EXISTS [%s].[%s]", syn.Schema, syn.Name))
	if err != nil {
		return fmt.Errorf("gosmo: drop synonym [%s].[%s]: %w", syn.Schema, syn.Name, err)
	}
	return nil
}
