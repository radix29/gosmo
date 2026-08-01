package gosmo

import (
	"strings"
	"testing"
)

func TestValidServerPermission(t *testing.T) {
	if !validServerPermission("CONTROL SERVER") {
		t.Error("CONTROL SERVER should be a recognized server permission")
	}
	if !validServerPermission("CONNECT SQL") {
		t.Error("CONNECT SQL should be a recognized server permission")
	}
	if validServerPermission("DROP TABLE students; --") {
		t.Error("an injection attempt was accepted as a valid server permission")
	}
	if validServerPermission("") {
		t.Error("empty string should not be a recognized server permission")
	}
}

func TestGrantServerPermissionRejectsUnknownPermission(t *testing.T) {
	s := &Server{}
	err := s.GrantServerPermission("CONTROL SERVER; DROP DATABASE master; --", "attacker")
	if err == nil {
		t.Fatal("GrantServerPermission accepted an unrecognized permission name, want an error")
	}
}

func TestDenyAndRevokeServerPermissionRejectUnknownPermission(t *testing.T) {
	s := &Server{}
	if err := s.DenyServerPermission("NOT A REAL PERMISSION", "sa"); err == nil {
		t.Error("DenyServerPermission accepted an unrecognized permission, want an error")
	}
	if err := s.RevokeServerPermission("NOT A REAL PERMISSION", "sa"); err == nil {
		t.Error("RevokeServerPermission accepted an unrecognized permission, want an error")
	}
}

func TestValidDatabasePermission(t *testing.T) {
	if !validDatabasePermission("CONNECT") {
		t.Error("CONNECT should be a recognized database permission")
	}
	if !validDatabasePermission("CREATE TABLE") {
		t.Error("CREATE TABLE should be a recognized database permission")
	}
	if validDatabasePermission("SELECT * FROM users; --") {
		t.Error("an injection attempt was accepted as a valid database permission")
	}
}

func TestGrantDatabasePermissionRejectsUnknownPermission(t *testing.T) {
	d := &Database{name: "appdb", server: &Server{}}
	err := d.GrantDatabasePermission("CONTROL; DROP TABLE Users; --", "attacker")
	if err == nil {
		t.Fatal("GrantDatabasePermission accepted an unrecognized permission name, want an error")
	}
}

func TestValidDatabaseOption(t *testing.T) {
	if !validDatabaseOption(DBOptAutoClose) {
		t.Error("DBOptAutoClose should be a recognized database option")
	}
	if validDatabaseOption(DatabaseOption("DROP DATABASE appdb")) {
		t.Error("an injection attempt was accepted as a valid database option")
	}
}

func TestIsSimpleSetValue(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"ON", true},
		{"OFF", true},
		{"CHECKSUM", true},
		{"SNAPSHOT_ISOLATION", true},
		{"", false},
		{"ON; DROP TABLE Users; --", false},
		{"ON'", false},
		{"ON/*", false},
	}
	for _, c := range cases {
		if got := isSimpleSetValue(c.in); got != c.want {
			t.Errorf("isSimpleSetValue(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSetDatabaseOptionRejectsUnknownOptionAndUnsafeValue(t *testing.T) {
	d := &Database{name: "appdb", server: &Server{}}
	if err := d.SetDatabaseOption(DatabaseOption("EVIL_OPTION"), "ON"); err == nil {
		t.Error("SetDatabaseOption accepted an unrecognized option, want an error")
	}
	if err := d.SetDatabaseOption(DBOptAutoClose, "ON; DROP DATABASE appdb; --"); err == nil {
		t.Error("SetDatabaseOption accepted an unsafe value, want an error")
	}
}

func TestValidCategoryClass(t *testing.T) {
	if !validCategoryClass(CategoryClassJob) {
		t.Error("CategoryClassJob should be a recognized category class")
	}
	if validCategoryClass(CategoryClass("JOB; DROP TABLE msdb.dbo.sysjobs; --")) {
		t.Error("an injection attempt was accepted as a valid category class")
	}
}

func TestCreateAndDeleteCategoryRejectUnknownClass(t *testing.T) {
	s := &Server{}
	if err := s.CreateCategory(CategoryClass("EVIL"), "cat"); err == nil {
		t.Error("CreateCategory accepted an unrecognized category class, want an error")
	}
	if err := s.DeleteCategory(CategoryClass("EVIL"), "cat"); err == nil {
		t.Error("DeleteCategory accepted an unrecognized category class, want an error")
	}
}

func TestValidRecoveryModel(t *testing.T) {
	if !validRecoveryModel(RecoveryModelFull) {
		t.Error("RecoveryModelFull should be a recognized recovery model")
	}
	if validRecoveryModel(RecoveryModel("FULL; DROP DATABASE appdb; --")) {
		t.Error("an injection attempt was accepted as a valid recovery model")
	}
}

func TestSetRecoveryModelRejectsUnknownModel(t *testing.T) {
	d := &Database{name: "appdb", server: &Server{}}
	if err := d.SetRecoveryModel(RecoveryModel("FULL; DROP DATABASE appdb; --")); err == nil {
		t.Error("SetRecoveryModel accepted an unrecognized recovery model, want an error")
	}
}

func TestCreateDatabaseRejectsUnknownRecoveryModel(t *testing.T) {
	s := &Server{}
	err := s.CreateDatabase("appdb", &CreateDatabaseOptions{RecoveryModel: RecoveryModel("FULL; DROP DATABASE appdb; --")})
	if err == nil {
		t.Error("CreateDatabase accepted an unrecognized recovery model, want an error")
	}
}

func TestValidBackupAction(t *testing.T) {
	if !validBackupAction(BackupActionDatabase) {
		t.Error("BackupActionDatabase should be a recognized backup action")
	}
	if validBackupAction(BackupAction("DATABASE; DROP DATABASE appdb; --")) {
		t.Error("an injection attempt was accepted as a valid backup action")
	}
}

func TestBuildBackupStatementRejectsUnknownAction(t *testing.T) {
	_, err := BuildBackupStatement(BackupOptions{
		Database: "appdb",
		Action:   BackupAction("DATABASE; DROP DATABASE appdb; --"),
		Devices:  []string{`C:\Backups\appdb.bak`},
	})
	if err == nil {
		t.Error("BuildBackupStatement accepted an unrecognized action, want an error")
	}
}

func TestBuildRestoreStatementRejectsUnknownAction(t *testing.T) {
	_, err := BuildRestoreStatement(RestoreOptions{
		Database: "appdb",
		Action:   BackupAction("DATABASE; DROP DATABASE appdb; --"),
		Devices:  []string{`C:\Backups\appdb.bak`},
	})
	if err == nil {
		t.Error("BuildRestoreStatement accepted an unrecognized action, want an error")
	}
}

func TestValidDataType(t *testing.T) {
	if !validDataType(DataTypeInt) {
		t.Error("DataTypeInt should be a recognized data type")
	}
	if validDataType(DataType("int); DROP TABLE Users; --")) {
		t.Error("an injection attempt was accepted as a valid data type")
	}
}

func TestAddAndAlterColumnRejectUnknownDataType(t *testing.T) {
	tbl := &Table{}
	col := ColumnDefinition{Name: "Evil", DataType: DataType("int); DROP TABLE Users; --")}
	if err := tbl.AddColumn(col); err == nil {
		t.Error("AddColumn accepted an unrecognized data type, want an error")
	}
	if err := tbl.AlterColumn(col); err == nil {
		t.Error("AlterColumn accepted an unrecognized data type, want an error")
	}
}

func TestCreateTableRejectsUnknownDataType(t *testing.T) {
	d := &Database{name: "appdb", server: &Server{}}
	req := CreateTableRequest{
		Name: "Evil",
		Columns: []ColumnDefinition{
			{Name: "Id", DataType: DataType("int); DROP TABLE Users; --")},
		},
	}
	if err := d.CreateTable(req); err == nil {
		t.Error("CreateTable accepted an unrecognized data type, want an error")
	}
}

func TestCreateSequenceRejectsUnknownDataType(t *testing.T) {
	d := &Database{name: "appdb", server: &Server{}}
	req := CreateSequenceRequest{
		Name:     "EvilSeq",
		DataType: DataType("int); DROP TABLE Users; --"),
	}
	if err := d.CreateSequence(req); err == nil {
		t.Error("CreateSequence accepted an unrecognized data type, want an error")
	}
}

func TestCategoriesRejectsUnknownClass(t *testing.T) {
	s := &Server{}
	if _, err := s.Categories(CategoryClass("EVIL")); err == nil {
		t.Error("Categories accepted an unrecognized category class, want an error")
	}
}

func TestValidPartitionBoundary(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"100", true},
		{"-100", true},
		{"3.14", true},
		{"0x1F", true},
		{"'2024-01-01'", true},
		{"N'active'", true},
		{"NULL", true},
		{"", false},
		{"100); DROP TABLE Users; --", false},
		{"'a'; DROP TABLE Users; --", false},
		{"'unterminated", false},
	}
	for _, c := range cases {
		if got := validPartitionBoundary(c.in); got != c.want {
			t.Errorf("validPartitionBoundary(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCreatePartitionFunctionRejectsUnknownDataTypeAndBadBoundary(t *testing.T) {
	d := &Database{name: "appdb", server: &Server{}}
	if err := d.CreatePartitionFunction(CreatePartitionFunctionRequest{
		Name:       "pf1",
		InputType:  DataType("int); DROP TABLE Users; --"),
		Boundaries: []string{"100"},
	}); err == nil {
		t.Error("CreatePartitionFunction accepted an unrecognized data type, want an error")
	}
	if err := d.CreatePartitionFunction(CreatePartitionFunctionRequest{
		Name:       "pf1",
		InputType:  DataTypeInt,
		Boundaries: []string{"100); DROP TABLE Users; --"},
	}); err == nil {
		t.Error("CreatePartitionFunction accepted an invalid boundary literal, want an error")
	}
}

func TestSplitAndMergeRangeRejectBadBoundary(t *testing.T) {
	pf := &PartitionFunction{db: &Database{name: "appdb", server: &Server{}}, Name: "pf1"}
	if err := pf.SplitRange("100); DROP TABLE Users; --"); err == nil {
		t.Error("SplitRange accepted an invalid boundary literal, want an error")
	}
	if err := pf.MergeRange("100); DROP TABLE Users; --"); err == nil {
		t.Error("MergeRange accepted an invalid boundary literal, want an error")
	}
}

// TestScopedConfigForSecondaryClauseOrder pins that FOR SECONDARY precedes
// SET. Appending it after the assignment — "... SET MAXDOP = 4 FOR
// SECONDARY" — is a syntax error, not a differently-ordered but valid form,
// so that whole argument was unusable.
func TestScopedConfigForSecondaryClauseOrder(t *testing.T) {
	got, err := buildScopedConfigStatement("MAXDOP", "4", false)
	if err != nil {
		t.Fatalf("buildScopedConfigStatement: %v", err)
	}
	if want := "ALTER DATABASE SCOPED CONFIGURATION SET MAXDOP = 4"; got != want {
		t.Errorf("primary: got %q, want %q", got, want)
	}

	got, err = buildScopedConfigStatement("MAXDOP", "PRIMARY", true)
	if err != nil {
		t.Fatalf("buildScopedConfigStatement: %v", err)
	}
	if want := "ALTER DATABASE SCOPED CONFIGURATION FOR SECONDARY SET MAXDOP = PRIMARY"; got != want {
		t.Errorf("secondary: got %q, want %q", got, want)
	}
	if strings.HasSuffix(got, "FOR SECONDARY") {
		t.Errorf("FOR SECONDARY appended after the assignment: %q", got)
	}
}

// TestScopedConfigStatementRejectsInjection pins that the validation still
// runs on the extracted builder, not just at the old call site.
func TestScopedConfigStatementRejectsInjection(t *testing.T) {
	if _, err := buildScopedConfigStatement("MAXDOP; DROP TABLE x --", "4", false); err == nil {
		t.Error("buildScopedConfigStatement accepted an injected name")
	}
	if _, err := buildScopedConfigStatement("MAXDOP", "4; DROP TABLE x --", false); err == nil {
		t.Error("buildScopedConfigStatement accepted an injected value")
	}
}
