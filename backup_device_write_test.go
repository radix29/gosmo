package gosmo

import (
	"context"
	"strings"
	"testing"
)

func TestCreateBackupDeviceStatementShape(t *testing.T) {
	cases := []struct {
		name     string
		devName  string
		devType  BackupDeviceType
		physical string
		want     string
	}{
		{
			name: "disk device", devName: "NightlyDev", devType: BackupDeviceDisk,
			physical: `C:\Backups\nightly.bak`,
			want:     `EXEC sp_addumpdevice @devtype = N'disk', @logicalname = N'NightlyDev', @physicalname = N'C:\Backups\nightly.bak'`,
		},
		{
			// An empty type is the ordinary case for a caller that only ever
			// makes disk devices; it must not reach the server as @devtype = N''.
			name: "an empty type defaults to disk", devName: "Dev", devType: "",
			physical: "/var/opt/mssql/backup/d.bak",
			want:     `EXEC sp_addumpdevice @devtype = N'disk', @logicalname = N'Dev', @physicalname = N'/var/opt/mssql/backup/d.bak'`,
		},
		{
			name: "tape device", devName: "TapeDev", devType: BackupDeviceTape,
			physical: `\\.\tape0`,
			want:     `EXEC sp_addumpdevice @devtype = N'tape', @logicalname = N'TapeDev', @physicalname = N'\\.\tape0'`,
		},
		{
			// Every operand is a literal, so an apostrophe in either the name
			// or the path has to be doubled or the EXEC ends early.
			name: "quotes in the literals are escaped", devName: "o'brien", devType: BackupDeviceDisk,
			physical: `C:\o'brien\d.bak`,
			want:     `EXEC sp_addumpdevice @devtype = N'disk', @logicalname = N'o''brien', @physicalname = N'C:\o''brien\d.bak'`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, col := WithScript(context.Background())
			if _, err := (&Server{}).CreateBackupDeviceContext(ctx, tc.devName, tc.devType, tc.physical); err != nil {
				t.Fatalf("CreateBackupDeviceContext: %v", err)
			}
			if len(col.Statements) != 1 {
				t.Fatalf("got %d statements, want 1: %v", len(col.Statements), col.Statements)
			}
			if col.Statements[0] != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", col.Statements[0], tc.want)
			}
		})
	}
}

func TestCreateBackupDeviceRequiresNameAndPath(t *testing.T) {
	ctx, col := WithScript(context.Background())
	if _, err := (&Server{}).CreateBackupDeviceContext(ctx, "  ", BackupDeviceDisk, "/tmp/d.bak"); err == nil {
		t.Error("a device with no name was accepted")
	}
	if _, err := (&Server{}).CreateBackupDeviceContext(ctx, "Dev", BackupDeviceDisk, ""); err == nil {
		t.Error("a device with no physical name was accepted")
	}
	if len(col.Statements) != 0 {
		t.Errorf("a statement was built anyway: %v", col.Statements)
	}
}

// Under WithScript the EXEC was only collected, so reading the device back
// would find nothing. The name-only handle is what a caller gets instead.
func TestCreateBackupDeviceUnderScriptReturnsAHandle(t *testing.T) {
	ctx, col := WithScript(context.Background())
	d, err := (&Server{}).CreateBackupDeviceContext(ctx, "NightlyDev", BackupDeviceDisk, `C:\b\n.bak`)
	if err != nil {
		t.Fatalf("CreateBackupDeviceContext: %v", err)
	}
	if d == nil || d.Name != "NightlyDev" {
		t.Fatalf("got %#v, want a handle named NightlyDev", d)
	}
	if len(col.Statements) != 1 {
		t.Fatalf("collected statements: %v", col.Statements)
	}
	// The handle must still be usable for a follow-up write.
	if err := d.DropContext(ctx, false); err != nil {
		t.Fatalf("DropContext on the returned handle: %v", err)
	}
}

// @delfile is what separates unregistering the alias from deleting the backup
// file behind it; the two must not be the same statement.
func TestDropBackupDeviceDelFile(t *testing.T) {
	cases := []struct {
		name       string
		deleteFile bool
		want       string
	}{
		{"keep the file", false, `EXEC sp_dropdevice @logicalname = N'NightlyDev'`},
		{"delete the file", true, `EXEC sp_dropdevice @logicalname = N'NightlyDev', @delfile = N'DELFILE'`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, col := WithScript(context.Background())
			if err := (&Server{}).BackupDevice("NightlyDev").DropContext(ctx, tc.deleteFile); err != nil {
				t.Fatalf("DropContext: %v", err)
			}
			if len(col.Statements) != 1 || col.Statements[0] != tc.want {
				t.Errorf("got %v, want [%s]", col.Statements, tc.want)
			}
		})
	}
}

// A logical device is addressed bare. Passing its name as a path builds
// FROM DISK = N'name', which SQL Server reads as a file of that name in the
// default backup directory — a different device entirely, and one that
// usually does not exist.
func TestBackupTargetClause(t *testing.T) {
	if got, want := DeviceTarget("NightlyDev").clause(), "[NightlyDev]"; got != want {
		t.Errorf("DeviceTarget clause = %q, want %q", got, want)
	}
	if got, want := DiskTarget(`C:\b\n.bak`).clause(), `DISK = N'C:\b\n.bak'`; got != want {
		t.Errorf("DiskTarget clause = %q, want %q", got, want)
	}
	if got, want := (&BackupDevice{Name: "NightlyDev"}).Target().clause(), "[NightlyDev]"; got != want {
		t.Errorf("BackupDevice.Target clause = %q, want %q", got, want)
	}
}

// The three RESTORE-side reads that take a path must go on building the same
// statement they did before BackupTarget existed.
func TestVerifyBackupStillTakesAPath(t *testing.T) {
	ctx, col := WithScript(context.Background())
	if err := (&Server{}).VerifyBackupContext(ctx, `C:\b\n.bak`); err != nil {
		t.Fatalf("VerifyBackupContext: %v", err)
	}
	want := `RESTORE VERIFYONLY FROM DISK = N'C:\b\n.bak'`
	if len(col.Statements) != 1 || col.Statements[0] != want {
		t.Errorf("got %v, want [%s]", col.Statements, want)
	}
}

func TestVerifyBackupFromDevice(t *testing.T) {
	ctx, col := WithScript(context.Background())
	if err := (&Server{}).VerifyBackupFromContext(ctx, DeviceTarget("NightlyDev")); err != nil {
		t.Fatalf("VerifyBackupFromContext: %v", err)
	}
	want := "RESTORE VERIFYONLY FROM [NightlyDev]"
	if len(col.Statements) != 1 || col.Statements[0] != want {
		t.Errorf("got %v, want [%s]", col.Statements, want)
	}
}

// -- ScriptBackupDevice ----------------------------------------------------

func TestBuildBackupDeviceScriptCreate(t *testing.T) {
	d := &BackupDevice{Name: "NightlyDev", Type: "DISK", PhysicalName: `C:\Backups\nightly.bak`}
	got := buildBackupDeviceScript(d, ScriptOptions{})

	want := `EXEC sp_addumpdevice @devtype = N'disk', @logicalname = N'NightlyDev', @physicalname = N'C:\Backups\nightly.bak';`
	if !strings.Contains(got, want) {
		t.Errorf("script does not create the device:\n%s", got)
	}
	// type_desc is the catalog's spelling; sp_addumpdevice takes the keyword.
	if strings.Contains(got, "N'DISK'") {
		t.Errorf("script passed type_desc where sp_addumpdevice wants a keyword:\n%s", got)
	}
}

func TestBuildBackupDeviceScriptDropGuard(t *testing.T) {
	d := &BackupDevice{Name: "o'brien", Type: "DISK", PhysicalName: `C:\b\o.bak`}
	got := buildBackupDeviceScript(d, ScriptOptions{Verb: ScriptDrop})

	want := "IF EXISTS (SELECT 1 FROM sys.backup_devices WHERE name = N'o''brien')"
	if !strings.Contains(got, want) {
		t.Errorf("script does not guard the drop with %s:\n%s", want, got)
	}
	if !strings.Contains(got, `EXEC sp_dropdevice @logicalname = N'o''brien';`) {
		t.Errorf("script does not drop the device:\n%s", got)
	}
	if strings.Contains(got, "sp_addumpdevice") {
		t.Errorf("ScriptDrop emitted a create:\n%s", got)
	}
	// A scripted drop must not take the physical file with it — that is a
	// choice the operator makes, not one a generated script makes for them.
	if strings.Contains(got, "DELFILE") {
		t.Errorf("scripted drop deletes the backup file:\n%s", got)
	}
}

func TestBuildBackupDeviceScriptTapeKeepsItsType(t *testing.T) {
	d := &BackupDevice{Name: "TapeDev", Type: "TAPE", PhysicalName: `\\.\tape0`}
	got := buildBackupDeviceScript(d, ScriptOptions{})
	if !strings.Contains(got, "@devtype = N'tape'") {
		t.Errorf("a tape device scripts as a disk device:\n%s", got)
	}
}
