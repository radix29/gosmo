package gosmo

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"slices"
	"strings"
	"testing"
)

// A TableRef carries no ObjectID, and every read keyed by one used to ask for
// object 0 and answer with an empty result — no columns, no indexes, a row
// count of zero — which a caller could not tell from a real empty table.
// Each now refuses the handle with ErrHandleNotLoaded before any query: the
// Database here has no pool behind it, so a read that got past the guard
// fails on the missing connection instead, and is reported.
func TestTableRefReadsAreRefused(t *testing.T) {
	cases := []struct {
		name string
		call func(context.Context, *Table) error
	}{
		{"Detail", func(c context.Context, t *Table) error { return errOnly(t.Detail(c)) }},
		{"Columns", func(c context.Context, t *Table) error { return errOnly(t.Columns(c)) }},
		{"ForeignKeys", func(c context.Context, t *Table) error { return errOnly(t.ForeignKeys(c)) }},
		{"ForeignKeyByName", func(c context.Context, t *Table) error { return errOnly(t.ForeignKeyByName(c, "fk")) }},
		{"CheckConstraints", func(c context.Context, t *Table) error { return errOnly(t.CheckConstraints(c)) }},
		{"Triggers", func(c context.Context, t *Table) error { return errOnly(t.Triggers(c)) }},
		{"RowCount", func(c context.Context, t *Table) error { return errOnly(t.RowCount(c)) }},
		{"Indexes", func(c context.Context, t *Table) error { return errOnly(t.Indexes(c)) }},
		{"IndexByName", func(c context.Context, t *Table) error { return errOnly(t.IndexByName(c, "ix")) }},
		{"DataSpace", func(c context.Context, t *Table) error { return errOnly(t.DataSpace(c)) }},
		{"XMLIndexes", func(c context.Context, t *Table) error { return errOnly(t.XMLIndexes(c)) }},
		{"Partitions", func(c context.Context, t *Table) error { return errOnly(t.Partitions(c)) }},
		{"SpaceUsed", func(c context.Context, t *Table) error { return errOnly(t.SpaceUsed(c)) }},
		{"Statistics", func(c context.Context, t *Table) error { return errOnly(t.Statistics(c)) }},
		{"StatisticByName", func(c context.Context, t *Table) error { return errOnly(t.StatisticByName(c, "st")) }},
		{"EdgeConstraints", func(c context.Context, t *Table) error { return errOnly(t.EdgeConstraints(c)) }},
		{"Index.IncludedColumnsSupported", func(_ context.Context, t *Table) error {
			return t.IndexRef("ix").IncludedColumnsSupported()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Database{server: &Server{}, Name: "AppDB"}
			err := tc.call(context.Background(), d.TableRef("dbo", "t"))
			if !errors.Is(err, ErrHandleNotLoaded) {
				t.Fatalf("err = %v, want ErrHandleNotLoaded", err)
			}
		})
	}
}

// A create on a TableRef is a valid write — it names the table in the
// statement — and only its read-back needs the ObjectID the handle lacks.
// createdObject answers that refusal with the new object's handle, as it does
// a read-back the caller cannot see, rather than report a create that
// happened as failed.
func TestCreateOnATableRefReturnsTheHandle(t *testing.T) {
	ref := captureTable(t).db.TableRef("dbo", "t")

	idx, err := ref.CreateIndex(t.Context(), CreateIndexRequest{Name: "ix", KeyColumns: []IndexColumnDef{{Name: "a"}}})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if idx.Name != "ix" || idx.Table() != ref {
		t.Errorf("CreateIndex = %+v, want the IndexRef handle", idx)
	}
	st, err := ref.CreateStatistic(t.Context(), CreateStatisticRequest{Name: "st", Columns: []string{"a"}})
	if err != nil {
		t.Fatalf("CreateStatistic: %v", err)
	}
	if st.Name != "st" || st.Table() != ref {
		t.Errorf("CreateStatistic = %+v, want the StatisticRef handle", st)
	}
	if captured.find("INDEX [ix]") == "" || captured.find("STATISTICS [st]") == "" {
		t.Error("the creates were not sent")
	}
}

// Every other …Ref handle whose child reads key on a catalog id is refused
// the same way, before any query: the parents here have no pool behind them,
// so a read that got past its guard fails on the missing connection instead,
// and is reported.
func TestRefChildReadsAreRefused(t *testing.T) {
	s := &Server{}
	d := &Database{server: s, Name: "AppDB"}
	cases := []struct {
		name string
		call func(context.Context) error
	}{
		{"Alert.Notifications", func(c context.Context) error { return errOnly(s.AlertRef("a").Notifications(c)) }},
		{"Operator.NotifyingAlerts", func(c context.Context) error { return errOnly(s.OperatorRef("o").NotifyingAlerts(c)) }},
		{"Operator.NotifyingJobs", func(c context.Context) error { return errOnly(s.OperatorRef("o").NotifyingJobs(c)) }},
		{"Assembly.Files", func(c context.Context) error { return errOnly(d.AssemblyRef("a").Files(c)) }},
		{"Assembly.FileContent", func(c context.Context) error { return errOnly(d.AssemblyRef("a").FileContent(c, 1)) }},
		{"Assembly.Modules", func(c context.Context) error { return errOnly(d.AssemblyRef("a").Modules(c)) }},
		{"AvailabilityGroup.Replicas", func(c context.Context) error { return errOnly(s.AvailabilityGroupRef("ag").Replicas(c)) }},
		{"AvailabilityGroup.Databases", func(c context.Context) error { return errOnly(s.AvailabilityGroupRef("ag").Databases(c)) }},
		{"AvailabilityGroup.Listeners", func(c context.Context) error { return errOnly(s.AvailabilityGroupRef("ag").Listeners(c)) }},
		{"FullTextCatalog.Indexes", func(c context.Context) error { return errOnly(d.FullTextCatalogRef("c").Indexes(c)) }},
		{"FullTextStoplist.Stopwords", func(c context.Context) error { return errOnly(d.FullTextStoplistRef("l").Stopwords(c)) }},
		{"SearchPropertyList.Properties", func(c context.Context) error { return errOnly(d.SearchPropertyListRef("p").Properties(c)) }},
		{"FullTextIndex.Populations", func(c context.Context) error {
			return errOnly(d.TableRef("dbo", "t").FullTextIndexRef().Populations(c))
		}},
		{"ResourcePool.WorkloadGroups", func(c context.Context) error { return errOnly(s.ResourcePoolRef("p").WorkloadGroups(c)) }},
		{"BrokerQueue.MessageCount", func(c context.Context) error { return errOnly(d.BrokerQueueRef("dbo", "q").MessageCount(c)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(t.Context()); !errors.Is(err, ErrHandleNotLoaded) {
				t.Fatalf("err = %v, want ErrHandleNotLoaded", err)
			}
		})
	}
}

// A JobRef's per-job reads are keyed by job_id, which the handle lacks. They
// look it up by name and query with what came back, rather than asking for
// job_id NULL and answering "no steps"; a job that is not there is
// ErrNotFound.
func TestJobRefReadsLookUpTheJobID(t *testing.T) {
	reads := []struct {
		name string
		call func(context.Context, *Job) error
	}{
		{"Steps", func(c context.Context, j *Job) error { return errOnly(j.Steps(c)) }},
		{"History", func(c context.Context, j *Job) error { return errOnly(j.History(c, 0)) }},
		{"Schedules", func(c context.Context, j *Job) error { return errOnly(j.Schedules(c)) }},
		{"ReorderSteps", func(c context.Context, j *Job) error {
			return j.ReorderSteps(c, func(n int) []int { return nil })
		}},
	}
	for _, tc := range reads {
		t.Run(tc.name, func(t *testing.T) {
			d := qsRecDB(t, 17, []string{"job_id"}, [][]driver.Value{{"ID-1"}})
			_ = tc.call(t.Context(), d.server.JobRef("j")) // the child row does not scan; only the SQL matters
			calls := qsRec.recorded()
			if len(calls) < 2 {
				t.Fatalf("got %d statements, want the id lookup and the read", len(calls))
			}
			if !strings.Contains(calls[0].sql, "msdb.dbo.sysjobs WHERE name = @p1") || !slices.Equal(calls[0].args, []any{"j"}) {
				t.Errorf("first statement is not the job_id lookup by name: %s %v", calls[0].sql, calls[0].args)
			}
			if !slices.Equal(calls[1].args, []any{"ID-1"}) {
				t.Errorf("read args = %v, want the looked-up job_id", calls[1].args)
			}

			d = qsRecDB(t, 17, nil, nil)
			if err := tc.call(t.Context(), d.server.JobRef("gone")); !errors.Is(err, ErrNotFound) {
				t.Errorf("missing job: err = %v, want ErrNotFound", err)
			}
		})
	}
}

// A LoginRef's reads keyed by SID or login type look both up by name, rather
// than binding a nil SID (no mappings) or skipping on an empty type (no
// mapped object); a login that is not there is ErrNotFound.
func TestLoginRefReadsLookUpTheSID(t *testing.T) {
	sid := []byte{1, 2, 3}
	reads := []struct {
		name string
		call func(context.Context, *Login) error
	}{
		{"UserMappings", func(c context.Context, l *Login) error { return errOnly(l.UserMappings(c)) }},
		{"ResolveMapping", func(c context.Context, l *Login) error { return l.ResolveMapping(c) }},
	}
	for _, tc := range reads {
		t.Run(tc.name, func(t *testing.T) {
			d := qsRecDB(t, 17, []string{"sid", "type_desc"}, [][]driver.Value{{sid, "CERTIFICATE_MAPPED_LOGIN"}})
			_ = tc.call(t.Context(), d.server.LoginRef("l"))
			calls := qsRec.recorded()
			if len(calls) < 2 {
				t.Fatalf("got %d statements, want the SID lookup and the read", len(calls))
			}
			if !strings.Contains(calls[0].sql, "FROM sys.server_principals") || !slices.Equal(calls[0].args, []any{"l"}) {
				t.Errorf("first statement is not the SID lookup by name: %s %v", calls[0].sql, calls[0].args)
			}
			if tc.name == "ResolveMapping" && !bytes.Equal(calls[1].args[0].([]byte), sid) {
				t.Errorf("read args = %v, want the looked-up SID", calls[1].args)
			}

			d = qsRecDB(t, 17, nil, nil)
			if err := tc.call(t.Context(), d.server.LoginRef("gone")); !errors.Is(err, ErrNotFound) {
				t.Errorf("missing login: err = %v, want ErrNotFound", err)
			}
		})
	}
}
