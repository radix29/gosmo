package gosmo

import (
	"errors"
	"testing"
)

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
		// Every metacharacter at once, from the securable-search test this
		// one absorbed when escapeLikePattern turned out to be a duplicate.
		{`_%[\`, `\_\%\[\\`},
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
	// An empty schema must not become "[].[name]" — OBJECT_ID resolves that to
	// NULL, and the caller sees an empty result instead of a failure.
	if got := qualifiedName("", "Users"); got != "[Users]" {
		t.Errorf("qualifiedName(\"\", Users) = %q, want [Users]", got)
	}
}

// errOnly drops a Create*'s returned object, for a test that only asserts on
// the error or on the statement it scripted.
func errOnly[T any](_ T, err error) error { return err }

// matchName: an exact match wins, a single case-insensitive one is the
// fallback, and several case-insensitive ones with no exact one are refused
// rather than resolved to whichever came first.
func TestMatchName(t *testing.T) {
	key := func(s string) string { return s }
	for _, tc := range []struct {
		items []string
		name  string
		want  string
		err   error
	}{
		{[]string{"IX_a", "ix_A"}, "ix_A", "ix_A", nil},
		{[]string{"IX_a", "ix_A"}, "IX_a", "IX_a", nil},
		{[]string{"IX_a", "ix_A"}, "ix_a", "", ErrAmbiguous},
		{[]string{"IX_a", "other"}, "ix_a", "IX_a", nil},
		{[]string{"IX_a"}, "IX_b", "", ErrNotFound},
		{nil, "x", "", ErrNotFound},
	} {
		got, err := matchName(tc.items, tc.name, key)
		if got != tc.want || !errors.Is(err, tc.err) || (tc.err == nil) != (err == nil) {
			t.Errorf("matchName(%q, %q) = %q, %v; want %q, %v", tc.items, tc.name, got, err, tc.want, tc.err)
		}
	}
}
