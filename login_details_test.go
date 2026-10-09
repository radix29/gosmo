package gosmo

import (
	"database/sql"
	"testing"
	"time"
)

// LOGINPROPERTY answers "never" with 1900-01-01 shifted by the server's UTC
// offset, not NULL — 1900-01-01 02:00 for six of seven SQL logins on a UTC+2
// host. Passed through, Login Properties ▸ Status showed it as the last bad
// password time.
func TestLoginPropertyTimeDropsTheSentinel(t *testing.T) {
	real := time.Date(2026, 10, 9, 0, 44, 6, 293e6, time.UTC)
	cases := []struct {
		name string
		in   sql.NullTime
		want time.Time
	}{
		{"NULL", sql.NullTime{}, time.Time{}},
		{"sentinel at UTC", sql.NullTime{Time: time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}, time.Time{}},
		{"sentinel east of UTC", sql.NullTime{Time: time.Date(1900, 1, 1, 2, 0, 0, 0, time.UTC), Valid: true}, time.Time{}},
		{"sentinel west of UTC", sql.NullTime{Time: time.Date(1899, 12, 31, 19, 0, 0, 0, time.UTC), Valid: true}, time.Time{}},
		{"a real time", sql.NullTime{Time: real, Valid: true}, real},
	}
	for _, tc := range cases {
		if got := loginPropertyTime(tc.in); !got.Equal(tc.want) {
			t.Errorf("%s: loginPropertyTime(%v) = %v, want %v", tc.name, tc.in.Time, got, tc.want)
		}
	}
}
