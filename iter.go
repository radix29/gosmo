package gosmo

// iter.go exposes the main SMO collections as range-over-func iterators,
// using the iter.Seq2 type from the standard library (stable since Go 1.23,
// idiomatic in Go 1.26). Callers can range over these directly:
//
//	for t, err := range db.TableSeq(ctx) {
//	    if err != nil { ... }
//	    fmt.Println(t.FullName())
//	}
//
// Every iterator takes a context.Context and runs on the matching
// ...Context collection method, so ranging over one is cancellable and can
// carry a deadline like any other read in this package. ctx is captured
// when the iterator is built; the fetch it governs doesn't run until the
// iterator is ranged over, so a ctx already cancelled by then still stops
// it, and an iterator built but never ranged over queries nothing.
//
// # These are deferred, not streaming
//
// Ranging over one of these runs the underlying ...Context method to
// completion first and then yields from the slice it returned — the fetch is
// deferred until the range begins, but it is not incremental. Two
// consequences worth knowing before choosing an iterator over the slice
// method:
//
//   - Breaking out early saves no query work and no memory. The whole
//     collection was already fetched and is already resident. `for x := range
//     db.TableSeq(ctx)` with a `break` on the first match costs exactly what
//     `db.TablesContext(ctx)` costs.
//   - The error arrives as a single (zero, err) yield in place of any items,
//     never partway through a successful run, because there is no partway: the
//     fetch either produced the whole slice or failed. A loop that checks err
//     on the first iteration has checked it for good.
//
// So these exist for the call-site ergonomics of range-over-func, not to
// bound memory or to let a caller stop the server mid-scan. Where either of
// those matters, none of the collection methods in this package offer it
// today — reach for the ...Context method and a bounded query (a WHERE, a
// TOP) instead.
//
// If a genuinely streaming variant is ever added, it belongs alongside these
// under new names, not as a change to what these do: a caller relying on
// "one query, then yields that cannot fail" would break silently otherwise.

import (
	"context"
	"iter"
)

// seqFrom adapts a FooContext(ctx) ([]T, error)-shaped collection method
// into an iter.Seq2[T, error]: a single (zero, err) if the fetch itself
// fails, then one (item, nil) per element.
//
// fetch runs once per range, not once per iterator, so ranging the same
// iterator value twice issues the query twice and can legitimately return
// different results the second time. Assign the slice from the ...Context
// method instead where one snapshot has to serve two passes.
//
// The yield loop stops on a false return (the caller's break), which skips
// the remaining yields but cannot un-run fetch — see this file's package
// comment on why these are deferred rather than streaming.
func seqFrom[T any](ctx context.Context, fetch func(context.Context) ([]T, error)) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		items, err := fetch(ctx)
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
func (s *Server) DatabaseSeq(ctx context.Context) iter.Seq2[*Database, error] {
	return seqFrom(ctx, s.DatabasesContext)
}

// LoginSeq returns an iterator over all logins on the server.
func (s *Server) LoginSeq(ctx context.Context) iter.Seq2[*Login, error] {
	return seqFrom(ctx, s.LoginsContext)
}

// JobSeq returns an iterator over all SQL Server Agent jobs.
func (s *Server) JobSeq(ctx context.Context) iter.Seq2[*Job, error] {
	return seqFrom(ctx, s.JobsContext)
}

// ServerPermissionSeq returns an iterator over all server-level GRANT/DENY entries.
func (s *Server) ServerPermissionSeq(ctx context.Context) iter.Seq2[*ServerPermissionEntry, error] {
	return seqFrom(ctx, s.ServerPermissionsContext)
}

// CredentialSeq returns an iterator over all server-level credentials.
func (s *Server) CredentialSeq(ctx context.Context) iter.Seq2[*Credential, error] {
	return seqFrom(ctx, s.CredentialsContext)
}

// LanguageSeq returns an iterator over all languages installed on the server.
func (s *Server) LanguageSeq(ctx context.Context) iter.Seq2[*Language, error] {
	return seqFrom(ctx, s.LanguagesContext)
}

// DiskVolumeSeq returns an iterator over the server's storage volumes.
func (s *Server) DiskVolumeSeq(ctx context.Context) iter.Seq2[DiskVolumeInfo, error] {
	return seqFrom(ctx, s.DiskVolumesContext)
}

// BackupHeaderSeq returns an iterator over the backup sets on a backup device.
func (s *Server) BackupHeaderSeq(ctx context.Context, device string) iter.Seq2[*BackupHeader, error] {
	return seqFrom(ctx, func(ctx context.Context) ([]*BackupHeader, error) {
		return s.BackupHeadersContext(ctx, device)
	})
}

// BackupFileSeq returns an iterator over the database files inside the
// backup set on a backup device.
func (s *Server) BackupFileSeq(ctx context.Context, device string) iter.Seq2[*BackupFile, error] {
	return seqFrom(ctx, func(ctx context.Context) ([]*BackupFile, error) {
		return s.BackupFileListContext(ctx, device)
	})
}

// BackupHistorySeq returns an iterator over databaseName's backup/restore
// history, as recorded in msdb.
func (s *Server) BackupHistorySeq(ctx context.Context, databaseName string) iter.Seq2[*BackupInfo, error] {
	return seqFrom(ctx, func(ctx context.Context) ([]*BackupInfo, error) {
		return s.BackupHistoryContext(ctx, databaseName)
	})
}

// ServerRoleSeq returns an iterator over all server-level roles.
func (s *Server) ServerRoleSeq(ctx context.Context) iter.Seq2[*ServerRole, error] {
	return seqFrom(ctx, s.ServerRolesContext)
}

// ServerRoleMemberSeq returns an iterator over a server role's members.
func (s *Server) ServerRoleMemberSeq(ctx context.Context, roleName string) iter.Seq2[*RoleMember, error] {
	return seqFrom(ctx, func(ctx context.Context) ([]*RoleMember, error) {
		return s.ServerRoleMembersContext(ctx, roleName)
	})
}

// LinkedServerSeq returns an iterator over all linked servers.
func (s *Server) LinkedServerSeq(ctx context.Context) iter.Seq2[*LinkedServer, error] {
	return seqFrom(ctx, s.LinkedServersContext)
}

// MailProfileSeq returns an iterator over all Database Mail profiles.
func (s *Server) MailProfileSeq(ctx context.Context) iter.Seq2[*MailProfile, error] {
	return seqFrom(ctx, s.MailProfilesContext)
}

// ConfigurationSeq returns an iterator over all sp_configure options.
func (s *Server) ConfigurationSeq(ctx context.Context) iter.Seq2[*ConfigurationOption, error] {
	return seqFrom(ctx, s.ConfigurationsContext)
}

// CategorySeq returns an iterator over every category of the given class
// (job or alert categories).
func (s *Server) CategorySeq(ctx context.Context, class CategoryClass) iter.Seq2[*Category, error] {
	return seqFrom(ctx, func(ctx context.Context) ([]*Category, error) {
		return s.CategoriesContext(ctx, class)
	})
}

// JobHistorySeq returns an iterator over the most recent job history
// entries across every SQL Server Agent job, up to limit.
func (s *Server) JobHistorySeq(ctx context.Context, limit int) iter.Seq2[*JobHistoryEntry, error] {
	return seqFrom(ctx, func(ctx context.Context) ([]*JobHistoryEntry, error) {
		return s.JobHistoryContext(ctx, limit)
	})
}

// ActiveSessionSeq returns an iterator over every session currently
// connected to the server, optionally including system sessions.
func (s *Server) ActiveSessionSeq(ctx context.Context, includeSystem bool) iter.Seq2[*ActiveSession, error] {
	return seqFrom(ctx, func(ctx context.Context) ([]*ActiveSession, error) {
		return s.ActiveSessionsContext(ctx, includeSystem)
	})
}

// ReadErrorLogSeq returns an iterator over the lines of the given SQL
// Server error log (0 = current, 1 = Errorlog.1, …).
func (s *Server) ReadErrorLogSeq(ctx context.Context, logNumber int) iter.Seq2[*ErrorLogEntry, error] {
	return seqFrom(ctx, func(ctx context.Context) ([]*ErrorLogEntry, error) {
		return s.ReadErrorLogContext(ctx, logNumber)
	})
}

// -- Database ------------------------------------------------------------------

// TableSeq returns an iterator over all user tables in the database.
func (d *Database) TableSeq(ctx context.Context) iter.Seq2[*Table, error] {
	return seqFrom(ctx, d.TablesContext)
}

// ViewSeq returns an iterator over all views in the database.
func (d *Database) ViewSeq(ctx context.Context) iter.Seq2[*View, error] {
	return seqFrom(ctx, d.ViewsContext)
}

// SystemViewSeq returns an iterator over every system catalog view in the
// "sys" schema.
func (d *Database) SystemViewSeq(ctx context.Context) iter.Seq2[*View, error] {
	return seqFrom(ctx, d.SystemViewsContext)
}

// StoredProcedureSeq returns an iterator over all stored procedures.
func (d *Database) StoredProcedureSeq(ctx context.Context) iter.Seq2[*StoredProcedure, error] {
	return seqFrom(ctx, d.StoredProceduresContext)
}

// SystemStoredProcedureSeq returns an iterator over every system stored
// procedure in the "sys" schema.
func (d *Database) SystemStoredProcedureSeq(ctx context.Context) iter.Seq2[*StoredProcedure, error] {
	return seqFrom(ctx, d.SystemStoredProceduresContext)
}

// SystemFunctionSeq returns an iterator over every system function in the
// "sys" schema.
func (d *Database) SystemFunctionSeq(ctx context.Context) iter.Seq2[*UserDefinedFunction, error] {
	return seqFrom(ctx, d.SystemFunctionsContext)
}

// UserSeq returns an iterator over all database users.
func (d *Database) UserSeq(ctx context.Context) iter.Seq2[*User, error] {
	return seqFrom(ctx, d.UsersContext)
}

// SchemaSeq returns an iterator over all schemas in the database.
func (d *Database) SchemaSeq(ctx context.Context) iter.Seq2[*Schema, error] {
	return seqFrom(ctx, d.SchemasContext)
}

// SequenceSeq returns an iterator over all sequences in the database.
func (d *Database) SequenceSeq(ctx context.Context) iter.Seq2[*Sequence, error] {
	return seqFrom(ctx, d.SequencesContext)
}

// SynonymSeq returns an iterator over all synonyms in the database.
func (d *Database) SynonymSeq(ctx context.Context) iter.Seq2[*Synonym, error] {
	return seqFrom(ctx, d.SynonymsContext)
}

// PartitionFunctionSeq returns an iterator over all partition functions in the database.
func (d *Database) PartitionFunctionSeq(ctx context.Context) iter.Seq2[*PartitionFunction, error] {
	return seqFrom(ctx, d.PartitionFunctionsContext)
}

// PartitionSchemeSeq returns an iterator over all partition schemes in the database.
func (d *Database) PartitionSchemeSeq(ctx context.Context) iter.Seq2[*PartitionScheme, error] {
	return seqFrom(ctx, d.PartitionSchemesContext)
}

// DatabaseExtendedPropertySeq returns an iterator over all extended
// properties at database level.
func (d *Database) DatabaseExtendedPropertySeq(ctx context.Context) iter.Seq2[*ExtendedProperty, error] {
	return seqFrom(ctx, d.DatabaseExtendedPropertiesContext)
}

// ColumnMasterKeySeq returns an iterator over all column master keys in the database.
func (d *Database) ColumnMasterKeySeq(ctx context.Context) iter.Seq2[*ColumnMasterKey, error] {
	return seqFrom(ctx, d.ColumnMasterKeysContext)
}

// ColumnEncryptionKeySeq returns an iterator over all column encryption keys in the database.
func (d *Database) ColumnEncryptionKeySeq(ctx context.Context) iter.Seq2[*ColumnEncryptionKey, error] {
	return seqFrom(ctx, d.ColumnEncryptionKeysContext)
}

// SecurityPolicySeq returns an iterator over all security policies in the database.
func (d *Database) SecurityPolicySeq(ctx context.Context) iter.Seq2[*SecurityPolicy, error] {
	return seqFrom(ctx, d.SecurityPoliciesContext)
}

// DatabasePermissionSeq returns an iterator over all database-scoped GRANT/DENY entries.
func (d *Database) DatabasePermissionSeq(ctx context.Context) iter.Seq2[*DatabasePermissionEntry, error] {
	return seqFrom(ctx, d.DatabasePermissionsContext)
}

// FileSeq returns an iterator over every file in the database.
func (d *Database) FileSeq(ctx context.Context) iter.Seq2[*DatabaseFileInfo, error] {
	return seqFrom(ctx, d.FilesContext)
}

// TableChangeTrackingSeq returns an iterator over every user table's
// change tracking state.
func (d *Database) TableChangeTrackingSeq(ctx context.Context) iter.Seq2[*TableChangeTracking, error] {
	return seqFrom(ctx, d.TableChangeTrackingContext)
}

// TriggerSeq returns an iterator over every DML trigger in the database
// (as opposed to Table.TriggerSeq's single-table scope).
func (d *Database) TriggerSeq(ctx context.Context) iter.Seq2[*Trigger, error] {
	return seqFrom(ctx, d.TriggersContext)
}

// DatabaseRoleSeq returns an iterator over all database-level roles.
func (d *Database) DatabaseRoleSeq(ctx context.Context) iter.Seq2[*DatabaseRole, error] {
	return seqFrom(ctx, d.DatabaseRolesContext)
}

// RoleMemberSeq returns an iterator over a database role's members.
func (d *Database) RoleMemberSeq(ctx context.Context, roleName string) iter.Seq2[*RoleMember, error] {
	return seqFrom(ctx, func(ctx context.Context) ([]*RoleMember, error) {
		return d.RoleMembersContext(ctx, roleName)
	})
}

// FileGroupSeq returns an iterator over all filegroups in the database.
func (d *Database) FileGroupSeq(ctx context.Context) iter.Seq2[*FileGroup, error] {
	return seqFrom(ctx, d.FileGroupsContext)
}

// DatabaseScopedConfigSeq returns an iterator over all database-scoped
// configuration options.
func (d *Database) DatabaseScopedConfigSeq(ctx context.Context) iter.Seq2[*DatabaseScopedConfig, error] {
	return seqFrom(ctx, d.DatabaseScopedConfigsContext)
}

// UserDefinedFunctionSeq returns an iterator over all user-created
// functions (as opposed to SystemFunctionSeq's "sys" schema functions).
func (d *Database) UserDefinedFunctionSeq(ctx context.Context) iter.Seq2[*UserDefinedFunction, error] {
	return seqFrom(ctx, d.UserDefinedFunctionsContext)
}

// TablesBySchemaSeq returns an iterator over every user table in the
// given schema.
func (d *Database) TablesBySchemaSeq(ctx context.Context, schema string) iter.Seq2[*Table, error] {
	return seqFrom(ctx, func(ctx context.Context) ([]*Table, error) {
		return d.TablesBySchemaContext(ctx, schema)
	})
}

// DependencySeq returns an iterator over the objects schema.name's own
// definition references.
func (d *Database) DependencySeq(ctx context.Context, schema, name string) iter.Seq2[*Dependency, error] {
	return seqFrom(ctx, func(ctx context.Context) ([]*Dependency, error) {
		return d.DependenciesContext(ctx, schema, name)
	})
}

// DependentSeq returns an iterator over the objects whose own definition
// references schema.name.
func (d *Database) DependentSeq(ctx context.Context, schema, name string) iter.Seq2[*Dependency, error] {
	return seqFrom(ctx, func(ctx context.Context) ([]*Dependency, error) {
		return d.DependentsContext(ctx, schema, name)
	})
}

// ExtendedPropertySeq returns an iterator over the extended properties at
// the given level (as opposed to DatabaseExtendedPropertySeq's
// database-level-only shortcut).
func (d *Database) ExtendedPropertySeq(ctx context.Context, level ExtendedPropertyLevel) iter.Seq2[*ExtendedProperty, error] {
	return seqFrom(ctx, func(ctx context.Context) ([]*ExtendedProperty, error) {
		return d.ExtendedPropertiesContext(ctx, level)
	})
}

// SchemaPermissionSeq returns an iterator over a schema's explicit
// GRANT/DENY entries.
func (d *Database) SchemaPermissionSeq(ctx context.Context, schemaName string) iter.Seq2[*PermissionEntry, error] {
	return seqFrom(ctx, func(ctx context.Context) ([]*PermissionEntry, error) {
		return d.SchemaPermissionsContext(ctx, schemaName)
	})
}

// PermissionSeq returns an iterator over the GRANT/DENY entries recorded
// for schema.name.
func (d *Database) PermissionSeq(ctx context.Context, schema, name string) iter.Seq2[*PermissionEntry, error] {
	return seqFrom(ctx, func(ctx context.Context) ([]*PermissionEntry, error) {
		return d.PermissionsContext(ctx, schema, name)
	})
}

// PermissionsForPrincipalSeq returns an iterator over every explicit
// GRANT/DENY entry recorded for principal across database-, schema-, and
// table/view-scoped securables.
func (d *Database) PermissionsForPrincipalSeq(ctx context.Context, principal string) iter.Seq2[*PrincipalSecurable, error] {
	return seqFrom(ctx, func(ctx context.Context) ([]*PrincipalSecurable, error) {
		return d.PermissionsForPrincipalContext(ctx, principal)
	})
}

// SearchSeq returns an iterator over every table, view, stored procedure,
// function, and trigger whose name contains pattern.
func (d *Database) SearchSeq(ctx context.Context, pattern string) iter.Seq2[*SearchResult, error] {
	return seqFrom(ctx, func(ctx context.Context) ([]*SearchResult, error) {
		return d.SearchContext(ctx, pattern)
	})
}

// -- Login ---------------------------------------------------------------------

// UserMappingSeq returns an iterator over every database this login is
// mapped into.
func (l *Login) UserMappingSeq(ctx context.Context) iter.Seq2[*LoginUserMapping, error] {
	return seqFrom(ctx, l.UserMappingsContext)
}

// -- Table ---------------------------------------------------------------------

// ColumnSeq returns an iterator over all columns in the table, in ordinal order.
func (t *Table) ColumnSeq(ctx context.Context) iter.Seq2[*Column, error] {
	return seqFrom(ctx, t.ColumnsContext)
}

// IndexSeq returns an iterator over all indexes on the table.
func (t *Table) IndexSeq(ctx context.Context) iter.Seq2[*Index, error] {
	return seqFrom(ctx, t.IndexesContext)
}

// ForeignKeySeq returns an iterator over all foreign keys on the table.
func (t *Table) ForeignKeySeq(ctx context.Context) iter.Seq2[*ForeignKey, error] {
	return seqFrom(ctx, t.ForeignKeysContext)
}

// PartitionSeq returns an iterator over per-partition row counts for the table.
func (t *Table) PartitionSeq(ctx context.Context) iter.Seq2[*PartitionInfo, error] {
	return seqFrom(ctx, t.PartitionsContext)
}

// StatisticSeq returns an iterator over all statistics on the table.
func (t *Table) StatisticSeq(ctx context.Context) iter.Seq2[*Statistic, error] {
	return seqFrom(ctx, t.StatisticsContext)
}

// TriggerSeq returns an iterator over all DML triggers attached to the table.
func (t *Table) TriggerSeq(ctx context.Context) iter.Seq2[*Trigger, error] {
	return seqFrom(ctx, t.TriggersContext)
}

// CheckConstraintSeq returns an iterator over all CHECK constraints on the table.
func (t *Table) CheckConstraintSeq(ctx context.Context) iter.Seq2[*CheckConstraint, error] {
	return seqFrom(ctx, t.CheckConstraintsContext)
}

// FragmentationStatsSeq returns an iterator over fragmentation info for
// all indexes on the table. mode must be one of "LIMITED" (fast, default),
// "SAMPLED", or "DETAILED".
func (t *Table) FragmentationStatsSeq(ctx context.Context, mode string) iter.Seq2[*IndexFragmentation, error] {
	return seqFrom(ctx, func(ctx context.Context) ([]*IndexFragmentation, error) {
		return t.FragmentationStatsContext(ctx, mode)
	})
}

// -- Statistic -------------------------------------------------------------

// ColumnSeq returns an iterator over this statistic's columns, in
// stat-column order.
func (st *Statistic) ColumnSeq(ctx context.Context) iter.Seq2[string, error] {
	return seqFrom(ctx, st.ColumnsContext)
}

// DensityVectorSeq returns an iterator over this statistic's density
// vector.
func (st *Statistic) DensityVectorSeq(ctx context.Context) iter.Seq2[*StatisticDensity, error] {
	return seqFrom(ctx, st.DensityVectorContext)
}

// HistogramSeq returns an iterator over this statistic's histogram steps.
func (st *Statistic) HistogramSeq(ctx context.Context) iter.Seq2[*StatisticHistogramStep, error] {
	return seqFrom(ctx, st.HistogramContext)
}

// -- SQL Server Agent ------------------------------------------------------

// StepSeq returns an iterator over a job's steps, in step_id order.
func (j *Job) StepSeq(ctx context.Context) iter.Seq2[*JobStep, error] {
	return seqFrom(ctx, j.StepsContext)
}

// HistorySeq returns an iterator over this job's most recent history
// entries, up to limit.
func (j *Job) HistorySeq(ctx context.Context, limit int) iter.Seq2[*JobHistoryEntry, error] {
	return seqFrom(ctx, func(ctx context.Context) ([]*JobHistoryEntry, error) {
		return j.HistoryContext(ctx, limit)
	})
}

// ScheduleSeq returns an iterator over all SQL Server Agent schedules.
func (s *Server) ScheduleSeq(ctx context.Context) iter.Seq2[*Schedule, error] {
	return seqFrom(ctx, s.SchedulesContext)
}

// ScheduleSeq returns an iterator over the schedules attached to this job.
func (j *Job) ScheduleSeq(ctx context.Context) iter.Seq2[*Schedule, error] {
	return seqFrom(ctx, j.SchedulesContext)
}

// JobSeq returns an iterator over the jobs this schedule is attached to.
func (sch *Schedule) JobSeq(ctx context.Context) iter.Seq2[*Job, error] {
	return seqFrom(ctx, sch.JobsContext)
}

// AlertSeq returns an iterator over all SQL Server Agent alerts.
func (s *Server) AlertSeq(ctx context.Context) iter.Seq2[*Alert, error] {
	return seqFrom(ctx, s.AlertsContext)
}

// EventAlertSeq returns an iterator over the SQL-only-implementable subset
// of alerts (see Server.EventAlerts).
func (s *Server) EventAlertSeq(ctx context.Context) iter.Seq2[*Alert, error] {
	return seqFrom(ctx, s.EventAlertsContext)
}

// OperatorSeq returns an iterator over all SQL Server Agent operators.
func (s *Server) OperatorSeq(ctx context.Context) iter.Seq2[*Operator, error] {
	return seqFrom(ctx, s.OperatorsContext)
}

// NotificationSeq returns an iterator over every operator notified by this alert.
func (a *Alert) NotificationSeq(ctx context.Context) iter.Seq2[*AlertNotification, error] {
	return seqFrom(ctx, a.NotificationsContext)
}

// NotifyingAlertSeq returns an iterator over every alert configured to
// notify this operator.
func (o *Operator) NotifyingAlertSeq(ctx context.Context) iter.Seq2[*AlertNotificationRef, error] {
	return seqFrom(ctx, o.NotifyingAlertsContext)
}

// NotifyingJobSeq returns an iterator over every job configured to e-mail
// this operator on completion.
func (o *Operator) NotifyingJobSeq(ctx context.Context) iter.Seq2[*JobNotificationRef, error] {
	return seqFrom(ctx, o.NotifyingJobsContext)
}
