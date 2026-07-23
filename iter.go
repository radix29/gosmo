package gosmo

// iter.go exposes lazy iterators over the main SMO collections using the
// iter.Seq / iter.Seq2 types from the standard library (stable since Go 1.23,
// idiomatic in Go 1.26).  Callers can range over these directly:
//
//	for t, err := range db.TableSeq() {
//	    if err != nil { ... }
//	    fmt.Println(t.FullName())
//	}

import "iter"

// seqFrom adapts a Foo() ([]T, error)-shaped collection method into an
// iter.Seq2[T, error]: a single (zero, err) if the fetch itself fails,
// then one (item, nil) per element.
func seqFrom[T any](fetch func() ([]T, error)) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		items, err := fetch()
		if err != nil {
			var zero T
			yield(zero, err)
			return
		}
		for _, it := range items {
			if !yield(it, nil) {
				return
			}
		}
	}
}

// -- Server --------------------------------------------------------------------

// DatabaseSeq returns an iterator over all databases on the server.
// The second yield value carries any error that stopped the iteration.
func (s *Server) DatabaseSeq() iter.Seq2[*Database, error] { return seqFrom(s.Databases) }

// LoginSeq returns an iterator over all logins on the server.
func (s *Server) LoginSeq() iter.Seq2[*Login, error] { return seqFrom(s.Logins) }

// JobSeq returns an iterator over all SQL Server Agent jobs.
func (s *Server) JobSeq() iter.Seq2[*Job, error] { return seqFrom(s.Jobs) }

// ServerPermissionSeq returns an iterator over all server-level GRANT/DENY entries.
func (s *Server) ServerPermissionSeq() iter.Seq2[*ServerPermissionEntry, error] {
	return seqFrom(s.ServerPermissions)
}

// CredentialSeq returns an iterator over all server-level credentials.
func (s *Server) CredentialSeq() iter.Seq2[*Credential, error] { return seqFrom(s.Credentials) }

// LanguageSeq returns an iterator over all languages installed on the server.
func (s *Server) LanguageSeq() iter.Seq2[*Language, error] { return seqFrom(s.Languages) }

// DiskVolumeSeq returns an iterator over the server's storage volumes.
func (s *Server) DiskVolumeSeq() iter.Seq2[DiskVolumeInfo, error] { return seqFrom(s.DiskVolumes) }

// BackupHeaderSeq returns an iterator over the backup sets on a backup device.
func (s *Server) BackupHeaderSeq(device string) iter.Seq2[*BackupHeader, error] {
	return seqFrom(func() ([]*BackupHeader, error) { return s.BackupHeaders(device) })
}

// BackupFileSeq returns an iterator over the database files inside the
// backup set on a backup device.
func (s *Server) BackupFileSeq(device string) iter.Seq2[*BackupFile, error] {
	return seqFrom(func() ([]*BackupFile, error) { return s.BackupFileList(device) })
}

// ServerRoleSeq returns an iterator over all server-level roles.
func (s *Server) ServerRoleSeq() iter.Seq2[*ServerRole, error] { return seqFrom(s.ServerRoles) }

// ServerRoleMemberSeq returns an iterator over a server role's members.
func (s *Server) ServerRoleMemberSeq(roleName string) iter.Seq2[*RoleMember, error] {
	return seqFrom(func() ([]*RoleMember, error) { return s.ServerRoleMembers(roleName) })
}

// LinkedServerSeq returns an iterator over all linked servers.
func (s *Server) LinkedServerSeq() iter.Seq2[*LinkedServer, error] { return seqFrom(s.LinkedServers) }

// MailProfileSeq returns an iterator over all Database Mail profiles.
func (s *Server) MailProfileSeq() iter.Seq2[*MailProfile, error] { return seqFrom(s.MailProfiles) }

// ConfigurationSeq returns an iterator over all sp_configure options.
func (s *Server) ConfigurationSeq() iter.Seq2[*ConfigurationOption, error] {
	return seqFrom(s.Configurations)
}

// CategorySeq returns an iterator over every category of the given class
// (job or alert categories).
func (s *Server) CategorySeq(class CategoryClass) iter.Seq2[*Category, error] {
	return seqFrom(func() ([]*Category, error) { return s.Categories(class) })
}

// JobHistorySeq returns an iterator over the most recent job history
// entries across every SQL Server Agent job, up to limit.
func (s *Server) JobHistorySeq(limit int) iter.Seq2[*JobHistoryEntry, error] {
	return seqFrom(func() ([]*JobHistoryEntry, error) { return s.JobHistory(limit) })
}

// -- Database ------------------------------------------------------------------

// TableSeq returns an iterator over all user tables in the database.
func (d *Database) TableSeq() iter.Seq2[*Table, error] { return seqFrom(d.Tables) }

// ViewSeq returns an iterator over all views in the database.
func (d *Database) ViewSeq() iter.Seq2[*View, error] { return seqFrom(d.Views) }

// SystemViewSeq returns an iterator over every system catalog view in the
// "sys" schema.
func (d *Database) SystemViewSeq() iter.Seq2[*View, error] { return seqFrom(d.SystemViews) }

// StoredProcedureSeq returns an iterator over all stored procedures.
func (d *Database) StoredProcedureSeq() iter.Seq2[*StoredProcedure, error] {
	return seqFrom(d.StoredProcedures)
}

// SystemStoredProcedureSeq returns an iterator over every system stored
// procedure in the "sys" schema.
func (d *Database) SystemStoredProcedureSeq() iter.Seq2[*StoredProcedure, error] {
	return seqFrom(d.SystemStoredProcedures)
}

// SystemFunctionSeq returns an iterator over every system function in the
// "sys" schema.
func (d *Database) SystemFunctionSeq() iter.Seq2[*UserDefinedFunction, error] {
	return seqFrom(d.SystemFunctions)
}

// UserSeq returns an iterator over all database users.
func (d *Database) UserSeq() iter.Seq2[*User, error] { return seqFrom(d.Users) }

// SchemaSeq returns an iterator over all schemas in the database.
func (d *Database) SchemaSeq() iter.Seq2[*Schema, error] { return seqFrom(d.Schemas) }

// SequenceSeq returns an iterator over all sequences in the database.
func (d *Database) SequenceSeq() iter.Seq2[*Sequence, error] { return seqFrom(d.Sequences) }

// SynonymSeq returns an iterator over all synonyms in the database.
func (d *Database) SynonymSeq() iter.Seq2[*Synonym, error] { return seqFrom(d.Synonyms) }

// PartitionFunctionSeq returns an iterator over all partition functions in the database.
func (d *Database) PartitionFunctionSeq() iter.Seq2[*PartitionFunction, error] {
	return seqFrom(d.PartitionFunctions)
}

// PartitionSchemeSeq returns an iterator over all partition schemes in the database.
func (d *Database) PartitionSchemeSeq() iter.Seq2[*PartitionScheme, error] {
	return seqFrom(d.PartitionSchemes)
}

// DatabaseExtendedPropertySeq returns an iterator over all extended
// properties at database level.
func (d *Database) DatabaseExtendedPropertySeq() iter.Seq2[*ExtendedProperty, error] {
	return seqFrom(d.DatabaseExtendedProperties)
}

// ColumnMasterKeySeq returns an iterator over all column master keys in the database.
func (d *Database) ColumnMasterKeySeq() iter.Seq2[*ColumnMasterKey, error] {
	return seqFrom(d.ColumnMasterKeys)
}

// ColumnEncryptionKeySeq returns an iterator over all column encryption keys in the database.
func (d *Database) ColumnEncryptionKeySeq() iter.Seq2[*ColumnEncryptionKey, error] {
	return seqFrom(d.ColumnEncryptionKeys)
}

// SecurityPolicySeq returns an iterator over all security policies in the database.
func (d *Database) SecurityPolicySeq() iter.Seq2[*SecurityPolicy, error] {
	return seqFrom(d.SecurityPolicies)
}

// DatabasePermissionSeq returns an iterator over all database-scoped GRANT/DENY entries.
func (d *Database) DatabasePermissionSeq() iter.Seq2[*DatabasePermissionEntry, error] {
	return seqFrom(d.DatabasePermissions)
}

// FileSeq returns an iterator over every file in the database.
func (d *Database) FileSeq() iter.Seq2[*DatabaseFileInfo, error] { return seqFrom(d.Files) }

// TableChangeTrackingSeq returns an iterator over every user table's
// change tracking state.
func (d *Database) TableChangeTrackingSeq() iter.Seq2[*TableChangeTracking, error] {
	return seqFrom(d.TableChangeTracking)
}

// DatabaseRoleSeq returns an iterator over all database-level roles.
func (d *Database) DatabaseRoleSeq() iter.Seq2[*DatabaseRole, error] {
	return seqFrom(d.DatabaseRoles)
}

// RoleMemberSeq returns an iterator over a database role's members.
func (d *Database) RoleMemberSeq(roleName string) iter.Seq2[*RoleMember, error] {
	return seqFrom(func() ([]*RoleMember, error) { return d.RoleMembers(roleName) })
}

// FileGroupSeq returns an iterator over all filegroups in the database.
func (d *Database) FileGroupSeq() iter.Seq2[*FileGroup, error] { return seqFrom(d.FileGroups) }

// DatabaseScopedConfigSeq returns an iterator over all database-scoped
// configuration options.
func (d *Database) DatabaseScopedConfigSeq() iter.Seq2[*DatabaseScopedConfig, error] {
	return seqFrom(d.DatabaseScopedConfigs)
}

// UserDefinedFunctionSeq returns an iterator over all user-created
// functions (as opposed to SystemFunctionSeq's "sys" schema functions).
func (d *Database) UserDefinedFunctionSeq() iter.Seq2[*UserDefinedFunction, error] {
	return seqFrom(d.UserDefinedFunctions)
}

// TablesBySchemaSeq returns an iterator over every user table in the
// given schema.
func (d *Database) TablesBySchemaSeq(schema string) iter.Seq2[*Table, error] {
	return seqFrom(func() ([]*Table, error) { return d.TablesBySchema(schema) })
}

// DependencySeq returns an iterator over the objects schema.name's own
// definition references.
func (d *Database) DependencySeq(schema, name string) iter.Seq2[*Dependency, error] {
	return seqFrom(func() ([]*Dependency, error) { return d.Dependencies(schema, name) })
}

// DependentSeq returns an iterator over the objects whose own definition
// references schema.name.
func (d *Database) DependentSeq(schema, name string) iter.Seq2[*Dependency, error] {
	return seqFrom(func() ([]*Dependency, error) { return d.Dependents(schema, name) })
}

// ExtendedPropertySeq returns an iterator over the extended properties at
// the given level (as opposed to DatabaseExtendedPropertySeq's
// database-level-only shortcut).
func (d *Database) ExtendedPropertySeq(level ExtendedPropertyLevel) iter.Seq2[*ExtendedProperty, error] {
	return seqFrom(func() ([]*ExtendedProperty, error) { return d.ExtendedProperties(level) })
}

// SchemaPermissionSeq returns an iterator over a schema's explicit
// GRANT/DENY entries.
func (d *Database) SchemaPermissionSeq(schemaName string) iter.Seq2[*PermissionEntry, error] {
	return seqFrom(func() ([]*PermissionEntry, error) { return d.SchemaPermissions(schemaName) })
}

// -- Login ---------------------------------------------------------------------

// UserMappingSeq returns an iterator over every database this login is
// mapped into.
func (l *Login) UserMappingSeq() iter.Seq2[*LoginUserMapping, error] {
	return seqFrom(l.UserMappings)
}

// -- Table ---------------------------------------------------------------------

// ColumnSeq returns an iterator over all columns in the table, in ordinal order.
func (t *Table) ColumnSeq() iter.Seq2[*Column, error] { return seqFrom(t.Columns) }

// IndexSeq returns an iterator over all indexes on the table.
func (t *Table) IndexSeq() iter.Seq2[*Index, error] { return seqFrom(t.Indexes) }

// ForeignKeySeq returns an iterator over all foreign keys on the table.
func (t *Table) ForeignKeySeq() iter.Seq2[*ForeignKey, error] { return seqFrom(t.ForeignKeys) }

// PartitionSeq returns an iterator over per-partition row counts for the table.
func (t *Table) PartitionSeq() iter.Seq2[*PartitionInfo, error] { return seqFrom(t.Partitions) }

// StatisticSeq returns an iterator over all statistics on the table.
func (t *Table) StatisticSeq() iter.Seq2[*Statistic, error] { return seqFrom(t.Statistics) }

// TriggerSeq returns an iterator over all DML triggers attached to the table.
func (t *Table) TriggerSeq() iter.Seq2[*Trigger, error] { return seqFrom(t.Triggers) }

// CheckConstraintSeq returns an iterator over all CHECK constraints on the table.
func (t *Table) CheckConstraintSeq() iter.Seq2[*CheckConstraint, error] {
	return seqFrom(t.CheckConstraints)
}

// -- Statistic -------------------------------------------------------------

// ColumnSeq returns an iterator over this statistic's columns, in
// stat-column order.
func (st *Statistic) ColumnSeq() iter.Seq2[string, error] { return seqFrom(st.Columns) }

// DensityVectorSeq returns an iterator over this statistic's density
// vector.
func (st *Statistic) DensityVectorSeq() iter.Seq2[*StatisticDensity, error] {
	return seqFrom(st.DensityVector)
}

// HistogramSeq returns an iterator over this statistic's histogram steps.
func (st *Statistic) HistogramSeq() iter.Seq2[*StatisticHistogramStep, error] {
	return seqFrom(st.Histogram)
}

// -- SQL Server Agent ------------------------------------------------------

// StepSeq returns an iterator over a job's steps, in step_id order.
func (j *Job) StepSeq() iter.Seq2[*JobStep, error] { return seqFrom(j.Steps) }

// HistorySeq returns an iterator over this job's most recent history
// entries, up to limit.
func (j *Job) HistorySeq(limit int) iter.Seq2[*JobHistoryEntry, error] {
	return seqFrom(func() ([]*JobHistoryEntry, error) { return j.History(limit) })
}

// ScheduleSeq returns an iterator over all SQL Server Agent schedules.
func (s *Server) ScheduleSeq() iter.Seq2[*Schedule, error] { return seqFrom(s.Schedules) }

// ScheduleSeq returns an iterator over the schedules attached to this job.
func (j *Job) ScheduleSeq() iter.Seq2[*Schedule, error] { return seqFrom(j.Schedules) }

// JobSeq returns an iterator over the jobs this schedule is attached to.
func (sch *Schedule) JobSeq() iter.Seq2[*Job, error] { return seqFrom(sch.Jobs) }

// AlertSeq returns an iterator over all SQL Server Agent alerts.
func (s *Server) AlertSeq() iter.Seq2[*Alert, error] { return seqFrom(s.Alerts) }

// EventAlertSeq returns an iterator over the SQL-only-implementable subset
// of alerts (see Server.EventAlerts).
func (s *Server) EventAlertSeq() iter.Seq2[*Alert, error] { return seqFrom(s.EventAlerts) }

// OperatorSeq returns an iterator over all SQL Server Agent operators.
func (s *Server) OperatorSeq() iter.Seq2[*Operator, error] { return seqFrom(s.Operators) }

// NotificationSeq returns an iterator over every operator notified by this alert.
func (a *Alert) NotificationSeq() iter.Seq2[*AlertNotification, error] {
	return seqFrom(a.Notifications)
}

// NotifyingAlertSeq returns an iterator over every alert configured to
// notify this operator.
func (o *Operator) NotifyingAlertSeq() iter.Seq2[*AlertNotificationRef, error] {
	return seqFrom(o.NotifyingAlerts)
}

// NotifyingJobSeq returns an iterator over every job configured to e-mail
// this operator on completion.
func (o *Operator) NotifyingJobSeq() iter.Seq2[*JobNotificationRef, error] {
	return seqFrom(o.NotifyingJobs)
}
