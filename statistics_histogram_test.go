package gosmo

import (
	"testing"
	"time"
)

// RANGE_HI_KEY arrives as whatever go-mssqldb makes of the leading key
// column's type; each value below is what the driver handed back from
// win10cli for that type (a probe, 2026-10-09), and want is what the server
// itself prints.
func TestHistogramKeyFormatsByType(t *testing.T) {
	plus2 := time.FixedZone("", 2*60*60)
	cases := []struct {
		typ   string
		scale int64
		v     any
		want  string
	}{
		{"INT", 0, nil, "NULL"},
		{"INT", 0, int64(42), "42"},
		{"DECIMAL", 2, []byte("123.45"), "123.45"},
		{"NUMERIC", 0, []byte("123456789012345678"), "123456789012345678"},
		{"MONEY", 0, []byte("12.3456"), "12.3456"},
		{"SMALLMONEY", 0, []byte("1.5000"), "1.5000"},
		{"UNIQUEIDENTIFIER", 0,
			[]byte{0xff, 0x19, 0x96, 0x6f, 0x86, 0x8b, 0x11, 0xd0, 0xb4, 0x2d, 0x00, 0xc0, 0x4f, 0xc9, 0x64, 0xff},
			"6F9619FF-8B86-D011-B42D-00C04FC964FF"},
		{"VARBINARY", 0, []byte{0x01, 0x02}, "0x0102"},
		{"DATETIME2", 3, time.Date(2026, 1, 2, 3, 4, 5, 123e6, time.UTC), "2026-01-02 03:04:05.123"},
		{"DATETIME2", 0, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "2026-01-02 03:04:05"},
		{"DATE", 0, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), "2026-01-02"},
		{"DATETIME", 0, time.Date(2026, 1, 2, 3, 4, 5, 997e6, time.UTC), "2026-01-02 03:04:05.997"},
		{"SMALLDATETIME", 0, time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC), "2026-01-02 03:04:00"},
		{"DATETIMEOFFSET", 2, time.Date(2026, 1, 2, 3, 4, 5, 120e6, plus2), "2026-01-02 03:04:05.12 +02:00"},
		{"TIME", 4, time.Date(1, 1, 1, 3, 4, 5, 123400e3, time.UTC), "03:04:05.1234"},
		{"REAL", 0, float64(float32(0.1)), "0.1"},
		{"FLOAT", 0, 0.1, "0.1"},
		{"NCHAR", 0, "abc", "abc"},
		{"BIT", 0, true, "1"},
		{"BIT", 0, false, "0"},
	}
	for _, tc := range cases {
		k := histogramKeyType{name: tc.typ, scale: tc.scale}
		if got := k.format(tc.v); got != tc.want {
			t.Errorf("%s(%d) %#v: got %q, want %q", tc.typ, tc.scale, tc.v, got, tc.want)
		}
	}
}
