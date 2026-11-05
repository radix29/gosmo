//go:build livedb

package gosmo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
)

// skipOnHostMemory skips t when err is Msg 701, "There is insufficient system
// memory in resource pool '…' to run this query": the test host's free
// memory, not gosmo. Late in a full livedb run on 13 and 14 it fails the
// memory-optimized replays, each of which passes run alone straight after.
// Any other error returns, for the caller to fail on.
//
// Call it only where memory-optimized objects are created or replayed
// (liveExecMemoryOptimized, liveRunMemoryOptimizedScript): a 701 anywhere
// else is not known to be the host, and fails.
func skipOnHostMemory(t testing.TB, err error) {
	t.Helper()
	msErr, ok := errors.AsType[mssql.Error](err)
	if !ok || msErr.Number != 701 {
		return
	}
	pool := "(unnamed)"
	if _, rest, ok := strings.Cut(msErr.Message, "resource pool '"); ok {
		if p, _, ok := strings.Cut(rest, "'"); ok {
			pool = p
		}
	}
	t.Skipf("Msg 701 in resource pool %q: the host is short of memory, not a gosmo failure (%s)", pool, msErr.Message)
}

// liveExecMemoryOptimized is liveExecIn for statements that create
// memory-optimized objects: a Msg 701 skips the test (skipOnHostMemory).
func liveExecMemoryOptimized(t *testing.T, d *Database, ctx context.Context, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := d.exec(ctx, s); err != nil {
			skipOnHostMemory(t, err)
			t.Fatalf("exec %.60q: %v", s, err)
		}
	}
}

// liveRunMemoryOptimizedScript is liveRunScript for a script that creates
// memory-optimized objects: a Msg 701 skips the test (skipOnHostMemory).
func liveRunMemoryOptimizedScript(t *testing.T, d *Database, ctx context.Context, script string) {
	t.Helper()
	for _, batch := range splitGoBatches(script) {
		if _, err := d.exec(ctx, batch); err != nil {
			skipOnHostMemory(t, err)
			t.Fatalf("the generated script does not run:\n%s\n\nfailing batch:\n%s\n\n%v", script, batch, err)
		}
	}
}

// skipTB records a Skipf instead of skipping, so a test can look at it.
type skipTB struct {
	testing.TB
	reason string
}

func (s *skipTB) Helper()                       {}
func (s *skipTB) Skipf(format string, a ...any) { s.reason = fmt.Sprintf(format, a...) }

// skipOnHostMemory skips on Msg 701 alone, wrapped or not, and names the
// pool. It needs no server.
func TestSkipOnHostMemoryOnlyOn701(t *testing.T) {
	const msg701 = "There is insufficient system memory in resource pool 'default' to run this query."
	for _, c := range []struct {
		name string
		err  error
		skip bool
	}{
		{"701", mssql.Error{Number: 701, Message: msg701}, true},
		{"701 wrapped", fmt.Errorf("replay: %w", mssql.Error{Number: 701, Message: msg701}), true},
		{"another number", mssql.Error{Number: 41805, Message: msg701}, false},
		{"not a server error", errors.New(msg701), false},
		{"nil", nil, false},
	} {
		tb := &skipTB{TB: t}
		skipOnHostMemory(tb, c.err)
		if c.skip != (tb.reason != "") {
			t.Errorf("%s: skipped = %t (%q), want %t", c.name, tb.reason != "", tb.reason, c.skip)
		}
		if c.skip && !strings.Contains(tb.reason, `resource pool "default"`) {
			t.Errorf("%s: the reason does not name the pool: %q", c.name, tb.reason)
		}
	}
}
