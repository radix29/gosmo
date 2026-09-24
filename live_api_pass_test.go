//go:build livedb

// Live checks for the 2026-09-23 breaking API pass (the review plan's
// S15-S19) — what the fakes cannot show: that a name-resolved index read
// works from handles with no ObjectID, that ALTER INDEX ... SET accepts a
// statement without IGNORE_DUP_KEY on a constraint-backing index, that a
// backup reaches a logical device, that a failed backup on the progress path
// reports every message, that a zero scale survives CREATE TABLE, and that a
// role name holding ", " comes back as one role.
//
//	go test -tags livedb . -run TestLiveAPIPass -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own database, login and backup device.
package gosmo

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestLiveAPIPass(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const dbName = "gosmo_apipass_live"
	d, drop := liveScratchDB(t, db, ctx, dbName)
	defer drop()
	srv := d.Server()

	liveExecIn(t, d, ctx,
		`CREATE TABLE dbo.T (ID INT NOT NULL CONSTRAINT PK_T PRIMARY KEY, A INT NULL)`,
		`CREATE INDEX IX_T_A ON dbo.T (A)`,
		`INSERT dbo.T (ID, A) SELECT TOP (500) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)), 1 FROM sys.all_objects`,
	)

	t.Run("IndexRef on a TableRef", func(t *testing.T) {
		// A TableRef has ObjectID 0; StorageInfo read by it until S15 and
		// found nothing.
		ix := d.TableRef("dbo", "T").IndexRef("IX_T_A")
		info, err := ix.StorageInfo(ctx)
		if err != nil {
			t.Fatalf("StorageInfo: %v", err)
		}
		if info.RowCount != 500 {
			t.Errorf("RowCount = %d, want 500", info.RowCount)
		}
		f, err := ix.Fragmentation(ctx, FragmentationSampled)
		if err != nil {
			t.Fatalf("Fragmentation: %v", err)
		}
		if f.IndexName != "IX_T_A" || f.PageCount == 0 {
			t.Errorf("Fragmentation = %+v, want IX_T_A with pages", f)
		}
		if _, err := d.TableRef("dbo", "T").IndexRef("nope").StorageInfo(ctx); err == nil {
			t.Error("StorageInfo of a missing index succeeded, want an error")
		}
		if err := ix.Rebuild(ctx, IndexRebuildOptions{FillFactor: 80, PadIndex: new(true)}); err != nil {
			t.Fatalf("Rebuild: %v", err)
		}
		if err := ix.UpdateStatistics(ctx, 0); err != nil {
			t.Fatalf("UpdateStatistics: %v", err)
		}
	})

	t.Run("SetOptions without IGNORE_DUP_KEY on a primary key", func(t *testing.T) {
		pk := d.TableRef("dbo", "T").IndexRef("PK_T")
		if err := pk.SetOptions(ctx, IndexSetOptions{AllowPageLocks: new(false)}); err != nil {
			t.Fatalf("SetOptions: %v", err)
		}
		tbl, err := d.TableByName(ctx, "dbo", "T")
		if err != nil {
			t.Fatalf("TableByName: %v", err)
		}
		got, err := tbl.IndexByName(ctx, "PK_T")
		if err != nil {
			t.Fatalf("IndexByName: %v", err)
		}
		if got.AllowPageLocks || !got.AllowRowLocks {
			t.Errorf("after SetOptions: page locks %v, row locks %v; want false, true (untouched)", got.AllowPageLocks, got.AllowRowLocks)
		}
		if got.Table() != tbl {
			t.Error("IndexByName's index does not carry the table it was read from")
		}
	})

	t.Run("datetime2(0) and decimal(p,0)", func(t *testing.T) {
		_, err := d.CreateTable(ctx, CreateTableRequest{Schema: "dbo", Name: "Scales", Columns: []ColumnDefinition{
			{Name: "D0", DataType: DataTypeDatetime2, Scale: new(0), IsNullable: true},
			{Name: "T0", DataType: DataTypeTime, Scale: new(0), IsNullable: true},
			{Name: "N0", DataType: DataTypeDecimal, Precision: new(10), Scale: new(0), IsNullable: true},
		}})
		if err != nil {
			t.Fatalf("CreateTable: %v", err)
		}
		cols, err := d.TableRef("dbo", "Scales").Columns(ctx)
		if err != nil {
			t.Fatalf("Columns: %v", err)
		}
		for _, c := range cols {
			if c.Scale != 0 {
				t.Errorf("%s: scale %d, want 0", c.Name, c.Scale)
			}
			if c.Name == "N0" && c.Precision != 10 {
				t.Errorf("N0: precision %d, want 10", c.Precision)
			}
		}
	})

	t.Run("backup to a logical device", func(t *testing.T) {
		dir := srv.Info().DefaultBackupPath
		sep := `\`
		if strings.HasPrefix(dir, "/") {
			sep = "/"
		}
		const devName = "gosmo_apipass_dev"
		path := strings.TrimRight(dir, sep) + sep + devName + ".bak"
		cleanup := func() {
			if dev, err := srv.BackupDeviceByName(ctx, devName); err == nil {
				_ = dev.Drop(ctx, true)
			}
		}
		cleanup()
		defer cleanup()
		dev, err := srv.CreateBackupDevice(ctx, CreateBackupDeviceRequest{Name: devName, Type: BackupDeviceDisk, PhysicalName: path})
		if err != nil {
			t.Fatalf("CreateBackupDevice: %v", err)
		}
		var pcts []int
		err = srv.Backup(ctx, BackupOptions{
			Database: dbName, Devices: []BackupTarget{dev.Target()}, Init: true, CopyOnly: true,
			Progress: func(pct int, _ string) {
				if pct >= 0 {
					pcts = append(pcts, pct)
				}
			},
		})
		if err != nil {
			t.Fatalf("Backup to the device: %v", err)
		}
		if len(pcts) == 0 {
			t.Error("no progress reported")
		}
		headers, err := srv.BackupHeaders(ctx, dev.Target())
		if err != nil || len(headers) != 1 || headers[0].DatabaseName != dbName {
			t.Fatalf("BackupHeaders = %+v, %v; want one set of %s", headers, err, dbName)
		}
		// Written to the device's file, not to a file named after the device
		// in the default directory — which is what DISK = N'devname' did.
		files, err := srv.BackupFileList(ctx, headers[0].Position, DiskTarget(path))
		if err != nil || len(files) == 0 {
			t.Errorf("BackupFileList of the device's own path = %v, %v", files, err)
		}
		if err := srv.VerifyBackup(ctx, dev.Target()); err != nil {
			t.Errorf("VerifyBackup: %v", err)
		}
	})

	t.Run("a failed backup with progress reports every message", func(t *testing.T) {
		dir := srv.Info().DefaultBackupPath
		sep := `\`
		if strings.HasPrefix(dir, "/") {
			sep = "/"
		}
		bad := strings.TrimRight(dir, sep) + sep + "gosmo_no_such_dir" + sep + "x.bak"
		err := srv.Backup(ctx, BackupOptions{
			Database: dbName, Devices: []BackupTarget{DiskTarget(bad)}, CopyOnly: true,
			Progress: func(int, string) {},
		})
		if err == nil {
			t.Fatal("backup to a missing directory succeeded")
		}
		msg := err.Error()
		if !strings.Contains(msg, "Cannot open backup device") || !strings.Contains(msg, "terminating abnormally") {
			t.Errorf("error = %q, want both the cause and the termination", msg)
		}
	})

	t.Run("a role name holding a comma", func(t *testing.T) {
		const login = "gosmo_apipass_login"
		dropLogin := func() {
			db.ExecContext(context.Background(), "IF SUSER_ID('"+login+"') IS NOT NULL DROP LOGIN ["+login+"]")
		}
		dropLogin()
		defer dropLogin()
		if _, err := db.ExecContext(ctx, "CREATE LOGIN ["+login+"] WITH PASSWORD = N'Pa55-"+login+"!', CHECK_POLICY = OFF"); err != nil {
			t.Fatalf("CREATE LOGIN: %v", err)
		}
		liveExecIn(t, d, ctx,
			"CREATE USER [apipass_user] FOR LOGIN ["+login+"]",
			"CREATE ROLE [readers, and writers]",
			"ALTER ROLE [readers, and writers] ADD MEMBER [apipass_user]",
			"ALTER ROLE [db_datareader] ADD MEMBER [apipass_user]",
		)
		l, err := srv.LoginByName(ctx, login)
		if err != nil {
			t.Fatalf("LoginByName: %v", err)
		}
		ms, err := l.userMappingsIn(ctx, d, false)
		if err != nil || len(ms) != 1 {
			t.Fatalf("userMappingsIn = %+v, %v; want one mapping", ms, err)
		}
		if want := []string{"db_datareader", "readers, and writers"}; !slices.Equal(ms[0].Roles, want) {
			t.Errorf("roles = %q, want %q", ms[0].Roles, want)
		}
		if err := l.UnmapFromDatabase(ctx, dbName); err != nil {
			t.Fatalf("UnmapFromDatabase: %v", err)
		}
		if ms, err := l.userMappingsIn(ctx, d, false); err != nil || len(ms) != 0 {
			t.Errorf("after UnmapFromDatabase: %+v, %v; want no mapping", ms, err)
		}
	})
}
