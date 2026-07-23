package gosmo

import "testing"

func TestCategoryClassCode(t *testing.T) {
	cases := []struct {
		c    CategoryClass
		want int
	}{
		{CategoryClassJob, 1},
		{CategoryClassAlert, 2},
		{CategoryClassOperator, 3},
		{CategoryClass("BOGUS"), 0},
	}
	for _, c := range cases {
		if got := c.c.code(); got != c.want {
			t.Errorf("%q.code() = %d, want %d", c.c, got, c.want)
		}
	}
}
