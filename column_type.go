package gosmo

import (
	"fmt"
	"strings"
)

// ============================================================
// Column type formatting (used by ScriptTable and by callers rendering a
// Column's type for display, e.g. SSMS's Table Properties > Columns page)
// ============================================================

// TypeString returns the T-SQL data-type fragment for a Column read from
// sys.columns.
//
// A user-defined alias or CLR type is schema-qualified and bracketed
// ([dbo].[Phone]), and never given a length: an alias type's length is part
// of its own definition, and an unqualified name resolves against the
// executing user's default schema — a different type, or none.
func (col *Column) TypeString() string {
	return catalogType{
		dt: col.DataType, typeSchema: col.TypeSchema, userDefined: col.IsUserDefinedType,
		maxLength: col.MaxLength, precision: col.Precision, scale: col.Scale,
		xmlSchema: col.XMLSchemaCollectionSchema, xmlCollection: col.XMLSchemaCollection, xmlDocument: col.IsXMLDocument,
		vectorDimensions: col.VectorDimensions, vectorBaseType: col.VectorBaseType,
	}.String()
}

// catalogType is a data type as sys.columns and sys.parameters describe it,
// shared by Column.TypeString and Parameter.TypeString.
type catalogType struct {
	dt                          DataType
	typeSchema                  string
	userDefined                 bool
	maxLength, precision, scale int
	// xmlSchema and xmlCollection name a typed xml's schema collection;
	// xmlDocument is its DOCUMENT facet.
	xmlSchema, xmlCollection string
	xmlDocument              bool
	// vectorDimensions and vectorBaseType are a vector's.
	vectorDimensions int
	vectorBaseType   string
}

// defaultVectorBaseType is the base type vector(n) means when none is given.
const defaultVectorBaseType = "float32"

// String renders the type: a user-defined type qualified and bare, a typed
// xml with its facet and collection, a vector with its dimensions, any other
// type through TypeString. A bare "xml" is untyped and a bare "vector"
// does not parse (Msg 2715), so dropping either facet changed or broke the
// script.
func (t catalogType) String() string {
	switch {
	case t.userDefined:
		return qualifiedName(t.typeSchema, string(t.dt))
	case t.dt == DataTypeXML && t.xmlCollection != "":
		facet := "CONTENT"
		if t.xmlDocument {
			facet = "DOCUMENT"
		}
		return fmt.Sprintf("%s(%s %s)", t.dt, facet, qualifiedName(t.xmlSchema, t.xmlCollection))
	case t.dt == DataTypeVector && t.vectorDimensions > 0:
		if t.vectorBaseType != "" && !strings.EqualFold(t.vectorBaseType, defaultVectorBaseType) {
			return fmt.Sprintf("%s(%d, %s)", t.dt, t.vectorDimensions, t.vectorBaseType)
		}
		return fmt.Sprintf("%s(%d)", t.dt, t.vectorDimensions)
	}
	return TypeString(t.dt, t.maxLength, t.precision, t.scale)
}

// TypeString renders a catalog data type with whatever length, precision
// or scale that type actually carries — shared by Column.TypeString and
// Parameter.TypeString, which read the same columns out of sys.columns and
// sys.parameters. nchar/nvarchar store max_length in bytes (2 per
// character); -1 is MAX.
//
// It is exported for a caller holding the raw catalog fields rather than a
// Column — a CatalogColumn, an alias type's base type — so every display of
// a type goes through this one renderer instead of a copy of it. dt must be
// sys.types' own lower-case name; a user-defined type comes back bare, and
// only Column.TypeString and Parameter.TypeString know to qualify it.
func TypeString(dt DataType, maxLength, precision, scale int) string {
	switch dt {
	case DataTypeVarChar, DataTypeChar, DataTypeBinary, DataTypeVarBinary:
		if maxLength == -1 {
			return fmt.Sprintf("%s(MAX)", dt)
		}
		if maxLength > 0 {
			return fmt.Sprintf("%s(%d)", dt, maxLength)
		}
	case DataTypeNVarChar, DataTypeNChar:
		if maxLength == -1 {
			return fmt.Sprintf("%s(MAX)", dt)
		}
		if maxLength > 0 {
			return fmt.Sprintf("%s(%d)", dt, maxLength/2)
		}
	case DataTypeDecimal, DataTypeNumeric:
		if precision > 0 {
			return fmt.Sprintf("%s(%d,%d)", dt, precision, scale)
		}
	case DataTypeDatetime2, DataTypeTime, DataTypeDatetimeOffset:
		// Always emitted: the bare type means scale 7, so dropping a zero
		// scale — the common "no fractional seconds" choice — turned
		// datetime2(0) into datetime2(7). The catalog always reports one.
		return fmt.Sprintf("%s(%d)", dt, scale)
	}
	return string(dt)
}
