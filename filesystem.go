package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// filesystem.go exposes the SQL Server host's own filesystem: the directory
// listing, fixed-drive list and existence check behind SMO's
// Server.EnumDirectories/EnumFiles. Every path here is interpreted by the
// *server*, not by the process calling gosmo — the two are routinely
// different machines with different path conventions, which is the whole
// reason these exist rather than the caller using os.ReadDir.

// FileSystemEntry is one file or directory in a server-side directory
// listing. Size and LastModified are zero for entries the server reports
// without them (the xp_dirtree fallback on pre-2017 instances).
type FileSystemEntry struct {
	Name         string
	FullPath     string
	IsDirectory  bool
	Size         int64
	LastModified time.Time
}

// FixedDrive is one fixed drive (a volume, on Windows) visible to the SQL
// Server host. On Linux the server reports the single root filesystem.
type FixedDrive struct {
	Name           string // "C:\" on Windows, "/" on Linux
	Type           string // e.g. "DRIVE_FIXED"
	FreeSpaceBytes int64
}

// EnumFileSystem lists the files and directories directly inside path on the
// server host.
func (s *Server) EnumFileSystem(path string) ([]*FileSystemEntry, error) {
	return s.EnumFileSystemContext(context.Background(), path)
}

// EnumFileSystemContext is the context-aware variant of EnumFileSystem.
//
// On SQL Server 2017 and later this reads sys.dm_os_enumerate_filesystem,
// which reports sizes and timestamps; older instances fall back to
// xp_dirtree, which reports names and the file/directory flag only.
func (s *Server) EnumFileSystemContext(ctx context.Context, path string) ([]*FileSystemEntry, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("gosmo: enumerate filesystem: empty path")
	}
	if s.info != nil && s.info.VersionMajor > 0 && s.info.VersionMajor < 14 {
		return s.enumFileSystemDirTree(ctx, path)
	}
	return s.enumFileSystemDMF(ctx, path)
}

func (s *Server) enumFileSystemDMF(ctx context.Context, path string) ([]*FileSystemEntry, error) {
	// level = 0 is not cosmetic: sys.dm_os_enumerate_filesystem walks the
	// whole subtree under @p1, so without it a listing of C:\ enumerates the
	// entire drive. SQL Server pushes the predicate into the function rather
	// than filtering afterwards — verified on 17.0.1125.2, where
	// 'C:\Program Files\Microsoft SQL Server' returned 3091 rows in 2.1s
	// unfiltered and 6 rows in 0.6s with this WHERE clause.
	const q = `
	SELECT full_filesystem_path, file_or_directory_name, is_directory,
	       size_in_bytes, last_write_time
	FROM   sys.dm_os_enumerate_filesystem(@p1, '*')
	WHERE  level = 0`

	rows, err := s.query(ctx, q, path)
	if err != nil {
		return nil, fmt.Errorf("gosmo: enumerate filesystem %q: %w", path, err)
	}
	defer rows.Close()

	var entries []*FileSystemEntry
	for rows.Next() {
		e := &FileSystemEntry{}
		var full, name sql.NullString
		var isDir sql.NullInt64
		var size sql.NullInt64
		var mod sql.NullTime
		if err := rows.Scan(&full, &name, &isDir, &size, &mod); err != nil {
			return nil, fmt.Errorf("gosmo: enumerate filesystem %q: %w", path, err)
		}
		e.Name = name.String
		e.FullPath = full.String
		e.IsDirectory = isDir.Int64 == 1
		e.Size = size.Int64
		e.LastModified = mod.Time
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: enumerate filesystem %q: %w", path, err)
	}
	return entries, nil
}

// enumFileSystemDirTree is the pre-2017 fallback. xp_dirtree's third
// argument asks for the file/directory flag; depth 1 keeps it to the
// immediate children.
func (s *Server) enumFileSystemDirTree(ctx context.Context, path string) ([]*FileSystemEntry, error) {
	rows, err := s.query(ctx, "EXEC master.dbo.xp_dirtree @p1, 1, 1", path)
	if err != nil {
		return nil, fmt.Errorf("gosmo: enumerate filesystem %q: %w", path, err)
	}
	defer rows.Close()

	sep := serverPathSeparator(path)
	var entries []*FileSystemEntry
	for rows.Next() {
		var name sql.NullString
		var depth, isFile sql.NullInt64
		if err := rows.Scan(&name, &depth, &isFile); err != nil {
			return nil, fmt.Errorf("gosmo: enumerate filesystem %q: %w", path, err)
		}
		entries = append(entries, &FileSystemEntry{
			Name:        name.String,
			FullPath:    strings.TrimRight(path, `/\`) + sep + name.String,
			IsDirectory: isFile.Int64 != 1,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: enumerate filesystem %q: %w", path, err)
	}
	return entries, nil
}

// FixedDrives returns the fixed drives visible to the SQL Server host.
func (s *Server) FixedDrives() ([]*FixedDrive, error) {
	return s.FixedDrivesContext(context.Background())
}

// FixedDrivesContext is the context-aware variant of FixedDrives. It reads
// sys.dm_os_enumerate_fixed_drives on SQL Server 2019 and later, and falls
// back to xp_fixeddrives — which reports the drive letter and free megabytes
// only — on older instances.
//
// The fallback is Windows-only: xp_fixeddrives does not exist on SQL Server
// on Linux, so a pre-2019 Linux instance returns an error here rather than
// the single root filesystem FixedDrive documents. That is deliberate rather
// than unnoticed — a Linux host has no drive list to browse, and goSSMS's
// file dialog only asks for one when a path walks above a root, which
// "/"-separated paths never do (PosixPathRules.Parent("/") == "/"). Do not
// "fix" it by synthesizing a "/" entry; there is no caller that would see it.
func (s *Server) FixedDrivesContext(ctx context.Context) ([]*FixedDrive, error) {
	if s.info != nil && s.info.VersionMajor > 0 && s.info.VersionMajor < 15 {
		return s.fixedDrivesXP(ctx)
	}

	const q = `
	SELECT fixed_drive_path, drive_type_desc, free_space_in_bytes
	FROM   sys.dm_os_enumerate_fixed_drives`

	rows, err := s.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: enumerate fixed drives: %w", err)
	}
	defer rows.Close()

	var drives []*FixedDrive
	for rows.Next() {
		var name, typ sql.NullString
		var free sql.NullInt64
		if err := rows.Scan(&name, &typ, &free); err != nil {
			return nil, fmt.Errorf("gosmo: enumerate fixed drives: %w", err)
		}
		drives = append(drives, &FixedDrive{Name: name.String, Type: typ.String, FreeSpaceBytes: free.Int64})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: enumerate fixed drives: %w", err)
	}
	return drives, nil
}

func (s *Server) fixedDrivesXP(ctx context.Context) ([]*FixedDrive, error) {
	rows, err := s.query(ctx, "EXEC master.dbo.xp_fixeddrives")
	if err != nil {
		return nil, fmt.Errorf("gosmo: enumerate fixed drives: %w", err)
	}
	defer rows.Close()

	var drives []*FixedDrive
	for rows.Next() {
		var letter sql.NullString
		var freeMB sql.NullInt64
		if err := rows.Scan(&letter, &freeMB); err != nil {
			return nil, fmt.Errorf("gosmo: enumerate fixed drives: %w", err)
		}
		drives = append(drives, &FixedDrive{
			Name:           letter.String + `:\`,
			Type:           "DRIVE_FIXED",
			FreeSpaceBytes: freeMB.Int64 * 1024 * 1024,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: enumerate fixed drives: %w", err)
	}
	return drives, nil
}

// FileSystemExists reports whether path exists on the server host and
// whether it is a directory. A path that doesn't exist is not an error:
// exists is false and err is nil.
func (s *Server) FileSystemExists(path string) (exists, isDirectory bool, err error) {
	return s.FileSystemExistsContext(context.Background(), path)
}

// FileSystemExistsContext is the context-aware variant of FileSystemExists.
func (s *Server) FileSystemExistsContext(ctx context.Context, path string) (exists, isDirectory bool, err error) {
	if strings.TrimSpace(path) == "" {
		return false, false, nil
	}
	// xp_fileexist reports a directory as "File Exists" = 0 with "File is a
	// Directory" = 1, so the two columns have to be OR-ed rather than the
	// first one taken as the answer.
	rows, err := s.query(ctx, "EXEC master.dbo.xp_fileexist @p1", path)
	if err != nil {
		return false, false, fmt.Errorf("gosmo: file exists %q: %w", path, err)
	}
	defer rows.Close()

	var fileExists, isDir, parentExists sql.NullInt64
	if rows.Next() {
		if err := rows.Scan(&fileExists, &isDir, &parentExists); err != nil {
			return false, false, fmt.Errorf("gosmo: file exists %q: %w", path, err)
		}
	}
	if err := rows.Err(); err != nil {
		return false, false, fmt.Errorf("gosmo: file exists %q: %w", path, err)
	}
	return fileExists.Int64 == 1 || isDir.Int64 == 1, isDir.Int64 == 1, nil
}

// serverPathSeparator returns the separator a server-side path uses — a
// backslash for Windows paths, "/" otherwise. Deciding from the path itself
// rather than from runtime.GOOS is what keeps a Linux client correct against
// a Windows server and vice versa.
func serverPathSeparator(path string) string {
	if strings.Contains(path, `\`) {
		return `\`
	}
	return "/"
}
