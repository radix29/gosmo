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
