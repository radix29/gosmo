//go:build livedb

// Live checks for four reads that mis-decoded what the server sends:
// an extended property with a NULL value, histogram keys of types go-mssqldb
// returns as bytes or UTC times, a caller-dependent reference to a procedure,
// and LOGINPROPERTY's 1900-01-01 "never".
//
//	go test -tags livedb . -run TestLiveKReads -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database and login.
package gosmo

import (
	"database/sql"
	"slices"
	"testing"
)

func TestLiveKReads(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	srv := liveServer(t, db, ctx)
	const dbName, loginName = "gosmo_live_kreads", "gosmo_live_kreads_login"
	exec := func(q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	dropDB := `IF DB_ID('` + dbName + `') IS NOT NULL BEGIN ALTER DATABASE [` + dbName + `] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE [` + dbName + `] END`
	dropLogin := `IF SUSER_ID('` + loginName + `') IS NOT NULL DROP LOGIN [` + loginName + `]`
	exec(dropDB)
	exec(dropLogin)
	exec(`CREATE DATABASE [` + dbName + `]`)
	t.Cleanup(func() { db.Exec(dropDB) })
	in := func(q string) { t.Helper(); exec(`USE [` + dbName + `]; ` + q) }

	d, err := srv.DatabaseByName(ctx, dbName)
	if err != nil {
		t.Fatalf("DatabaseByName: %v", err)
	}

	t.Run("NULL extended property", func(t *testing.T) {
		in(`CREATE TABLE dbo.ep (a int)`)
		in(`EXEC sp_addextendedproperty @name = N'x', @level0type = N'SCHEMA', @level0name = N'dbo', @level1type = N'TABLE', @level1name = N'ep'`)
		in(`EXEC sp_addextendedproperty @name = N'y'`)
		props, err := d.ExtendedProperties(ctx, ExtendedPropertyLevel{Level0Type: "SCHEMA", Level0Name: "dbo", Level1Type: "TABLE", Level1Name: "ep"})
		if err != nil {
			t.Fatalf("ExtendedProperties: %v", err)
		}
		if len(props) != 1 || *props[0] != (ExtendedProperty{Name: "x", IsNull: true}) {
			t.Errorf("table properties = %v, want x with a NULL value", props)
		}
		dbProps, err := d.DatabaseExtendedProperties(ctx)
		if err != nil {
			t.Fatalf("DatabaseExtendedProperties: %v", err)
		}
		if len(dbProps) != 1 || *dbProps[0] != (ExtendedProperty{Name: "y", IsNull: true}) {
			t.Errorf("database properties = %v, want y with a NULL value", dbProps)
		}
	})

	t.Run("histogram keys", func(t *testing.T) {
		in(`CREATE TABLE dbo.h (d decimal(9,2), m money, g uniqueidentifier, dt2 datetime2(3), dd date, r real, dto datetimeoffset(2), tm time(4), bt bit)`)
		in(`INSERT dbo.h VALUES (123.45, 12.3456, '6F9619FF-8B86-D011-B42D-00C04FC964FF', '2026-01-02T03:04:05.123', '2026-01-02', 0.1, '2026-01-02T03:04:05.12+02:00', '03:04:05.1234', 1)`)
		want := map[string]string{
			"d": "123.45", "m": "12.3456", "g": "6F9619FF-8B86-D011-B42D-00C04FC964FF",
			"dt2": "2026-01-02 03:04:05.123", "dd": "2026-01-02", "r": "0.1",
			"dto": "2026-01-02 03:04:05.12 +02:00", "tm": "03:04:05.1234", "bt": "1",
		}
		tbl := d.TableRef("dbo", "h")
		for col, key := range want {
			in(`CREATE STATISTICS [s_` + col + `] ON dbo.h ([` + col + `]) WITH FULLSCAN`)
			steps, err := tbl.StatisticRef("s_" + col).Histogram(ctx)
			if err != nil {
				t.Errorf("%s: Histogram: %v", col, err)
				continue
			}
			if len(steps) != 1 || steps[0].RangeHighKey != key {
				var got []string
				for _, s := range steps {
					got = append(got, s.RangeHighKey)
				}
				t.Errorf("%s: RANGE_HI_KEY = %q, want [%q]", col, got, key)
			}
		}
	})

	t.Run("caller-dependent dependents", func(t *testing.T) {
		in(`EXEC('CREATE SCHEMA s2')`)
		in(`EXEC('CREATE PROCEDURE dbo.target AS SELECT 1')`)
		in(`EXEC('CREATE PROCEDURE dbo.calls_unqualified AS EXEC target')`)
		in(`EXEC('CREATE PROCEDURE s2.calls_unqualified AS EXEC target')`)
		in(`EXEC('CREATE PROCEDURE dbo.calls_qualified AS EXEC dbo.target')`)
		in(`CREATE TABLE dbo.t (a int)`)
		in(`EXEC('CREATE VIEW dbo.v_sb WITH SCHEMABINDING AS SELECT a FROM dbo.t')`)
		in(`EXEC('CREATE VIEW dbo.v_plain AS SELECT a FROM t')`)

		names := func(deps []*Dependency) []string {
			var out []string
			for _, dep := range deps {
				out = append(out, dep.Schema+"."+dep.Name)
			}
			slices.Sort(out)
			return out
		}
		deps, err := d.Dependents(ctx, "dbo", "target")
		if err != nil {
			t.Fatalf("Dependents(dbo.target): %v", err)
		}
		if got, want := names(deps), []string{"dbo.calls_qualified", "dbo.calls_unqualified", "s2.calls_unqualified"}; !slices.Equal(got, want) {
			t.Errorf("Dependents(dbo.target) = %v, want %v", got, want)
		}
		deps, err = d.Dependents(ctx, "dbo", "t")
		if err != nil {
			t.Fatalf("Dependents(dbo.t): %v", err)
		}
		bound := map[string]bool{}
		for _, dep := range deps {
			bound[dep.Name] = dep.IsSchemaBound
		}
		if len(bound) != 2 || !bound["v_sb"] || bound["v_plain"] {
			t.Errorf("Dependents(dbo.t) schema binding = %v, want v_sb bound, v_plain not", bound)
		}
		if deps, err := d.Dependents(ctx, "dbo", "nosuch"); err != nil || len(deps) != 0 {
			t.Errorf("Dependents(dbo.nosuch) = %v, %v; want none, no error", deps, err)
		}
	})

	t.Run("bad password sentinel", func(t *testing.T) {
		exec(`CREATE LOGIN [` + loginName + `] WITH PASSWORD = N'Kr3ads!` + loginName + `', CHECK_POLICY = ON`)
		t.Cleanup(func() { db.Exec(dropLogin) })
		var raw sql.NullTime
		if err := db.QueryRowContext(ctx, `SELECT CAST(LOGINPROPERTY(N'`+loginName+`', 'BadPasswordTime') AS datetime2)`).Scan(&raw); err != nil {
			t.Fatalf("raw BadPasswordTime: %v", err)
		}
		t.Logf("server answers BadPasswordTime %v (valid %v)", raw.Time, raw.Valid)
		det, err := srv.LoginRef(loginName).Details(ctx)
		if err != nil {
			t.Fatalf("Details: %v", err)
		}
		if !det.BadPasswordTime.IsZero() {
			t.Errorf("BadPasswordTime = %v, want zero for a login with no failed attempt", det.BadPasswordTime)
		}
		if det.PasswordLastSet.IsZero() {
			t.Error("PasswordLastSet is zero for a login whose password was just set")
		}
	})
}
