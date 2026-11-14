package gosmo

import (
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestFabricDWStatisticsAvoidUnsupportedDMV(t *testing.T) {
	for _, fabric := range []bool{true, false} {
		for _, view := range []bool{false, true} {
			for _, byName := range []bool{false, true} {
				t.Run(fmt.Sprintf("fabric=%v/view=%v/byName=%v", fabric, view, byName), func(t *testing.T) {
					d := captureDatabase(t)
					d.server.IsFabricDW = fabric
					updated := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
					values := []driver.Value{
						"st_Sales", int64(7), false, true, true, "[Amount] > 0",
						updated, int64(50), int64(100), int64(10), int64(150),
						true, false, int64(9),
					}
					if fabric {
						for _, i := range []int{7, 8, 9, 10, 13} {
							values[i] = nil
						}
					}
					captured.reset(cannedRow{
						match: "FROM   sys.stats s",
						cols: []string{"name", "stats_id", "auto_created", "user_created", "has_filter", "filter_definition",
							"last_updated", "rows_sampled", "rows", "steps", "unfiltered_rows", "no_recompute", "is_incremental", "modification_counter"},
						rows: [][]driver.Value{values},
					})
					table := d.TableRef("sales", "Orders")
					table.ObjectID = 42
					v := d.ViewRef("sales", "Orders")
					v.ObjectID = 42
					var stats []*Statistic
					var err error
					if byName {
						var st *Statistic
						if view {
							st, err = v.StatisticByName(t.Context(), "st_Sales")
						} else {
							st, err = table.StatisticByName(t.Context(), "st_Sales")
						}
						stats = []*Statistic{st}
					} else if view {
						stats, err = v.Statistics(t.Context())
					} else {
						stats, err = table.Statistics(t.Context())
					}
					if err != nil || len(stats) != 1 || stats[0] == nil {
						t.Fatalf("statistics = %v, %v; want one statistic", stats, err)
					}
					st := stats[0]
					if st.Name != "st_Sales" || st.StatID != 7 || !st.IsUserCreated ||
						!st.HasFilter || st.FilterDef != "[Amount] > 0" || !st.NoRecompute ||
						!st.LastUpdated.Equal(updated) {
						t.Errorf("statistics lost catalog metadata: %+v", st)
					}
					if (st.View() == v) != view || (st.Table() == table) == view {
						t.Errorf("statistic has the wrong table/view owner: %+v", st)
					}
					q, args := captured.findArgs("FROM   sys.stats s")
					if strings.Contains(q, "dm_db_stats_properties") == fabric {
						t.Errorf("fabric=%v: incorrect properties source:\n%s", fabric, q)
					}
					if fabric {
						if !strings.Contains(q, "STATS_DATE(s.object_id, s.stats_id)") {
							t.Errorf("Fabric query lost supported last-updated metadata:\n%s", q)
						}
						if st.TotalRows != 0 || st.RowsSampled != 0 || st.Steps != 0 ||
							st.UnfilteredRows != 0 || st.ModificationCounter != 0 {
							t.Errorf("unavailable DMV fields were not left unset: %+v", st)
						}
					} else if st.TotalRows != 100 || st.RowsSampled != 50 || st.Steps != 10 ||
						st.UnfilteredRows != 150 || st.ModificationCounter != 9 {
						t.Errorf("non-Fabric properties changed: %+v", st)
					}
					if len(args) == 0 || args[0] != int64(42) {
						t.Fatalf("query lost its owning object id: %v", args)
					}
					if byName && (len(args) != 2 || args[1] != "st_Sales" || !strings.Contains(q, "s.name = @p2")) {
						t.Errorf("by-name lookup lost its statistic parameter: %s %v", q, args)
					}
				})
			}
		}
	}
}

func TestFabricDWStatisticsDBCCUsesSupportedOptions(t *testing.T) {
	for _, fabric := range []bool{true, false} {
		t.Run(fmt.Sprintf("fabric=%v", fabric), func(t *testing.T) {
			d := captureDatabase(t)
			d.server.IsFabricDW = fabric
			st := d.TableRef("s'a]les", "Order's]").StatisticRef("st_']")
			for _, option := range []string{"STAT_HEADER", "DENSITY_VECTOR", "HISTOGRAM"} {
				switch option {
				case "STAT_HEADER":
					captured.reset(cannedRow{match: "DBCC SHOW_STATISTICS",
						cols: []string{"Rows"}, rows: [][]driver.Value{{int64(100)}}})
					h, err := st.Header(t.Context())
					if err != nil || h.Rows != 100 {
						t.Fatalf("Header = %+v, %v; want 100 rows", h, err)
					}
				case "DENSITY_VECTOR":
					captured.reset(cannedRow{match: "DBCC SHOW_STATISTICS",
						cols: []string{"All density", "Average Length", "Columns"},
						rows: [][]driver.Value{{float64(0.01), float64(4), "OrderID"}}})
					density, err := st.DensityVector(t.Context())
					if err != nil || len(density) != 1 || density[0].Columns != "OrderID" {
						t.Fatalf("DensityVector = %+v, %v; want OrderID", density, err)
					}
				case "HISTOGRAM":
					captured.reset(cannedRow{match: "DBCC SHOW_STATISTICS",
						cols: []string{"RANGE_HI_KEY", "RANGE_ROWS", "EQ_ROWS", "DISTINCT_RANGE_ROWS", "AVG_RANGE_ROWS"},
						rows: [][]driver.Value{{int64(10), float64(2), float64(1), int64(1), float64(2)}}})
					histogram, err := st.Histogram(t.Context())
					if err != nil || len(histogram) != 1 || histogram[0].RangeHighKey != "10" {
						t.Fatalf("Histogram = %+v, %v; want a step ending at 10", histogram, err)
					}
				}
				want := "DBCC SHOW_STATISTICS (N'[s''a]]les].[Order''s]]]', N'st_'']') WITH " + option
				if !fabric {
					want += ", NO_INFOMSGS"
				}
				if got := captured.find("DBCC SHOW_STATISTICS"); !strings.Contains(got, want) ||
					strings.Contains(got, "NO_INFOMSGS") == fabric {
					t.Errorf("DBCC query = %q, want %q", got, want)
				}
			}
		})
	}
}
