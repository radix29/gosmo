package gosmo

import (
	"strings"
	"testing"
)

func dmlColumns() []*Column {
	return []*Column{
		{Name: "id", DataType: DataTypeInt, IsIdentity: true, OrdinalPosition: 1},
		{Name: "name", DataType: DataTypeNVarChar, MaxLength: 100, OrdinalPosition: 2},
		{Name: "total", DataType: DataTypeDecimal, Precision: 10, Scale: 2, OrdinalPosition: 3},
		{Name: "total_with_tax", DataType: DataTypeDecimal, Precision: 10, Scale: 2, IsComputed: true, OrdinalPosition: 4},
	}
}

func TestBuildSelectScriptListsEveryColumn(t *testing.T) {
	got := buildSelectScript("dbo", "Orders", dmlColumns())
	for _, want := range []string{"[id]", "[name]", "[total]", "[total_with_tax]", "FROM   [dbo].[Orders]"} {
		if !strings.Contains(got, want) {
			t.Errorf("SELECT template missing %s:\n%s", want, got)
		}
	}
}

func TestBuildInsertScriptSkipsIdentityAndComputedColumns(t *testing.T) {
	got := buildInsertScript("dbo", "Orders", dmlColumns())

	// Both reject an explicit value, so a template listing them can only fail.
	if strings.Contains(got, "[id]") {
		t.Errorf("INSERT template includes the identity column:\n%s", got)
	}
	if strings.Contains(got, "[total_with_tax]") {
		t.Errorf("INSERT template includes a computed column:\n%s", got)
	}
	if !strings.Contains(got, "<name, nvarchar(50),>") {
		t.Errorf("INSERT template placeholder wrong (nvarchar length is stored in bytes):\n%s", got)
	}
	if !strings.Contains(got, "<total, decimal(10,2),>") {
		t.Errorf("INSERT template placeholder wrong:\n%s", got)
	}
}

func TestBuildUpdateScriptSetsOnlyWritableColumns(t *testing.T) {
	got := buildUpdateScript("dbo", "Orders", dmlColumns())
	if strings.Contains(got, "[id] =") || strings.Contains(got, "[total_with_tax] =") {
		t.Errorf("UPDATE template assigns a column that cannot be written:\n%s", got)
	}
	if !strings.Contains(got, "[name] = <name, nvarchar(50),>") {
		t.Errorf("UPDATE template wrong:\n%s", got)
	}
	if !strings.Contains(got, "WHERE  <Search Conditions,,>") {
		t.Errorf("UPDATE template must not be runnable without a WHERE the operator writes:\n%s", got)
	}
}

func TestBuildExecuteScriptDeclaresOutputParameters(t *testing.T) {
	params := []*Parameter{
		{Name: "@customer_id", Ordinal: 1, DataType: DataTypeInt},
		{Name: "@total", Ordinal: 2, DataType: DataTypeDecimal, Precision: 10, Scale: 2, IsOutput: true},
	}
	got := buildExecuteScript("dbo", "usp_OrderTotal", params)

	// An OUTPUT argument has to be a variable — a <placeholder> there would
	// not parse.
	// DECLARE takes an @-prefixed name and nothing else: "DECLARE [total]
	// decimal(10,2)" parses as a cursor declaration and fails on the server
	// with "'decimal' is not a recognized CURSOR option" (verified live).
	if !strings.Contains(got, "DECLARE @total decimal(10,2);") {
		t.Errorf("OUTPUT parameter not declared:\n%s", got)
	}
	if !strings.Contains(got, "@total = @total OUTPUT") {
		t.Errorf("OUTPUT parameter not passed by variable:\n%s", got)
	}
	if !strings.Contains(got, "@customer_id = <customer_id, int,>") {
		t.Errorf("input parameter placeholder wrong:\n%s", got)
	}
	if !strings.Contains(got, "SELECT 'Return Value' = @return_value;") {
		t.Errorf("EXEC template does not surface the return value:\n%s", got)
	}
}

// G6: an alias type's unqualified name resolves against the executing user's
// default schema — another type, or none — so a parameter of one is rendered
// qualified and without a length, as a column of one is.
func TestParameterTypeStringQualifiesUserDefinedTypes(t *testing.T) {
	params := []*Parameter{
		{Name: "@phone", Ordinal: 1, DataType: "Phone", TypeSchema: "app", IsUserDefinedType: true, MaxLength: 40},
		{Name: "@out", Ordinal: 2, DataType: "Phone", TypeSchema: "app", IsUserDefinedType: true, MaxLength: 40, IsOutput: true},
		{Name: "@name", Ordinal: 3, DataType: DataTypeNVarChar, TypeSchema: "sys", MaxLength: 100},
	}
	if got := params[0].TypeString(); got != "[app].[Phone]" {
		t.Errorf("alias parameter TypeString = %q, want [app].[Phone]", got)
	}
	if got := params[2].TypeString(); got != "nvarchar(50)" {
		t.Errorf("built-in parameter TypeString = %q, want nvarchar(50)", got)
	}
	got := buildExecuteScript("dbo", "usp_Call", params)
	for _, want := range []string{"DECLARE @out [app].[Phone];", "@phone = <phone, [app].[Phone],>"} {
		if !strings.Contains(got, want) {
			t.Errorf("EXEC template missing %q:\n%s", want, got)
		}
	}
}

func TestBuildFunctionCallScriptShapeFollowsFunctionType(t *testing.T) {
	params := []*Parameter{{Name: "@id", Ordinal: 1, DataType: DataTypeInt}}

	scalar := buildFunctionCallScript("dbo", "fn_Total", "FN", params)
	if !strings.Contains(scalar, "SELECT [dbo].[fn_Total](<id, int,>)") {
		t.Errorf("scalar function call wrong:\n%s", scalar)
	}

	// A table-valued function can only be selected *from*.
	for _, ft := range []string{"IF", "TF"} {
		tvf := buildFunctionCallScript("dbo", "fn_Rows", ft, params)
		if !strings.Contains(tvf, "FROM   [dbo].[fn_Rows](<id, int,>)") {
			t.Errorf("%s function call wrong:\n%s", ft, tvf)
		}
	}
}

// A rowversion, a GENERATED ALWAYS period column and a graph table's internal
// columns all refuse an explicit value, so a template naming one always
// fails. An edge's endpoints are the exception on INSERT: the row must name
// them, through the bare $from_id/$to_id pseudo-columns.
func TestDMLTemplatesSkipServerFilledColumns(t *testing.T) {
	cols := []*Column{
		{Name: "graph_id_X", DataType: DataTypeBigInt, IsHidden: true, GraphType: GraphColumnID},
		{Name: "$edge_id_X", DataType: DataTypeNVarChar, MaxLength: 2000, GraphType: GraphColumnIDComputed},
		{Name: "from_id_X", DataType: DataTypeBigInt, IsHidden: true, GraphType: GraphColumnFromID},
		{Name: "$from_id_X", DataType: DataTypeNVarChar, MaxLength: 2000, GraphType: GraphColumnFromIDComputed},
		{Name: "$to_id_X", DataType: DataTypeNVarChar, MaxLength: 2000, GraphType: GraphColumnToIDComputed},
		{Name: "w", DataType: DataTypeInt},
		{Name: "rv", DataType: "timestamp"},
		{Name: "valid_from", DataType: DataTypeDatetime2, GeneratedAlwaysType: 1},
		{Name: "valid_to", DataType: DataTypeDatetime2, GeneratedAlwaysType: 2},
	}
	ins := buildInsertScript("dbo", "E", cols)
	upd := buildUpdateScript("dbo", "E", cols)
	for _, bad := range []string{"graph_id_X", "$edge_id_X", "from_id_X", "[rv]", "[valid_from]", "[valid_to]"} {
		if strings.Contains(ins, bad) {
			t.Errorf("INSERT template names %s:\n%s", bad, ins)
		}
		if strings.Contains(upd, bad) {
			t.Errorf("UPDATE template names %s:\n%s", bad, upd)
		}
	}
	want := "INSERT INTO [dbo].[E]\n           ($from_id\n          , $to_id\n          , [w])\n" +
		"VALUES     (<$from_id, nvarchar(1000),>\n          , <$to_id, nvarchar(1000),>\n          , <w, int,>);"
	if !strings.Contains(ins, want) {
		t.Errorf("INSERT template:\n%s\nwant it to contain:\n%s", ins, want)
	}
	if strings.Contains(upd, "$from_id") || strings.Contains(upd, "$to_id") {
		t.Errorf("UPDATE template assigns an edge endpoint, which cannot be updated:\n%s", upd)
	}
	if !strings.Contains(upd, "SET    [w] = <w, int,>\nWHERE") {
		t.Errorf("UPDATE template:\n%s", upd)
	}
}
