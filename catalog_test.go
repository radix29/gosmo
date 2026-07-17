package gosmo

import "testing"

func TestCatalogObjectType(t *testing.T) {
	cases := []struct {
		typeCode string
		want     CatalogObjectType
	}{
		{"V", CatalogView},
		{"U", CatalogTable},
	}
	for _, c := range cases {
		t.Run(c.typeCode, func(t *testing.T) {
			if got := catalogObjectType(c.typeCode); got != c.want {
				t.Errorf("catalogObjectType(%q) = %v, want %v", c.typeCode, got, c.want)
			}
		})
	}
}
