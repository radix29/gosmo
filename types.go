// Package smo provides a Go library that mimics Microsoft SQL Server Management Objects (SMO).
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
)

// RecoveryModel mirrors SQL Server recovery model options.
type RecoveryModel string

const (
	RecoveryModelSimple   RecoveryModel = "SIMPLE"
	RecoveryModelFull     RecoveryModel = "FULL"
	RecoveryModelBulkLogged RecoveryModel = "BULK_LOGGED"
)

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

// IndexType represents the type of an index.
type IndexType string

const (
	IndexTypeClustered    IndexType = "CLUSTERED"
	IndexTypeNonClustered IndexType = "NONCLUSTERED"
	IndexTypeXML          IndexType = "XML"
	IndexTypeSpatial      IndexType = "SPATIAL"
	IndexTypeColumnStore  IndexType = "COLUMNSTORE"
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
	PermSelect  ObjectPermission = "SELECT"
	PermInsert  ObjectPermission = "INSERT"
	PermUpdate  ObjectPermission = "UPDATE"
	PermDelete  ObjectPermission = "DELETE"
	PermExecute ObjectPermission = "EXECUTE"
	PermControl ObjectPermission = "CONTROL"
	PermView    ObjectPermission = "VIEW DEFINITION"
)

// BackupAction mirrors SQL Server backup types.
type BackupAction string

const (
	BackupActionDatabase BackupAction = "DATABASE"
	BackupActionLog      BackupAction = "LOG"
	BackupActionFiles    BackupAction = "FILES"
)

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
	Name      string
	IsDefault bool
	Files     []DatabaseFile
}

// DatabaseFile represents a single data or log file.
type DatabaseFile struct {
	Name            string
	PhysicalName    string
	Size            int64  // in KB
	MaxSize         int64  // in KB; -1 = unlimited
	GrowthType      string // "KB" | "PERCENT"
	Growth          int64
	IsPrimaryFile   bool
	FileGroupName   string
}

// ServerInfo holds basic information about the connected SQL Server instance.
type ServerInfo struct {
	Name            string
	Edition         string
	ProductVersion  string
	ProductLevel    string
	VersionMajor    int
	VersionMinor    int
	VersionBuild    int
	Collation       string
	IsClustered     bool
	IsHADREnabled   bool
	OSVersion       string
	Platform        string
	MaxConnections  int
	PhysicalMemoryMB int64
	LogicalCPUCount  int
	DefaultDataPath  string
	DefaultLogPath   string
	DefaultBackupPath string
}

// BackupInfo holds metadata about a specific database backup.
type BackupInfo struct {
	DatabaseName   string
	BackupSetName  string
	Description    string
	BackupType     BackupAction
	BackupStart    time.Time
	BackupFinish   time.Time
	BackupSize     int64
	DeviceName     string
	UserName       string
	ServerName     string
	DatabaseVersion int
	CompatibilityLevel CompatibilityLevel
}
