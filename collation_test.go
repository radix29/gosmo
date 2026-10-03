package gosmo

import "testing"

func TestCollationIgnoresCase(t *testing.T) {
	for _, tt := range []struct {
		collation string
		ignores   bool
	}{
		{"", true},
		{"SQL_Latin1_General_CP1_CI_AS", true},
		{"Latin1_General_100_CI_AS_SC_UTF8", true},
		{"Latin1_General_CS_AS", false},
		{"latin1_general_cs_as", false},
		{"Latin1_General_BIN", false},
		{"Latin1_General_100_BIN2_UTF8", false},
		// Token match, not substring: "CS" inside another token is not one.
		{"Hebrew_CSX_CI_AS", true},
	} {
		if got := CollationIgnoresCase(tt.collation); got != tt.ignores {
			t.Errorf("CollationIgnoresCase(%q) = %v, want %v", tt.collation, got, tt.ignores)
		}
		if got := SameName(tt.collation, "Sales", "sales"); got != tt.ignores {
			t.Errorf("SameName(%q, Sales, sales) = %v, want %v", tt.collation, got, tt.ignores)
		}
		if !SameName(tt.collation, "Sales", "Sales") {
			t.Errorf("SameName(%q, Sales, Sales) = false", tt.collation)
		}
	}
}
