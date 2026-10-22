package gosmo

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// SecurableSearch narrows FindSecurables.
//
// Name is matched case-insensitively as a substring of the securable's
// qualified name — "dbo.Orders" for a table, the bare name for a schema or a
// certificate — so both "ord" and "dbo.ord" find dbo.Orders. LIKE wildcards
// in it are matched literally, not as wildcards. An empty Name matches
// everything.
//
// Limit caps the rows returned; 0 means no cap. Results are ordered by class
// (securableSearchOrder), then by qualified name, so a capped search returns
// a stable prefix rather than an arbitrary subset.
type SecurableSearch struct {
	Name  string
	Limit int
}

// securableSearchOrder is the order FindSecurables lists the classes in:
// schemas, the object classes, then types and certificates.
var securableSearchOrder = []SecurableClass{
	SecurableSchema, SecurableTable, SecurableView, SecurableProcedure,
	SecurableScalarFunction, SecurableInlineFunction, SecurableTableFunction,
	SecurableSequence, SecurableSynonym, SecurableUserType, SecurableCertificate,
}

// findSecurablesFrom is the derived table FindSecurables searches: one row
// per securable, its class, schema, name and lowered qualified name. The
// object branch turns sys.objects.type into the class name with a CASE built
// from objectSecurableClasses, so the type list lives in one place.
var findSecurablesFrom = func() string {
	var kind strings.Builder
	kind.WriteString("CASE o.type")
	for _, t := range slices.Sorted(maps.Keys(objectSecurableClasses)) {
		fmt.Fprintf(&kind, " WHEN '%s' THEN '%s'", t, objectSecurableClasses[t])
	}
	kind.WriteString(" END")
	return `
    SELECT 'SCHEMA' AS kind, '' AS [schema], s.name AS name,
           LOWER(s.name) AS qualified
    FROM   sys.schemas s
    JOIN   sys.database_principals p ON p.principal_id = s.principal_id
    UNION ALL
    SELECT ` + kind.String() + `, SCHEMA_NAME(o.schema_id), o.name,
           LOWER(SCHEMA_NAME(o.schema_id) + '.' + o.name)
    FROM   sys.objects o
    WHERE  o.is_ms_shipped = 0 AND o.type IN (` + objectSecurableTypes + `)
    UNION ALL
    SELECT 'TYPE', SCHEMA_NAME(t.schema_id), t.name,
           LOWER(SCHEMA_NAME(t.schema_id) + '.' + t.name)
    FROM   sys.types t
    WHERE  t.is_user_defined = 1
    UNION ALL
    SELECT 'CERTIFICATE', '', c.name, LOWER(c.name)
    FROM   sys.certificates c
    WHERE  c.name NOT LIKE '##%'`
}()

// findSecurablesRank orders the derived table's rows by securableSearchOrder.
var findSecurablesRank = func() string {
	var b strings.Builder
	b.WriteString("CASE x.kind")
	for i, c := range securableSearchOrder {
		fmt.Fprintf(&b, " WHEN '%s' THEN %d", c, i)
	}
	b.WriteString(" END")
	return b.String()
}()

// FindSecurables returns the securables below the database matching search:
// schemas, every object kind ApplyPermission takes, user types and
// certificates (other than the server's own ## ones). Each is the Securable
// ApplyPermission takes for it, with Columns nil; Schema is empty for a
// class that is not schema-scoped (a schema, a certificate).
//
// It returns just the identity of each securable, not the object: a
// permissions picker needs the name and the class and nothing else, and
// loading Table or View values for thousands of candidates is work no caller
// wants.
//
// One query over the catalog, for a caller that needs candidates matching
// what the user typed rather than the whole catalog — a database with
// thousands of tables makes "list everything and filter in the client" both
// slow to open and useless as a picker.
func (d *Database) FindSecurables(ctx context.Context, search SecurableSearch) ([]Securable, error) {
	top := ""
	if search.Limit > 0 {
		top = "TOP (@p2) "
	}
	q := `
SELECT ` + top + `x.kind, x.[schema], x.name
FROM (` + findSecurablesFrom + `
) x
WHERE  x.qualified LIKE @p1 ESCAPE '\'
ORDER  BY ` + findSecurablesRank + `, x.qualified`

	args := []any{"%" + likeEscape(strings.ToLower(search.Name)) + "%"}
	if search.Limit > 0 {
		args = append(args, search.Limit)
	}

	rows, err := d.query(ctx, q, args...)
	return scanRows(rows, err, fmt.Sprintf("find securables in %q", d.Name), func(scan func(...any) error) (Securable, error) {
		var r Securable
		if err := scan(&r.Class, &r.Schema, &r.Name); err != nil {
			return Securable{}, err
		}
		return r, nil
	})
}
