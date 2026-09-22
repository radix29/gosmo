package gosmo

import (
	"context"
	"fmt"
	"strings"
)

// ============================================================
// Scripter — schema objects below the table (index, constraint,
// sequence, synonym)
// ============================================================

// ScriptIndex generates the CREATE (or DROP) script for one index on a
// table. An index backing a primary key or unique constraint is scripted as
// the ALTER TABLE ... ADD CONSTRAINT it really is — CREATE INDEX cannot
// recreate it.
func (sc *Scripter) ScriptIndex(ctx context.Context, schema, table, name string) (string, error) {
	t, err := sc.db.TableByName(ctx, schema, table)
	if err != nil {
		return "", err
	}
	indexes, err := t.Indexes(ctx)
	if err != nil {
		return "", err
	}
	for _, idx := range indexes {
		if strings.EqualFold(idx.Name, name) {
			return buildIndexScript(idx, qualifiedName(schema, table), sc.opts), nil
		}
	}
	return "", notFoundf("gosmo: index %s on %s not found", quoteIdent(name), qualifiedName(schema, table))
}

// buildIndexScript assembles one index's script from metadata already read.
func buildIndexScript(idx *Index, tableName string, opts ScriptOptions) string {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		if idx.IsPrimaryKey || idx.IsUniqueConstraint {
			fmt.Fprintf(&sb, "ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s;\nGO\n", tableName, quoteIdent(idx.Name))
		} else {
			fmt.Fprintf(&sb, "DROP INDEX IF EXISTS %s ON %s;\nGO\n", quoteIdent(idx.Name), tableName)
		}
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}
	if idx.IsPrimaryKey || idx.IsUniqueConstraint {
		sb.WriteString(scriptKeyConstraint(idx, tableName, opts))
	} else {
		sb.WriteString(scriptIndex(idx, tableName, opts))
	}
	sb.WriteString(indexDisableStatement(idx, tableName))
	return sb.String()
}

// scriptKeyConstraint renders the primary key or unique constraint idx backs
// as the ALTER TABLE ... ADD CONSTRAINT that creates it.
func scriptKeyConstraint(idx *Index, tableName string, opts ScriptOptions) string {
	if !idx.IsPrimaryKey {
		return scriptUniqueConstraint(idx, tableName, opts)
	}
	var sb strings.Builder
	if opts.IncludeIfNotExists {
		sb.WriteString(constraintExistenceGuard(idx.Name, tableName))
	}
	clust := "NONCLUSTERED"
	if idx.IsClustered {
		clust = "CLUSTERED"
	}
	fmt.Fprintf(&sb, "ALTER TABLE %s\n    ADD CONSTRAINT %s PRIMARY KEY %s (%s)%s%s;\nGO\n\n",
		tableName, quoteIdent(idx.Name), clust, indexColumnList(idx.KeyColumns),
		indexWithClause(idx, " "), dataSpaceClause(idx.DataSpace))
	return sb.String()
}

// ScriptCheckConstraint generates the script for one CHECK constraint.
func (sc *Scripter) ScriptCheckConstraint(ctx context.Context, schema, table, name string) (string, error) {
	t, err := sc.db.TableByName(ctx, schema, table)
	if err != nil {
		return "", err
	}
	checks, err := t.CheckConstraints(ctx)
	if err != nil {
		return "", err
	}
	for _, ck := range checks {
		if strings.EqualFold(ck.Name, name) {
			return buildCheckConstraintScript(ck, qualifiedName(schema, table), sc.opts), nil
		}
	}
	return "", notFoundf("gosmo: check constraint %s on %s not found", quoteIdent(name), qualifiedName(schema, table))
}

// buildCheckConstraintScript assembles one CHECK constraint's script.
func buildCheckConstraintScript(ck *CheckConstraint, tableName string, opts ScriptOptions) string {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s;\nGO\n", tableName, quoteIdent(ck.Name))
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}
	sb.WriteString(scriptCheckConstraint(ck, tableName, opts))
	return sb.String()
}

// scriptCheckConstraint renders the ALTER TABLE ... ADD CONSTRAINT that
// creates ck — shared by the single-constraint script and the table's.
//
// A constraint is recreated as trusted as it was. An untrusted one — disabled,
// or enabled WITH NOCHECK — is added WITH NOCHECK, which skips the check of
// existing rows and leaves it untrusted; a disabled one is then switched off
// by the trailing NOCHECK CONSTRAINT. Adding an untrusted constraint WITH
// CHECK would instead fail on exactly the rows it was left untrusted for.
func scriptCheckConstraint(ck *CheckConstraint, tableName string, opts ScriptOptions) string {
	var sb strings.Builder
	if opts.IncludeIfNotExists {
		sb.WriteString(constraintExistenceGuard(ck.Name, tableName))
	}
	with := "WITH CHECK"
	if ck.IsDisabled || ck.IsNotTrusted {
		with = "WITH NOCHECK"
	}
	nfr := ""
	if ck.IsNotForReplication {
		nfr = " NOT FOR REPLICATION"
	}
	fmt.Fprintf(&sb, "ALTER TABLE %s %s\n    ADD CONSTRAINT %s CHECK%s %s;\nGO\n",
		tableName, with, quoteIdent(ck.Name), nfr, ck.Definition)
	if ck.IsDisabled {
		fmt.Fprintf(&sb, "ALTER TABLE %s NOCHECK CONSTRAINT %s;\nGO\n", tableName, quoteIdent(ck.Name))
	}
	sb.WriteString("\n")
	return sb.String()
}

// ScriptStatistic generates the CREATE (or DROP) script for one statistics
// object on a table.
//
// A statistic an index maintains is refused: it is created and dropped with
// its index, so CREATE STATISTICS under its name collides with the index and
// DROP STATISTICS on it fails (Msg 3739).
func (sc *Scripter) ScriptStatistic(ctx context.Context, schema, table, name string) (string, error) {
	t, err := sc.db.TableByName(ctx, schema, table)
	if err != nil {
		return "", err
	}
	st, err := t.StatisticByName(ctx, name)
	if err != nil {
		return "", err
	}
	if !st.IsAutoCreated && !st.IsUserCreated {
		return "", fmt.Errorf("gosmo: script statistic %q on %s: it belongs to the index of the same name; script the index instead",
			name, t.FullName())
	}
	var cols []string
	if v := sc.opts.verb(); v != ScriptDrop {
		if cols, err = st.Columns(ctx); err != nil {
			return "", err
		}
	}
	return buildStatisticScript(st, cols, t.FullName(), sc.opts)
}

// buildStatisticScript assembles one statistic's script from metadata
// already read: its filter and its NORECOMPUTE and INCREMENTAL options, the
// three things that make it the statistic it is. The sampling it was last
// built with is not a property of the statistic and is not scripted; the
// server picks its default sample, as SSMS's script leaves it to.
//
// The DROP is always guarded: DROP STATISTICS has no IF EXISTS form, and the
// DROP half of DROP And CREATE must be re-runnable.
func buildStatisticScript(st *Statistic, cols []string, tableName string, opts ScriptOptions) (string, error) {
	var sb strings.Builder
	guard := fmt.Sprintf("EXISTS (SELECT 1 FROM sys.stats WHERE name = N'%s' AND object_id = OBJECT_ID(N'%s'))",
		escapeSingle(st.Name), escapeSingle(tableName))
	v := opts.verb()
	if v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "IF %s\n    DROP STATISTICS %s.%s;\nGO\n", guard, tableName, quoteIdent(st.Name))
		if v == ScriptDrop {
			return sb.String(), nil
		}
		sb.WriteString("\n")
	}
	stmt, err := buildCreateStatisticStatement(tableName, CreateStatisticRequest{
		Name:             st.Name,
		Columns:          cols,
		FilterDefinition: st.FilterDef,
		NoRecompute:      st.NoRecompute,
		Incremental:      st.IsIncremental,
	})
	if err != nil {
		return "", err
	}
	if opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF NOT %s\n", guard)
	}
	sb.WriteString(stmt)
	sb.WriteString(";\nGO\n")
	return sb.String(), nil
}

// ScriptForeignKey generates the script for one foreign key.
func (sc *Scripter) ScriptForeignKey(ctx context.Context, schema, table, name string) (string, error) {
	t, err := sc.db.TableByName(ctx, schema, table)
	if err != nil {
		return "", err
	}
	fks, err := t.ForeignKeys(ctx)
	if err != nil {
		return "", err
	}
	for _, fk := range fks {
		if strings.EqualFold(fk.Name, name) {
			return buildForeignKeyScript(fk, qualifiedName(schema, table), sc.opts), nil
		}
	}
	return "", notFoundf("gosmo: foreign key %s on %s not found", quoteIdent(name), qualifiedName(schema, table))
}

// buildForeignKeyScript assembles one foreign key's script, reusing the same
// renderer ScriptTable uses for the table's own keys.
func buildForeignKeyScript(fk *ForeignKey, tableName string, opts ScriptOptions) string {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s;\nGO\n", tableName, quoteIdent(fk.Name))
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}
	sb.WriteString(scriptForeignKey(fk, tableName, opts))
	return sb.String()
}

// ScriptSequence generates the CREATE (or DROP) script for one sequence.
func (sc *Scripter) ScriptSequence(ctx context.Context, schema, name string) (string, error) {
	seqs, err := sc.db.Sequences(ctx)
	if err != nil {
		return "", err
	}
	for _, seq := range seqs {
		if strings.EqualFold(seq.Schema, schema) && strings.EqualFold(seq.Name, name) {
			return buildSequenceScript(seq, sc.opts), nil
		}
	}
	return "", notFoundf("gosmo: sequence %s not found", qualifiedName(schema, name))
}

// buildSequenceScript assembles one sequence's script. START WITH is the
// sequence's *current* value, not the value it was created with: scripting a
// sequence and running the script elsewhere has to keep handing out unused
// numbers, which is what SSMS does too.
func buildSequenceScript(seq *Sequence, opts ScriptOptions) string {
	fullName := qualifiedName(seq.Schema, seq.Name)
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "DROP SEQUENCE IF EXISTS %s;\nGO\n", fullName)
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}
	if opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF OBJECT_ID(N'%s', N'SO') IS NULL\n", escapeSingle(fullName))
	}
	// A sequence may be declared over a user-defined alias type, whose name
	// alone resolves against the executing principal's default schema when
	// the script is re-run — a different type, or none. Qualify anything
	// that is not a built-in.
	dataType := quoteIdent(string(seq.DataType))
	if s := seq.DataTypeSchema; s != "" && !strings.EqualFold(s, "sys") {
		dataType = qualifiedName(s, string(seq.DataType))
	}
	fmt.Fprintf(&sb, "CREATE SEQUENCE %s\n    AS %s\n    START WITH %d\n    INCREMENT BY %d\n    MINVALUE %d\n    MAXVALUE %d\n",
		fullName, dataType, seq.CurrentValue, seq.Increment, seq.MinValue, seq.MaxValue)
	if seq.IsCycling {
		sb.WriteString("    CYCLE\n")
	} else {
		sb.WriteString("    NO CYCLE\n")
	}
	switch {
	case seq.IsCached && seq.CacheSize > 0:
		fmt.Fprintf(&sb, "    CACHE %d;\n", seq.CacheSize)
	case seq.IsCached:
		sb.WriteString("    CACHE;\n")
	default:
		sb.WriteString("    NO CACHE;\n")
	}
	sb.WriteString("GO\n")
	return sb.String()
}

// ScriptSynonym generates the CREATE (or DROP) script for one synonym.
func (sc *Scripter) ScriptSynonym(ctx context.Context, schema, name string) (string, error) {
	syns, err := sc.db.Synonyms(ctx)
	if err != nil {
		return "", err
	}
	for _, syn := range syns {
		if strings.EqualFold(syn.Schema, schema) && strings.EqualFold(syn.Name, name) {
			return buildSynonymScript(syn, sc.opts), nil
		}
	}
	return "", notFoundf("gosmo: synonym %s not found", qualifiedName(schema, name))
}

// buildSynonymScript assembles one synonym's script. BaseObject comes from
// sys.synonyms already bracket-quoted, so it is emitted as stored.
func buildSynonymScript(syn *Synonym, opts ScriptOptions) string {
	fullName := qualifiedName(syn.Schema, syn.Name)
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "DROP SYNONYM IF EXISTS %s;\nGO\n", fullName)
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}
	if opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF OBJECT_ID(N'%s', N'SN') IS NULL\n", escapeSingle(fullName))
	}
	fmt.Fprintf(&sb, "CREATE SYNONYM %s FOR %s;\nGO\n", fullName, syn.BaseObject)
	return sb.String()
}

// ScriptPartitionFunction generates the CREATE (or DROP) script for one
// partition function.
func (sc *Scripter) ScriptPartitionFunction(ctx context.Context, name string) (string, error) {
	pfs, err := sc.db.PartitionFunctions(ctx)
	if err != nil {
		return "", err
	}
	for _, pf := range pfs {
		if strings.EqualFold(pf.Name, name) {
			return buildPartitionFunctionScript(pf, sc.opts), nil
		}
	}
	return "", notFoundf("gosmo: partition function %s not found", quoteIdent(name))
}

// buildPartitionFunctionScript assembles one partition function's script.
func buildPartitionFunctionScript(pf *PartitionFunction, opts ScriptOptions) string {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "DROP PARTITION FUNCTION IF EXISTS %s;\nGO\n", quoteIdent(pf.Name))
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}
	if opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF NOT EXISTS (SELECT 1 FROM sys.partition_functions WHERE name = N'%s')\n",
			escapeSingle(pf.Name))
	}
	side := "LEFT"
	if pf.IsRight {
		side = "RIGHT"
	}
	values := make([]string, len(pf.Boundaries))
	for i, b := range pf.Boundaries {
		values[i] = partitionBoundaryLiteral(pf.InputType, b)
	}
	fmt.Fprintf(&sb, "CREATE PARTITION FUNCTION %s (%s)\n    AS RANGE %s FOR VALUES (%s);\nGO\n",
		quoteIdent(pf.Name), sqlTypeString(pf.InputType, pf.MaxLength, pf.Precision, pf.Scale), side,
		strings.Join(values, ", "))
	return sb.String()
}

// partitionBoundaryLiteral renders one boundary value as a literal of the
// function's input type. sys.partition_range_values hands every value back
// as text, so a date or string boundary has to be quoted again or the
// generated CREATE won't parse.
func partitionBoundaryLiteral(dt DataType, value string) string {
	switch dt {
	case DataTypeBigInt, DataTypeInt, DataTypeSmallInt, DataTypeTinyInt, DataTypeBit,
		DataTypeDecimal, DataTypeNumeric, DataTypeFloat, DataTypeReal,
		DataTypeMoney, DataTypeSmallMoney:
		return value
	}
	return "N'" + escapeSingle(value) + "'"
}

// ScriptPartitionScheme generates the CREATE (or DROP) script for one
// partition scheme.
func (sc *Scripter) ScriptPartitionScheme(ctx context.Context, name string) (string, error) {
	schemes, err := sc.db.PartitionSchemes(ctx)
	if err != nil {
		return "", err
	}
	for _, ps := range schemes {
		if strings.EqualFold(ps.Name, name) {
			return buildPartitionSchemeScript(ps, sc.opts), nil
		}
	}
	return "", notFoundf("gosmo: partition scheme %s not found", quoteIdent(name))
}

// buildPartitionSchemeScript assembles one partition scheme's script.
func buildPartitionSchemeScript(ps *PartitionScheme, opts ScriptOptions) string {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "DROP PARTITION SCHEME IF EXISTS %s;\nGO\n", quoteIdent(ps.Name))
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}
	if opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF NOT EXISTS (SELECT 1 FROM sys.partition_schemes WHERE name = N'%s')\n",
			escapeSingle(ps.Name))
	}
	fgs := make([]string, len(ps.FileGroups))
	for i, fg := range ps.FileGroups {
		fgs[i] = quoteIdent(fg)
	}
	fmt.Fprintf(&sb, "CREATE PARTITION SCHEME %s\n    AS PARTITION %s TO (%s);\nGO\n",
		quoteIdent(ps.Name), quoteIdent(ps.FunctionName), strings.Join(fgs, ", "))
	return sb.String()
}
