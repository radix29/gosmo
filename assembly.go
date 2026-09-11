package gosmo

// CLR assemblies — Programmability ▸ Assemblies.
//
// An assembly is database-scoped and not schema-scoped: sys.assemblies has a
// principal_id (its owner) and no schema_id, so every name here is a single
// identifier, never a two-part one.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AssemblyPermissionSet is the CLR host policy an assembly runs under.
type AssemblyPermissionSet string

// The three permission sets CREATE ASSEMBLY accepts. UNSAFE and EXTERNAL_ACCESS
// additionally require the database to be trustworthy or the assembly to be
// signed.
const (
	AssemblySafe           AssemblyPermissionSet = "SAFE"
	AssemblyExternalAccess AssemblyPermissionSet = "EXTERNAL_ACCESS"
	AssemblyUnsafe         AssemblyPermissionSet = "UNSAFE"
)

// Assembly mirrors a sys.assemblies row.
type Assembly struct {
	db *Database

	Name       string
	AssemblyID int

	// Owner is the database principal that owns the assembly, empty when
	// principal_id is NULL or names a principal that no longer exists.
	Owner string

	// ClrName is the full .NET strong name — "name, version=…, culture=…,
	// publickeytoken=…, processorarchitecture=…". Empty on an assembly the
	// server could not read one from, and on one the caller lacks VIEW
	// DEFINITION on.
	ClrName string

	// PermissionSet is the host policy, as permission_set_desc reports it.
	PermissionSet AssemblyPermissionSet

	// IsVisible reports whether the assembly's routines can be bound to by
	// CREATE PROCEDURE/FUNCTION/TYPE. A referenced-only dependency is
	// registered with is_visible = 0.
	IsVisible bool

	// IsUserDefined is 0 on the assemblies SQL Server ships (Microsoft.
	// SqlServer.Types and friends), which are present in every database.
	IsUserDefined bool

	CreateDate time.Time
	ModifyDate time.Time
}

// Database returns the database the assembly belongs to.
func (a *Assembly) Database() *Database { return a.db }

// assemblySelect is the SELECT the listing and the by-name finder share.
//
// It does not filter is_user_defined: the shipped assemblies are part of what
// the database has, and a caller wanting only the user's own filters on the
// IsUserDefined field rather than getting a listing that silently omits rows
// the catalog shows.
//
// clr_name is guarded by HAS_PERMS_BY_NAME: reading it raises Msg 300 (VIEW
// DEFINITION denied) on any assembly the caller can see but not read the
// definition of, and one such row fails the whole statement. db_ddladmin is
// such a principal — ALTER ANY ASSEMBLY carries no VIEW DEFINITION — so
// without the guard it could not list the assemblies it is allowed to drop.
// The column comes back empty for those rows instead. The shipped assemblies
// are exempt: their clr_name reads for any principal, even public alone, while
// HAS_PERMS_BY_NAME answers 0 on them, so the guard would blank a name the
// caller can plainly see.
const assemblySelect = `
SELECT a.name, a.assembly_id,
       ISNULL(USER_NAME(a.principal_id), ''),
       ISNULL(CASE WHEN a.is_user_defined = 0
                     OR HAS_PERMS_BY_NAME(QUOTENAME(a.name), 'ASSEMBLY',
                                          'VIEW DEFINITION') = 1
                   THEN a.clr_name END, ''),
       ISNULL(a.permission_set_desc, ''),
       a.is_visible, ISNULL(a.is_user_defined, 0),
       a.create_date, a.modify_date
FROM   sys.assemblies a`

func scanAssembly(d *Database, scan func(...any) error) (*Assembly, error) {
	a := &Assembly{db: d}
	var permSet string
	if err := scan(&a.Name, &a.AssemblyID, &a.Owner, &a.ClrName,
		&permSet, &a.IsVisible, &a.IsUserDefined,
		&a.CreateDate, &a.ModifyDate); err != nil {
		return nil, err
	}
	a.PermissionSet = AssemblyPermissionSet(permSet)
	return a, nil
}

// Assemblies returns the CLR assemblies registered in the database.
func (d *Database) Assemblies() ([]*Assembly, error) {
	return d.AssembliesContext(context.Background())
}

// AssembliesContext is the context-aware variant of Assemblies.
func (d *Database) AssembliesContext(ctx context.Context) ([]*Assembly, error) {
	const q = assemblySelect + `
ORDER  BY a.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list assemblies in %q: %w", d.name, err)
	}
	defer rows.Close()

	var asms []*Assembly
	for rows.Next() {
		a, err := scanAssembly(d, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list assemblies in %q: %w", d.name, err)
		}
		asms = append(asms, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list assemblies in %q: %w", d.name, err)
	}
	return asms, nil
}

// AssemblyByName returns one assembly, or a not-found error (errors.Is
// ErrNotFound) when the database has none by that name.
func (d *Database) AssemblyByName(name string) (*Assembly, error) {
	return d.AssemblyByNameContext(context.Background(), name)
}

// AssemblyByNameContext is the context-aware variant of AssemblyByName.
func (d *Database) AssemblyByNameContext(ctx context.Context, name string) (*Assembly, error) {
	var a *Assembly
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		a, err = scanAssembly(d, row.Scan)
		return err
	}, assemblySelect+`
WHERE  a.name = @p1`, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: assembly %q not found in %q", name, d.name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read assembly %q in %q: %w", name, d.name, err)
	}
	return a, nil
}

// ============================================================
// Assembly files
// ============================================================

// AssemblyFile is one file registered with an assembly — the assembly's own
// DLL (file_id 1) plus any debug symbols or source files added with
// ALTER ASSEMBLY … ADD FILE.
//
// Content is deliberately not a field: an assembly binary runs to megabytes,
// and a listing that carried every payload would make reading the *names*
// cost the whole set. Fetch one with Assembly.FileContent.
type AssemblyFile struct {
	Name string

	// FileID is the file's id within the assembly. 1 is the assembly binary
	// itself.
	FileID int

	// ContentLength is the payload size in bytes, from DATALENGTH.
	ContentLength int64
}

// Files returns the files registered with the assembly, without their
// contents.
func (a *Assembly) Files() ([]*AssemblyFile, error) {
	return a.FilesContext(context.Background())
}

// FilesContext is the context-aware variant of Files.
func (a *Assembly) FilesContext(ctx context.Context) ([]*AssemblyFile, error) {
	const q = `
SELECT ISNULL(f.name, ''), f.file_id, ISNULL(DATALENGTH(f.content), 0)
FROM   sys.assembly_files f
WHERE  f.assembly_id = @p1
ORDER  BY f.file_id`

	rows, err := a.db.query(ctx, q, a.AssemblyID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list files of assembly %q in %q: %w", a.Name, a.db.name, err)
	}
	defer rows.Close()

	var files []*AssemblyFile
	for rows.Next() {
		f := &AssemblyFile{}
		if err := rows.Scan(&f.Name, &f.FileID, &f.ContentLength); err != nil {
			return nil, fmt.Errorf("gosmo: list files of assembly %q in %q: %w", a.Name, a.db.name, err)
		}
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list files of assembly %q in %q: %w", a.Name, a.db.name, err)
	}
	return files, nil
}

// FileContent returns the bytes of one of the assembly's files. fileID 1 is
// the assembly binary.
func (a *Assembly) FileContent(fileID int) ([]byte, error) {
	return a.FileContentContext(context.Background(), fileID)
}

// FileContentContext is the context-aware variant of FileContent.
func (a *Assembly) FileContentContext(ctx context.Context, fileID int) ([]byte, error) {
	const q = `
SELECT f.content
FROM   sys.assembly_files f
WHERE  f.assembly_id = @p1 AND f.file_id = @p2`

	var content []byte
	err := a.db.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&content)
	}, q, a.AssemblyID, fileID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: assembly %q in %q has no file %d", a.Name, a.db.name, fileID)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read file %d of assembly %q in %q: %w", fileID, a.Name, a.db.name, err)
	}
	return content, nil
}

// ============================================================
// Assembly modules
// ============================================================

// AssemblyModule is one T-SQL object bound to an assembly — a CLR stored
// procedure, function, trigger or aggregate, and the .NET class and method it
// runs.
type AssemblyModule struct {
	ObjectID int
	Schema   string
	Name     string

	// Type is the sys.objects type code, trimmed: "P" for a CLR procedure,
	// "FS"/"FT" for a scalar or table-valued function, "TA" for a trigger,
	// "AF" for an aggregate.
	Type string

	AssemblyClass  string
	AssemblyMethod string
}

// FullName returns the schema-qualified, bracket-quoted name.
func (m *AssemblyModule) FullName() string { return qualifiedName(m.Schema, m.Name) }

// Modules returns the CLR routines bound to the assembly.
func (a *Assembly) Modules() ([]*AssemblyModule, error) {
	return a.ModulesContext(context.Background())
}

// ModulesContext is the context-aware variant of Modules.
//
// assembly_method is NULL for a CLR aggregate, which has a class and no
// single entry point, so it comes back as the empty string rather than
// failing the scan.
func (a *Assembly) ModulesContext(ctx context.Context) ([]*AssemblyModule, error) {
	const q = `
SELECT am.object_id, SCHEMA_NAME(o.schema_id), o.name, RTRIM(o.type),
       ISNULL(am.assembly_class, ''), ISNULL(am.assembly_method, '')
FROM   sys.assembly_modules am
JOIN   sys.objects o ON o.object_id = am.object_id
WHERE  am.assembly_id = @p1
ORDER  BY SCHEMA_NAME(o.schema_id), o.name`

	rows, err := a.db.query(ctx, q, a.AssemblyID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list modules of assembly %q in %q: %w", a.Name, a.db.name, err)
	}
	defer rows.Close()

	var mods []*AssemblyModule
	for rows.Next() {
		m := &AssemblyModule{}
		if err := rows.Scan(&m.ObjectID, &m.Schema, &m.Name, &m.Type,
			&m.AssemblyClass, &m.AssemblyMethod); err != nil {
			return nil, fmt.Errorf("gosmo: list modules of assembly %q in %q: %w", a.Name, a.db.name, err)
		}
		mods = append(mods, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list modules of assembly %q in %q: %w", a.Name, a.db.name, err)
	}
	return mods, nil
}

// ============================================================
// Drop
// ============================================================

// DropAssembly drops an assembly by name — the form for a caller that has
// the name but not the object. An assembly still referenced by a routine or
// type is refused by the server, as is one another assembly depends on.
func (d *Database) DropAssembly(name string) error {
	return d.DropAssemblyContext(context.Background(), name)
}

// DropAssemblyContext is the context-aware variant of DropAssembly.
func (d *Database) DropAssemblyContext(ctx context.Context, name string) error {
	if _, err := d.exec(ctx, "DROP ASSEMBLY "+QuoteName(name)); err != nil {
		return fmt.Errorf("gosmo: drop assembly %q in %q: %w", name, d.name, err)
	}
	return nil
}

// Drop drops the assembly.
func (a *Assembly) Drop() error { return a.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (a *Assembly) DropContext(ctx context.Context) error {
	return a.db.DropAssemblyContext(ctx, a.Name)
}
