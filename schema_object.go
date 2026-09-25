package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// schema_object.go is the rename and the schema transfer every schema-scoped
// handle shares. Each family's Rename and Transfer is a one-line call into
// these, so the refusals, the statement and the error wording are the same
// whichever handle the caller holds. Until 2026-09-25 they were
// Database.RenameObject/TransferObject/TransferType and their siblings,
// addressed by (schema, name) on the parent — a second shape beside the
// handles' Drop for the same objects.

// Class arguments for renameSchemaObject and transferSchemaObject. Each is a
// fixed keyword chosen here, never caller input.
const (
	// renameObjectClass is sp_rename's default 'OBJECT' @objtype: tables,
	// views, procedures, functions, triggers, sequences, synonyms, rules,
	// defaults and constraints — every row of sys.objects.
	renameObjectClass = "OBJECT"
	// renameAliasTypeClass is 'USERDATATYPE', the whole of what sp_rename can
	// rename in sys.types: an alias type. A table type or a CLR type has no
	// @objtype at all, and passing one of those renames nothing while
	// reporting success.
	renameAliasTypeClass = "USERDATATYPE"

	// transferObjectClass is ALTER SCHEMA ... TRANSFER's default class,
	// which finds only rows of sys.objects.
	transferObjectClass = ""
	// transferTypeClass is TYPE:: — an alias, table or CLR type lives in
	// sys.types, where the default class finds nothing.
	transferTypeClass = "TYPE"
	// transferXMLSchemaCollectionClass is the whole three-word noun, not an
	// abbreviation of it.
	transferXMLSchemaCollectionClass = "XML SCHEMA COLLECTION"
)

// renameSchemaObject renames the object [schema].[oldName] with sp_rename.
//
// newName is a bare name: sp_rename refuses a qualified one, and a rename
// never moves an object between schemas — that is transferSchemaObject.
// sp_rename updates the object and nothing that names it; a view or
// procedure that referenced the old name breaks at its next use.
func (d *Database) renameSchemaObject(ctx context.Context, what, class, schema, oldName, newName string) error {
	if err := requireSchema("rename "+what, schema, oldName); err != nil {
		return err
	}
	if newName == "" {
		return fmt.Errorf("gosmo: rename %s %s: new name is required", what, qualifiedName(schema, oldName))
	}
	if _, err := d.exec(ctx,
		"EXEC sp_rename @objname = @p1, @newname = @p2, @objtype = N'"+class+"'",
		qualifiedName(schema, oldName), newName,
	); err != nil {
		return fmt.Errorf("gosmo: rename %s %s to %q: %w", what, qualifiedName(schema, oldName), newName, err)
	}
	return nil
}

// transferSchemaObject moves [schema].[name] into targetSchema (ALTER SCHEMA
// ... TRANSFER), which is the operation sp_rename cannot do. class is the
// securable-class prefix, empty for the default OBJECT class.
//
// The object keeps its name and its object_id; permissions granted on it
// directly are dropped by the server, which is the documented behaviour of
// ALTER SCHEMA TRANSFER and the reason it is not a cosmetic change — and the
// reason a same-schema transfer is refused rather than sent (see
// refuseSameSchemaTransfer).
func (d *Database) transferSchemaObject(ctx context.Context, what, class, targetSchema, schema, name string) error {
	if err := requireSchema("transfer "+what, schema, name); err != nil {
		return err
	}
	if targetSchema == "" {
		return fmt.Errorf("gosmo: transfer %s %s: target schema: %w", what, qualifiedName(schema, name), ErrSchemaRequired)
	}
	if err := d.refuseSameSchemaTransfer(ctx, what, targetSchema, schema, name); err != nil {
		return err
	}
	securable := qualifiedName(schema, name)
	if class != "" {
		securable = class + "::" + securable
	}
	if _, err := d.exec(ctx, fmt.Sprintf("ALTER SCHEMA %s TRANSFER %s",
		quoteIdent(targetSchema), securable)); err != nil {
		return fmt.Errorf("gosmo: transfer %s %s to schema %s: %w", what, qualifiedName(schema, name), quoteIdent(targetSchema), err)
	}
	return nil
}

// refuseSameSchemaTransfer refuses a transfer into the schema the object is
// already in. A same-schema transfer is not a no-op at the server — it still
// drops the permissions granted directly on the object — so it is refused
// rather than sent.
//
// Names that differ only in case are one schema under a case-insensitive
// collation and two under a case-sensitive one, so only the server can say
// which; that case alone costs a round trip. Comparing case-blind here
// refused a legitimate [sales] → [Sales] transfer in a _CS_ database.
func (d *Database) refuseSameSchemaTransfer(ctx context.Context, what, targetSchema, schema, name string) error {
	same := targetSchema == schema
	if !same && strings.EqualFold(targetSchema, schema) {
		err := d.queryRow(ctx, func(row *sql.Row) error { return row.Scan(&same) },
			`SELECT CAST(CASE WHEN SCHEMA_ID(@p1) = SCHEMA_ID(@p2) THEN 1 ELSE 0 END AS bit)`, targetSchema, schema)
		if err != nil {
			return fmt.Errorf("gosmo: transfer %s %s to schema %s: %w", what, qualifiedName(schema, name), quoteIdent(targetSchema), err)
		}
	}
	if same {
		return fmt.Errorf("gosmo: transfer %s %s: it is already in schema %s", what, qualifiedName(schema, name), quoteIdent(schema))
	}
	return nil
}

// dropSchemaObject is the bare DROP <keyword> [schema].[name] the
// schema-scoped families share. A name that matches nothing is the server's
// error, not a silent success — see the note on Table.Drop.
func (d *Database) dropSchemaObject(ctx context.Context, what, keyword, schema, name string) error {
	if err := requireSchema("drop "+what, schema, name); err != nil {
		return err
	}
	if _, err := d.exec(ctx, "DROP "+keyword+" "+qualifiedName(schema, name)); err != nil {
		return fmt.Errorf("gosmo: drop %s %s: %w", what, qualifiedName(schema, name), err)
	}
	return nil
}
