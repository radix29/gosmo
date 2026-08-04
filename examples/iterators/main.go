// Command iterators demonstrates gosmo's *Seq API. Every collection method
// has an iterator twin — Databases()/DatabaseSeq(ctx), Tables()/TableSeq(ctx)
// — usable directly in a range statement.
//
// What they are, precisely: ranging over one runs the matching ...Context
// method to completion and then yields from the slice it returned. The fetch
// is deferred until the range starts; it is not incremental. So
//
//   - breaking out early saves no query work and no memory — the whole
//     collection was already fetched;
//   - the error arrives as a single (zero, err) yield in place of every item,
//     never partway through a successful run, because there is no partway;
//   - the context governs that one fetch, so a deadline either kills the whole
//     range or none of it.
//
// They exist for the call-site ergonomics of range-over-func, not to bound
// memory or to stop the server mid-scan. Where those matter, use the
// ...Context method with a bounded query instead.
//
//	MSSQL_SERVER=localhost:1433 MSSQL_USER=sa MSSQL_PASSWORD=YourPw go run ./examples/iterators
package main

import (
	"context"
	"fmt"
	"time"

	gosmo "github.com/radix29/gosmo"
	"github.com/radix29/gosmo/examples/internal/demo"
)

func main() {
	// First, so it runs after the cleanup deferred below it.
	defer demo.Exit()

	srv := demo.Connect()
	defer srv.Close()

	ctx := context.Background()

	// -- The basic shape ---------------------------------------------------
	//
	// Two values per step: the item and the error. Checking err on the first
	// iteration has checked it for good — a failed fetch yields exactly one
	// (nil, err) and nothing else.
	demo.Section("Databases")
	for db, err := range srv.DatabaseSeq(ctx) {
		demo.Must(err)
		fmt.Printf("  %-30s %-10s compat=%d\n", db.Name(), db.State(), db.CompatibilityLevel())
	}

	// -- Breaking out ------------------------------------------------------
	//
	// break stops the loop, not the query: the fetch already ran to
	// completion before the first yield. It reads well, and that is the whole
	// benefit — it is not an optimization.
	demo.Section("First user database")
	var target *gosmo.Database
	for db, err := range srv.DatabaseSeq(ctx) {
		demo.Must(err)
		if db.IsSystem() {
			continue
		}
		target = db
		break
	}
	if target == nil {
		fmt.Println("  no user database on this instance; nothing further to show")
		return
	}
	fmt.Printf("  %s\n", target.Name())

	// -- Deferred, and re-run per range ------------------------------------
	//
	// Building the iterator queries nothing; ranging over it queries. Range
	// the same iterator value twice and the query runs twice, which can
	// legitimately give different answers. Assign the ...Context method's
	// slice when one snapshot has to serve two passes.
	demo.Section("Deferred fetch")
	seq := target.TableSeq(ctx)
	fmt.Println("  iterator built; no query has run yet")
	fmt.Printf("  first range:  %d tables\n", demo.Value(countSeq(seq)))
	fmt.Printf("  second range: %d tables (a second query)\n", demo.Value(countSeq(seq)))

	// -- Nesting -----------------------------------------------------------
	demo.Section("Tables and their columns")
	shown := 0
	for tbl, err := range target.TableSeq(ctx) {
		demo.Must(err)
		if shown >= 5 {
			break
		}
		shown++

		names := make([]string, 0, 8)
		for col, err := range tbl.ColumnSeq(ctx) {
			demo.Must(err)
			if len(names) == 6 {
				names = append(names, "...")
				break
			}
			names = append(names, col.Name+" "+string(col.DataType))
		}
		fmt.Printf("  %-40s %v\n", tbl.FullName(), names)
	}

	// -- Cancellation ------------------------------------------------------
	//
	// The context is captured when the iterator is built but governs the
	// fetch, which does not run until the range does — so a context already
	// cancelled by then still stops it, and it fails as a whole rather than
	// halfway.
	demo.Section("A context cancelled before the range starts")
	cancelled, stop := context.WithCancel(ctx)
	precancelled := srv.DatabaseSeq(cancelled)
	stop()
	for db, err := range precancelled {
		if err != nil {
			fmt.Printf("  fetch failed as one unit: %v\n", err)
			break
		}
		fmt.Printf("  unexpected row: %s\n", db.Name())
	}

	demo.Section("A deadline too short for the fetch")
	tight, cancelTight := context.WithTimeout(ctx, time.Millisecond)
	defer cancelTight()
	seen, failed := 0, error(nil)
	for _, err := range srv.ActiveSessionSeq(tight, true) {
		if err != nil {
			failed = err
			break
		}
		seen++
	}
	fmt.Printf("  %d row(s), ended with: %v\n", seen, failed)
	fmt.Println("  (an all-or-nothing outcome — a slow consumer cannot time the fetch out)")

	// -- Coverage ----------------------------------------------------------
	//
	// Roughly every listing method has an iterator twin; these are a sample.
	demo.Section("A sample of the server-scoped iterators")
	count := func(name string, run func() (int, error)) {
		n, err := run()
		if err != nil {
			fmt.Printf("  %-24s error: %v\n", name, err)
			return
		}
		fmt.Printf("  %-24s %d\n", name, n)
	}
	count("logins", func() (int, error) { return countSeq(srv.LoginSeq(ctx)) })
	count("server roles", func() (int, error) { return countSeq(srv.ServerRoleSeq(ctx)) })
	count("configurations", func() (int, error) { return countSeq(srv.ConfigurationSeq(ctx)) })
	count("linked servers", func() (int, error) { return countSeq(srv.LinkedServerSeq(ctx)) })
	count("credentials", func() (int, error) { return countSeq(srv.CredentialSeq(ctx)) })
	count("agent jobs", func() (int, error) { return countSeq(srv.JobSeq(ctx)) })
	count("agent schedules", func() (int, error) { return countSeq(srv.ScheduleSeq(ctx)) })
	count("languages", func() (int, error) { return countSeq(srv.LanguageSeq(ctx)) })

	demo.Section("A sample of the database-scoped iterators")
	count("schemas", func() (int, error) { return countSeq(target.SchemaSeq(ctx)) })
	count("tables", func() (int, error) { return countSeq(target.TableSeq(ctx)) })
	count("views", func() (int, error) { return countSeq(target.ViewSeq(ctx)) })
	count("procedures", func() (int, error) { return countSeq(target.StoredProcedureSeq(ctx)) })
	count("functions", func() (int, error) { return countSeq(target.UserDefinedFunctionSeq(ctx)) })
	count("triggers", func() (int, error) { return countSeq(target.TriggerSeq(ctx)) })
	count("users", func() (int, error) { return countSeq(target.UserSeq(ctx)) })
	count("roles", func() (int, error) { return countSeq(target.DatabaseRoleSeq(ctx)) })
}

// countSeq drains any iter.Seq2[T, error], returning the count or the error
// that replaced the items. It is generic over T, which is what makes one
// helper enough for every iterator gosmo has.
func countSeq[T any](seq func(func(T, error) bool)) (int, error) {
	n := 0
	for _, err := range seq {
		if err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}
