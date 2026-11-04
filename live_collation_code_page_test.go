//go:build livedb

// Live check of Server.CollationCodePage against COLLATIONPROPERTY.
//
//	go test -tags livedb . -run TestLiveCollationCodePage -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
package gosmo

import (
	"errors"
	"testing"
)

func TestLiveCollationCodePage(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	cases := map[string]int{
		"Latin1_General_CI_AS":         1252,
		"SQL_Latin1_General_CP1_CI_AS": 1252,
		"Japanese_CI_AS":               932,
		"Cyrillic_General_CI_AS":       1251,
		"Indic_General_90_CI_AS":       0, // Unicode-only
	}
	if srv.Info().VersionMajor >= int(SQLServer2019) {
		cases["Latin1_General_100_CI_AS_SC_UTF8"] = 65001
	}
	for name, want := range cases {
		got, err := srv.CollationCodePage(ctx, name)
		if err != nil || got != want {
			t.Errorf("%s: %d, %v; want %d", name, got, err, want)
		}
	}
	if _, err := srv.CollationCodePage(ctx, "No_Such_Collation"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown collation: %v, want ErrNotFound", err)
	}
}
