package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// backup_device.go covers logical backup devices — sys.backup_devices, SSMS's
// Server Objects > Backup Devices folder.
//
// # There is no ALTER
//
// sp_addumpdevice and sp_dropdevice are the whole write surface: a device's
// name, type and physical path are fixed at creation. Changing one means
// dropping the device and adding it again, which is why this file has no
// Alter method and the Properties page in goSSMS is read-only.

// BackupDevice mirrors a row of sys.backup_devices — a named alias for a
// physical backup location, usable anywhere a BACKUP or RESTORE statement
// takes a device.
type BackupDevice struct {
	server *Server

	Name string

	// Type is the device kind as sys.backup_devices reports it: "DISK",
	// "TAPE", "VIRTUAL_DEVICE", or "PERMANENT DUMP DEVICE" for one carried
	// over from a much older version. It is the catalog's type_desc, not the
	// keyword sp_addumpdevice takes — see BackupDeviceType for that.
	Type string

	// PhysicalName is the path (or tape/virtual device name) the alias
	// stands for.
	PhysicalName string
}

// BackupDeviceType is the device kind sp_addumpdevice takes.
type BackupDeviceType string

const (
	// BackupDeviceDisk is a file on the server's filesystem.
	BackupDeviceDisk BackupDeviceType = "disk"
	// BackupDeviceTape is a tape device. Tape backup is removed from current
	// SQL Server versions; a tape device is listed and dropped, not created.
	BackupDeviceTape BackupDeviceType = "tape"
)

const backupDeviceSelect = `
SELECT name, type_desc, physical_name
FROM   sys.backup_devices`

// BackupDevices returns every logical backup device on the server.
func (s *Server) BackupDevices() ([]*BackupDevice, error) {
	return s.BackupDevicesContext(context.Background())
}

// BackupDevicesContext is the context-aware variant of BackupDevices.
func (s *Server) BackupDevicesContext(ctx context.Context) ([]*BackupDevice, error) {
	rows, err := s.query(ctx, backupDeviceSelect+`
ORDER  BY name`)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list backup devices: %w", err)
	}
	defer rows.Close()

	var devices []*BackupDevice
	for rows.Next() {
		d, err := scanBackupDevice(s, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list backup devices: %w", err)
		}
		devices = append(devices, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list backup devices: %w", err)
	}
	return devices, nil
}

// BackupDeviceByName returns one backup device with every field populated, or
// a not-found error (errors.Is ErrNotFound) when the server has none by that
// name.
func (s *Server) BackupDeviceByName(name string) (*BackupDevice, error) {
	return s.BackupDeviceByNameContext(context.Background(), name)
}

// BackupDeviceByNameContext is the context-aware variant of
// BackupDeviceByName.
func (s *Server) BackupDeviceByNameContext(ctx context.Context, name string) (*BackupDevice, error) {
	var d *BackupDevice
	err := s.queryRow(ctx, func(row *sql.Row) error {
		var err error
		d, err = scanBackupDevice(s, row.Scan)
		return err
	}, backupDeviceSelect+`
WHERE  name = @p1`, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: backup device %q not found", name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read backup device %q: %w", name, err)
	}
	return d, nil
}

// BackupDevice returns a lightweight handle for a backup device by name,
// without querying sys.backup_devices — the device-side counterpart of
// Server.Database. Type and PhysicalName stay at their zero value;
// BackupDeviceByName is what populates them.
//
// DropContext addresses the device by name, so this handle is enough to drop
// one the caller already knows exists — and is the only usable form under a
// WithScript context, where BackupDeviceByNameContext's lookup is a real read
// and a device whose sp_addumpdevice was merely collected is not there to
// find.
func (s *Server) BackupDevice(name string) *BackupDevice {
	return &BackupDevice{server: s, Name: name}
}

func scanBackupDevice(s *Server, scan func(...any) error) (*BackupDevice, error) {
	d := &BackupDevice{server: s}
	var typeDesc, physical sql.NullString
	if err := scan(&d.Name, &typeDesc, &physical); err != nil {
		return nil, err
	}
	d.Type = typeDesc.String
	d.PhysicalName = physical.String
	return d, nil
}

// Target returns the device as a BackupTarget, for the RESTORE-side reads
// (BackupHeadersFrom, BackupFileListForSetFrom, VerifyBackupFrom) that report
// what a device holds.
func (d *BackupDevice) Target() BackupTarget { return DeviceTarget(d.Name) }

// -- Writes ----------------------------------------------------------------------

// CreateBackupDevice registers a logical backup device.
func (s *Server) CreateBackupDevice(name string, devType BackupDeviceType, physicalName string) (*BackupDevice, error) {
	return s.CreateBackupDeviceContext(context.Background(), name, devType, physicalName)
}

// CreateBackupDeviceContext is the context-aware variant of
// CreateBackupDevice.
//
// The statement is built as literals rather than bound parameters because
// every gosmo write goes through execContext, which under a WithScript context
// collects the statement text instead of running it — a parameterized EXEC
// would script as a statement carrying @p1 and nothing to bind it to.
func (s *Server) CreateBackupDeviceContext(ctx context.Context, name string, devType BackupDeviceType, physicalName string) (*BackupDevice, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("gosmo: create backup device: device has no name")
	}
	if physicalName == "" {
		return nil, fmt.Errorf("gosmo: create backup device %q: physical name is required", name)
	}
	if devType == "" {
		devType = BackupDeviceDisk
	}

	stmt := fmt.Sprintf("EXEC sp_addumpdevice @devtype = N'%s', @logicalname = N'%s', @physicalname = N'%s'",
		escapeSingle(string(devType)), escapeSingle(name), escapeSingle(physicalName))
	if err := s.execContext(ctx, stmt); err != nil {
		return nil, fmt.Errorf("gosmo: create backup device %q: %w", name, err)
	}
	if Scripting(ctx) {
		// The EXEC was only collected, so there is nothing to read back.
		return s.BackupDevice(name), nil
	}
	return s.BackupDeviceByNameContext(ctx, name)
}

// Drop removes the logical backup device. deleteFile also deletes the
// physical file behind it.
func (d *BackupDevice) Drop(deleteFile bool) error {
	return d.DropContext(context.Background(), deleteFile)
}

// DropContext is the context-aware variant of Drop.
//
// deleteFile is sp_dropdevice's @delfile: false unregisters the alias and
// leaves the backup file on disk, true deletes the file too and is not
// recoverable.
func (d *BackupDevice) DropContext(ctx context.Context, deleteFile bool) error {
	stmt := fmt.Sprintf("EXEC sp_dropdevice @logicalname = N'%s'", escapeSingle(d.Name))
	if deleteFile {
		stmt += ", @delfile = N'DELFILE'"
	}
	if err := d.server.execContext(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: drop backup device %q: %w", d.Name, err)
	}
	return nil
}

// Headers reads the backup sets the device holds (RESTORE HEADERONLY).
func (d *BackupDevice) Headers() ([]*BackupHeader, error) {
	return d.HeadersContext(context.Background())
}

// HeadersContext is the context-aware variant of Headers.
func (d *BackupDevice) HeadersContext(ctx context.Context) ([]*BackupHeader, error) {
	return d.server.BackupHeadersFromContext(ctx, d.Target())
}
