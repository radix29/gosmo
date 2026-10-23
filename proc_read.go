package gosmo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf16"

	mssql "github.com/microsoft/go-mssqldb"
)

// procRead is q as an RPC call of d's own sp_executesql: the query text is the
// bare name [d].sys.sp_executesql, which the driver sends as a procedure call
// (its isProc), with q, the parameter declarations and args as that
// procedure's positional parameters. q then compiles and runs in d, and the
// session never leaves its login database — so a pooled connection goes back
// to the pool holding no lock on d. A USE (useBatch) left it in d until the
// next checkout reset it, and someone else's RESTORE, SET OFFLINE or SET
// SINGLE_USER failed "Exclusive access could not be obtained" meanwhile
// (live_proc_read_test.go). Every pooled Database.query and queryRow goes
// this way; only a read inside InTransaction, whose session is meant to stay
// where it is, keeps useBatch.
//
// An RPC names its procedure when it runs, not when a batch compiles, so the
// Msg 924 trap that makes perDatabaseExec name the procedure through @proc
// does not arise: there is no batch. Parameters bind in d's collation, not the
// login database's, so the Msg 468 that recheckUse re-runs a useBatch read for
// does not arise either.
//
// An argument type procParamType does not declare is a programming error, and
// the read fails with it (ErrUnsupported): there is no fallback to useBatch.
func (d *Database) procRead(q string, args []any) (query string, rpcArgs []any, err error) {
	query = quoteIdent(d.Name) + ".sys.sp_executesql"
	if len(args) == 0 {
		return query, []any{q}, nil
	}
	decls := make([]string, len(args))
	for i, a := range args {
		t, ok := procParamType(a)
		if !ok {
			return "", nil, unsupportedf("gosmo: read in %s: argument %d is a %T, which procRead cannot declare", d.Name, i+1, a)
		}
		decls[i] = fmt.Sprintf("@p%d %s", i+1, t)
	}
	return query, append([]any{q, strings.Join(decls, ", ")}, args...), nil
}

// procParamType is the declaration procRead gives an argument inside d's
// sp_executesql, matching the type the driver sends it as (its makeParam and
// makeDecl, after convertInputParameter) so no value converts on the way in.
// A string is nvarchar(4000) up to that length, rather than the driver's
// exact length, so one query's plan is shared across values. The unsigned
// ints reach the driver as int64 (database/sql's default converter), and a
// time.Time as datetimeoffset(7) on TDS 7.3 and later, which every supported
// version speaks.
func procParamType(a any) (string, bool) {
	switch v := a.(type) {
	case nil:
		return "nvarchar(1)", true
	case string:
		if len(utf16.Encode([]rune(v))) > 4000 {
			return "nvarchar(max)", true
		}
		return "nvarchar(4000)", true
	case int, int64, uint, uint16, uint32, uint64:
		return "bigint", true
	case int32:
		return "int", true
	case int16:
		return "smallint", true
	case int8, uint8:
		return "tinyint", true
	case bool:
		return "bit", true
	case float64:
		return "float", true
	case float32:
		return "real", true
	case []byte:
		return "varbinary(max)", true
	case time.Time:
		return "datetimeoffset(7)", true
	}
	return "", false
}

// recheckEntry tells a procRead failure whose number says a database could
// not be entered (cannotEnterErrors) apart: d refusing the call, or q itself
// reaching another database that refuses — the two errors are identical,
// down to the line number. One call of d's sp_executesql with nothing in it
// decides: if d refuses that too, useErr is its error, wrapped as the USE
// failure every database-scoped read has always reported, and is the read's;
// if not, useErr is nil and err is q's own. Like recheckUse it costs a round
// trip on a failing read only, and like the read it leaves the session where
// it was.
func (d *Database) recheckEntry(ctx context.Context, conn sqlConn, err error) (useErr error) {
	me, ok := errors.AsType[mssql.Error](err)
	if !ok || !slices.Contains(cannotEnterErrors, me.Number) || ctx.Err() != nil {
		return nil
	}
	if _, perr := conn.ExecContext(ctx, quoteIdent(d.Name)+".sys.sp_executesql", "SELECT NULL"); perr != nil {
		return fmt.Errorf("gosmo: USE %s: %w", d.Name, perr)
	}
	return nil
}
