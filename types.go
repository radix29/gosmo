// Package gosmo provides a Go library that mimics Microsoft SQL Server Management Objects (SMO).
// It allows you to connect to SQL Server instances and programmatically manage databases,
// tables, schemas, users, logins, indexes, stored procedures, and more.
package gosmo

import "time"

// ============================================================
// Enums
// ============================================================

// ServerVersion represents a SQL Server version.
type ServerVersion int

const (
	SQLServer2012 ServerVersion = 11
	SQLServer2014 ServerVersion = 12
	SQLServer2016 ServerVersion = 13
	SQLServer2017 ServerVersion = 14
	SQLServer2019 ServerVersion = 15
	SQLServer2022 ServerVersion = 16
	SQLServer2025 ServerVersion = 17
)

// MinimumServerVersion is the oldest instance gosmo supports: SQL Server 2016
// SP1 (13.0.4001), the build that introduced CREATE OR ALTER.
//
// Exactly one statement gosmo builds needs SP1 —
// Database.CreateStoredProcedureContext's CREATE OR ALTER PROCEDURE
// (procedure.go). Every catalog read would run on 2016 RTM, so this one
// statement is the whole of the gap; supporting RTM means rewriting it as
// IF EXISTS ... ALTER ... ELSE CREATE, which is a decision to take rather
// than an oversight to fix. TestOnlyKnownSitesEmitCreateOrAlter fails if a
// second such statement appears without one.
//
// scripter.go is not a second site, though it names the keywords:
// alterModuleDefinition recognises a CREATE OR ALTER the server's own stored
// definition already contains and returns it unchanged.
const MinimumServerVersion = SQLServer2016

// RecoveryModel mirrors SQL Server recovery model options.
type RecoveryModel string

const (
	RecoveryModelSimple     RecoveryModel = "SIMPLE"
	RecoveryModelFull       RecoveryModel = "FULL"
	RecoveryModelBulkLogged RecoveryModel = "BULK_LOGGED"
)

var recoveryModelNames = map[RecoveryModel]bool{
	RecoveryModelSimple: true, RecoveryModelFull: true, RecoveryModelBulkLogged: true,
}

// validRecoveryModel reports whether m is a recognized recovery model.
func validRecoveryModel(m RecoveryModel) bool { return recoveryModelNames[m] }

// CompatibilityLevel mirrors SQL Server database compatibility levels.
type CompatibilityLevel int

const (
	CompatLevel2008 CompatibilityLevel = 100
	CompatLevel2012 CompatibilityLevel = 110
	CompatLevel2014 CompatibilityLevel = 120
	CompatLevel2016 CompatibilityLevel = 130
	CompatLevel2017 CompatibilityLevel = 140
	CompatLevel2019 CompatibilityLevel = 150
	CompatLevel2022 CompatibilityLevel = 160
	CompatLevel2025 CompatibilityLevel = 170
)

// DataType mirrors SQL Server column data types.
type DataType string

const (
	DataTypeBigInt           DataType = "bigint"
	DataTypeBinary           DataType = "binary"
	DataTypeBit              DataType = "bit"
	DataTypeChar             DataType = "char"
	DataTypeDate             DataType = "date"
	DataTypeDatetime         DataType = "datetime"
	DataTypeDatetime2        DataType = "datetime2"
	DataTypeDatetimeOffset   DataType = "datetimeoffset"
	DataTypeDecimal          DataType = "decimal"
	DataTypeFloat            DataType = "float"
	DataTypeGeography        DataType = "geography"
	DataTypeGeometry         DataType = "geometry"
	DataTypeHierarchyID      DataType = "hierarchyid"
	DataTypeImage            DataType = "image"
	DataTypeInt              DataType = "int"
	DataTypeMoney            DataType = "money"
	DataTypeNChar            DataType = "nchar"
	DataTypeNText            DataType = "ntext"
	DataTypeNumeric          DataType = "numeric"
	DataTypeNVarChar         DataType = "nvarchar"
	DataTypeReal             DataType = "real"
	DataTypeRowVersion       DataType = "rowversion"
	DataTypeSmallDatetime    DataType = "smalldatetime"
	DataTypeSmallInt         DataType = "smallint"
	DataTypeSmallMoney       DataType = "smallmoney"
	DataTypeSQLVariant       DataType = "sql_variant"
	DataTypeText             DataType = "text"
	DataTypeTime             DataType = "time"
	DataTypeTinyInt          DataType = "tinyint"
	DataTypeUniqueIdentifier DataType = "uniqueidentifier"
	DataTypeVarBinary        DataType = "varbinary"
	DataTypeVarChar          DataType = "varchar"
	DataTypeXML              DataType = "xml"
)

var dataTypeNames = map[DataType]bool{
	DataTypeBigInt: true, DataTypeBinary: true, DataTypeBit: true, DataTypeChar: true,
	DataTypeDate: true, DataTypeDatetime: true, DataTypeDatetime2: true, DataTypeDatetimeOffset: true,
	DataTypeDecimal: true, DataTypeFloat: true, DataTypeGeography: true, DataTypeGeometry: true,
	DataTypeHierarchyID: true, DataTypeImage: true, DataTypeInt: true, DataTypeMoney: true,
	DataTypeNChar: true, DataTypeNText: true, DataTypeNumeric: true, DataTypeNVarChar: true,
	DataTypeReal: true, DataTypeRowVersion: true, DataTypeSmallDatetime: true, DataTypeSmallInt: true,
	DataTypeSmallMoney: true, DataTypeSQLVariant: true, DataTypeText: true, DataTypeTime: true,
	DataTypeTinyInt: true, DataTypeUniqueIdentifier: true, DataTypeVarBinary: true,
	DataTypeVarChar: true, DataTypeXML: true,
}

// validDataType reports whether t is a recognized column data type.
func validDataType(t DataType) bool { return dataTypeNames[t] }

// IndexType represents the type of an index.
type IndexType string

// The values match sys.indexes.type_desc, except IndexTypeColumnStore,
// which predates IndexTypeClusteredColumnStore and keeps its original
// spelling. A type_desc with no constant here is carried through verbatim
// (see Table.IndexesContext), so Type is never empty for an index that
// exists.
const (
	IndexTypeClustered            IndexType = "CLUSTERED"
	IndexTypeNonClustered         IndexType = "NONCLUSTERED"
	IndexTypeXML                  IndexType = "XML"
	IndexTypeSpatial              IndexType = "SPATIAL"
	IndexTypeColumnStore          IndexType = "COLUMNSTORE"
	IndexTypeClusteredColumnStore IndexType = "CLUSTERED COLUMNSTORE"
)

// IsColumnStore reports whether t is either columnstore index type.
func (t IndexType) IsColumnStore() bool {
	return t == IndexTypeColumnStore || t == IndexTypeClusteredColumnStore
}

// XMLSecondaryIndexType selects which secondary XML index CREATE XML INDEX
// builds — the FOR clause. A secondary index is always built over an
// existing primary XML index (see CreateIndexRequest).
type XMLSecondaryIndexType string

const (
	// XMLSecondaryPath indexes path/value pairs, for predicates on a path.
	XMLSecondaryPath XMLSecondaryIndexType = "PATH"
	// XMLSecondaryValue indexes value/path pairs, for predicates whose path
	// is a wildcard or a descendant axis.
	XMLSecondaryValue XMLSecondaryIndexType = "VALUE"
	// XMLSecondaryProperty indexes primary-key/path/value triples, for
	// property-bag retrieval of one row's values.
	XMLSecondaryProperty XMLSecondaryIndexType = "PROPERTY"
)

// SpatialTessellation is a spatial index's tessellation scheme — the USING
// clause of CREATE SPATIAL INDEX. The GEOMETRY_ schemes apply to a geometry
// column and the GEOGRAPHY_ ones to a geography column; the server rejects
// the mismatch.
type SpatialTessellation string

const (
	SpatialGeometryGrid      SpatialTessellation = "GEOMETRY_GRID"
	SpatialGeometryAutoGrid  SpatialTessellation = "GEOMETRY_AUTO_GRID"
	SpatialGeographyGrid     SpatialTessellation = "GEOGRAPHY_GRID"
	SpatialGeographyAutoGrid SpatialTessellation = "GEOGRAPHY_AUTO_GRID"
)

// IsGeometry reports whether s tessellates a geometry column, the two
// schemes that take a bounding box.
func (s SpatialTessellation) IsGeometry() bool {
	return s == SpatialGeometryGrid || s == SpatialGeometryAutoGrid
}

// IsAutoGrid reports whether s is one of the automatic schemes, which pick
// their own grid densities and so take no GRIDS clause.
func (s SpatialTessellation) IsAutoGrid() bool {
	return s == SpatialGeometryAutoGrid || s == SpatialGeographyAutoGrid
}

// SpatialGridDensity is one grid level's density in a GRIDS clause.
type SpatialGridDensity string

const (
	SpatialGridLow    SpatialGridDensity = "LOW"
	SpatialGridMedium SpatialGridDensity = "MEDIUM"
	SpatialGridHigh   SpatialGridDensity = "HIGH"
)

// PermissionState represents GRANT / DENY / REVOKE.
type PermissionState string

const (
	PermissionGrant  PermissionState = "GRANT"
	PermissionDeny   PermissionState = "DENY"
	PermissionRevoke PermissionState = "REVOKE"
)

// ObjectPermission represents a single permission on a securable.
type ObjectPermission string

const (
	PermSelect             ObjectPermission = "SELECT"
	PermInsert             ObjectPermission = "INSERT"
	PermUpdate             ObjectPermission = "UPDATE"
	PermDelete             ObjectPermission = "DELETE"
	PermExecute            ObjectPermission = "EXECUTE"
	PermControl            ObjectPermission = "CONTROL"
	PermView               ObjectPermission = "VIEW DEFINITION"
	PermAlter              ObjectPermission = "ALTER"
	PermReferences         ObjectPermission = "REFERENCES"
	PermTakeOwnership      ObjectPermission = "TAKE OWNERSHIP"
	PermViewChangeTracking ObjectPermission = "VIEW CHANGE TRACKING"
)

// BackupAction mirrors SQL Server backup types.
type BackupAction string

const (
	BackupActionDatabase     BackupAction = "DATABASE"
	BackupActionLog          BackupAction = "LOG"
	BackupActionFiles        BackupAction = "FILES"
	BackupActionDifferential BackupAction = "DATABASE_DIFFERENTIAL"
)

var backupActionNames = map[BackupAction]bool{
	BackupActionDatabase: true, BackupActionLog: true, BackupActionFiles: true,
	BackupActionDifferential: true,
}

// validBackupAction reports whether a is a recognized backup/restore action.
func validBackupAction(a BackupAction) bool { return backupActionNames[a] }

// ============================================================
// Shared value types
// ============================================================

// ColumnDefault represents a column default constraint.
type ColumnDefault struct {
	Name       string
	Definition string // e.g. "(getdate())" or "((0))"
}

// FileGroup represents a SQL Server filegroup.
type FileGroup struct {
	Name string

	// Type is sys.filegroups.type_desc: "ROWS_FILEGROUP",
	// "FILESTREAM_DATA_FILEGROUP" or "MEMORY_OPTIMIZED_DATA_FILEGROUP". It
	// decides what a file added to the group becomes — ALTER DATABASE ADD FILE
	// has no file-type keyword, so the same clause makes a FILESTREAM file in a
	// FILESTREAM filegroup and an ordinary data file in a ROWS one.
	Type string

	// IsDefault is per filegroup *type*, not per database: a database with a
	// FILESTREAM filegroup reports one default ROWS filegroup and one default
	// FILESTREAM filegroup, both true.
	IsDefault  bool
	IsReadOnly bool
	Files      []DatabaseFile
}

// Filegroup type_desc values, as sys.filegroups reports them.
const (
	RowsFileGroup            = "ROWS_FILEGROUP"
	FileStreamFileGroup      = "FILESTREAM_DATA_FILEGROUP"
	MemoryOptimizedFileGroup = "MEMORY_OPTIMIZED_DATA_FILEGROUP"
)

// IsFileStream reports whether files added to this filegroup are FILESTREAM
// data files, which take neither SIZE nor FILEGROWTH (SQL Server error 5509)
// and whose FILENAME is a directory rather than a file.
func (fg *FileGroup) IsFileStream() bool { return fg.Type == FileStreamFileGroup }

// DatabaseFile represents a single data or log file.
type DatabaseFile struct {
	Name          string
	PhysicalName  string
	Size          int64  // in KB
	MaxSize       int64  // in KB; -1 = unlimited
	GrowthType    string // "KB" | "PERCENT"
	Growth        int64
	IsPrimaryFile bool
	FileGroupName string
}

// ServerInfo holds basic information about the connected SQL Server instance.
type ServerInfo struct {
	Name           string
	Edition        string
	ProductVersion string
	ProductLevel   string
	VersionMajor   int
	VersionMinor   int
	VersionBuild   int
	Collation      string
	IsClustered    bool
	IsHADREnabled  bool
	IsSingleUser   bool
	EngineEdition  int
	// OSVersion is @@VERSION verbatim: the multi-line SQL Server product
	// banner, whose last line names the host OS. Despite the name it is not
	// an OS version string, and it is unfit for a fixed-width label/value row
	// — the first line alone is longer than most. Platform is the parsed OS
	// family. A real OS string would be a new field, never a change of
	// meaning here.
	OSVersion string
	// Platform is the host operating system family — "Windows" or "Linux" —
	// derived from @@VERSION rather than sys.dm_os_host_info so it is
	// populated on pre-2017 instances too. Empty if @@VERSION names neither.
	Platform       string
	MaxConnections int

	// PhysicalMemoryMB and LogicalCPUCount come from sys.dm_os_sys_info and
	// are zero when SysInfoUnavailable is set. Read that flag before showing
	// either: zero there means "not readable", not "none".
	PhysicalMemoryMB int64
	LogicalCPUCount  int

	// SysInfoUnavailable reports that sys.dm_os_sys_info could not be read,
	// leaving PhysicalMemoryMB and LogicalCPUCount at zero. The usual cause
	// is a login without VIEW SERVER STATE (VIEW SERVER PERFORMANCE STATE on
	// SQL Server 2022 and later), which every other field here survives — a
	// db_owner with no server-level rights connects fine and gets everything
	// but these two.
	SysInfoUnavailable bool

	DefaultDataPath   string
	DefaultLogPath    string
	DefaultBackupPath string
}

// BackupInfo holds metadata about a specific database backup.
type BackupInfo struct {
	DatabaseName       string
	BackupSetName      string
	Description        string
	BackupType         BackupAction
	BackupStart        time.Time
	BackupFinish       time.Time
	BackupSize         int64
	DeviceName         string
	UserName           string
	ServerName         string
	DatabaseVersion    int
	CompatibilityLevel CompatibilityLevel
}
