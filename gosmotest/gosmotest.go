// Package gosmotest runs real gosmo code over canned rows, so a caller's
// tests exercise gosmo's own queries, scanning, Ref semantics and errors
// without a server.
//
// A Fake is a database/sql driver that answers each query from the first
// rule whose text it contains. Wrap it with Server and call gosmo as usual:
//
//	f := gosmotest.New(t)
//	f.On("FROM sys.tables", gosmotest.Rows(
//		[]any{int64(1), "dbo", "Customer", …},
//	))
//	srv := f.Server()
//	tables, err := srv.DatabaseRef("app").Tables(ctx, gosmo.TableQuery{})
//
// The rows are positional: each is one row of the result set exactly as the
// server would send it, in the query's SELECT order. That is the point — a
// gosmo change in a query's shape or its scanning fails the caller's test,
// where a hand-written fake of the gosmo method would not notice. Read the
// query's text in gosmo's source for the column order (a test that fails
// prints the query it could not answer, and Queries lists every one).
//
// What a rule sees is the query gosmo meant, not its transport: a database
// read sent as an RPC of [db].sys.sp_executesql, or as a "USE [db]; IF
// @@ERROR <> 0 RETURN; …" batch inside a transaction, arrives as its inner
// text with Query.Database set to db and Query.Args numbered from the inner
// query's first parameter.
//
// A query no rule answers fails the test (t.Errorf) and returns an error to
// gosmo, so an unexpected read is reported as one, naming the query. An
// Exec no rule answers succeeds with no rows affected: writes are recorded,
// not refused. The server metadata gosmo reads on connect (Server.Info) has
// default rules, answered from Fake.Info, which a test may change before
// its first call to Server.
//
// A query sent with a sqlexp.ReturnMessage — gosmo's BACKUP and RESTORE,
// which read the server's progress messages — is a write as an Exec is
// (Query.Messages; no rule needed), and gets its rule's answer as that
// message stream, as go-mssqldb delivers it: the result sets in turn, or a
// rule's Err as the stream's error messages (an mssql.Error's All one
// message each) rather than the query's error.
//
// A bulk load (gosmo.Database.BulkInsert) arrives as one Exec per row, its
// Text the driver's "INSERTBULK {…}" statement and its Args the row; a
// rule that fails one fails the load there. The load's closing flush
// reports the rows it was sent. The switch into the target database such a
// call makes on its own session is answered by the Fake itself, so the
// rows' Query.Database is the table's.
//
// A statement's Query also carries the context it was issued under (Ctx) and
// the transaction it ran in (Tx; Fake.TxLog lists their BEGIN and ends).
// OnExec scopes a rule to writes. Result.DropConn, with Err, leaves the
// connection invalid as a dropped link does, and Hold keeps a rule's answer
// back until a channel is closed or the statement's context is cancelled, so
// a test can act while a statement is in flight.
//
// A Fake is safe for concurrent use.
package gosmotest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/golang-sql/sqlexp"
	mssql "github.com/microsoft/go-mssqldb"
	"github.com/radix29/gosmo"
)

// Info is the server metadata a Fake reports on connect: what
// gosmo.Server.Info returns, and what gosmo's version gates read.
type Info struct {
	Name           string // SERVERPROPERTY('ServerName')
	Edition        string
	ProductVersion string // "16.0.4135.4": the major version gates gosmo's reads
	ProductLevel   string
	Collation      string
	EngineEdition  int    // gosmo.EngineEdition: 3 Enterprise, 5 Azure SQL Database, 8 Managed Instance
	Version        string // @@VERSION; its "on Linux" or "on Windows" sets Info.Platform
	Login          string // SUSER_NAME()
	DataPath       string
	LogPath        string
	BackupPath     string
	HADREnabled    bool // SERVERPROPERTY('IsHadrEnabled'): Always On availability groups enabled
}

// DefaultInfo is a SQL Server 2022 Developer Edition on Linux, logged in as sa.
var DefaultInfo = Info{
	Name:           "gosmotest",
	Edition:        "Developer Edition (64-bit)",
	ProductVersion: "16.0.4135.4",
	ProductLevel:   "RTM",
	Collation:      "SQL_Latin1_General_CP1_CI_AS",
	EngineEdition:  3,
	Version:        "Microsoft SQL Server 2022 (RTM-CU14) - 16.0.4135.4 (X64) Developer Edition (64-bit) on Linux (Ubuntu 22.04.4 LTS) <X64>",
	Login:          "sa",
	DataPath:       "/var/opt/mssql/data/",
	LogPath:        "/var/opt/mssql/data/",
	BackupPath:     "/var/opt/mssql/data/",
}

// Query is one statement gosmo sent, as a rule sees it and Queries records it.
type Query struct {
	// Database is the database the statement ran in when gosmo sent it there
	// itself — through [db].sys.sp_executesql or a USE batch — unbracketed.
	// "" for a statement run in the session's own database.
	Database string
	Text     string // the statement, the transport stripped
	Args     []any  // its parameters' values, @p1 first: an int as int64, as database/sql converts it
	Exec     bool   // sent through Exec, not Query
	// Messages is a query sent with a sqlexp.ReturnMessage, reading the
	// server's messages as it runs: a BACKUP or RESTORE with progress.
	Messages bool
	// Ctx is the context the statement was issued under: a test asserting
	// that a caller cancelled a read, or a Responder holding the query until
	// it is (Hold), looks here. Nil for a statement database/sql issued
	// without one (a prepared Stmt's Exec/Query).
	Ctx context.Context
	// Tx numbers the transaction the statement ran in, 1 for the Fake's
	// first; 0 for none. TxLog lists the transactions' BEGIN and ends.
	Tx int
}

// Bound is the statement with each @pN replaced by the literal its argument
// would be written as — what it amounts to, as gosmo's statement observer
// and a captured script show it. gosmo binds the string arguments of every
// msdb Agent procedure rather than splicing them, so Text reads
// "@job_name = @p1" and only this form says which job. A placeholder with no
// argument, or an argument of a type with no literal here, is left as is.
func (q Query) Bound() string {
	if len(q.Args) == 0 {
		return q.Text
	}
	return placeholder.ReplaceAllStringFunc(q.Text, func(m string) string {
		n, err := strconv.Atoi(m[2:])
		if err != nil || n < 1 || n > len(q.Args) {
			return m
		}
		switch v := q.Args[n-1].(type) {
		case nil:
			return "NULL"
		case string:
			return "N'" + strings.ReplaceAll(v, "'", "''") + "'"
		case bool:
			if v {
				return "1"
			}
			return "0"
		case int64, int32, int, float64:
			return fmt.Sprint(v)
		}
		return m
	})
}

var placeholder = regexp.MustCompile(`@p([0-9]+)`)

// Arg is the value of parameter @p(i+1), nil when there are fewer.
func (q Query) Arg(i int) any {
	if i < 0 || i >= len(q.Args) {
		return nil
	}
	return q.Args[i]
}

// String is the query as a failure message names it.
func (q Query) String() string {
	var sb strings.Builder
	if q.Database != "" {
		fmt.Fprintf(&sb, "in [%s]: ", q.Database)
	}
	sb.WriteString(strings.TrimSpace(q.Text))
	if len(q.Args) > 0 {
		fmt.Fprintf(&sb, " %v", q.Args)
	}
	return sb.String()
}

// Set is one result set: its rows, and optionally its column names. A Set
// with no Columns has as many as its first row has values (named c1, c2, …),
// which is all database/sql needs to scan it.
type Set struct {
	Columns []string
	Rows    [][]any
}

// Result is a rule's answer: the result sets, in order, or Err.
type Result struct {
	Sets []Set
	Err  error
	// DropConn, with Err, leaves the connection the statement ran on invalid
	// (driver.Validator), as go-mssqldb reports a session killed or a link
	// dropped mid-statement: database/sql discards it, and a caller holding
	// the session finds it lost.
	DropConn bool
}

// A Responder answers a query a rule matched.
type Responder func(Query) Result

// Rows answers with one result set of rows.
func Rows(rows ...[]any) Responder {
	return func(Query) Result { return Result{Sets: []Set{{Rows: rows}}} }
}

// Sets answers with several result sets, as a batch of several SELECTs does.
func Sets(sets ...Set) Responder {
	return func(Query) Result { return Result{Sets: sets} }
}

// Fail answers with err: the query's failure, as the driver would return it.
// An mssql.Error makes it the server's.
func Fail(err error) Responder {
	return func(Query) Result { return Result{Err: err} }
}

// Hold wraps r so that it answers only once release is closed, or fails with
// the statement's context error when that is cancelled first — the way a real
// driver answers a cancelled statement. It is how a test acts, or cancels,
// while a read or a write is genuinely in flight: without it a Fake answers
// instantly and every statement has finished before the test regains control.
// Scope the rule (OnExec, a Database or Arg check in OnFunc) so only the
// statement under test is held.
func Hold(release <-chan struct{}, r Responder) Responder {
	return func(q Query) Result {
		var done <-chan struct{}
		if q.Ctx != nil {
			done = q.Ctx.Done()
		}
		select {
		case <-release:
			return r(q)
		case <-done:
			return Result{Err: q.Ctx.Err()}
		}
	}
}

type rule struct {
	match   func(Query) bool
	respond Responder
}

// Fake is a SQL Server the test scripts. Create one with New.
type Fake struct {
	// Info is what the connect reads report; change it before Server.
	Info Info

	t testing.TB

	mu       sync.Mutex
	rules    []rule
	queries  []Query
	server   *gosmo.Server
	defaults []rule
	txLog    []string
	txSeq    int
}

// New is a Fake for t, with DefaultInfo and no rules. Its Server is closed
// when t ends.
func New(t testing.TB) *Fake {
	f := &Fake{Info: DefaultInfo, t: t}
	f.defaults = []rule{
		{contains("SERVERPROPERTY('ServerName')"), func(Query) Result {
			i := f.Info
			return Result{Sets: []Set{{Rows: [][]any{{i.Name, i.Edition, i.ProductVersion, i.ProductLevel, i.Collation,
				int64(0), boolInt(i.HADREnabled), int64(0), int64(i.EngineEdition), i.Version, i.DataPath, i.LogPath, i.BackupPath, i.Login}}}}}
		}},
		{contains("FROM   sys.dm_os_sys_info"), Rows([]any{int64(16384), int64(8)})},
		// What gosmo's InTransaction asks as it begins, and CurrentDatabase:
		// the session's database, master for one never moved. A test's own
		// rule for it answers first.
		{func(q Query) bool { return strings.TrimSpace(q.Text) == "SELECT DB_NAME()" }, func(q Query) Result {
			db := q.Database
			if db == "" {
				db = "master"
			}
			return Result{Sets: []Set{{Rows: [][]any{{db}}}}}
		}},
	}
	return f
}

// boolInt is b as the INT a CAST(SERVERPROPERTY(…) AS INT) flag reads.
func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func contains(s string) func(Query) bool {
	return func(q Query) bool { return strings.Contains(q.Text, s) }
}

// On answers every query whose text contains match with r. Rules are tried in
// the order they were added; the first that matches answers.
func (f *Fake) On(match string, r Responder) *Fake {
	return f.OnFunc(contains(match), r)
}

// OnExec is On for a statement sent through Exec only: a write. A rule that
// fails or holds a write this way is not consulted for a read that happens to
// contain the same text.
func (f *Fake) OnExec(match string, r Responder) *Fake {
	return f.OnFunc(func(q Query) bool { return q.Exec && strings.Contains(q.Text, match) }, r)
}

// OnRegexp answers every query whose text matches the regular expression
// expr with r.
func (f *Fake) OnRegexp(expr string, r Responder) *Fake {
	re := regexp.MustCompile(expr)
	return f.OnFunc(func(q Query) bool { return re.MatchString(q.Text) }, r)
}

// OnFunc answers every query match accepts with r — for a rule that depends
// on the database or the arguments as well as the text.
func (f *Fake) OnFunc(match func(Query) bool, r Responder) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append(f.rules, rule{match, r})
	return f
}

// Server is a gosmo.Server over f, made on the first call (which runs the
// connect reads) and the same one after. It is closed when the test ends.
func (f *Fake) Server() *gosmo.Server {
	f.t.Helper()
	f.mu.Lock()
	if s := f.server; s != nil {
		f.mu.Unlock()
		return s
	}
	f.mu.Unlock()
	s, err := f.Connect(context.Background())
	if err != nil {
		f.t.Fatalf("gosmotest: connect: %v", err)
	}
	f.mu.Lock()
	f.server = s
	f.mu.Unlock()
	return s
}

// Connect is a new gosmo.Server over f, as gosmo.Connect makes one per call:
// for code that dials and closes its own connection, where Server's one
// shared Server would be closed under the next caller. Each runs the connect
// reads, shares f's rules and record, and is closed when the test ends if
// the caller has not closed it.
func (f *Fake) Connect(ctx context.Context) (*gosmo.Server, error) {
	s, err := gosmo.NewServer(ctx, sql.OpenDB(connector{f}))
	if err != nil {
		return nil, err
	}
	f.t.Cleanup(func() { s.Close() })
	return s, nil
}

// Queries is every statement f has been sent, in order, the connect reads
// included.
func (f *Fake) Queries() []Query {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Query(nil), f.queries...)
}

// TxLog is every BEGIN, COMMIT and ROLLBACK f's connections saw, in order.
func (f *Fake) TxLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.txLog...)
}

// ErrNoRule is the error a query no rule answers returns to gosmo.
var ErrNoRule = errors.New("gosmotest: no rule answers the query")

// answer records q and finds its rule. ok is false when none matches.
func (f *Fake) answer(q Query) (Result, bool) {
	f.mu.Lock()
	f.queries = append(f.queries, q)
	rules := append(append([]rule(nil), f.rules...), f.defaults...)
	f.mu.Unlock()
	for _, r := range rules {
		if r.match(q) {
			return r.respond(q), true
		}
	}
	return Result{}, false
}

// -- the driver -------------------------------------------------------------

type connector struct{ f *Fake }

func (c connector) Connect(context.Context) (driver.Conn, error) { return &conn{f: c.f}, nil }
func (c connector) Driver() driver.Driver                        { return drv(c) }

type drv struct{ f *Fake }

func (d drv) Open(string) (driver.Conn, error) { return &conn{f: d.f}, nil }

// conn tracks the database a USE moved it to, as a session does; a
// procRead call runs in its database for that call only.
type conn struct {
	f  *Fake
	db string
	// msgq is the ReturnMessage the next query's answer goes to, if any.
	msgq *sqlexp.ReturnMessage
	tx   int  // the open transaction's number, 0 for none
	lost bool // a Result.DropConn failure ended this connection
}

// IsValid is driver.Validator: false once a DropConn failure ended c.
func (c *conn) IsValid() bool { return !c.lost }

// ResetSession is called by database/sql when c is handed back out of the
// pool. It clears the database a USE pinned, so one caller's database-scoped
// read cannot answer the next caller's server-scoped one — gosmo issues its
// own USE ahead of every database-scoped read, so carrying it over could only
// make a test pass on a query that never asked for it.
func (c *conn) ResetSession(context.Context) error {
	c.db = ""
	return nil
}

var (
	_ driver.QueryerContext    = (*conn)(nil)
	_ driver.ExecerContext     = (*conn)(nil)
	_ driver.NamedValueChecker = (*conn)(nil)
	_ driver.ConnBeginTx       = (*conn)(nil)
	_ driver.Validator         = (*conn)(nil)
	_ driver.SessionResetter   = (*conn)(nil)
)

func (c *conn) Prepare(q string) (driver.Stmt, error) {
	if strings.HasPrefix(q, bulkPrefix) {
		return &bulkStmt{c: c, q: q}, nil
	}
	return stmt{c, q}, nil
}
func (c *conn) Close() error              { return nil }
func (c *conn) Begin() (driver.Tx, error) { return c.begin(), nil }
func (c *conn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return c.begin(), nil
}

func (c *conn) begin() driver.Tx {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.txSeq++
	c.tx = c.f.txSeq
	c.f.txLog = append(c.f.txLog, "BEGIN")
	return tx{c}
}

// CheckNamedValue converts an argument as database/sql's default converter
// does, so a rule sees an int as int64 and an mssql.VarChar as its string,
// and passes through as sent what that converter refuses (a sql.Out) rather
// than failing the query. A sqlexp.ReturnMessage is no argument: it is
// where the query's answer goes (messages).
func (c *conn) CheckNamedValue(nv *driver.NamedValue) error {
	if m, ok := nv.Value.(*sqlexp.ReturnMessage); ok {
		// As go-mssqldb takes it: kept for the query, not an argument.
		sqlexp.ReturnMessageInit(m)
		c.msgq = m
		return driver.ErrRemoveArgument
	}
	if v, err := driver.DefaultParameterConverter.ConvertValue(nv.Value); err == nil {
		nv.Value = v
	}
	return nil
}

func (c *conn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if rest, found := strings.CutPrefix(q, sessionSwitch); found {
		return c.switchSession(rest)
	}
	msgq := c.msgq
	c.msgq = nil
	query, ok := c.unwrap(q, args)
	if !ok {
		return &rows{}, nil // a bare USE
	}
	query.Messages = msgq != nil
	query.Ctx, query.Tx = ctx, c.tx
	res, found := c.f.answer(query)
	if msgq != nil {
		return messages(msgq, res)
	}
	if !found {
		c.f.t.Errorf("gosmotest: no rule answers the query %s", query)
		res.Err = fmt.Errorf("%w: %s", ErrNoRule, query)
	}
	if res.Err != nil {
		c.lost = res.DropConn
		return nil, res.Err
	}
	return newRows(res.Sets)
}

func (c *conn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	query, ok := c.unwrap(q, args)
	if !ok {
		return driver.ResultNoRows, nil
	}
	query.Exec = true
	query.Ctx, query.Tx = ctx, c.tx
	res, _ := c.f.answer(query)
	if res.Err != nil {
		c.lost = res.DropConn
		return nil, res.Err
	}
	return driver.RowsAffected(0), nil
}

const useGuard = "; IF @@ERROR <> 0 RETURN; "

// unwrap strips gosmo's transport from q: a procRead RPC, a USE batch, or
// a bare USE (which moves the session and has nothing to answer: ok false).
func (c *conn) unwrap(q string, args []driver.NamedValue) (Query, bool) {
	vals := make([]any, len(args))
	for i, a := range args {
		vals[i] = a.Value
	}
	if db, found := strings.CutSuffix(q, ".sys.sp_executesql"); found && len(vals) > 0 {
		inner, _ := vals[0].(string)
		var innerArgs []any
		if len(vals) > 2 {
			innerArgs = vals[2:]
		}
		return Query{Database: unbracket(db), Text: inner, Args: innerArgs}, true
	}
	if use, rest, found := strings.Cut(q, useGuard); found && strings.HasPrefix(use, "USE ") {
		c.db = unbracket(strings.TrimPrefix(use, "USE "))
		return Query{Database: c.db, Text: rest, Args: vals}, true
	}
	if strings.HasPrefix(q, "USE ") && !strings.ContainsAny(q, ";\n") {
		c.db = unbracket(strings.TrimPrefix(q, "USE "))
		return Query{}, false
	}
	return Query{Database: c.db, Text: q, Args: vals}, true
}

// sessionSwitch opens the batch a call holding its own session sends to
// move it into its database (gosmo's withSessionIn), reading back the
// database it left: "…; USE [db]; IF @@ERROR <> 0 RETURN; SELECT @home".
const sessionSwitch = "DECLARE @home sysname = DB_NAME(); "

// switchSession moves c into the database rest's USE names and answers the
// one it was in, master for a session that never moved.
func (c *conn) switchSession(rest string) (driver.Rows, error) {
	use, _, found := strings.Cut(rest, useGuard)
	if !found || !strings.HasPrefix(use, "USE ") {
		return nil, fmt.Errorf("gosmotest: unrecognised session switch %q", rest)
	}
	home := c.db
	if home == "" {
		home = "master"
	}
	c.db = unbracket(strings.TrimPrefix(use, "USE "))
	return newRows([]Set{{Rows: [][]any{{home}}}})
}

func unbracket(name string) string { return gosmo.UnquoteName(name) }

type stmt struct {
	c *conn
	q string
}

func (s stmt) Close() error  { return nil }
func (s stmt) NumInput() int { return -1 }
func (s stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.c.ExecContext(context.Background(), s.q, named(args))
}
func (s stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.c.QueryContext(context.Background(), s.q, named(args))
}

// messages delivers res through m as go-mssqldb does: each result set
// announced (MsgNext) and closed (MsgNextResultSet), or the error as a
// MsgError and the close — the last close is the one after which
// rows.NextResultSet reports no more. The queue is bounded, so a reader
// that drains it as it goes is fed from a goroutine.
func messages(m *sqlexp.ReturnMessage, res Result) (driver.Rows, error) {
	sets := res.Sets
	if res.Err != nil {
		sets = nil
	}
	r, err := newRows(sets)
	if err != nil {
		return nil, err
	}
	go func() {
		ctx := context.Background()
		for _, err := range streamErrors(res.Err) {
			_ = sqlexp.ReturnMessageEnqueue(ctx, m, sqlexp.MsgError{Error: err})
		}
		for range sets {
			_ = sqlexp.ReturnMessageEnqueue(ctx, m, sqlexp.MsgNext{})
			_ = sqlexp.ReturnMessageEnqueue(ctx, m, sqlexp.MsgNextResultSet{})
		}
		if len(sets) == 0 {
			_ = sqlexp.ReturnMessageEnqueue(ctx, m, sqlexp.MsgNextResultSet{})
		}
	}()
	return r, nil
}

// streamErrors is err as the message stream carries it: a server error
// with several messages (mssql.Error.All) as one message each, as the
// server sends them; any other error as itself.
func streamErrors(err error) []error {
	if err == nil {
		return nil
	}
	se, ok := errors.AsType[mssql.Error](err)
	if !ok || len(se.All) == 0 {
		return []error{err}
	}
	out := make([]error, len(se.All))
	for i, e := range se.All {
		out[i] = e
	}
	return out
}

// bulkPrefix opens the statement go-mssqldb's CopyIn makes for a bulk load.
const bulkPrefix = "INSERTBULK "

// bulkStmt is a prepared bulk load: each Exec with values is a row,
// recorded and answered as any Exec; the Exec with none is the flush, which
// reports the rows sent.
type bulkStmt struct {
	c    *conn
	q    string
	rows int64
}

func (s *bulkStmt) Close() error  { return nil }
func (s *bulkStmt) NumInput() int { return -1 }
func (s *bulkStmt) Exec(args []driver.Value) (driver.Result, error) {
	if len(args) == 0 {
		return driver.RowsAffected(s.rows), nil
	}
	if _, err := s.c.ExecContext(context.Background(), s.q, named(args)); err != nil {
		return nil, err
	}
	s.rows++
	return driver.RowsAffected(1), nil
}
func (s *bulkStmt) Query([]driver.Value) (driver.Rows, error) {
	return nil, errors.New("gosmotest: a bulk load is not queried")
}

func named(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, a := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	return out
}

type tx struct{ c *conn }

func (t tx) end(how string) error {
	t.c.f.mu.Lock()
	defer t.c.f.mu.Unlock()
	t.c.tx = 0
	t.c.f.txLog = append(t.c.f.txLog, how)
	return nil
}

func (t tx) Commit() error   { return t.end("COMMIT") }
func (t tx) Rollback() error { return t.end("ROLLBACK") }

// rows serves Result.Sets, one after another.
type rows struct {
	sets []Set
	cols []string
	vals [][]driver.Value
	at   int
}

var _ driver.RowsNextResultSet = (*rows)(nil)

func newRows(sets []Set) (*rows, error) {
	r := &rows{sets: sets}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

// load makes sets[0] current, its values converted to the driver's types.
func (r *rows) load() error {
	r.cols, r.vals, r.at = nil, nil, 0
	if len(r.sets) == 0 {
		return nil
	}
	s := r.sets[0]
	r.cols = s.Columns
	if r.cols == nil && len(s.Rows) > 0 {
		r.cols = make([]string, len(s.Rows[0]))
		for i := range r.cols {
			r.cols[i] = fmt.Sprintf("c%d", i+1)
		}
	}
	for n, row := range s.Rows {
		if len(row) != len(r.cols) {
			return fmt.Errorf("gosmotest: row %d has %d values, the set %d columns", n+1, len(row), len(r.cols))
		}
		vals := make([]driver.Value, len(row))
		for i, v := range row {
			cv, err := driver.DefaultParameterConverter.ConvertValue(v)
			if err != nil {
				return fmt.Errorf("gosmotest: row %d, column %d: %w", n+1, i+1, err)
			}
			vals[i] = cv
		}
		r.vals = append(r.vals, vals)
	}
	return nil
}

func (r *rows) Columns() []string { return r.cols }
func (r *rows) Close() error      { return nil }

func (r *rows) Next(dest []driver.Value) error {
	if r.at >= len(r.vals) {
		return io.EOF
	}
	copy(dest, r.vals[r.at])
	r.at++
	return nil
}

func (r *rows) HasNextResultSet() bool { return len(r.sets) > 1 }

func (r *rows) NextResultSet() error {
	if len(r.sets) <= 1 {
		return io.EOF
	}
	r.sets = r.sets[1:]
	return r.load()
}
