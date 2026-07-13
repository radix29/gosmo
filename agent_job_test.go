package gosmo

import (
	"testing"
	"time"
)

func TestParseSQLAgentDate(t *testing.T) {
	got := parseSQLAgentDate(20240315, 143059)
	want := time.Date(2024, time.March, 15, 14, 30, 59, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("parseSQLAgentDate(20240315, 143059) = %v, want %v", got, want)
	}
}

func TestParseSQLAgentDateMidnight(t *testing.T) {
	got := parseSQLAgentDate(20200101, 0)
	want := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("parseSQLAgentDate(20200101, 0) = %v, want %v", got, want)
	}
}

func TestParseSQLAgentDuration(t *testing.T) {
	cases := []struct {
		dur  int
		want time.Duration
	}{
		{0, 0},
		{10203, 1*time.Hour + 2*time.Minute + 3*time.Second},
		{130245, 13*time.Hour + 2*time.Minute + 45*time.Second},
		{959, 9*time.Minute + 59*time.Second},
		{5, 5 * time.Second},
	}
	for _, c := range cases {
		if got := parseSQLAgentDuration(c.dur); got != c.want {
			t.Errorf("parseSQLAgentDuration(%d) = %v, want %v", c.dur, got, c.want)
		}
	}
}
