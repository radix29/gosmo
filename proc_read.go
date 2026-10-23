package gosmo

import (
	"fmt"
	"strings"
	"unicode/utf16"
)

// procReads routes Database.query and queryRow, outside InTransaction, through
// procRead instead of useBatch. It is the A1 spike (OPEN-THREADS.md § Idle
// pooled connections camp in the last database read): off, so every caller
// keeps useBatch, and flipped only by the tests that run both paths.
var procReads = false

// procRead is q as an RPC call of d's own sp_executesql: the query text is the
// bare name [d].sys.sp_executesql, which the driver sends as a procedure call
// (its isProc), with q, the parameter declarations and args as that
// procedure's positional parameters. q then compiles and runs in d, and the
// session never leaves its login database — so a pooled connection goes back
// to the pool holding no lock on d, which useBatch's USE leaves behind.
//
// An RPC names its procedure when it runs, not when a batch compiles, so the
// Msg 924 trap that makes perDatabaseExec name the procedure through @proc
// does not arise: there is no batch.
//
// ok is false when an argument has a type procParamType does not declare (or
// is a sql.NamedArg); the caller then keeps useBatch for that read.
func (d *Database) procRead(q string, args []any) (query string, rpcArgs []any, ok bool) {
	query = quoteIdent(d.Name) + ".sys.sp_executesql"
	if len(args) == 0 {
		return query, []any{q}, true
	}
	decls := make([]string, len(args))
	for i, a := range args {
		t, ok := procParamType(a)
		if !ok {
			return "", nil, false
		}
		decls[i] = fmt.Sprintf("@p%d %s", i+1, t)
	}
	return query, append([]any{q, strings.Join(decls, ", ")}, args...), true
}

// procParamType is the declaration procRead gives an argument inside d's
// sp_executesql, matching the type the driver sends it as (its makeParam and
// makeDecl) so no value converts on the way in. A string is nvarchar(4000)
// up to that length, rather than the driver's exact length, so one query's
// plan is shared across values.
func procParamType(a any) (string, bool) {
	switch v := a.(type) {
	case nil:
		return "nvarchar(1)", true
	case string:
		if len(utf16.Encode([]rune(v))) > 4000 {
			return "nvarchar(max)", true
		}
		return "nvarchar(4000)", true
	case int, int64:
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
	}
	return "", false
}
