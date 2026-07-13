package gosmo

import "testing"

func TestColTypeSQL(t *testing.T) {
	cases := []struct {
		name string
		col  ColumnDefinition
		want string
	}{
		{"varchar with length", ColumnDefinition{DataType: DataTypeVarChar, MaxLength: 50}, "varchar(50)"},
		{"varchar MAX", ColumnDefinition{DataType: DataTypeVarChar, MaxLength: -1}, "varchar(MAX)"},
		{"varchar no length", ColumnDefinition{DataType: DataTypeVarChar, MaxLength: 0}, "varchar"},
		{"nvarchar with length", ColumnDefinition{DataType: DataTypeNVarChar, MaxLength: 100}, "nvarchar(100)"},
		{"decimal with precision", ColumnDefinition{DataType: DataTypeDecimal, Precision: 18, Scale: 2}, "decimal(18,2)"},
		{"decimal no precision", ColumnDefinition{DataType: DataTypeDecimal}, "decimal"},
		{"datetime2 with scale", ColumnDefinition{DataType: DataTypeDatetime2, Scale: 3}, "datetime2(3)"},
		{"datetime2 no scale", ColumnDefinition{DataType: DataTypeDatetime2}, "datetime2"},
		{"plain int", ColumnDefinition{DataType: DataTypeInt}, "int"},
		{"bit", ColumnDefinition{DataType: DataTypeBit}, "bit"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := colTypeSQL(c.col); got != c.want {
				t.Errorf("colTypeSQL(%+v) = %q, want %q", c.col, got, c.want)
			}
		})
	}
}

// TestAddColumnRequiresName and TestAlterColumnRequiresName pin the
// early-return validation that runs before either method ever touches
// t.db — the only part of the DDL flow testable without a live server.
func TestAddColumnRequiresName(t *testing.T) {
	tbl := &Table{Schema: "dbo", Name: "T"}
	if err := tbl.AddColumn(ColumnDefinition{DataType: DataTypeInt}); err == nil {
		t.Error("AddColumn with empty column name = nil error, want error")
	}
}

func TestAlterColumnRequiresName(t *testing.T) {
	tbl := &Table{Schema: "dbo", Name: "T"}
	if err := tbl.AlterColumn(ColumnDefinition{DataType: DataTypeInt}); err == nil {
		t.Error("AlterColumn with empty column name = nil error, want error")
	}
}
