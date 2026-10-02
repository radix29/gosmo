package gosmo

import "testing"

func TestQuoteNameIfNeeded(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Orders", "Orders"},
		{"_x1", "_x1"},
		{"a$b#c@d", "a$b#c@d"},
		{"User", "[User]"}, // bare, it is the USER function
		{"desc", "[desc]"},
		{"current_timestamp", "[current_timestamp]"},
		{"Name", "Name"}, // not reserved
		{"", "[]"},
		{"1st", "[1st]"},
		{"@v", "[@v]"},
		{"#t", "[#t]"},
		{"$x", "[$x]"},
		{"order id", "[order id]"},
		{"a]b", "[a]]b]"},
		{"Größe", "[Größe]"}, // non-ASCII is always bracketed
	} {
		if got := QuoteNameIfNeeded(tc.in); got != tc.want {
			t.Errorf("QuoteNameIfNeeded(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestUnquoteName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"[a]]b]", "a]b"},
		{"[a]]]]b]", "a]]b"},
		{"[[x]", "[x"},
		{`"a""b"`, `a"b`},
		{"[]", ""},
		{"plain", "plain"},
		{"[open", "[open"},
		{"[a]b]", "[a]b]"},         // an undoubled ']' inside: not one quoted part
		{"[dbo].[t]", "[dbo].[t]"}, // two parts
		{"[", "["},
		{`"a"b"`, `"a"b"`},
	} {
		if got := UnquoteName(tc.in); got != tc.want {
			t.Errorf("UnquoteName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, name := range []string{"a]b", "]]", "x", "", "[y]", `q"`} {
		if got := UnquoteName(QuoteName(name)); got != name {
			t.Errorf("UnquoteName(QuoteName(%q)) = %q", name, got)
		}
	}
}

func TestIsReservedKeyword(t *testing.T) {
	for _, w := range []string{"select", "USER", "Desc", "Asc", "file", "PLAN", "percent", "open",
		"current", "schema", "database", "option", "try_convert", "semanticsimilaritydetailstable"} {
		if !IsReservedKeyword(w) {
			t.Errorf("IsReservedKeyword(%q) = false", w)
		}
	}
	// ODBC and future keywords, and words that are keywords only in a
	// context: all accepted bare by the server (probed on 13 and 17).
	for _, w := range []string{"name", "cast", "apply", "ties", "offset", "date", "count", "within", "user1", ""} {
		if IsReservedKeyword(w) {
			t.Errorf("IsReservedKeyword(%q) = true", w)
		}
	}
	if n := len(reservedKeywords); n != 184 {
		t.Errorf("%d reserved keywords, want 184 (179 the server refuses + 5 documented ones it accepts)", n)
	}
}

func TestQuoteAnsiLiteral(t *testing.T) {
	if got := QuoteAnsiLiteral("it's"); got != "'it''s'" {
		t.Errorf("QuoteAnsiLiteral = %s", got)
	}
	if got := QuoteLiteral("it's"); got != "N'it''s'" {
		t.Errorf("QuoteLiteral = %s", got)
	}
}
