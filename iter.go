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

// -- Server --------------------------------------------------------------------

// DatabaseSeq returns an iterator over all databases on the server.
// The second yield value carries any error that stopped the iteration.
func (s *Server) DatabaseSeq() iter.Seq2[*Database, error] {
	return func(yield func(*Database, error) bool) {
		dbs, err := s.Databases()
		if err != nil {
			yield(nil, err)
			return
		}
		for _, d := range dbs {
			if !yield(d, nil) {
				return
			}
		}
	}
}

// LoginSeq returns an iterator over all logins on the server.
func (s *Server) LoginSeq() iter.Seq2[*Login, error] {
	return func(yield func(*Login, error) bool) {
		logins, err := s.Logins()
		if err != nil {
			yield(nil, err)
			return
		}
		for _, l := range logins {
			if !yield(l, nil) {
				return
			}
		}
	}
}

// JobSeq returns an iterator over all SQL Server Agent jobs.
func (s *Server) JobSeq() iter.Seq2[*Job, error] {
	return func(yield func(*Job, error) bool) {
		jobs, err := s.Jobs()
		if err != nil {
			yield(nil, err)
			return
		}
		for _, j := range jobs {
			if !yield(j, nil) {
				return
			}
		}
	}
}

// -- Database ------------------------------------------------------------------

// TableSeq returns an iterator over all user tables in the database.
func (d *Database) TableSeq() iter.Seq2[*Table, error] {
	return func(yield func(*Table, error) bool) {
		tables, err := d.Tables()
		if err != nil {
			yield(nil, err)
			return
		}
		for _, t := range tables {
			if !yield(t, nil) {
				return
			}
		}
	}
}

// ViewSeq returns an iterator over all views in the database.
func (d *Database) ViewSeq() iter.Seq2[*View, error] {
	return func(yield func(*View, error) bool) {
		views, err := d.Views()
		if err != nil {
			yield(nil, err)
			return
		}
		for _, v := range views {
			if !yield(v, nil) {
				return
			}
		}
	}
}

// StoredProcedureSeq returns an iterator over all stored procedures.
func (d *Database) StoredProcedureSeq() iter.Seq2[*StoredProcedure, error] {
	return func(yield func(*StoredProcedure, error) bool) {
		procs, err := d.StoredProcedures()
		if err != nil {
			yield(nil, err)
			return
		}
		for _, p := range procs {
			if !yield(p, nil) {
				return
			}
		}
	}
}

// UserSeq returns an iterator over all database users.
func (d *Database) UserSeq() iter.Seq2[*User, error] {
	return func(yield func(*User, error) bool) {
		users, err := d.Users()
		if err != nil {
			yield(nil, err)
			return
		}
		for _, u := range users {
			if !yield(u, nil) {
				return
			}
		}
	}
}

// SchemaSeq returns an iterator over all schemas in the database.
func (d *Database) SchemaSeq() iter.Seq2[*Schema, error] {
	return func(yield func(*Schema, error) bool) {
		schemas, err := d.Schemas()
		if err != nil {
			yield(nil, err)
			return
		}
		for _, s := range schemas {
			if !yield(s, nil) {
				return
			}
		}
	}
}

// SequenceSeq returns an iterator over all sequences in the database.
func (d *Database) SequenceSeq() iter.Seq2[*Sequence, error] {
	return func(yield func(*Sequence, error) bool) {
		seqs, err := d.Sequences()
		if err != nil {
			yield(nil, err)
			return
		}
		for _, s := range seqs {
			if !yield(s, nil) {
				return
			}
		}
	}
}

// -- Table ---------------------------------------------------------------------

// ColumnSeq returns an iterator over all columns in the table, in ordinal order.
func (t *Table) ColumnSeq() iter.Seq2[*Column, error] {
	return func(yield func(*Column, error) bool) {
		cols, err := t.Columns()
		if err != nil {
			yield(nil, err)
			return
		}
		for _, c := range cols {
			if !yield(c, nil) {
				return
			}
		}
	}
}

// IndexSeq returns an iterator over all indexes on the table.
func (t *Table) IndexSeq() iter.Seq2[*Index, error] {
	return func(yield func(*Index, error) bool) {
		indexes, err := t.Indexes()
		if err != nil {
			yield(nil, err)
			return
		}
		for _, idx := range indexes {
			if !yield(idx, nil) {
				return
			}
		}
	}
}

// ForeignKeySeq returns an iterator over all foreign keys on the table.
func (t *Table) ForeignKeySeq() iter.Seq2[*ForeignKey, error] {
	return func(yield func(*ForeignKey, error) bool) {
		fks, err := t.ForeignKeys()
		if err != nil {
			yield(nil, err)
			return
		}
		for _, fk := range fks {
			if !yield(fk, nil) {
				return
			}
		}
	}
}

// StatisticSeq returns an iterator over all statistics on the table.
func (t *Table) StatisticSeq() iter.Seq2[*Statistic, error] {
	return func(yield func(*Statistic, error) bool) {
		stats, err := t.Statistics()
		if err != nil {
			yield(nil, err)
			return
		}
		for _, s := range stats {
			if !yield(s, nil) {
				return
			}
		}
	}
}
