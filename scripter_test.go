package gosmo

import "testing"

func TestScriptColType(t *testing.T) {
	cases := []struct {
		name string
		col  *Column
		want string
	}{
		{"varchar with length", &Column{DataType: DataTypeVarChar, MaxLength: 50}, "varchar(50)"},
		{"varchar MAX", &Column{DataType: DataTypeVarChar, MaxLength: -1}, "varchar(MAX)"},
		{"varchar zero length", &Column{DataType: DataTypeVarChar, MaxLength: 0}, "varchar"},
		// nvarchar/nchar store max_length in bytes (2 per char) in sys.columns.
		{"nvarchar halves byte length", &Column{DataType: DataTypeNVarChar, MaxLength: 100}, "nvarchar(50)"},
		{"nvarchar MAX", &Column{DataType: DataTypeNVarChar, MaxLength: -1}, "nvarchar(MAX)"},
		{"nchar halves byte length", &Column{DataType: DataTypeNChar, MaxLength: 20}, "nchar(10)"},
		{"decimal with precision", &Column{DataType: DataTypeDecimal, Precision: 10, Scale: 4}, "decimal(10,4)"},
		{"decimal no precision", &Column{DataType: DataTypeDecimal}, "decimal"},
		{"time with scale", &Column{DataType: DataTypeTime, Scale: 7}, "time(7)"},
		{"time no scale", &Column{DataType: DataTypeTime}, "time"},
		{"plain bigint", &Column{DataType: DataTypeBigInt}, "bigint"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := scriptColType(c.col); got != c.want {
				t.Errorf("scriptColType(%+v) = %q, want %q", c.col, got, c.want)
			}
		})
	}
}
