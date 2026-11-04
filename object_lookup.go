package gosmo

import (
	"context"
	"fmt"
	"time"
)

// ============================================================
// Kind-generic object lookup
// ============================================================

// ObjectInfo is one row of sys.objects: a schema-scoped object of any kind —
// table, view, module, synonym, sequence, constraint or DML trigger — found
// by name without knowing which family it belongs to. It is what a caller
// holding only a name ("Sales.Customer") reads first, to dispatch on Type to
// the family's own *ByName.
type ObjectInfo struct {
	ObjectID int
	Schema   string
	Name     string
	// Type is sys.objects.type with its padding trimmed: "U", "V", "P", "FN",
	// "IF", "TF", "TR", "SN", "SO", "PK", …
	Type     string
	TypeDesc string // e.g. "USER_TABLE", "SQL_STORED_PROCEDURE"
	// ParentSchema and ParentName name the object a child belongs to — a
	// constraint's or DML trigger's table; both "" for an object with none.
	ParentSchema string
	ParentName   string
	Created      time.Time
	Modified     time.Time
	IsMSShipped  bool
}

const objectInfoSelect = `
SELECT o.object_id, SCHEMA_NAME(o.schema_id), o.name, RTRIM(o.type), o.type_desc,
       ISNULL(OBJECT_SCHEMA_NAME(o.parent_object_id), N''), ISNULL(OBJECT_NAME(o.parent_object_id), N''),
       o.create_date, o.modify_date, o.is_ms_shipped
FROM   sys.objects o`

func scanObjectInfo(_ *Database, scan func(...any) error) (*ObjectInfo, error) {
	o := &ObjectInfo{}
	if err := scan(&o.ObjectID, &o.Schema, &o.Name, &o.Type, &o.TypeDesc,
		&o.ParentSchema, &o.ParentName, &o.Created, &o.Modified, &o.IsMSShipped); err != nil {
		return nil, err
	}
	return o, nil
}

// ObjectByName returns the schema-scoped object schema.name, whatever its
// kind — ErrNotFound when there is none. Object names are unique within a
// schema across every kind sys.objects holds, so the answer is one row.
//
// The name is compared by the catalog's own = (its collation), as OBJECT_ID
// does: in a case-sensitive database sales.customer is not Sales.Customer,
// and folding case here would find an object the server would not.
func (d *Database) ObjectByName(ctx context.Context, schema, name string) (*ObjectInfo, error) {
	if err := requireSchema("object by name", schema, name); err != nil {
		return nil, err
	}
	return readByName(ctx, d, scanObjectInfo, objectInfoSelect+`
WHERE  SCHEMA_NAME(o.schema_id) COLLATE DATABASE_DEFAULT = @p1
  AND  o.name                                              = @p2`, []any{schema, name},
		notFoundf("gosmo: object %s not found in %q", qualifiedName(schema, name), d.Name),
		fmt.Sprintf("find object %s in %q", qualifiedName(schema, name), d.Name))
}

// ObjectsNamed returns every schema-scoped object called name, in any schema,
// ordered by schema — what resolving a one-part name needs: one row is the
// object, several are an ambiguity only the caller can settle, none is
// nothing by that name. The comparison follows the catalog's collation, as
// ObjectByName's does.
func (d *Database) ObjectsNamed(ctx context.Context, name string) ([]*ObjectInfo, error) {
	rows, err := d.query(ctx, objectInfoSelect+`
WHERE  o.name = @p1
ORDER  BY SCHEMA_NAME(o.schema_id)`, name)
	return scanRows(rows, err, fmt.Sprintf("find objects named %q in %q", name, d.Name), func(scan func(...any) error) (*ObjectInfo, error) {
		return scanObjectInfo(d, scan)
	})
}
