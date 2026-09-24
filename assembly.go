package gosmo

// CLR assemblies — Programmability ▸ Assemblies.
//
// An assembly is database-scoped and not schema-scoped: sys.assemblies has a
// principal_id (its owner) and no schema_id, so every name here is a single
// identifier, never a two-part one.

import (
	"context"
	"database/sql"
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
func (d *Database) Assemblies(ctx context.Context) ([]*Assembly, error) {
	const q = assemblySelect + `
ORDER  BY a.name`

	rows, err := d.query(ctx, q)
	return scanRows(rows, err, fmt.Sprintf("list assemblies in %q", d.Name), func(scan func(...any) error) (*Assembly, error) {
		return scanAssembly(d, scan)
	})
}

// AssemblyByName returns one assembly, or a not-found error (errors.Is
// ErrNotFound) when the database has none by that name.
func (d *Database) AssemblyByName(ctx context.Context, name string) (*Assembly, error) {
	var a *Assembly
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		a, err = scanAssembly(d, row.Scan)
		return err
	}, assemblySelect+`
WHERE  a.name = @p1`, name)
	return foundRow(a, err, notFoundf("gosmo: assembly %q not found in %q", name, d.Name), fmt.Sprintf("read assembly %q in %q", name, d.Name))
}

// AssemblyRef returns a lightweight handle for an assembly by name, without
// querying the catalog — the counterpart of Server.DatabaseRef. Every field
// but the name stays at its zero value; AssemblyByName is what populates them.
//
// Every write on *Assembly addresses it by name, so this handle is enough to
// drop one the caller already knows exists — and is the form to use when
// there is nothing to read yet, such as a script of a CREATE that was only
// collected.
func (d *Database) AssemblyRef(name string) *Assembly {
	return &Assembly{db: d, Name: name}
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
func (a *Assembly) Files(ctx context.Context) ([]*AssemblyFile, error) {
	const q = `
SELECT ISNULL(f.name, ''), f.file_id, ISNULL(DATALENGTH(f.content), 0)
FROM   sys.assembly_files f
WHERE  f.assembly_id = @p1
ORDER  BY f.file_id`

	rows, err := a.db.query(ctx, q, a.AssemblyID)
	return scanRows(rows, err, fmt.Sprintf("list files of assembly %q in %q", a.Name, a.db.Name), func(scan func(...any) error) (*AssemblyFile, error) {
		f := &AssemblyFile{}
		if err := scan(&f.Name, &f.FileID, &f.ContentLength); err != nil {
			return nil, err
		}
		return f, nil
	})
}

// FileContent returns the bytes of one of the assembly's files. fileID 1 is
// the assembly binary.
func (a *Assembly) FileContent(ctx context.Context, fileID int) ([]byte, error) {
	const q = `
SELECT f.content
FROM   sys.assembly_files f
WHERE  f.assembly_id = @p1 AND f.file_id = @p2`

	var content []byte
	err := a.db.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&content)
	}, q, a.AssemblyID, fileID)
	return foundRow(content, err, notFoundf("gosmo: assembly %q in %q has no file %d", a.Name, a.db.Name, fileID), fmt.Sprintf("read file %d of assembly %q in %q", fileID, a.Name, a.db.Name))
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
//
// assembly_method is NULL for a CLR aggregate, which has a class and no single
// entry point, so it comes back as the empty string rather than failing the
// scan.
func (a *Assembly) Modules(ctx context.Context) ([]*AssemblyModule, error) {
	const q = `
SELECT am.object_id, SCHEMA_NAME(o.schema_id), o.name, RTRIM(o.type),
       ISNULL(am.assembly_class, ''), ISNULL(am.assembly_method, '')
FROM   sys.assembly_modules am
JOIN   sys.objects o ON o.object_id = am.object_id
WHERE  am.assembly_id = @p1
ORDER  BY SCHEMA_NAME(o.schema_id), o.name`

	rows, err := a.db.query(ctx, q, a.AssemblyID)
	return scanRows(rows, err, fmt.Sprintf("list modules of assembly %q in %q", a.Name, a.db.Name), func(scan func(...any) error) (*AssemblyModule, error) {
		m := &AssemblyModule{}
		if err := scan(&m.ObjectID, &m.Schema, &m.Name, &m.Type,
			&m.AssemblyClass, &m.AssemblyMethod); err != nil {
			return nil, err
		}
		return m, nil
	})
}

// ============================================================
// Drop
// ============================================================

// Drop drops the assembly. An assembly still referenced by a routine or type
// is refused by the server, as is one another assembly depends on.
func (a *Assembly) Drop(ctx context.Context) error {
	if _, err := a.db.exec(ctx, "DROP ASSEMBLY "+QuoteName(a.Name)); err != nil {
		return fmt.Errorf("gosmo: drop assembly %q in %q: %w", a.Name, a.db.Name, err)
	}
	return nil
}
