package gosmo

import "testing"

func TestQuoteIdent(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"Users", "[Users]"},
		{"", "[]"},
		{"a]b", "[a]]b]"},
		{"]]", "[]]]]]"},
	}
	for _, c := range cases {
		if got := quoteIdent(c.name); got != c.want {
			t.Errorf("quoteIdent(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestEscapeSingle(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"O'Brien", "O''Brien"},
		{"", ""},
		{"no quotes", "no quotes"},
		{"''", "''''"},
	}
	for _, c := range cases {
		if got := escapeSingle(c.in); got != c.want {
			t.Errorf("escapeSingle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNullableStr(t *testing.T) {
	if got := nullableStr(""); got != "NULL" {
		t.Errorf("nullableStr(\"\") = %q, want NULL", got)
	}
	if got := nullableStr("abc"); got != "N'abc'" {
		t.Errorf("nullableStr(\"abc\") = %q, want N'abc'", got)
	}
	if got := nullableStr("it's"); got != "N'it''s'" {
		t.Errorf("nullableStr(\"it's\") = %q, want N'it''s'", got)
	}
}

func TestBoolToInt(t *testing.T) {
	if got := boolToInt(true); got != 1 {
		t.Errorf("boolToInt(true) = %d, want 1", got)
	}
	if got := boolToInt(false); got != 0 {
		t.Errorf("boolToInt(false) = %d, want 0", got)
	}
}

func TestLikeEscape(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"plain", "plain"},
		{"50%", `50\%`},
		{"a_b", `a\_b`},
		{"[abc]", `\[abc]`},
		{`back\slash`, `back\\slash`},
	}
	for _, c := range cases {
		if got := likeEscape(c.in); got != c.want {
			t.Errorf("likeEscape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestQualifiedName(t *testing.T) {
	if got := qualifiedName("dbo", "Users"); got != "[dbo].[Users]" {
		t.Errorf("qualifiedName(dbo, Users) = %q, want [dbo].[Users]", got)
	}
	if got := qualifiedName("my]schema", "tbl"); got != "[my]]schema].[tbl]" {
		t.Errorf("qualifiedName(my]schema, tbl) = %q, want [my]]schema].[tbl]", got)
	}
}
