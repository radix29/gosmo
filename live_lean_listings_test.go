//go:build livedb

// Live verification of the lean listings (T52, T53): module listings carry
// no text and Definition(ctx) reads it by name; Catalog is one batch of four
// result sets; SecurityPolicies reads every predicate in one query; and
// UserMappings reads every database in one batch, skipping one it cannot
// enter.
//
//	go test -tags livedb . -run TestLiveLeanListings -v \
//	-livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway databases and login; touches nothing
// else.
package gosmo

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestLiveLeanListings(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)

	d, drop := liveScratchDB(t, db, ctx, "gosmo_lean_live")
	t.Cleanup(drop)

	liveExecIn(t, d, ctx,
		`CREATE TABLE dbo.lean_t (id INT NOT NULL PRIMARY KEY, owner_name SYSNAME NOT NULL)`,
		`CREATE TABLE dbo.lean_u (id INT NOT NULL PRIMARY KEY, owner_name SYSNAME NOT NULL)`,
		`CREATE PROCEDURE dbo.lean_p AS SELECT 'lean proc marker'`,
		`CREATE VIEW dbo.lean_v AS SELECT id, 'lean view marker' AS m FROM dbo.lean_t`,
		`CREATE VIEW dbo.lean_enc WITH ENCRYPTION AS SELECT 1 AS one`,
		`CREATE FUNCTION dbo.lean_f() RETURNS TABLE AS RETURN SELECT id, owner_name, 'lean fn marker' AS m FROM dbo.lean_t`,
		`CREATE TRIGGER dbo.lean_tr ON dbo.lean_t AFTER INSERT AS SELECT 'lean trigger marker'`,
		`CREATE RULE dbo.lean_rule AS @value > 0`,
		`CREATE DEFAULT dbo.lean_default AS 42`,
		`CREATE SCHEMA rls`,
		`CREATE FUNCTION rls.lean_pred(@owner AS SYSNAME) RETURNS TABLE WITH SCHEMABINDING
		 AS RETURN SELECT 1 AS ok WHERE @owner = USER_NAME()`,
		`CREATE SECURITY POLICY rls.lean_pol_a
		 ADD FILTER PREDICATE rls.lean_pred(owner_name) ON dbo.lean_t,
		 ADD BLOCK PREDICATE rls.lean_pred(owner_name) ON dbo.lean_t AFTER INSERT
		 WITH (STATE = ON)`,
		`CREATE SECURITY POLICY rls.lean_pol_b
		 ADD FILTER PREDICATE rls.lean_pred(owner_name) ON dbo.lean_u
		 WITH (STATE = OFF)`,
		`CREATE SECURITY POLICY rls.lean_pol_empty WITH (STATE = OFF)`,
	)

	t.Run("Definition reads each module by name", func(t *testing.T) {
		for name, tc := range map[string]struct {
			read func() (string, error)
			want string
		}{
			"procedure": {func() (string, error) { return d.StoredProcedureRef("dbo", "lean_p").Definition(ctx) }, "lean proc marker"},
			"view":      {func() (string, error) { return d.ViewRef("dbo", "lean_v").Definition(ctx) }, "lean view marker"},
			"function":  {func() (string, error) { return d.UserDefinedFunctionRef("dbo", "lean_f").Definition(ctx) }, "lean fn marker"},
			"trigger":   {func() (string, error) { return d.TriggerRef("dbo", "lean_tr").Definition(ctx) }, "lean trigger marker"},
			"rule":      {func() (string, error) { return d.RuleRef("dbo", "lean_rule").Definition(ctx) }, "CREATE RULE"},
			"default":   {func() (string, error) { return d.DefaultRef("dbo", "lean_default").Definition(ctx) }, "CREATE DEFAULT"},
			"system":    {func() (string, error) { return d.StoredProcedureRef("sys", "sp_who").Definition(ctx) }, "sp_who"},
		} {
			got, err := tc.read()
			if err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("%s: definition = %.80q, want it to contain %q", name, got, tc.want)
			}
		}
	})

	t.Run("an encrypted module reads empty, a missing or other-kind one not found", func(t *testing.T) {
		if got, err := d.ViewRef("dbo", "lean_enc").Definition(ctx); err != nil || got != "" {
			t.Errorf("encrypted view: %q, %v; want \"\", nil", got, err)
		}
		if _, err := d.ViewRef("dbo", "lean_t").Definition(ctx); !errors.Is(err, ErrNotFound) {
			t.Errorf("a table read as a view: err = %v, want ErrNotFound", err)
		}
		if _, err := d.StoredProcedureRef("dbo", "lean_v").Definition(ctx); !errors.Is(err, ErrNotFound) {
			t.Errorf("a view read as a procedure: err = %v, want ErrNotFound", err)
		}
		if _, err := d.ViewRef("dbo", "no_such").Definition(ctx); !errors.Is(err, ErrNotFound) {
			t.Errorf("missing view: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("the listings still list what they listed", func(t *testing.T) {
		views, err := d.Views(ctx)
		if err != nil {
			t.Fatalf("Views: %v", err)
		}
		var names []string
		for _, v := range views {
			names = append(names, v.Name)
		}
		if !slices.Equal(names, []string{"lean_enc", "lean_v"}) {
			t.Errorf("views = %v, want the encrypted one too", names)
		}
		procs, err := d.StoredProcedures(ctx)
		if err != nil || len(procs) != 1 || procs[0].Name != "lean_p" {
			t.Errorf("StoredProcedures = %v, %v", procs, err)
		}
		fns, err := d.UserDefinedFunctions(ctx)
		if err != nil || len(fns) != 2 {
			t.Errorf("UserDefinedFunctions = %d, %v; want lean_f and the predicate", len(fns), err)
		}
		trs, err := d.Triggers(ctx)
		if err != nil || len(trs) != 1 || trs[0].Name != "lean_tr" || !slices.Equal(trs[0].Events, []string{"INSERT"}) {
			t.Errorf("Triggers = %+v, %v", trs, err)
		}
		sys, err := d.SystemStoredProcedures(ctx)
		if err != nil || len(sys) < 500 {
			t.Errorf("SystemStoredProcedures = %d, %v; want the shipped hundreds", len(sys), err)
		}
	})

	t.Run("Catalog in one batch", func(t *testing.T) {
		cat, err := d.Catalog(ctx)
		if err != nil {
			t.Fatalf("Catalog: %v", err)
		}
		cols := map[string][]string{}
		for _, o := range cat.Objects {
			for _, c := range o.Columns {
				cols[o.Schema+"."+o.Name] = append(cols[o.Schema+"."+o.Name], c.Name)
			}
		}
		if !slices.Equal(cols["dbo.lean_t"], []string{"id", "owner_name"}) || !slices.Equal(cols["dbo.lean_v"], []string{"id", "m"}) {
			t.Errorf("object columns = %v", cols)
		}
		if !slices.Equal(cat.Schemas, []string{"dbo"}) {
			t.Errorf("schemas = %v", cat.Schemas)
		}
		var fn *CatalogObject
		for i := range cat.Functions {
			if cat.Functions[i].Name == "lean_f" {
				fn = &cat.Functions[i]
			}
		}
		if fn == nil || len(fn.Columns) != 3 || fn.Columns[2].Name != "m" {
			t.Errorf("lean_f = %+v, want its three result columns", fn)
		}

		sysCat, err := d.SystemCatalog(ctx)
		if err != nil {
			t.Fatalf("SystemCatalog: %v", err)
		}
		if len(sysCat.Objects) < 100 || len(sysCat.Functions) == 0 {
			t.Errorf("SystemCatalog = %d objects, %d functions", len(sysCat.Objects), len(sysCat.Functions))
		}
	})

	t.Run("SecurityPolicies groups each policy's predicates", func(t *testing.T) {
		pols, err := d.SecurityPolicies(ctx)
		if err != nil {
			t.Fatalf("SecurityPolicies: %v", err)
		}
		got := map[string][]string{}
		for _, p := range pols {
			got[p.Name] = []string{}
			for _, pr := range p.Predicates {
				got[p.Name] = append(got[p.Name], pr.PredicateType+" "+pr.TargetTable+" "+pr.Operation)
			}
		}
		want := map[string][]string{
			"lean_pol_a":     {"BLOCK lean_t AFTER INSERT", "FILTER lean_t "},
			"lean_pol_b":     {"FILTER lean_u "},
			"lean_pol_empty": {},
		}
		for name, w := range want {
			if !slices.Equal(got[name], w) {
				t.Errorf("%s predicates = %q, want %q", name, got[name], w)
			}
		}
		one, err := d.SecurityPolicyByName(ctx, "rls", "lean_pol_b")
		if err != nil || len(one.Predicates) != 1 || one.Predicates[0].TargetTable != "lean_u" {
			t.Errorf("SecurityPolicyByName = %+v, %v", one, err)
		}
	})

	t.Run("UserMappings reads every database in one batch and skips one it cannot enter", func(t *testing.T) {
		d2, drop2 := liveScratchDB(t, db, ctx, "gosmo_lean_live2")
		t.Cleanup(drop2)
		d3, drop3 := liveScratchDB(t, db, ctx, "gosmo_lean_live3")
		t.Cleanup(drop3)

		const login = "gosmo_lean_login"
		dropLogin := func() {
			db.ExecContext(context.Background(), "IF SUSER_ID('"+login+"') IS NOT NULL DROP LOGIN ["+login+"]")
		}
		dropLogin()
		t.Cleanup(dropLogin)
		if _, err := db.ExecContext(ctx, "CREATE LOGIN ["+login+"] WITH PASSWORD = N'Pa55-"+login+"!', CHECK_POLICY = OFF"); err != nil {
			t.Fatalf("CREATE LOGIN: %v", err)
		}
		liveExecIn(t, d, ctx,
			"CREATE USER [lean_user] FOR LOGIN ["+login+"] WITH DEFAULT_SCHEMA = rls",
			"ALTER ROLE [db_datareader] ADD MEMBER [lean_user]",
			"ALTER ROLE [db_datawriter] ADD MEMBER [lean_user]",
		)
		liveExecIn(t, d2, ctx, "CREATE USER [lean_user2] FOR LOGIN ["+login+"]")
		liveExecIn(t, d3, ctx, "CREATE USER [lean_user3] FOR LOGIN ["+login+"]")

		l, err := d.Server().LoginByName(ctx, login)
		if err != nil {
			t.Fatalf("LoginByName: %v", err)
		}
		summary := func(ms []*LoginUserMapping) []string {
			var out []string
			for _, m := range ms {
				if strings.HasPrefix(m.Database, "gosmo_lean_live") {
					out = append(out, m.Database+":"+m.User+":"+m.DefaultSchema+":"+strings.Join(m.Roles, ","))
				}
			}
			slices.Sort(out)
			return out
		}

		ms, err := l.UserMappings(ctx)
		if err != nil {
			t.Fatalf("UserMappings: %v", err)
		}
		want := []string{
			"gosmo_lean_live2:lean_user2:dbo:",
			"gosmo_lean_live3:lean_user3:dbo:",
			"gosmo_lean_live:lean_user:rls:db_datareader,db_datawriter",
		}
		if got := summary(ms); !slices.Equal(got, want) {
			t.Errorf("mappings = %q, want %q", got, want)
		}

		// A database held in SINGLE_USER by another connection is ONLINE but
		// cannot be entered (Msg 924): the batch's TRY/CATCH skips it.
		if _, err := db.ExecContext(ctx, "ALTER DATABASE [gosmo_lean_live3] SET SINGLE_USER WITH ROLLBACK IMMEDIATE"); err != nil {
			t.Fatalf("SINGLE_USER: %v", err)
		}
		holder, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("Conn: %v", err)
		}
		if _, err := holder.ExecContext(ctx, "USE [gosmo_lean_live3]"); err != nil {
			holder.Close()
			t.Fatalf("hold gosmo_lean_live3: %v", err)
		}
		ms, err = l.UserMappings(ctx)
		holder.Close()
		db.ExecContext(context.Background(), "ALTER DATABASE [gosmo_lean_live3] SET MULTI_USER")
		if err != nil {
			t.Fatalf("UserMappings with a database held: %v", err)
		}
		if got := summary(ms); !slices.Equal(got, []string{want[0], want[2]}) {
			t.Errorf("mappings with gosmo_lean_live3 held = %q, want it skipped and the rest read", got)
		}
	})
}
