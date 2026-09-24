package gosmo

import (
	"context"
	"fmt"
	"math/big"
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
	isConstraint := idx.IsPrimaryKey || idx.IsUniqueConstraint
	drop := fmt.Sprintf("DROP INDEX IF EXISTS %s ON %s;\nGO\n", quoteIdent(idx.Name), tableName)
	if isConstraint {
		drop = fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s;\nGO\n", tableName, quoteIdent(idx.Name))
	}
	return opts.envelope(drop, "", func(sb *strings.Builder) {
		if isConstraint {
			sb.WriteString(scriptKeyConstraint(idx, tableName, opts))
		} else {
			sb.WriteString(scriptIndex(idx, tableName, opts))
		}
		sb.WriteString(indexDisableStatement(idx, tableName))
	})
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
	drop := fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s;\nGO\n", tableName, quoteIdent(ck.Name))
	return opts.envelope(drop, "", func(sb *strings.Builder) {
		sb.WriteString(scriptCheckConstraint(ck, tableName, opts))
	})
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
	exists := fmt.Sprintf("EXISTS (SELECT 1 FROM sys.stats WHERE name = N'%s' AND object_id = OBJECT_ID(N'%s'))",
		escapeSingle(st.Name), escapeSingle(tableName))
	drop := fmt.Sprintf("IF %s\n    DROP STATISTICS %s.%s;\nGO\n", exists, tableName, quoteIdent(st.Name))
	guard := fmt.Sprintf("IF NOT %s\n", exists)
	return opts.envelopeErr(drop, guard, func(sb *strings.Builder) error {
		stmt, err := buildCreateStatisticStatement(tableName, CreateStatisticRequest{
			Name:             st.Name,
			Columns:          cols,
			FilterDefinition: st.FilterDef,
			NoRecompute:      st.NoRecompute,
			Incremental:      st.IsIncremental,
		})
		if err != nil {
			return err
		}
		sb.WriteString(stmt)
		sb.WriteString(";\nGO\n")
		return nil
	})
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
	drop := fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s;\nGO\n", tableName, quoteIdent(fk.Name))
	return opts.envelope(drop, "", func(sb *strings.Builder) {
		sb.WriteString(scriptForeignKey(fk, tableName, opts))
	})
}

// ScriptSequence generates the CREATE (or DROP) script for one sequence.
func (sc *Scripter) ScriptSequence(ctx context.Context, schema, name string) (string, error) {
	seq, err := sc.db.SequenceByName(ctx, schema, name)
	if err != nil {
		return "", err
	}
	return buildSequenceScript(seq, sc.opts), nil
}

// buildSequenceScript assembles one sequence's script. START WITH is the
// next value the sequence would hand out (sequenceStartWith), not the value
// it was created with: scripting a sequence and running the script elsewhere
// has to keep handing out unused numbers.
func buildSequenceScript(seq *Sequence, opts ScriptOptions) string {
	fullName := qualifiedName(seq.Schema, seq.Name)
	drop := fmt.Sprintf("DROP SEQUENCE IF EXISTS %s;\nGO\n", fullName)
	return opts.envelope(drop, "", func(sb *strings.Builder) {
		// A sequence may be declared over a user-defined alias type, whose name
		// alone resolves against the executing principal's default schema when
		// the script is re-run — a different type, or none. Qualify anything
		// that is not a built-in. A built-in decimal/numeric carries its
		// precision: the bare name means (18,0), so numeric(12,0) came back
		// wider than it went in.
		dataType := quoteIdent(string(seq.DataType))
		if s := seq.DataTypeSchema; s != "" && !strings.EqualFold(s, "sys") {
			dataType = qualifiedName(s, string(seq.DataType))
		} else if (seq.DataType == DataTypeDecimal || seq.DataType == DataTypeNumeric) && seq.Precision > 0 {
			dataType += fmt.Sprintf("(%d,%d)", seq.Precision, seq.Scale)
		}
		start, exhausted := sequenceStartWith(seq)
		if exhausted {
			// The name stays out of the comment: a bracketed name may hold a
			// line break, which would end the comment and turn the rest into
			// T-SQL.
			fmt.Fprintf(sb, "-- This sequence is exhausted: it does not cycle and has handed out\n"+
				"-- its last value, %s. START WITH repeats that value, so the\n"+
				"-- recreated sequence hands it out once more, then is exhausted too.\n", start)
		}
		if opts.IncludeIfNotExists {
			fmt.Fprintf(sb, "IF OBJECT_ID(N'%s', N'SO') IS NULL\n", escapeSingle(fullName))
		}
		fmt.Fprintf(sb, "CREATE SEQUENCE %s\n    AS %s\n    START WITH %s\n    INCREMENT BY %s\n    MINVALUE %s\n    MAXVALUE %s\n",
			fullName, dataType, start, seq.Increment, seq.MinValue, seq.MaxValue)
		if seq.IsCycling {
			sb.WriteString("    CYCLE\n")
		} else {
			sb.WriteString("    NO CYCLE\n")
		}
		switch {
		case seq.IsCached && seq.CacheSize > 0:
			fmt.Fprintf(sb, "    CACHE %d;\n", seq.CacheSize)
		case seq.IsCached:
			sb.WriteString("    CACHE;\n")
		default:
			sb.WriteString("    NO CACHE;\n")
		}
		sb.WriteString("GO\n")
	})
}

// sequenceStartWith returns the value a recreated seq must START WITH so that
// its first NEXT VALUE FOR is one the original has not handed out yet.
//
// current_value is the *last* value handed out, so starting there re-issues
// it — a duplicate key the first time the copy is used. The next value is
// last_used_value + increment; when last_used_value is NULL nothing has been
// handed out since CREATE or RESTART, and start_value (which RESTART moves)
// is next. Before 2017 there is no last_used_value, and current_value +
// increment is used instead: for a sequence never used, that skips one
// number, but it never repeats one.
//
// Past the end of its range a cycling sequence wraps to MINVALUE (MAXVALUE
// for a negative increment). A non-cycling one is exhausted: there is no
// unused value to start at, so its last value is returned with exhausted
// set, and the caller says so in the script.
func sequenceStartWith(seq *Sequence) (start string, exhausted bool) {
	last := seq.LastUsedValue
	if last == "" && seq.noLastUsed {
		last = seq.CurrentValue
	}
	if last == "" {
		return seq.StartValue, false
	}
	lastN, ok1 := new(big.Int).SetString(last, 10)
	inc, ok2 := new(big.Int).SetString(seq.Increment, 10)
	lo, ok3 := new(big.Int).SetString(seq.MinValue, 10)
	hi, ok4 := new(big.Int).SetString(seq.MaxValue, 10)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		// Not reachable from a catalog read — a sequence's values are
		// integers — but a hand-built Sequence may carry anything; its
		// last value is at least within range.
		return last, false
	}
	next := new(big.Int).Add(lastN, inc)
	switch {
	case inc.Sign() > 0 && next.Cmp(hi) > 0, inc.Sign() < 0 && next.Cmp(lo) < 0:
		if !seq.IsCycling {
			return last, true
		}
		if inc.Sign() > 0 {
			return seq.MinValue, false
		}
		return seq.MaxValue, false
	}
	return next.String(), false
}

// ScriptSynonym generates the CREATE (or DROP) script for one synonym.
func (sc *Scripter) ScriptSynonym(ctx context.Context, schema, name string) (string, error) {
	syn, err := sc.db.SynonymByName(ctx, schema, name)
	if err != nil {
		return "", err
	}
	return buildSynonymScript(syn, sc.opts), nil
}

// buildSynonymScript assembles one synonym's script. BaseObject comes from
// sys.synonyms already bracket-quoted, so it is emitted as stored.
func buildSynonymScript(syn *Synonym, opts ScriptOptions) string {
	fullName := qualifiedName(syn.Schema, syn.Name)
	drop := fmt.Sprintf("DROP SYNONYM IF EXISTS %s;\nGO\n", fullName)
	guard := fmt.Sprintf("IF OBJECT_ID(N'%s', N'SN') IS NULL\n", escapeSingle(fullName))
	return opts.envelope(drop, guard, func(sb *strings.Builder) {
		fmt.Fprintf(sb, "CREATE SYNONYM %s FOR %s;\nGO\n", fullName, syn.BaseObject)
	})
}

// ScriptPartitionFunction generates the CREATE (or DROP) script for one
// partition function.
func (sc *Scripter) ScriptPartitionFunction(ctx context.Context, name string) (string, error) {
	pf, err := sc.db.PartitionFunctionByName(ctx, name)
	if err != nil {
		return "", err
	}
	return buildPartitionFunctionScript(pf, sc.opts), nil
}

// buildPartitionFunctionScript assembles one partition function's script.
func buildPartitionFunctionScript(pf *PartitionFunction, opts ScriptOptions) string {
	drop := fmt.Sprintf("DROP PARTITION FUNCTION IF EXISTS %s;\nGO\n", quoteIdent(pf.Name))
	guard := fmt.Sprintf("IF NOT EXISTS (SELECT 1 FROM sys.partition_functions WHERE name = N'%s')\n",
		escapeSingle(pf.Name))
	return opts.envelope(drop, guard, func(sb *strings.Builder) {
		side := "LEFT"
		if pf.IsRight {
			side = "RIGHT"
		}
		fmt.Fprintf(sb, "CREATE PARTITION FUNCTION %s (%s)\n    AS RANGE %s FOR VALUES (%s);\nGO\n",
			quoteIdent(pf.Name), sqlTypeString(pf.InputType, pf.MaxLength, pf.Precision, pf.Scale), side,
			strings.Join(pf.Boundaries, ", "))
	})
}

// ScriptPartitionScheme generates the CREATE (or DROP) script for one
// partition scheme.
func (sc *Scripter) ScriptPartitionScheme(ctx context.Context, name string) (string, error) {
	ps, err := sc.db.PartitionSchemeByName(ctx, name)
	if err != nil {
		return "", err
	}
	return buildPartitionSchemeScript(ps, sc.opts), nil
}

// buildPartitionSchemeScript assembles one partition scheme's script.
func buildPartitionSchemeScript(ps *PartitionScheme, opts ScriptOptions) string {
	drop := fmt.Sprintf("DROP PARTITION SCHEME IF EXISTS %s;\nGO\n", quoteIdent(ps.Name))
	guard := fmt.Sprintf("IF NOT EXISTS (SELECT 1 FROM sys.partition_schemes WHERE name = N'%s')\n",
		escapeSingle(ps.Name))
	return opts.envelope(drop, guard, func(sb *strings.Builder) {
		fgs := make([]string, len(ps.FileGroups))
		for i, fg := range ps.FileGroups {
			fgs[i] = quoteIdent(fg)
		}
		fmt.Fprintf(sb, "CREATE PARTITION SCHEME %s\n    AS PARTITION %s TO (%s);\nGO\n",
			quoteIdent(ps.Name), quoteIdent(ps.FunctionName), strings.Join(fgs, ", "))
	})
}
