package gosmo

import (
	"strings"
	"testing"
)

// TestOpenQueryQuotesBothLayers pins the two quoting layers a linked-server
// read needs: the database name bracket-quoted inside the remote text, then
// the whole text as one N” literal — a name holding both ']' and '\” must
// come out with the bracket doubled once and the quote doubled once.
func TestOpenQueryQuotesBothLayers(t *testing.T) {
	got := openQuery("LS]1", linkedCatalogQuery("db'x]y"))
	if !strings.HasPrefix(got, "SELECT * FROM OPENQUERY([LS]]1], N'SELECT ") {
		t.Errorf("prefix: %q", got)
	}
	if !strings.Contains(got, "FROM   [db''x]]y].sys.objects o") {
		t.Errorf("database not quoted in both layers: %q", got)
	}
	if !strings.Contains(got, "o.type IN (''U'',''V'')") {
		t.Errorf("remote literals not doubled: %q", got)
	}
	if !strings.HasSuffix(got, "c.column_id')") {
		t.Errorf("suffix: %q", got)
	}
}
