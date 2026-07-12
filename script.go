package gosmo

import "context"

type scriptCtxKey struct{}

// ScriptCollector accumulates the SQL statements a write method would have
// executed, instead of running them. See WithScript.
type ScriptCollector struct {
	Statements []string
}

// WithScript returns a derived context carrying a *ScriptCollector. Every
// gosmo write method invoked with the returned context appends its
// statement to the collector and returns as if it had succeeded, without
// touching the server — callers use this to preview or hand off the exact
// SQL a set of pending edits would run (e.g. an "Script Changes" action
// that opens the statements in a query editor instead of executing them).
//
// Read methods are unaffected: only the exec chokepoints
// (Server.execContext, Database.exec) consult the collector.
func WithScript(ctx context.Context) (context.Context, *ScriptCollector) {
	c := &ScriptCollector{}
	return context.WithValue(ctx, scriptCtxKey{}, c), c
}

func scriptFrom(ctx context.Context) (*ScriptCollector, bool) {
	c, ok := ctx.Value(scriptCtxKey{}).(*ScriptCollector)
	return c, ok
}

// execContext is the chokepoint every server-scoped write method (and, via
// Database.exec, every database-scoped one) funnels through. stmt must
// already be a complete, self-contained statement — every write method in
// this package builds one via QuoteName/QuoteLiteral/escapeSingle before
// reaching here, since none of these are parameterizable DDL/EXEC calls.
func (s *Server) execContext(ctx context.Context, stmt string) error {
	if c, ok := scriptFrom(ctx); ok {
		c.Statements = append(c.Statements, stmt)
		return nil
	}
	_, err := s.db.ExecContext(ctx, stmt)
	return err
}
