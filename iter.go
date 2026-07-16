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

// -- Database ------------------------------------------------------------------

// TableSeq returns an iterator over all user tables in the database.
func (d *Database) TableSeq() iter.Seq2[*Table, error] { return seqFrom(d.Tables) }

// ViewSeq returns an iterator over all views in the database.
func (d *Database) ViewSeq() iter.Seq2[*View, error] { return seqFrom(d.Views) }

// StoredProcedureSeq returns an iterator over all stored procedures.
func (d *Database) StoredProcedureSeq() iter.Seq2[*StoredProcedure, error] {
	return seqFrom(d.StoredProcedures)
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
