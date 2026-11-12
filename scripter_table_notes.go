package gosmo

import (
	"context"
	"fmt"
	"strings"

	"github.com/radix29/gosmo/tsql"
)

// previewFeaturesNote is a comment naming the PREVIEW_FEATURES database
// scoped configuration when a column is vector(n, float16), "" otherwise.
// SQL Server 2025 parses float16 only with that setting on in the current
// database; off, the CREATE TABLE fails with Msg 195, "'float16' is not a
// recognized vector base type". The script names the setting rather than
// setting it: replaying a table script must not silently change a
// database-wide configuration of its target.
func previewFeaturesNote(cols []*Column) string {
	var names []string
	for _, col := range cols {
		if col.GraphType != 0 || col.IsDroppedLedgerColumn {
			continue
		}
		if col.DataType == DataTypeVector && strings.EqualFold(col.VectorBaseType, "float16") {
			names = append(names, tsql.CommentSafe(quoteIdent(col.Name)))
		}
	}
	return float16Note(names)
}

// float16Note is previewFeaturesNote's comment for names already made
// comment-safe — columns, or a module's parameters — "" for none.
func float16Note(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return previewFeaturesComment(strings.Join(names, ", ") + ": vector(n, float16) is")
}

// float16DefinitionNote is the same comment for a module whose definition
// uses the type where nothing catalogs it by name (usesFloat16Vector).
var float16DefinitionNote = previewFeaturesComment("The definition uses vector(n, float16),")

// float16DynamicNote is the comment for a module that spells the type only in
// dynamic SQL (buildsFloat16Vector): the script replays, the module fails
// when it runs the batch.
var float16DynamicNote = "-- The definition's dynamic SQL uses vector(n, float16), a SQL Server 2025\n" +
	"-- preview feature. The module fails with Msg 195 when it runs it unless\n" +
	"-- the target database enables it:\n" +
	"--   ALTER DATABASE SCOPED CONFIGURATION SET PREVIEW_FEATURES = ON;\n"

// previewFeaturesComment completes the PREVIEW_FEATURES comment after lead,
// which must already be comment-safe.
func previewFeaturesComment(lead string) string {
	return "-- " + lead + " a SQL Server 2025 preview feature. This script\n" +
		"-- fails with Msg 195 unless the target database enables it first:\n" +
		"--   ALTER DATABASE SCOPED CONFIGURATION SET PREVIEW_FEATURES = ON;\n"
}

// schemaBoundDependent is a module bound to a table WITH SCHEMABINDING —
// every natively compiled module is — which DROP TABLE cannot get past.
type schemaBoundDependent struct {
	schema, name, typeDesc string
	native                 bool
}

// schemaBoundDependents lists the modules schema-bound to t, other than its
// own triggers (a natively compiled trigger is schema-bound to its table, and
// DROP TABLE takes it along rather than refusing).
//
// hidden reports that the login could not read them: without VIEW DEFINITION
// on the database the view returns no rows at all (Database.HasPermission),
// so an empty list is then no answer. Without SELECT on the view, which
// public lacks and db_datareader has, the read is refused outright (Msg 229),
// and that error is returned. The probe runs only when the list is empty: a
// row at all means the permission is held.
func (t *Table) schemaBoundDependents(ctx context.Context) (deps []schemaBoundDependent, hidden bool, err error) {
	const q = `
SELECT DISTINCT SCHEMA_NAME(o.schema_id), o.name, o.type_desc,
       CAST(ISNULL(m.uses_native_compilation, 0) AS bit)
FROM   sys.sql_expression_dependencies sed
JOIN   sys.objects o ON o.object_id = sed.referencing_id
LEFT JOIN sys.sql_modules m ON m.object_id = o.object_id
WHERE  sed.referenced_id = @p1 AND sed.is_schema_bound_reference = 1
  AND  o.object_id <> @p1 AND o.parent_object_id <> @p1
ORDER  BY 1, 2`
	rows, err := t.db.query(ctx, q, t.ObjectID)
	deps, err = scanRows(rows, err, fmt.Sprintf("schema-bound dependents of %s", t.FullName()), func(scan func(...any) error) (schemaBoundDependent, error) {
		var d schemaBoundDependent
		err := scan(&d.schema, &d.name, &d.typeDesc, &d.native)
		return d, err
	})
	if err != nil || len(deps) > 0 {
		return deps, false, err
	}
	visible, err := t.db.HasPermission(ctx, "VIEW DEFINITION")
	if err != nil {
		return nil, false, fmt.Errorf("gosmo: schema-bound dependents of %s: %w", t.FullName(), err)
	}
	return nil, !visible, nil
}

// schemaBoundNote is a comment ahead of a table's DROP naming the modules
// schema-bound to it, "" when there are none. DROP TABLE fails on them with
// Msg 3729 and leaves the table in place, so a DROP AND CREATE's guarded
// CREATE is then skipped and the script changes nothing. A temporal table's
// SET (SYSTEM_VERSIONING = OFF) is refused the same way, so it too stays as
// it was (its DROP then fails with Msg 13552). The script names them rather
// than dropping them: they are other objects, with their own permissions,
// and SSMS's table script does not touch them either.
//
// hidden is a login that could not read them (see schemaBoundDependents):
// the note then says so rather than going missing, which would read as
// "nothing blocks this DROP". A comment, not a refusal: the script is still
// right, and its CREATE half needs no such read.
func schemaBoundNote(fullName string, deps []schemaBoundDependent, hidden bool) string {
	if hidden {
		return fmt.Sprintf("-- Could not check what is schema-bound to %s: without VIEW DEFINITION on\n"+
			"-- the database, sys.sql_expression_dependencies lists nothing. A module\n"+
			"-- schema-bound to it makes this DROP fail with Msg 3729.\n", tsql.CommentSafe(fullName))
	}
	if len(deps) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "-- This DROP fails with Msg 3729 while these are schema-bound to %s:\n", tsql.CommentSafe(fullName))
	native := false
	for _, d := range deps {
		kind := d.typeDesc
		if d.native {
			kind += ", natively compiled"
			native = true
		}
		fmt.Fprintf(&sb, "--   %s (%s)\n", tsql.CommentSafe(qualifiedName(d.schema, d.name)), kind)
	}
	sb.WriteString("-- Drop them first, or ALTER them without SCHEMABINDING.\n")
	if native {
		sb.WriteString("-- A natively compiled module requires SCHEMABINDING: drop it and recreate it after.\n")
	}
	return sb.String()
}

// referencingForeignKey is a foreign key on another table that references
// this one, which DROP TABLE cannot get past.
type referencingForeignKey struct {
	schema, table, name string
}

// referencingForeignKeys lists the foreign keys on other tables that
// reference t. A self-reference is left out: DROP TABLE takes it along.
func (t *Table) referencingForeignKeys(ctx context.Context) ([]referencingForeignKey, error) {
	const q = `
SELECT SCHEMA_NAME(p.schema_id), p.name, fk.name
FROM   sys.foreign_keys fk
JOIN   sys.objects p ON p.object_id = fk.parent_object_id
WHERE  fk.referenced_object_id = @p1 AND fk.parent_object_id <> @p1
ORDER  BY 1, 2, 3`
	rows, err := t.db.query(ctx, q, t.ObjectID)
	return scanRows(rows, err, fmt.Sprintf("foreign keys referencing %s", t.FullName()), func(scan func(...any) error) (referencingForeignKey, error) {
		var fk referencingForeignKey
		err := scan(&fk.schema, &fk.table, &fk.name)
		return fk, err
	})
}

// referencingForeignKeyNote is a comment ahead of a table's DROP naming the
// foreign keys on other tables that reference it, "" when there are none.
// DROP TABLE fails on them with Msg 3726 and leaves the table in place, so
// a DROP AND CREATE's CREATE then fails or is skipped and the script changes
// nothing — except that a temporal table's SET (SYSTEM_VERSIONING = OFF),
// which they do not block, has already run and stays run. The script names
// them rather than dropping them, as SSMS's table script does not either:
// they belong to other tables.
func referencingForeignKeyNote(fullName string, fks []referencingForeignKey, versioned bool) string {
	if len(fks) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "-- This DROP fails with Msg 3726 while these foreign keys reference %s:\n", tsql.CommentSafe(fullName))
	for _, fk := range fks {
		fmt.Fprintf(&sb, "--   %s.%s (FOREIGN KEY)\n", tsql.CommentSafe(qualifiedName(fk.schema, fk.table)), tsql.CommentSafe(quoteIdent(fk.name)))
	}
	sb.WriteString("-- Drop them first, and recreate them after.\n")
	if versioned {
		sb.WriteString("-- They do not block SET (SYSTEM_VERSIONING = OFF): it runs, and stays off.\n")
	}
	return sb.String()
}
