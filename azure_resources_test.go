package gosmo

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// azureReads is every read in azure_resources.go, each over a Database whose
// server is s (the server-level reads use s directly).
var azureReads = []struct {
	name string
	read func(d *Database) error
}{
	{"Server.ServerResourceStats", func(d *Database) error {
		_, err := d.server.ServerResourceStats(context.Background(), 0)
		return err
	}},
	{"Server.LatestServerResourceStats", func(d *Database) error {
		_, err := d.server.LatestServerResourceStats(context.Background())
		return err
	}},
	{"Server.InstanceResourceGovernance", func(d *Database) error {
		_, err := d.server.InstanceResourceGovernance(context.Background())
		return err
	}},
	{"Server.OSJobObject", func(d *Database) error {
		_, err := d.server.OSJobObject(context.Background())
		return err
	}},
	{"Server.UserDBResourceGovernance", func(d *Database) error {
		_, err := d.server.UserDBResourceGovernance(context.Background())
		return err
	}},
	{"Database.ResourceStats", func(d *Database) error {
		_, err := d.ResourceStats(context.Background(), 0)
		return err
	}},
	{"Database.LatestResourceStats", func(d *Database) error {
		_, err := d.LatestResourceStats(context.Background())
		return err
	}},
	{"Database.ResourceGovernance", func(d *Database) error {
		_, err := d.ResourceGovernance(context.Background())
		return err
	}},
}

// TestAzureResourceReadsRefuseOffAzure pins the gate every read here opens
// with: off an Azure engine edition the views do not exist, so the read is
// refused with ErrUnsupportedVersion — what the live version sweep counts as
// "refused", not "failed" — before anything reaches the server.
func TestAzureResourceReadsRefuseOffAzure(t *testing.T) {
	for _, edition := range []EngineEdition{EngineStandard, EngineEnterprise, EngineExpress} {
		for _, r := range azureReads {
			t.Run(r.name, func(t *testing.T) {
				d := qsRecDB(t, 17, nil, nil)
				d.server.info.EngineEdition = edition
				if err := r.read(d); !errors.Is(err, ErrUnsupportedVersion) {
					t.Fatalf("edition %d: err = %v, want ErrUnsupportedVersion", edition, err)
				}
				if calls := qsRec.recorded(); len(calls) != 0 {
					t.Errorf("edition %d: a refused read reached the driver: %q", edition, calls[0].sql)
				}
			})
		}
	}
}

// TestDatabaseAzureReadsRefuseWithoutServerInfo pins serverInfo's nil-safety:
// a Database built without a server, or over one that never read its
// ServerInfo, is refused rather than panicking.
func TestDatabaseAzureReadsRefuseWithoutServerInfo(t *testing.T) {
	for _, d := range []*Database{{Name: "appdb"}, {Name: "appdb", server: &Server{}}} {
		if _, err := d.ResourceStats(context.Background(), 0); !errors.Is(err, ErrUnsupportedVersion) {
			t.Errorf("ResourceStats: err = %v, want ErrUnsupportedVersion", err)
		}
		if _, err := d.ResourceGovernance(context.Background()); !errors.Is(err, ErrUnsupportedVersion) {
			t.Errorf("ResourceGovernance: err = %v, want ErrUnsupportedVersion", err)
		}
	}
}

// TestAzureSingleRowReadsReportAnEmptyViewAsNotFound pins the single-row
// reads on Azure: an empty view (a fresh instance, an idle database, a host
// not in a job object) is ErrNotFound, not a zero-valued row.
func TestAzureSingleRowReadsReportAnEmptyViewAsNotFound(t *testing.T) {
	for _, r := range azureReads {
		if strings.HasSuffix(r.name, "ResourceStats") && !strings.Contains(r.name, "Latest") ||
			r.name == "Server.UserDBResourceGovernance" {
			continue // lists: empty is an empty slice
		}
		t.Run(r.name, func(t *testing.T) {
			d := qsRecDB(t, 17, nil, nil)
			d.server.info.EngineEdition = EngineAzureManagedInst
			if err := r.read(d); !errors.Is(err, ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound", err)
			}
		})
	}
}

// TestAzureResourceReadsBindTheirArguments pins the arguments the parameterised
// reads bind: the row cap (0 meaning "all retained", 1 for the Latest forms),
// and ResourceGovernance's database name, filtered in a WHERE that
// userDBResourceGovernanceRowQuery splices in with strings.Replace — which
// would silently become a no-op, and return every database's row, if the
// ORDER BY it replaces were reworded.
func TestAzureResourceReadsBindTheirArguments(t *testing.T) {
	ctx := context.Background()
	newDB := func(t *testing.T) *Database {
		d := qsRecDB(t, 17, nil, nil)
		d.server.info.EngineEdition = EngineAzureManagedInst
		return d
	}
	cases := []struct {
		name string
		read func(d *Database)
		want []any
	}{
		{"ServerResourceStats(0)", func(d *Database) { d.server.ServerResourceStats(ctx, 0) }, []any{int64(serverResourceStatsAll)}},
		{"LatestServerResourceStats", func(d *Database) { d.server.LatestServerResourceStats(ctx) }, []any{int64(1)}},
		{"ResourceStats(0)", func(d *Database) { d.ResourceStats(ctx, 0) }, []any{int64(databaseResourceStatsAll)}},
		{"ResourceStats(-5)", func(d *Database) { d.ResourceStats(ctx, -5) }, []any{int64(databaseResourceStatsAll)}},
		{"LatestResourceStats", func(d *Database) { d.LatestResourceStats(ctx) }, []any{int64(1)}},
		{"ResourceGovernance", func(d *Database) { d.ResourceGovernance(ctx) }, []any{"appdb"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.read(newDB(t))
			call := qsRec.last(t)
			if !slices.Equal(call.args, c.want) {
				t.Errorf("bound %v, want %v", call.args, c.want)
			}
		})
	}
	if !strings.Contains(userDBResourceGovernanceRowQuery, "WHERE database_name = @p1") ||
		strings.Contains(userDBResourceGovernanceRowQuery, "ORDER BY database_name") {
		t.Errorf("userDBResourceGovernanceRowQuery is not narrowed to one database:\n%s", userDBResourceGovernanceRowQuery)
	}
}
