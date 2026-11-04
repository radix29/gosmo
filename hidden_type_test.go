package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
)

// TestHiddenTypeColumnIsKept pins the read of a column whose alias type the
// login cannot see: sys.types has no row for it, so columnSelect LEFT JOINs
// and names the base type — an inner join dropped the column (4 of
// Sales.Customer's 5 for a db_datareader login, 2026-10-29). The row is what
// the server returns for such a column: base name, the column's own
// max_length, no type schema, is_user_defined defaulted to 1, hidden 1.
func TestHiddenTypeColumnIsKept(t *testing.T) {
	maskRow = []driver.Value{
		"Phone", int64(4), "nvarchar", int64(50), int64(0), int64(0), true, false, false,
		nil, nil, nil,
		false, "SQL_Latin1_General_CP1_CI_AS",
		nil, nil,
		false,
		nil, true,
		false, false,
		false, false,
		"",
		int64(0), false, false,
		int64(0), false,
		"", "", "",
		"", "", false,
		int64(0), "",
		"", "", "", "",
		true,
	}
	db, err := sql.Open("maskrow", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(context.Background(), "SELECT")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := scanColumns(rows)
	if err != nil || len(cols) != 1 {
		t.Fatalf("scanColumns = %v, %v", cols, err)
	}
	c := cols[0]
	if !c.IsTypeHidden || !c.IsUserDefinedType || c.TypeSchema != "" {
		t.Errorf("IsTypeHidden %v, IsUserDefinedType %v, TypeSchema %q", c.IsTypeHidden, c.IsUserDefinedType, c.TypeSchema)
	}
	// The storage type, not "[].[nvarchar]" as a user type with no schema.
	if got := c.TypeString(); got != "nvarchar(25)" {
		t.Errorf("TypeString = %q, want nvarchar(25)", got)
	}
	err = hiddenTypeRefusal("table [dbo].[T]", cols)
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "[Phone]") {
		t.Errorf("hiddenTypeRefusal = %v, want ErrUnsupported naming [Phone]", err)
	}
	c.IsTypeHidden = false
	if err := hiddenTypeRefusal("table [dbo].[T]", cols); err != nil {
		t.Errorf("visible type refused: %v", err)
	}
}

// A hidden CLR type has no base system type name (TYPE_NAME(240) is NULL):
// it renders as nothing rather than a guessed type.
func TestHiddenCLRTypeRendersEmpty(t *testing.T) {
	p := &Parameter{DataType: "", MaxLength: -1, IsUserDefinedType: true, IsTypeHidden: true}
	if got := p.TypeString(); got != "" {
		t.Errorf("TypeString = %q, want empty", got)
	}
}
