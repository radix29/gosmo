// The reflective half of the live version sweep (live_versionsweep_test.go),
// kept out of the livedb build so its write guard is tested offline by
// TestReflectSweepRefusesWrites.
package gosmo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// sweepSkip names the methods the sweep must not call because they write.
// The list is a heuristic kept here, in the test, rather than in a shared
// helper: a method classified wrongly has to be visible at the point it is
// skipped. Keys are "<type>.<method>".
//
// Every entry is checked against a real method below, so a renamed or deleted
// method fails the test instead of silently un-skipping — or silently
// skipping nothing.
var sweepSkip = map[string]string{
	"Server.CycleErrorLog":          "rolls the error log over",
	"Database.ClearQueryStore":      "discards the query store's contents",
	"Database.FlushQueryStore":      "forces a query store write",
	"Database.SetOffline":           "takes the database offline",
	"Database.SetOnline":            "brings the database online",
	"Database.RecoverFromRestoring": "runs recovery on a RESTORING database",
	"Table.Truncate":                "deletes every row",
	"Statistic.Drop":                "drops the statistic",
	"Index.Disable":                 "disables the index",
	"Index.Drop":                    "drops the index",
	"Index.Enable":                  "rebuilds the index",
	"Index.Reorganize":              "reorganizes the index",
	"Login.Disable":                 "disables the login",
	"Login.Drop":                    "drops the login",
	"Login.Enable":                  "enables the login",
	"Job.Disable":                   "disables the job",
	"Job.Drop":                      "drops the job",
	"Job.Enable":                    "enables the job",
	"Job.Stop":                      "stops a running job",
	"ResourcePool.Drop":             "drops the pool (refused for the built-in ones, which are all a pristine instance has)",
	"Server.StartDatabaseMail":      "starts the Database Mail queue",
	"Server.StopDatabaseMail":       "stops the Database Mail queue",
}

// sweep records what ran and what failed, so the result is one report rather
// than a stream of failures in call order.
type sweep struct {
	t       *testing.T
	ctx     context.Context
	calls   int
	failed  []string
	refused []string        // gosmo's own version refusals; expected, not failures
	called  map[string]bool // every label reached, for the coverage check below
}

// call runs one read and records its outcome. A panic is recorded like an
// error: the sweep's value is in reaching the end of the list.
func (sw *sweep) call(label string, fn func() error) {
	sw.calls++
	if sw.called == nil {
		sw.called = map[string]bool{}
	}
	sw.called[label] = true
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
		}()
		return fn()
	}()
	switch {
	case err == nil:
	case errors.Is(err, ErrUnsupportedVersion):
		// gosmo declining a feature the instance is too old for is the gate
		// working, not a defect — the whole point of running the sweep on the
		// oldest instance available. Counted separately so an old major can
		// still be 0 failures.
		sw.refused = append(sw.refused, fmt.Sprintf("%s: %v", label, err))
	case strings.HasPrefix(label, "Database.LatestResourceStats") && errors.Is(err, ErrNotFound):
		// sys.dm_db_resource_stats gains its first row a few seconds after a
		// database is created (3 s on a Managed Instance, 2026-09-10), and the
		// sweep's database is seconds old, so an empty view is a race rather
		// than a defect. The query ran, which is what is being swept.
	case strings.HasPrefix(label, "Table.FullTextIndex") && errors.Is(err, ErrNotFound):
		// Only dbo.sweep_parent has a full-text index (and only where the
		// component is installed); every other table answering "none" is
		// the read working.
	case strings.HasPrefix(label, "Database.EncryptionKey") && errors.Is(err, ErrNotFound):
		// The sweep's database is not TDE-encrypted, so "no key" is the
		// read working.
	default:
		sw.failed = append(sw.failed, fmt.Sprintf("%s: %v", label, err))
	}
}

// reflectSweep calls every exported Xxx(ctx) (…, error) method on recv that
// sweepSkip does not name — and every Xxx(ctx, q) whose q is one of
// sweepZeroQueries, with q's zero value, which asks for everything. Methods
// taking other arguments beyond the context are driven by hand in
// TestLiveVersionSweep; this half is what keeps a newly added listing covered
// without anyone remembering to add it here.
//
// Each call runs under its own WithScript context. Reads still reach the
// server, so the sweep measures what it always did; a write is only
// collected, and fails the sweep as one that wrote. That makes sweepSkip a
// detector rather than the only guard: a new ctx-only write nobody listed —
// a Database.Checkpoint, a Table.Rebuild — would otherwise run the first
// time the sweep met a real server, which may be the AG cluster.
func (sw *sweep) reflectSweep(typeName, instance string, recv any) {
	ctxT := reflect.TypeOf((*context.Context)(nil)).Elem()
	errT := reflect.TypeOf((*error)(nil)).Elem()
	rv := reflect.ValueOf(recv)
	rt := rv.Type()

	for i := 0; i < rt.NumMethod(); i++ {
		m := rt.Method(i)
		ft := m.Type
		if ft.NumIn() < 2 || ft.In(1) != ctxT {
			continue
		}
		wctx, capture := WithScript(sw.ctx)
		args := []reflect.Value{rv, reflect.ValueOf(wctx)}
		if ft.NumIn() == 3 && slices.Contains(sweepZeroQueries, ft.In(2)) {
			args = append(args, reflect.Zero(ft.In(2)))
		} else if ft.NumIn() != 2 {
			continue
		}
		if ft.NumOut() == 0 || ft.Out(ft.NumOut()-1) != errT {
			continue
		}
		if _, skip := sweepSkip[typeName+"."+m.Name]; skip {
			continue
		}
		label := fmt.Sprintf("%s.%s [%s]", typeName, m.Name, instance)
		sw.call(label, func() error {
			out := m.Func.Call(args)
			if capture.Len() > 0 {
				// Ahead of the method's own error: a write that also
				// failed, or was refused as too new, is still a write.
				return fmt.Errorf("wrote under the sweep — add it to sweepSkip: %.200q",
					strings.Join(strings.Fields(capture.String()), " "))
			}
			if e := out[len(out)-1].Interface(); e != nil {
				return e.(error)
			}
			return nil
		})
	}
}

// sweepZeroQueries are the listing arguments whose zero value means "all",
// so a listing taking one is as reachable by reflection as a ctx-only one.
var sweepZeroQueries = []reflect.Type{
	reflect.TypeOf(ObjectFilter{}),
	reflect.TypeOf(TableQuery{}),
}

// checkSkipList fails on an entry naming a method that no longer exists, so
// the exclusions cannot rot into a list of names that skip nothing.
func checkSkipList(t *testing.T) {
	t.Helper()
	types := map[string]reflect.Type{
		"Server":    reflect.TypeOf(&Server{}),
		"Database":  reflect.TypeOf(&Database{}),
		"Table":     reflect.TypeOf(&Table{}),
		"Index":     reflect.TypeOf(&Index{}),
		"Statistic": reflect.TypeOf(&Statistic{}),
		"Login":     reflect.TypeOf(&Login{}),
		"Job":       reflect.TypeOf(&Job{}),

		"ResourcePool": reflect.TypeOf(&ResourcePool{}),
	}
	for key := range sweepSkip {
		typeName, method, ok := strings.Cut(key, ".")
		rt := types[typeName]
		if !ok || rt == nil {
			t.Errorf("sweepSkip key %q names no swept type", key)
			continue
		}
		if _, found := rt.MethodByName(method); !found {
			t.Errorf("sweepSkip names %s, which %s does not have — stale exclusion", key, rt)
		}
	}
}

// TestSweepSkipNamesRealMethods runs the sweep's stale-entry check offline,
// so a rename that orphans a sweepSkip entry fails go test ./... rather than
// waiting for the next live run.
func TestSweepSkipNamesRealMethods(t *testing.T) {
	checkSkipList(t)
}

// sweepWriter has the shape of a write added later without a sweepSkip
// entry: an exported method taking only ctx, returning an error — beside a
// read of the same shape, which must still be swept as one.
type sweepWriter struct{ s *Server }

func (w sweepWriter) Checkpoint(ctx context.Context) error {
	_, err := w.s.exec(ctx, "CHECKPOINT")
	return err
}

func (w sweepWriter) Answer(ctx context.Context) (int, error) { return 42, nil }

// TestReflectSweepRefusesWrites pins the guard reflectSweep runs under: a
// ctx-only write sweepSkip does not name is collected, not executed, and
// fails the sweep, while a read beside it still runs and passes.
func TestReflectSweepRefusesWrites(t *testing.T) {
	s := captureServer(t, 13)

	sw := &sweep{t: t, ctx: t.Context()}
	sw.reflectSweep("sweepWriter", "fake", sweepWriter{s})
	if sw.calls != 2 || !sw.called["sweepWriter.Answer [fake]"] {
		t.Fatalf("swept %d methods (%v), want Answer and Checkpoint", sw.calls, sw.called)
	}
	if len(sw.failed) != 1 || !strings.Contains(sw.failed[0], "sweepWriter.Checkpoint") ||
		!strings.Contains(sw.failed[0], "wrote under the sweep") || !strings.Contains(sw.failed[0], "CHECKPOINT") {
		t.Errorf("failures = %q, want one naming the Checkpoint write", sw.failed)
	}
	if q := captured.find("CHECKPOINT"); q != "" {
		t.Errorf("the write reached the driver: %q", q)
	}

	// A real write with its exclusion removed, through a real swept type.
	reason := sweepSkip["Table.Truncate"]
	delete(sweepSkip, "Table.Truncate")
	t.Cleanup(func() { sweepSkip["Table.Truncate"] = reason })
	captured.reset()

	sw = &sweep{t: t, ctx: t.Context()}
	sw.reflectSweep("Table", "dbo.t", s.DatabaseRef("d").TableRef("dbo", "t"))
	var wrote []string
	for _, f := range sw.failed {
		if strings.Contains(f, "wrote under the sweep") {
			wrote = append(wrote, f)
		}
	}
	if len(wrote) != 1 || !strings.HasPrefix(wrote[0], "Table.Truncate ") {
		t.Errorf("writes reported = %q, want only Table.Truncate", wrote)
	}
	if q := captured.find("TRUNCATE"); q != "" {
		t.Errorf("Truncate reached the driver: %q", q)
	}
}
