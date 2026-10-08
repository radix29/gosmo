package gosmo

import "testing"

func TestGrantColumnPermissionRendersOneStatementForAllColumns(t *testing.T) {
	d, ctx, script := scriptedDB(t)
	err := d.ApplyPermission(ctx, VerbGrant, Securable{Class: SecurableTable, Schema: "dbo", Name: "Employees", Columns: []string{"FirstName", "LastName"}}, PermSelect, "app_reader", PermissionOptions{})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	want := "GRANT SELECT ([FirstName], [LastName]) ON [dbo].[Employees] TO [app_reader]"
	if got := onlyStatement(t, script); got != want {
		t.Errorf("statement =\n%q\nwant\n%q", got, want)
	}
}

func TestColumnPermissionWithGrantOption(t *testing.T) {
	d, ctx, script := scriptedDB(t)
	err := d.ApplyPermission(ctx, VerbGrant, Securable{Class: SecurableTable, Schema: "dbo", Name: "Employees", Columns: []string{"Salary"}}, PermUpdate, "hr_role", PermissionOptions{WithGrantOption: true})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	want := "GRANT UPDATE ([Salary]) ON [dbo].[Employees] TO [hr_role] WITH GRANT OPTION"
	if got := onlyStatement(t, script); got != want {
		t.Errorf("statement =\n%q\nwant\n%q", got, want)
	}
}

func TestRevokeColumnPermissionUsesFrom(t *testing.T) {
	d, ctx, script := scriptedDB(t)
	err := d.ApplyPermission(ctx, VerbRevoke, Securable{Class: SecurableTable, Schema: "dbo", Name: "Employees", Columns: []string{"Salary"}}, PermSelect, "app_reader", PermissionOptions{})
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	want := "REVOKE SELECT ([Salary]) ON [dbo].[Employees] FROM [app_reader]"
	if got := onlyStatement(t, script); got != want {
		t.Errorf("statement =\n%q\nwant\n%q", got, want)
	}
}

// A column name is an identifier spliced into DDL, so it must be
// bracket-quoted with its own brackets escaped, exactly like every other
// identifier in this package.
func TestColumnPermissionQuotesColumnNames(t *testing.T) {
	d, ctx, script := scriptedDB(t)
	err := d.ApplyPermission(ctx, VerbGrant, Securable{Class: SecurableTable, Schema: "dbo", Name: "T", Columns: []string{"od]d name"}}, PermSelect, "app_reader", PermissionOptions{})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	want := "GRANT SELECT ([od]]d name]) ON [dbo].[T] TO [app_reader]"
	if got := onlyStatement(t, script); got != want {
		t.Errorf("statement =\n%q\nwant\n%q", got, want)
	}
}

// Only SELECT, UPDATE and REFERENCES have a column-level form; anything else
// must be refused here rather than by SQL Server's syntax error.
func TestColumnPermissionRejectsNonColumnPermission(t *testing.T) {
	d, ctx, script := scriptedDB(t)
	if err := d.ApplyPermission(ctx, VerbGrant, Securable{Class: SecurableTable, Schema: "dbo", Name: "T", Columns: []string{"c"}}, PermDelete, "app_reader", PermissionOptions{}); err == nil {
		t.Error("DELETE was accepted as a column permission, want an error")
	}
	if err := d.ApplyPermission(ctx, VerbGrant, Securable{Class: SecurableTable, Schema: "dbo", Name: "T", Columns: []string{"c"}}, PermControl, "app_reader", PermissionOptions{}); err == nil {
		t.Error("CONTROL was accepted as a column permission, want an error")
	}
	if len(script.Statements()) != 0 {
		t.Errorf("a rejected statement was still collected: %v", script.Statements())
	}
}

// An empty, non-nil column list must not quietly widen into an
// object-level grant; nil is how a caller says "the whole object".
func TestColumnPermissionRejectsEmptyColumnList(t *testing.T) {
	d, ctx, script := scriptedDB(t)
	if err := d.ApplyPermission(ctx, VerbRevoke, Securable{Class: SecurableTable, Schema: "dbo", Name: "T", Columns: []string{}}, PermSelect, "app_reader", PermissionOptions{}); err == nil {
		t.Error("an empty column list was accepted on revoke, want an error")
	}
	if len(script.Statements()) != 0 {
		t.Errorf("an empty column list produced a statement: %v", script.Statements())
	}
}

func TestColumnPermissionNames(t *testing.T) {
	got := ColumnPermissionNames()
	want := []string{"REFERENCES", "SELECT", "UPDATE"}
	if len(got) != len(want) {
		t.Fatalf("ColumnPermissionNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ColumnPermissionNames() = %v, want %v (sorted)", got, want)
		}
	}
}

// The column-level trio and the object-level trio target the same object but
// are not interchangeable — this pins that the object-level one never
// acquires a column list by accident.
func TestObjectPermissionHasNoColumnList(t *testing.T) {
	d, ctx, script := scriptedDB(t)
	if err := d.ApplyPermission(ctx, VerbGrant, Securable{Class: SecurableTable, Schema: "dbo", Name: "Employees"}, PermSelect, "app_reader", PermissionOptions{}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	want := "GRANT SELECT ON [dbo].[Employees] TO [app_reader]"
	if got := onlyStatement(t, script); got != want {
		t.Errorf("statement =\n%q\nwant\n%q", got, want)
	}
}
