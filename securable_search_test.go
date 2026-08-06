package gosmo

import "testing"

// A LIKE pattern built from user input must match the typed characters
// literally. Identifiers legally contain _ and %, and a name containing [
// turns the pattern into a character class that silently matches nothing —
// the search box would just come up empty with no explanation.
func TestEscapeLikePattern(t *testing.T) {
	cases := []struct{ in, want string }{
		{"orders", "orders"},
		{"", ""},
		{"my_table", `my\_table`},
		{"100%", `100\%`},
		{"[a-z]", `\[a-z]`},
		{`back\slash`, `back\\slash`},
		{`_%[\`, `\_\%\[\\`},
	}
	for _, c := range cases {
		if got := escapeLikePattern(c.in); got != c.want {
			t.Errorf("escapeLikePattern(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
