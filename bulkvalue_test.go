package gosmo

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustConverter(t *testing.T, c BulkColumn) func(any) (any, error) {
	t.Helper()
	conv, err := c.Converter()
	if err != nil {
		t.Fatalf("Converter(%+v): %v", c, err)
	}
	return conv
}

// sameBulkValue compares converted values: times by instant and offset,
// bytes by content, everything else by ==.
func sameBulkValue(got, want any) bool {
	switch w := want.(type) {
	case time.Time:
		g, ok := got.(time.Time)
		_, gOff := g.Zone()
		_, wOff := w.Zone()
		return ok && g.Equal(w) && gOff == wOff
	case []byte:
		g, ok := got.([]byte)
		return ok && bytes.Equal(g, w)
	}
	return got == want
}

type myInt int16 // a named integer, which database/sql would send as int64

// TestBulkConverterValues pins what each value is sent as, by hand. Each
// result is then converted again and must come back unchanged: a caller may
// check its values and hand them to BulkInsert, which checks them again.
func TestBulkConverterValues(t *testing.T) {
	utc := func(y, mo, d, h, mi, s, ns int) time.Time {
		return time.Date(y, time.Month(mo), d, h, mi, s, ns, time.UTC)
	}
	guid := []byte{0xFF, 0x19, 0x96, 0x6F, 0x86, 0x8B, 0x11, 0xD0, 0xB4, 0x2D, 0x00, 0xC0, 0x4F, 0xD4, 0x30, 0xC8}
	cases := []struct {
		col      BulkColumn
		in, want any
	}{
		{BulkColumn{Type: "bit"}, true, true},
		{BulkColumn{Type: "bit"}, "TRUE", true},
		{BulkColumn{Type: "bit"}, "0", false},
		{BulkColumn{Type: "tinyint"}, 255, int64(255)},
		{BulkColumn{Type: "tinyint"}, uint8(7), int64(7)},
		{BulkColumn{Type: "smallint"}, myInt(-32768), int64(-32768)},
		{BulkColumn{Type: "int"}, "+7", int64(7)},
		{BulkColumn{Type: "int"}, 42.0, int64(42)},
		{BulkColumn{Type: "bigint"}, int64(math.MinInt64), int64(math.MinInt64)},
		{BulkColumn{Type: "bigint"}, "-9223372036854775808", int64(math.MinInt64)},
		{BulkColumn{Type: "decimal", Precision: 5, Scale: 2}, "123.45", "123.45"},
		{BulkColumn{Type: "decimal", Precision: 5, Scale: 2}, "007", "7"},
		{BulkColumn{Type: "numeric", Precision: 5, Scale: 2}, ".5", "0.5"},
		{BulkColumn{Type: "decimal", Precision: 5, Scale: 2}, 999, "999"},
		{BulkColumn{Type: "decimal", Precision: 5, Scale: 2}, 1.005, "1.00"}, // 1.005 is 1.00499… in binary
		// An exact binary tie rounds away from zero, as CAST does
		// (strconv rounds it to even); a value just off the tie rounds to
		// nearest. Live: TestLiveBulkInsertRoundsFloatsAsInsert.
		{BulkColumn{Type: "decimal", Precision: 5, Scale: 2}, -0.125, "-0.13"},
		{BulkColumn{Type: "decimal", Precision: 5, Scale: 2}, 0.125, "0.13"},
		{BulkColumn{Type: "decimal", Precision: 5, Scale: 2}, 0.375, "0.38"},
		{BulkColumn{Type: "decimal", Precision: 5, Scale: 0}, 2.5, "3"},
		{BulkColumn{Type: "decimal", Precision: 5, Scale: 0}, -2.5, "-3"},
		{BulkColumn{Type: "decimal", Precision: 5, Scale: 0}, 0.5, "1"},
		{BulkColumn{Type: "decimal", Precision: 1, Scale: 0}, 0.5, "1"},
		{BulkColumn{Type: "decimal", Precision: 5, Scale: 0}, -0.5, "-1"},
		{BulkColumn{Type: "decimal", Precision: 3, Scale: 0}, 99.5, "100"},
		{BulkColumn{Type: "decimal", Precision: 5, Scale: 1}, 9.75, "9.8"},
		{BulkColumn{Type: "decimal", Precision: 5, Scale: 2}, 9.995, "9.99"}, // 9.99499… in binary
		{BulkColumn{Type: "decimal", Precision: 5, Scale: 2}, 1.015, "1.01"}, // 1.01499…
		{BulkColumn{Type: "decimal", Precision: 5, Scale: 2}, 1.025, "1.02"}, // 1.02499…
		{BulkColumn{Type: "decimal", Precision: 5, Scale: 2}, 1.045, "1.04"}, // 1.04499…
		{BulkColumn{Type: "money"}, 0.03125, "0.0313"},
		{BulkColumn{Type: "money"}, -0.03125, "-0.0313"},
		{BulkColumn{Type: "money"}, 2.00005, "2.0000"}, // 2.0000499…
		{BulkColumn{Type: "decimal", Precision: 38, Scale: 0}, strings.Repeat("9", 38), strings.Repeat("9", 38)},
		{BulkColumn{Type: "money"}, "12.50", "12.50"},
		{BulkColumn{Type: "money"}, "-922337203685477.5808", "-922337203685477.5808"},
		{BulkColumn{Type: "money"}, 3, "3"},
		{BulkColumn{Type: "money"}, 0.1 + 0.2, "0.3000"},
		{BulkColumn{Type: "smallmoney"}, "-214748.3648", "-214748.3648"},
		{BulkColumn{Type: "float"}, 1.5e300, 1.5e300},
		{BulkColumn{Type: "float"}, "0.1", 0.1},
		{BulkColumn{Type: "float"}, 3, 3.0},
		{BulkColumn{Type: "real"}, float32(0.1), float32(0.1)},
		{BulkColumn{Type: "real"}, "0.1", float32(0.1)},
		{BulkColumn{Type: "real"}, "3.4028235E+38", float32(math.MaxFloat32)},
		{BulkColumn{Type: "date"}, utc(1, 1, 1, 0, 0, 0, 0), utc(1, 1, 1, 0, 0, 0, 0)},
		{BulkColumn{Type: "datetime"}, utc(1753, 1, 1, 0, 0, 0, 0), utc(1753, 1, 1, 0, 0, 0, 0)},
		{BulkColumn{Type: "datetime"}, utc(9999, 12, 31, 23, 59, 59, 998499999), utc(9999, 12, 31, 23, 59, 59, 998499999)},
		{BulkColumn{Type: "smalldatetime"}, utc(2024, 2, 29, 13, 45, 29, 998000000), utc(2024, 2, 29, 13, 45, 0, 0)},
		{BulkColumn{Type: "smalldatetime"}, utc(2024, 2, 29, 13, 45, 29, 999000000), utc(2024, 2, 29, 13, 46, 0, 0)},
		{BulkColumn{Type: "smalldatetime"}, utc(2024, 2, 29, 23, 59, 30, 0), utc(2024, 3, 1, 0, 0, 0, 0)},
		{BulkColumn{Type: "datetime2"}, "2024-01-01 00:00:00", "2024-01-01 00:00:00"}, // text is the driver's
		{BulkColumn{Type: "uniqueidentifier"}, guid, guid},
		{BulkColumn{Type: "uniqueidentifier"}, "6F9619FF-8B86-D011-B42D-00C04FD430C8", guid},
		{BulkColumn{Type: "uniqueidentifier"}, "{6f9619ff-8b86-d011-b42d-00c04fd430c8}", guid},
		{BulkColumn{Type: "varbinary", Length: 4}, []byte{1, 0xFF}, []byte{1, 0xFF}},
		{BulkColumn{Type: "binary", Length: 4}, []byte{1, 0xFF}, []byte{1, 0xFF, 0, 0}},
		{BulkColumn{Type: "image", Length: 16}, bytes.Repeat([]byte{0xAB}, 17), bytes.Repeat([]byte{0xAB}, 17)},
		{BulkColumn{Type: "nvarchar", Length: 2}, "😀", "😀"},
		{BulkColumn{Type: "nvarchar", Length: 3}, 123, "123"},
		{BulkColumn{Type: "nchar", Length: 4}, "ab", "ab  "},
		{BulkColumn{Type: "ntext", Length: 8}, strings.Repeat("n", 9), strings.Repeat("n", 9)},
		{BulkColumn{Type: "xml"}, "<a>é</a>", "<a>é</a>"},
		{BulkColumn{Type: "varchar", Length: 6, CodePage: 1252, DatabaseCodePage: 1252}, "café €", []byte{'c', 'a', 'f', 0xE9, ' ', 0x80}},
		{BulkColumn{Type: "varchar", Length: 6, CodePage: 1252, DatabaseCodePage: 1252}, -42, []byte("-42")},
		{BulkColumn{Type: "varchar", Length: 3, CodePage: 1252, DatabaseCodePage: 1252}, []byte{0xE9}, []byte{0xE9}},
		{BulkColumn{Type: "char", Length: 3, CodePage: 1252, DatabaseCodePage: 1252}, []byte{0xE9}, []byte{0xE9, ' ', ' '}},
		{BulkColumn{Type: "char", Length: 4, CodePage: 1252, DatabaseCodePage: 1252}, "é", []byte{0xE9, ' ', ' ', ' '}},
		{BulkColumn{Type: "text", Length: 16, CodePage: 1252, DatabaseCodePage: 1252}, strings.Repeat("t", 17), bytes.Repeat([]byte{'t'}, 17)},
		{BulkColumn{Type: "varchar", Length: 4, CodePage: 932, DatabaseCodePage: 932}, "日本", []byte{0x93, 0xFA, 0x96, 0x7B}},
		{BulkColumn{Type: "text", CodePage: 1251, DatabaseCodePage: 1251}, "Жж", []byte{0xC6, 0xE6}},
		// A UTF-8 column takes a string, its length in UTF-8 bytes, whatever
		// the database's code page.
		{BulkColumn{Type: "varchar", Length: 5, CodePage: 65001, DatabaseCodePage: 65001}, "café", "café"},
		{BulkColumn{Type: "char", Length: 7, CodePage: 65001, DatabaseCodePage: 1252}, "日", "日    "},
		{BulkColumn{Type: "varchar", Length: 5, CodePage: 65001, DatabaseCodePage: 1252}, []byte("café"), "café"},
		// The bytes travel in the database's code page and the server
		// converts them; the length is the column's.
		{BulkColumn{Type: "varchar", Length: 4, CodePage: 932, DatabaseCodePage: 65001}, "日本", []byte("日本")},
		{BulkColumn{Type: "char", Length: 4, CodePage: 1252, DatabaseCodePage: 65001}, "é", []byte("é   ")},
		{BulkColumn{Type: "varchar", Length: 2, CodePage: 1251, DatabaseCodePage: 932}, "Ж", []byte{0x84, 0x47}},
		// No check for these: passed on as they are.
		{BulkColumn{Type: "sql_variant"}, "x", "x"},
		{BulkColumn{Type: ""}, int64(5), int64(5)},
		{BulkColumn{Type: "int"}, true, true},
	}
	for _, c := range cases {
		conv := mustConverter(t, c.col)
		got, err := conv(c.in)
		if err != nil {
			t.Errorf("%s %#v: %v", c.col.Type, c.in, err)
			continue
		}
		if !sameBulkValue(got, c.want) {
			t.Errorf("%s %#v = %#v (%T), want %#v (%T)", c.col.Type, c.in, got, got, c.want, c.want)
			continue
		}
		again, err := conv(got)
		if err != nil || !sameBulkValue(again, got) {
			t.Errorf("%s %#v: converting %#v again = %#v, %v; want it unchanged", c.col.Type, c.in, got, again, err)
		}
	}
}

// TestBulkConverterRefuses: each value go-mssqldb would load wrong, or SQL
// Server would round or reject unnamed, is a *BulkValueError that says why
// and wraps ErrInvalidRequest.
func TestBulkConverterRefuses(t *testing.T) {
	utc := func(y, mo, d, h, mi, s, ns int) time.Time {
		return time.Date(y, time.Month(mo), d, h, mi, s, ns, time.UTC)
	}
	cases := []struct {
		col  BulkColumn
		in   any
		want string
	}{
		{BulkColumn{Type: "bit"}, "2", "not a bit"},
		{BulkColumn{Type: "tinyint"}, 300, "300 is out of range (0 to 255)"},
		{BulkColumn{Type: "tinyint"}, "300", "300 is out of range (0 to 255)"},
		{BulkColumn{Type: "tinyint"}, -1, "out of range"},
		{BulkColumn{Type: "int"}, int64(math.MaxInt32) + 1, "out of range"},
		{BulkColumn{Type: "bigint"}, "9223372036854775808", "out of range"},
		{BulkColumn{Type: "bigint"}, float64(math.MaxInt64), "out of range"},
		{BulkColumn{Type: "int"}, 1.5, "1.5 is not a whole number"},
		{BulkColumn{Type: "int"}, math.Inf(1), "not a whole number"},
		{BulkColumn{Type: "int"}, "1.0", "not an integer"},
		{BulkColumn{Type: "int"}, " 1", "not an integer"},
		{BulkColumn{Type: "decimal", Precision: 5, Scale: 2}, "1234.5", "does not fit decimal(5,2)"},
		{BulkColumn{Type: "decimal", Precision: 5, Scale: 2}, 1000, "does not fit decimal(5,2)"},
		{BulkColumn{Type: "decimal", Precision: 5, Scale: 2}, 999.995, "does not fit decimal(5,2)"},
		{BulkColumn{Type: "decimal", Precision: 3, Scale: 0}, 999.5, "does not fit decimal(3,0)"},
		{BulkColumn{Type: "decimal", Precision: 5, Scale: 2}, "1.234", "more than 2 decimal places"},
		{BulkColumn{Type: "decimal", Precision: 5, Scale: 2}, "1e3", "not a decimal"},
		{BulkColumn{Type: "decimal", Precision: 5, Scale: 2}, math.NaN(), "not a finite number"},
		{BulkColumn{Type: "money"}, "922337203685477.5808", "out of range (-922337203685477.5808 to 922337203685477.5807)"},
		{BulkColumn{Type: "money"}, "1.23456", "more than 4 decimal places"},
		{BulkColumn{Type: "money"}, "$1", "not a money amount"},
		{BulkColumn{Type: "smallmoney"}, 214749, "out of range (-214748.3648 to 214748.3647)"},
		{BulkColumn{Type: "float"}, math.NaN(), "out of range for float"},
		{BulkColumn{Type: "float"}, "1E+309", "out of range for float"},
		{BulkColumn{Type: "float"}, "x", "not a number"},
		{BulkColumn{Type: "real"}, 3.5e38, "out of range for real"},
		{BulkColumn{Type: "real"}, "3.5E+38", "out of range for real"},
		{BulkColumn{Type: "date"}, utc(0, 12, 31, 0, 0, 0, 0), "out of range for date"},
		{BulkColumn{Type: "datetime2"}, utc(10000, 1, 1, 0, 0, 0, 0), "out of range for datetime2"},
		{BulkColumn{Type: "datetime"}, utc(1752, 12, 31, 0, 0, 0, 0), "out of range for datetime"},
		{BulkColumn{Type: "datetime"}, utc(9999, 12, 31, 23, 59, 59, 998500000), "out of range for datetime"},
		{BulkColumn{Type: "smalldatetime"}, utc(1899, 12, 31, 23, 59, 0, 0), "out of range for smalldatetime"},
		{BulkColumn{Type: "smalldatetime"}, utc(2079, 6, 6, 23, 59, 30, 0), "out of range for smalldatetime"},
		{BulkColumn{Type: "uniqueidentifier"}, []byte{1, 2}, "2 bytes is not a uniqueidentifier"},
		{BulkColumn{Type: "uniqueidentifier"}, "nope", "not a uniqueidentifier"},
		{BulkColumn{Type: "uniqueidentifier"}, "6F9619FF8B86D011B42D00C04FD430C8", "not a uniqueidentifier"},
		{BulkColumn{Type: "varbinary", Length: 2}, []byte{1, 2, 3}, "3 bytes is longer than the column's 2"},
		{BulkColumn{Type: "nvarchar", Length: 1}, "😀", "2 characters is longer than the column's 1"},
		{BulkColumn{Type: "nvarchar", Length: -1}, "a\xffb", "not valid UTF-8"},
		{BulkColumn{Type: "varchar", Length: 3, CodePage: 1252, DatabaseCodePage: 1252}, "café", "4 bytes is longer than the column's 3"},
		{BulkColumn{Type: "varchar", Length: 3, CodePage: 1252, DatabaseCodePage: 1252}, []byte("abcd"), "4 bytes is longer than the column's 3"},
		{BulkColumn{Type: "varchar", Length: 10, CodePage: 1252, DatabaseCodePage: 1252}, "a日b", `'日' (U+65E5) is not in the column's code page 1252`},
		{BulkColumn{Type: "varchar", Length: -1, CodePage: 1252, DatabaseCodePage: 1252}, "\xff", "not valid UTF-8"},
		{BulkColumn{Type: "varchar", Length: 10, CodePage: 932, DatabaseCodePage: 1252}, "日", "not in the database's code page 1252"},
		{BulkColumn{Type: "varchar", Length: 10, CodePage: 1252, DatabaseCodePage: 65001}, "日", "not in the column's code page 1252"},
		{BulkColumn{Type: "varchar", Length: 4, CodePage: 932, DatabaseCodePage: 65001}, "日本語", "6 bytes is longer than the column's 4"},
		{BulkColumn{Type: "varchar", Length: 4, CodePage: 65001, DatabaseCodePage: 1252}, "café", "5 bytes is longer than the column's 4"},
		{BulkColumn{Type: "varchar", Length: -1, CodePage: 65001, DatabaseCodePage: 65001}, []byte{0xFF}, "not valid UTF-8"},
	}
	for _, c := range cases {
		got, err := mustConverter(t, c.col)(c.in)
		ve, ok := errors.AsType[*BulkValueError](err)
		if !ok || !strings.Contains(err.Error(), c.want) || !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s %#v = %#v, %v; want a *BulkValueError with %q wrapping ErrInvalidRequest", c.col.Type, c.in, got, err, c.want)
			continue
		}
		if ve.Row != 0 || ve.Column != "" || err.Error() != ve.Reason {
			t.Errorf("%s %#v: %+v, want the reason alone outside a load with no column name", c.col.Type, c.in, ve)
		}
	}
}

func TestBulkValueErrorNamesRowAndColumn(t *testing.T) {
	_, err := mustConverter(t, BulkColumn{Name: "q]ty", Type: "tinyint"})(300)
	if got, want := err.Error(), "column [q]]ty]: 300 is out of range (0 to 255)"; got != want {
		t.Errorf("error %q, want %q", got, want)
	}
	if got, want := (&BulkValueError{Row: 3, Column: "a", Reason: "why"}).Error(), "row 3, column [a]: why"; got != want {
		t.Errorf("error %q, want %q", got, want)
	}
}

func TestBulkConverterNoEncoder(t *testing.T) {
	for _, c := range []BulkColumn{
		{Name: "v", Type: "varchar", Length: 10, CodePage: 1252},
		{Type: "char", Length: 10, CodePage: 1361, DatabaseCodePage: 1252},
		{Type: "text", CodePage: 1252, DatabaseCodePage: 1361},
		{Type: "varchar", CodePage: 0, DatabaseCodePage: 0},
	} {
		if _, err := c.Converter(); !errors.Is(err, ErrUnsupported) || !strings.HasPrefix(err.Error(), "gosmo: ") {
			t.Errorf("%+v: err %v, want a gosmo: ErrUnsupported error", c, err)
		}
	}
	// A UTF-8 column needs no database code page.
	if _, err := (BulkColumn{Type: "varchar", Length: 1, CodePage: 65001}).Converter(); err != nil {
		t.Errorf("UTF-8 column with no database code page: %v", err)
	}
}

// -- a driver that serves load's column read and records what it sends ------

type bulkFakeLog struct {
	mu       sync.Mutex
	queryArg any              // bulkColumnsQuery's @p1
	cols     [][]driver.Value // its rows
	prepared string
	execs    [][]driver.Value
}

var bulkFake bulkFakeLog

type bulkFakeDriver struct{}

func (bulkFakeDriver) Open(string) (driver.Conn, error) { return bulkFakeConn{}, nil }

func init() { sql.Register("gosmobulkfake", bulkFakeDriver{}) }

type bulkFakeConn struct{}

func (bulkFakeConn) Close() error              { return nil }
func (bulkFakeConn) Begin() (driver.Tx, error) { return nil, driver.ErrSkip }
func (bulkFakeConn) Prepare(q string) (driver.Stmt, error) {
	bulkFake.mu.Lock()
	defer bulkFake.mu.Unlock()
	bulkFake.prepared = q
	return bulkFakeStmt{}, nil
}

func (bulkFakeConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if q != bulkColumnsQuery || len(args) != 1 {
		return nil, errors.New("unexpected query: " + q)
	}
	bulkFake.mu.Lock()
	defer bulkFake.mu.Unlock()
	bulkFake.queryArg = args[0].Value
	return &bulkFakeRows{rows: bulkFake.cols}, nil
}

type bulkFakeStmt struct{}

func (bulkFakeStmt) Close() error  { return nil }
func (bulkFakeStmt) NumInput() int { return -1 }
func (bulkFakeStmt) Exec(args []driver.Value) (driver.Result, error) {
	bulkFake.mu.Lock()
	defer bulkFake.mu.Unlock()
	if len(args) == 0 {
		return driver.RowsAffected(len(bulkFake.execs)), nil
	}
	bulkFake.execs = append(bulkFake.execs, args)
	return driver.RowsAffected(0), nil
}
func (bulkFakeStmt) Query([]driver.Value) (driver.Rows, error) { return nil, driver.ErrSkip }

type bulkFakeRows struct{ rows [][]driver.Value }

func (*bulkFakeRows) Columns() []string {
	return []string{"name", "type", "length", "precision", "scale", "cp", "dbcp"}
}
func (*bulkFakeRows) Close() error { return nil }
func (r *bulkFakeRows) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		return io.EOF
	}
	copy(dest, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}

// TestBulkLoadConvertsEachValue drives load against the fake: the column read
// names the target, values are sent converted (a column the catalog does not
// list unchecked), and a bad value stops the load naming its 1-based row and
// its column.
func TestBulkLoadConvertsEachValue(t *testing.T) {
	col := func(name, typ string, length, cp int) []driver.Value {
		return []driver.Value{name, typ, int64(length), int64(0), int64(0), int64(cp), int64(1252)}
	}
	bulkFake = bulkFakeLog{cols: [][]driver.Value{
		col("Qty", "tinyint", 1, 0),
		col("name", "varchar", 4, 1252),
	}}
	db, err := sql.Open("gosmobulkfake", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := t.Context()
	run := func(rows [][]any) (int64, error) {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		bc := BulkCopy{Schema: "dbo", Table: "t", Columns: []string{"Qty", "name", "extra"}}
		return bc.load(ctx, conn, "[dbo].[t]", SliceRows(rows))
	}

	n, err := run([][]any{{7, "café", 300}, {nil, nil, nil}})
	if err != nil || n != 2 {
		t.Fatalf("load = %d, %v; want 2 rows", n, err)
	}
	if bulkFake.queryArg != "[dbo].[t]" {
		t.Errorf("column read for %v, want [dbo].[t]", bulkFake.queryArg)
	}
	want := [][]driver.Value{{int64(7), []byte{'c', 'a', 'f', 0xE9}, int64(300)}, {nil, nil, nil}}
	if len(bulkFake.execs) != 2 {
		t.Fatalf("sent %v, want %v", bulkFake.execs, want)
	}
	for r := range want {
		for c := range want[r] {
			if !sameBulkValue(bulkFake.execs[r][c], want[r][c]) {
				t.Errorf("row %d column %d sent %#v, want %#v", r+1, c, bulkFake.execs[r][c], want[r][c])
			}
		}
	}

	bulkFake.execs = nil
	n, err = run([][]any{{1, "a", nil}, {300, "b", nil}})
	ve, ok := errors.AsType[*BulkValueError](err)
	if !ok || ve.Row != 2 || ve.Column != "Qty" || !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("load = %d, %v; want a *BulkValueError for row 2, column Qty", n, err)
	}
	if want := "gosmo: bulk insert into [dbo].[t]: row 2, column [Qty]: 300 is out of range (0 to 255)"; err.Error() != want {
		t.Errorf("error %q, want %q", err, want)
	}
	if n != 1 || len(bulkFake.execs) != 1 {
		t.Errorf("load sent %d rows (count %d) before the bad one, want 1", len(bulkFake.execs), n)
	}
}
