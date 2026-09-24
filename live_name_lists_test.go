//go:build livedb

// Live verification of T3 and T8. Name lists were aggregated with ',' and
// split back apart in Go, so a name containing the separator came back as
// two: a foreign key on [ref,1] scripted as ([ref], [1]), a role member
// [r, x] as two ADD MEMBER statements. And one varbinary partition function
// failed every partition-function read in the database with Msg 9809.
//
//	go test -tags livedb . -run TestLiveNameLists -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops two throwaway databases, one server role and one login.
package gosmo

import (
	"context"
	"slices"
	"testing"
)

func TestLiveNameListsSurviveTheirSeparator(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_namelists_src_live")
	defer dropSrc()
	dst, dropDst := liveScratchDB(t, db, ctx, "gosmo_namelists_dst_live")
	defer dropDst()

	for _, d := range []*Database{src, dst} {
		liveExecIn(t, d, ctx,
			"ALTER DATABASE ["+d.Name+"] ADD FILEGROUP [fg,1]",
			"CREATE USER [r, x] WITHOUT LOGIN",
		)
	}
	liveExecIn(t, src, ctx,
		"CREATE PARTITION FUNCTION pf_int (int) AS RANGE RIGHT FOR VALUES (100, 200)",
		"CREATE PARTITION FUNCTION pf_vb (varbinary(8)) AS RANGE LEFT FOR VALUES (0x0A, 0x0B0C)",
		"CREATE PARTITION FUNCTION pf_str (nvarchar(20)) AS RANGE LEFT FOR VALUES (N'a,b', N'O''x')",
		"CREATE PARTITION FUNCTION pf_dt (datetime2(3)) AS RANGE RIGHT FOR VALUES ('2026-01-01T12:30:00.123')",
		"CREATE PARTITION FUNCTION pf_float (float) AS RANGE LEFT FOR VALUES (0.1, 1e300)",
		"CREATE PARTITION FUNCTION pf_money (money) AS RANGE LEFT FOR VALUES (10.1234)",
		"CREATE PARTITION FUNCTION pf_guid (uniqueidentifier) AS RANGE LEFT FOR VALUES ('6F9619FF-8B86-D011-B42D-00C04FC964FF')",
		"CREATE PARTITION SCHEME ps_int AS PARTITION pf_int TO ([fg,1], [PRIMARY], [fg,1])",
		"CREATE TABLE dbo.t_k ([k,1] int NOT NULL PRIMARY KEY)",
		"CREATE TABLE dbo.t_ref ([ref,1] int NULL CONSTRAINT fk_ref REFERENCES dbo.t_k ([k,1]))",
		"CREATE ROLE app_role",
		"ALTER ROLE app_role ADD MEMBER [r, x]",
	)

	// T3: every function reads, the varbinary one included.
	pfs, err := src.PartitionFunctions(ctx)
	if err != nil {
		t.Fatalf("PartitionFunctions with a varbinary function present: %v", err)
	}
	want := map[string][]string{
		"pf_int":   {"100", "200"},
		"pf_vb":    {"0x0A", "0x0B0C"},
		"pf_str":   {"N'a,b'", "N'O''x'"},
		"pf_dt":    {"N'2026-01-01T12:30:00.123'"},
		"pf_float": {"1.0000000000000001e-001", "1.0000000000000001e+300"},
		"pf_money": {"10.1234"},
		"pf_guid":  {"N'6F9619FF-8B86-D011-B42D-00C04FC964FF'"},
	}
	if len(pfs) != len(want) {
		t.Fatalf("PartitionFunctions returned %d, want %d", len(pfs), len(want))
	}
	for _, pf := range pfs {
		t.Logf("%s: %v", pf.Name, pf.Boundaries)
		if w := want[pf.Name]; !slices.Equal(pf.Boundaries, w) {
			t.Errorf("%s boundaries = %q, want %q", pf.Name, pf.Boundaries, w)
		}
	}

	ps, err := src.PartitionSchemeByName(ctx, "ps_int")
	if err != nil {
		t.Fatalf("PartitionSchemeByName: %v", err)
	}
	if w := []string{"fg,1", "PRIMARY", "fg,1"}; !slices.Equal(ps.FileGroups, w) {
		t.Errorf("scheme filegroups = %q, want %q", ps.FileGroups, w)
	}

	tbl, err := src.TableByName(ctx, "dbo", "t_ref")
	if err != nil {
		t.Fatalf("TableByName: %v", err)
	}
	fk, err := tbl.ForeignKeyByName(ctx, "fk_ref")
	if err != nil {
		t.Fatalf("ForeignKeyByName: %v", err)
	}
	if !slices.Equal(fk.Columns, []string{"ref,1"}) || !slices.Equal(fk.ReferencedColumns, []string{"k,1"}) {
		t.Errorf("fk columns = %q -> %q, want [ref,1] -> [k,1]", fk.Columns, fk.ReferencedColumns)
	}

	role, err := src.RoleByName(ctx, "app_role")
	if err != nil {
		t.Fatalf("RoleByName: %v", err)
	}
	if !slices.Equal(role.Members, []string{"r, x"}) {
		t.Errorf("role members = %q, want [r, x]", role.Members)
	}
	roles, err := src.DatabaseRoles(ctx)
	if err != nil {
		t.Fatalf("DatabaseRoles: %v", err)
	}
	for _, r := range roles {
		if r.Name == "app_role" && !slices.Equal(r.Members, []string{"r, x"}) {
			t.Errorf("listed role members = %q, want [r, x]", r.Members)
		}
	}

	// Every script replays, and the copy reads back the same.
	sc := NewScripter(src, DefaultScriptOptions())
	for _, pf := range pfs {
		s, err := sc.ScriptPartitionFunction(ctx, pf.Name)
		if err != nil {
			t.Fatalf("ScriptPartitionFunction %s: %v", pf.Name, err)
		}
		liveRunScript(t, dst, ctx, s)
	}
	for _, script := range []func() (string, error){
		func() (string, error) { return sc.ScriptPartitionScheme(ctx, "ps_int") },
		func() (string, error) { return sc.ScriptTable(ctx, "dbo", "t_k") },
		func() (string, error) { return sc.ScriptTable(ctx, "dbo", "t_ref") },
		func() (string, error) { return sc.ScriptDatabaseRole(ctx, "app_role") },
	} {
		s, err := script()
		if err != nil {
			t.Fatalf("script: %v", err)
		}
		t.Logf("%s", s)
		liveRunScript(t, dst, ctx, s)
	}

	copies, err := dst.PartitionFunctions(ctx)
	if err != nil {
		t.Fatalf("PartitionFunctions on the copy: %v", err)
	}
	for _, pf := range copies {
		if w := want[pf.Name]; !slices.Equal(pf.Boundaries, w) {
			t.Errorf("copy of %s boundaries = %q, want %q", pf.Name, pf.Boundaries, w)
		}
	}
	dstRef, err := dst.TableByName(ctx, "dbo", "t_ref")
	if err != nil {
		t.Fatalf("TableByName on the copy: %v", err)
	}
	if fk, err := dstRef.ForeignKeyByName(ctx, "fk_ref"); err != nil {
		t.Errorf("copy's foreign key: %v", err)
	} else if !slices.Equal(fk.Columns, []string{"ref,1"}) || !slices.Equal(fk.ReferencedColumns, []string{"k,1"}) {
		t.Errorf("copy's fk columns = %q -> %q, want [ref,1] -> [k,1]", fk.Columns, fk.ReferencedColumns)
	}
	if r, err := dst.RoleByName(ctx, "app_role"); err != nil {
		t.Errorf("copy's role: %v", err)
	} else if !slices.Equal(r.Members, []string{"r, x"}) {
		t.Errorf("copy's role members = %q, want [r, x]", r.Members)
	}

	// Server roles read the same way.
	srv := src.Server()
	// The login goes first on the way out: a role with a member refuses DROP.
	dropServerObjects := func() {
		c := context.Background()
		db.ExecContext(c, "IF SUSER_ID(N'gosmo_live_l, 1') IS NOT NULL DROP LOGIN [gosmo_live_l, 1]")
		db.ExecContext(c, "IF SUSER_ID(N'gosmo_live_sr') IS NOT NULL DROP SERVER ROLE gosmo_live_sr")
	}
	dropServerObjects()
	defer dropServerObjects()
	for _, q := range []string{
		"CREATE LOGIN [gosmo_live_l, 1] WITH PASSWORD = 'Str0ng!Passw0rd#', CHECK_POLICY = OFF",
		"CREATE SERVER ROLE gosmo_live_sr",
		"ALTER SERVER ROLE gosmo_live_sr ADD MEMBER [gosmo_live_l, 1]",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	sr, err := srv.ServerRoleByName(ctx, "gosmo_live_sr")
	if err != nil {
		t.Fatalf("ServerRoleByName: %v", err)
	}
	if !slices.Equal(sr.Members, []string{"gosmo_live_l, 1"}) {
		t.Errorf("server role members = %q, want [gosmo_live_l, 1]", sr.Members)
	}
	srs, err := srv.ServerRoles(ctx)
	if err != nil {
		t.Fatalf("ServerRoles: %v", err)
	}
	for _, r := range srs {
		if r.Name == "gosmo_live_sr" && !slices.Equal(r.Members, []string{"gosmo_live_l, 1"}) {
			t.Errorf("listed server role members = %q, want [gosmo_live_l, 1]", r.Members)
		}
	}
}
