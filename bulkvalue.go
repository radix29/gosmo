package gosmo

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	mssql "github.com/microsoft/go-mssqldb"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

// ============================================================
// Bulk-load value checking and encoding
// ============================================================

// BulkColumn is a bulk-load destination column as BulkInsert checks the
// values sent to it. BulkInsert reads it from the catalog itself; a caller
// fills one in only to check values without loading (BulkColumn.Converter),
// as a dry run does.
type BulkColumn struct {
	// Name is the column's name, used only in a BulkValueError. Empty leaves
	// the column out of the message.
	Name string

	// Type is the column's system type as sys.types names it ("int",
	// "nvarchar", "datetime2"); an alias type's base type, so sysname is
	// "nvarchar". A type with no check here (sql_variant, the CLR types,
	// timestamp, one gosmo does not know, or "") passes values through
	// unchanged, for the driver to send or refuse.
	Type string

	// Length is the declared n of char, varchar, nchar, nvarchar, binary and
	// varbinary — bytes, except nchar/nvarchar's UTF-16 code units — and -1
	// for (max). sys.columns.max_length is bytes, so it is halved for
	// nchar/nvarchar. Other types ignore it: text, ntext, image and xml have
	// no declared length (sys.columns' 16 is a pointer's size).
	Length int

	// Precision and Scale are decimal/numeric's.
	Precision, Scale int

	// CodePage is the code page of a char, varchar or text column's
	// collation (Server.CollationCodePage): 65001 for a _UTF8 collation,
	// 1252 for Latin1_General.
	CodePage int

	// DatabaseCodePage is the code page of the database's default collation,
	// which is what a bulk load's char bytes are read in: INSERT BULK
	// declares the columns with no COLLATE, so the server takes the bytes in
	// the database default and converts them to the column's collation, a
	// character the column's code page lacks becoming '?'. Unused for a
	// UTF-8 column.
	DatabaseCodePage int
}

// BulkValueError is a value BulkInsert refused, or BulkColumn.Converter's
// function did, before it reached the server: an int out of its column's
// range, a string too long or with a character the code page lacks. It
// wraps ErrInvalidRequest.
type BulkValueError struct {
	// Row is the 1-based row of the load; 0 when the value was checked
	// outside one.
	Row int64

	// Column is the column's name; empty when the BulkColumn had none.
	Column string

	// Reason says what is wrong with the value: "300 is out of range (0 to
	// 255)".
	Reason string
}

func (e *BulkValueError) Error() string {
	var b strings.Builder
	if e.Row > 0 {
		fmt.Fprintf(&b, "row %d, ", e.Row)
	}
	if e.Column != "" {
		b.WriteString("column ")
		b.WriteString(QuoteName(e.Column))
		b.WriteString(": ")
	}
	b.WriteString(e.Reason)
	return b.String()
}

func (e *BulkValueError) Unwrap() error { return ErrInvalidRequest }

// Converter returns the function BulkInsert applies to each non-nil value
// bound for c before sending it: it refuses, as a *BulkValueError, what
// go-mssqldb would load wrong or the server would round or reject without
// naming the row, and returns the value as the driver should send it. A nil
// value is the caller's (SQL NULL) and is not passed to it.
//
// go-mssqldb's bulk copy checks almost nothing — an int too big for a tinyint
// wraps (300 loads as 44), a float bound for an int is truncated, a varchar
// gets the string's UTF-8 bytes whatever the code page, a datetime before
// 1753 is encoded as garbage, a too-long string fails the whole load with no
// row named. The function checks, by column type:
//
//   - tinyint … bigint: any Go integer, a whole float, or decimal text
//     ("+7"), in the type's range; sent as int64.
//   - bit: a bool, or "1", "0", "true", "false" in any case.
//   - decimal, numeric: an integer, decimal text (no exponent) with no more
//     than Scale decimal places, or a finite float, rounded to Scale as CAST
//     rounds; the integer part within Precision-Scale digits. Sent as text,
//     which the driver scales exactly.
//   - money, smallmoney: as decimal, with 4 places, in the type's range.
//   - float, real: a number or its text, finite and within real's range for
//     real; real is sent as float32.
//   - date … datetimeoffset: a time.Time within the type's range;
//     smalldatetime rounded to the minute as SQL Server rounds (29.998
//     seconds down, 29.999 up), which go-mssqldb would truncate. Text passes
//     through to the driver's own parsing.
//   - uniqueidentifier: 16 bytes in wire order, or the dashed text.
//   - binary, varbinary, image: []byte within Length; binary(n) zero-padded
//     to n as an INSERT pads.
//   - nchar, nvarchar, ntext, xml: valid UTF-8 text within Length UTF-16
//     units; nchar(n) space-padded.
//   - char, varchar, text: valid UTF-8 text, each character in the column's
//     code page, within Length bytes of it, char(n) space-padded, and sent
//     as bytes in DatabaseCodePage (what INSERT BULK reads them in), so a
//     character that page lacks is refused too. A UTF-8 column takes the
//     text as a string: the server describes it to go-mssqldb (which has not
//     negotiated UTF-8) as nvarchar, so the driver sends UTF-16, and bytes
//     would fail with Msg 4895. []byte is taken as already encoded and only
//     its length is checked.
//
// An integer bound for a string column is its decimal text. A value of a
// type not listed for its column is passed on for the driver to refuse.
// Each value is first reduced to a driver.Value (a driver.Valuer's Value, a
// named or sized integer to int64), as database/sql does. The function is
// idempotent — what it returns, passed again, comes back unchanged — so a
// caller may check its values (a dry run, a source that wants its own line
// numbers in the message) and still hand them to BulkInsert.
//
// Converter's error is an ErrUnsupported one for a char column whose code
// page gosmo has no encoder for (0, a Unicode-only collation, among them).
// The returned function keeps state (a code page's encoder) and is for one
// goroutine.
func (c BulkColumn) Converter() (func(v any) (any, error), error) {
	conv, err := c.converter()
	if err != nil {
		return nil, err
	}
	return func(v any) (any, error) {
		if dv, err := driver.DefaultParameterConverter.ConvertValue(v); err == nil {
			v = dv
		}
		out, reason := conv(v)
		if reason != "" {
			return nil, &BulkValueError{Column: c.Name, Reason: reason}
		}
		return out, nil
	}, nil
}

// bulkConv is one column's check: the value to send, or why not.
type bulkConv func(v any) (any, string)

func (c BulkColumn) converter() (bulkConv, error) {
	switch c.Type {
	case "bit":
		return convBit, nil
	case "tinyint":
		return intConv(0, math.MaxUint8), nil
	case "smallint":
		return intConv(math.MinInt16, math.MaxInt16), nil
	case "int":
		return intConv(math.MinInt32, math.MaxInt32), nil
	case "bigint":
		return intConv(math.MinInt64, math.MaxInt64), nil
	case "decimal", "numeric":
		return decimalConv(c.Precision, c.Scale), nil
	case "money":
		return moneyConv(math.MinInt64, math.MaxInt64), nil
	case "smallmoney":
		return moneyConv(math.MinInt32, math.MaxInt32), nil
	case "float":
		return floatConv(64), nil
	case "real":
		return floatConv(32), nil
	case "date", "time", "datetime", "smalldatetime", "datetime2", "datetimeoffset":
		return timeConv(c.Type), nil
	case "uniqueidentifier":
		return convGUID, nil
	case "binary", "varbinary":
		return binaryConv(c.Length, c.Type == "binary"), nil
	case "image":
		return binaryConv(-1, false), nil
	case "nchar", "nvarchar":
		return ncharConv(c.Length, c.Type == "nchar"), nil
	case "ntext", "xml":
		return ncharConv(-1, false), nil
	case "char", "varchar":
		return charConv(c.Name, c.Length, c.Type == "char", c.CodePage, c.DatabaseCodePage)
	case "text":
		return charConv(c.Name, -1, false, c.CodePage, c.DatabaseCodePage)
	}
	return func(v any) (any, string) { return v, "" }, nil
}

func convBit(v any) (any, string) {
	s, ok := v.(string)
	if !ok {
		return v, ""
	}
	switch strings.ToLower(s) {
	case "1", "true":
		return true, ""
	case "0", "false":
		return false, ""
	}
	return nil, fmt.Sprintf("%q is not a bit (1, 0, true or false)", s)
}

func intConv(lo, hi int64) bulkConv {
	outOfRange := func(text string) (any, string) {
		return nil, fmt.Sprintf("%s is out of range (%d to %d)", text, lo, hi)
	}
	return func(v any) (any, string) {
		switch v := v.(type) {
		case int64:
			if v < lo || v > hi {
				return outOfRange(strconv.FormatInt(v, 10))
			}
			return v, ""
		case float64:
			if v != math.Trunc(v) || math.IsInf(v, 0) {
				return nil, fmt.Sprintf("%s is not a whole number", strconv.FormatFloat(v, 'g', -1, 64))
			}
			// float64(hi) can round up past hi (MaxInt64 does), so the upper
			// bound is exclusive at the next power of two there.
			if v < float64(lo) || v > float64(hi) || v == float64(hi) && hi == math.MaxInt64 {
				return outOfRange(strconv.FormatFloat(v, 'f', -1, 64))
			}
			return int64(v), ""
		case string:
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil && !errors.Is(err, strconv.ErrRange) {
				return nil, fmt.Sprintf("%q is not an integer", v)
			}
			if err != nil || n < lo || n > hi {
				return outOfRange(v)
			}
			return n, ""
		}
		return v, ""
	}
}

// splitDecimal splits -?digits(.digits)? (a + sign allowed, either digit run
// empty but not both) into its sign and digit runs.
func splitDecimal(s string) (neg bool, intPart, frac string, ok bool) {
	t := s
	if t != "" && (t[0] == '-' || t[0] == '+') {
		neg, t = t[0] == '-', t[1:]
	}
	intPart, frac, _ = strings.Cut(t, ".")
	ok = intPart+frac != "" && digitsOnly(intPart) && digitsOnly(frac)
	return neg, intPart, frac, ok
}

func digitsOnly(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func decimalText(neg bool, intPart, frac string) string {
	if intPart == "" {
		intPart = "0"
	}
	if neg {
		intPart = "-" + intPart
	}
	if frac == "" {
		return intPart
	}
	return intPart + "." + frac
}

// numberText is an integer or a float as decimal text, a float rounded to
// places: what decimal and money check like a caller's text. ok is false
// for any other value, and for a float that is not finite.
func numberText(v any, places int) (text string, ok bool) {
	switch v := v.(type) {
	case int64:
		return strconv.FormatInt(v, 10), true
	case float64:
		if math.IsInf(v, 0) || math.IsNaN(v) {
			return "", false
		}
		return strconv.FormatFloat(v, 'f', places, 64), true
	}
	return "", false
}

// decimalConv returns the digits as text, which go-mssqldb scales to the
// column exactly (a float would round).
func decimalConv(precision, scale int) bulkConv {
	return func(v any) (any, string) {
		s, ok := v.(string)
		if !ok {
			if s, ok = numberText(v, scale); !ok {
				if f, isFloat := v.(float64); isFloat {
					return nil, fmt.Sprintf("%v is not a finite number", f)
				}
				return v, ""
			}
		}
		neg, intPart, frac, ok := splitDecimal(s)
		if !ok {
			return nil, fmt.Sprintf("%q is not a decimal number", s)
		}
		intPart = strings.TrimLeft(intPart, "0")
		if len(frac) > scale {
			return nil, fmt.Sprintf("%s has more than %d decimal places", s, scale)
		}
		if len(intPart) > precision-scale {
			return nil, fmt.Sprintf("%s does not fit decimal(%d,%d)", s, precision, scale)
		}
		return decimalText(neg, intPart, frac), ""
	}
}

// moneyConv checks the value as money stores it, an integer count of
// ten-thousandths in [lo, hi], and returns the digits as text — the only
// form go-mssqldb takes for money.
func moneyConv(lo, hi int64) bulkConv {
	return func(v any) (any, string) {
		s, ok := v.(string)
		if !ok {
			if s, ok = numberText(v, 4); !ok {
				if f, isFloat := v.(float64); isFloat {
					return nil, fmt.Sprintf("%v is not a finite number", f)
				}
				return v, ""
			}
		}
		neg, intPart, frac, ok := splitDecimal(s)
		if !ok {
			return nil, fmt.Sprintf("%q is not a money amount", s)
		}
		if len(frac) > 4 {
			return nil, fmt.Sprintf("%s has more than 4 decimal places", s)
		}
		digits := strings.TrimLeft(intPart+frac+strings.Repeat("0", 4-len(frac)), "0")
		var units uint64
		inRange := len(digits) <= 19
		for i := 0; inRange && i < len(digits); i++ {
			d := uint64(digits[i] - '0')
			inRange = units <= (math.MaxUint64-d)/10
			units = units*10 + d
		}
		limit := uint64(hi)
		if neg {
			limit = uint64(-(lo + 1)) + 1
		}
		if !inRange || units > limit {
			return nil, fmt.Sprintf("%s is out of range (%s to %s)", s, moneyText(lo), moneyText(hi))
		}
		return decimalText(neg, strings.TrimLeft(intPart, "0"), frac), ""
	}
}

// moneyText renders ten-thousandths as money's decimal text.
func moneyText(units int64) string {
	s := strconv.FormatInt(units, 10)
	return s[:len(s)-4] + "." + s[len(s)-4:]
}

// floatConv returns float64 for float and float32 for real. Text is parsed at
// the column's own size, so the shortest round-trip text of a stored real
// gives back its bits.
func floatConv(bits int) bulkConv {
	name := map[int]string{32: "real", 64: "float"}[bits]
	return func(v any) (any, string) {
		var f float64
		text := ""
		switch v := v.(type) {
		case float64:
			f = v
		case int64:
			f = float64(v)
		case string:
			var err error
			f, err = strconv.ParseFloat(v, bits)
			if err != nil && !errors.Is(err, strconv.ErrRange) {
				return nil, fmt.Sprintf("%q is not a number", v)
			}
			if err != nil {
				return nil, fmt.Sprintf("%s is out of range for %s", v, name)
			}
			text = v
		default:
			return v, ""
		}
		if text == "" {
			text = strconv.FormatFloat(f, 'g', -1, 64)
		}
		if math.IsInf(f, 0) || math.IsNaN(f) || bits == 32 && math.Abs(f) > math.MaxFloat32 {
			return nil, fmt.Sprintf("%s is out of range for %s", text, name)
		}
		if bits == 32 {
			return float32(f), ""
		}
		return f, ""
	}
}

// timeConv applies each date/time type's range to a time.Time, and rounds
// smalldatetime to the minute as SQL Server does (29.998 seconds down,
// 29.999 up): go-mssqldb would drop the seconds.
func timeConv(typ string) bulkConv {
	return func(v any) (any, string) {
		t, ok := v.(time.Time)
		if !ok {
			return v, ""
		}
		text := func() string { return t.Format("2006-01-02 15:04:05.999999999") }
		switch typ {
		case "date", "datetime2", "datetimeoffset":
			if t.Year() < 1 || t.Year() > 9999 {
				return nil, fmt.Sprintf("%s is out of range for %s (years 1 to 9999)", text(), typ)
			}
		case "datetime":
			// .9985 and up rounds to the next 1/300 second, past the last day.
			if t.Year() < 1753 || t.Year() > 9999 || t.Year() == 9999 && t.YearDay() == 365 && t.Hour() == 23 && t.Minute() == 59 && t.Second() == 59 && t.Nanosecond() >= 998500000 {
				return nil, fmt.Sprintf("%s is out of range for datetime (1753-01-01 to 9999-12-31 23:59:59.997)", text())
			}
		case "smalldatetime":
			r := t
			if r.Second()*1000+r.Nanosecond()/1e6 >= 29999 {
				r = r.Add(time.Minute)
			}
			r = r.Truncate(time.Minute)
			if r.Before(time.Date(1900, 1, 1, 0, 0, 0, 0, r.Location())) || r.After(time.Date(2079, 6, 6, 23, 59, 0, 0, r.Location())) {
				return nil, fmt.Sprintf("%s is out of range for smalldatetime (1900-01-01 to 2079-06-06 23:59)", text())
			}
			return r, ""
		}
		return t, ""
	}
}

// convGUID takes a uniqueidentifier's 16 wire-order bytes as they are, and
// its dashed text (8-4-4-4-12 hex digits, either case, braces optional) as
// those bytes, the first three groups byte-reversed.
func convGUID(v any) (any, string) {
	switch v := v.(type) {
	case []byte:
		if len(v) != 16 {
			return nil, fmt.Sprintf("%d bytes is not a uniqueidentifier (16)", len(v))
		}
		return v, ""
	case string:
		t := v
		if len(t) == 38 && t[0] == '{' && t[37] == '}' {
			t = t[1:37]
		}
		ok := len(t) == 36
		for i := 0; ok && i < 36; i++ {
			switch i {
			case 8, 13, 18, 23:
				ok = t[i] == '-'
			default:
				ok = isHexDigit(t[i])
			}
		}
		var g mssql.UniqueIdentifier
		if !ok || g.Scan(t) != nil {
			return nil, fmt.Sprintf("%q is not a uniqueidentifier", truncateForError(v))
		}
		b, _ := g.Value()
		return b, ""
	}
	return v, ""
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// binaryConv checks bytes against n and zero-pads binary(n).
func binaryConv(n int, fixed bool) bulkConv {
	return func(v any) (any, string) {
		b, ok := v.([]byte)
		if !ok {
			return v, ""
		}
		if n > 0 && len(b) > n {
			return nil, fmt.Sprintf("%d bytes is longer than the column's %d", len(b), n)
		}
		if fixed && len(b) < n {
			b = append(b[:len(b):len(b)], make([]byte, n-len(b))...)
		}
		return b, ""
	}
}

// stringOf is v as text when it is text or an integer (an integer's decimal
// text, as the driver would send it).
func stringOf(v any) (string, bool) {
	switch v := v.(type) {
	case string:
		return v, true
	case int64:
		return strconv.FormatInt(v, 10), true
	}
	return "", false
}

const notUTF8 = "the text is not valid UTF-8"

// ncharConv checks length in UTF-16 code units, nvarchar(n)'s unit, and pads
// nchar(n) with spaces.
func ncharConv(n int, fixed bool) bulkConv {
	return func(v any) (any, string) {
		s, ok := stringOf(v)
		if !ok {
			return v, ""
		}
		if !utf8.ValidString(s) {
			return nil, notUTF8
		}
		if n <= 0 {
			return s, ""
		}
		units := 0
		for _, r := range s {
			units += utf16.RuneLen(r)
		}
		if units > n {
			return nil, fmt.Sprintf("%d characters is longer than the column's %d", units, n)
		}
		if fixed && units < n {
			s += strings.Repeat(" ", n-units)
		}
		return s, ""
	}
}

// charConv encodes to the database's code page, which INSERT BULK reads char
// bytes in, and refuses a character the column's code page lacks, which the
// server's conversion would store as '?'. Length is in bytes of the column's
// code page, char(n)'s and varchar(n)'s unit, and char(n) is padded with
// spaces. A UTF-8 column takes a string (see Converter).
func charConv(name string, n int, fixed bool, codePage, dbCodePage int) (bulkConv, error) {
	tooLong := func(got int) string { return fmt.Sprintf("%d bytes is longer than the column's %d", got, n) }
	if codePage == 65001 {
		return func(v any) (any, string) {
			if b, ok := v.([]byte); ok {
				if !utf8.Valid(b) {
					return nil, notUTF8
				}
				v = string(b)
			}
			s, ok := stringOf(v)
			if !ok {
				return v, ""
			}
			if !utf8.ValidString(s) {
				return nil, notUTF8
			}
			if n > 0 && len(s) > n {
				return nil, tooLong(len(s))
			}
			if fixed && len(s) < n {
				s += strings.Repeat(" ", n-len(s))
			}
			return s, ""
		}, nil
	}
	load, err := encoderFor(name, dbCodePage)
	if err != nil {
		return nil, err
	}
	// col encodes only to check and measure, when the column's code page is
	// not the one the bytes travel in.
	col := load
	if codePage != dbCodePage {
		if col, err = encoderFor(name, codePage); err != nil {
			return nil, err
		}
	}
	encode := func(enc *encoding.Encoder, cp int, s, whose string) ([]byte, string) {
		if enc == nil {
			return []byte(s), ""
		}
		b, err := enc.Bytes([]byte(s))
		if err != nil {
			return nil, unencodable(s, enc, cp, whose)
		}
		return b, ""
	}
	return func(v any) (any, string) {
		if b, ok := v.([]byte); ok {
			// Already encoded, in the database's code page. Only when that is
			// the column's too can the length be measured; a space is one
			// byte in every code page here.
			if codePage != dbCodePage {
				return b, ""
			}
			if n > 0 && len(b) > n {
				return nil, tooLong(len(b))
			}
			if fixed && len(b) < n {
				b = append(b[:len(b):len(b)], strings.Repeat(" ", n-len(b))...)
			}
			return b, ""
		}
		s, ok := stringOf(v)
		if !ok {
			return v, ""
		}
		if !utf8.ValidString(s) {
			return nil, notUTF8
		}
		stored, why := encode(col, codePage, s, "column's")
		if why != "" {
			return nil, why
		}
		if n > 0 && len(stored) > n {
			return nil, tooLong(len(stored))
		}
		if fixed && len(stored) < n {
			pad := strings.Repeat(" ", n-len(stored))
			s += pad
			stored = append(stored, pad...)
		}
		if codePage == dbCodePage {
			return stored, ""
		}
		b, why := encode(load, dbCodePage, s, "database's")
		if why != "" {
			return nil, why
		}
		return b, ""
	}, nil
}

// encoderFor returns code page cp's encoder, nil for UTF-8; name is the
// column's, for the error.
func encoderFor(name string, cp int) (*encoding.Encoder, error) {
	if cp == 65001 {
		return nil, nil
	}
	e, ok := codePages[cp]
	if !ok {
		col := "a char column"
		if name != "" {
			col = "column " + QuoteName(name)
		}
		return nil, unsupportedf("gosmo: bulk load into %s: no encoder for code page %d", col, cp)
	}
	return e.NewEncoder(), nil
}

// unencodable names the first character of s that code page cp lacks.
func unencodable(s string, enc *encoding.Encoder, cp int, whose string) string {
	for _, r := range s {
		if _, err := enc.String(string(r)); err != nil {
			return fmt.Sprintf("%q (U+%04X) is not in the %s code page %d", r, r, whose, cp)
		}
	}
	return fmt.Sprintf("the text cannot be encoded in the %s code page %d", whose, cp)
}

// codePages are the code pages SQL Server collations use, except UTF-8.
var codePages = map[int]encoding.Encoding{
	437:  charmap.CodePage437,
	850:  charmap.CodePage850,
	874:  charmap.Windows874,
	932:  japanese.ShiftJIS,
	936:  simplifiedchinese.GBK,
	949:  korean.EUCKR,
	950:  traditionalchinese.Big5,
	1250: charmap.Windows1250,
	1251: charmap.Windows1251,
	1252: charmap.Windows1252,
	1253: charmap.Windows1253,
	1254: charmap.Windows1254,
	1255: charmap.Windows1255,
	1256: charmap.Windows1256,
	1257: charmap.Windows1257,
	1258: charmap.Windows1258,
}

// truncateForError shortens s for an error message: a cell can be megabytes.
func truncateForError(s string) string {
	if len(s) <= 40 {
		return s
	}
	return s[:40] + "…"
}
