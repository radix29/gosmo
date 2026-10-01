package gosmo

import (
	"strings"
	"testing"
)

// A mail read is one batch: the base table under the HAS_PERMS_BY_NAME
// predicate, the view under ELSE, and the caller's filter and order in both.
// Live: TestLiveDatabaseMailReads (db_datareader, the DENY, the role member).
func TestMailReadsBranchToTheBaseTable(t *testing.T) {
	for _, tc := range []struct {
		name, q, table, base, view string
	}{
		{"items", mailItemsQuery("SELECT x FROM %s WHERE sent_status = @p1 ORDER BY mailitem_id DESC"),
			"sysmail_mailitems", "FROM   msdb.dbo.sysmail_mailitems) AS ai", "FROM msdb.dbo.sysmail_allitems WHERE"},
		{"events", mailEventsQuery("SELECT x FROM %s WHERE sent_status = @p1 ORDER BY mailitem_id DESC"),
			"sysmail_log", "FROM   msdb.dbo.sysmail_log) AS sl", "FROM msdb.dbo.sysmail_event_log WHERE"},
	} {
		ifPart, elsePart, ok := strings.Cut(tc.q, "\nELSE\n")
		if !ok {
			t.Fatalf("%s: no ELSE in %q", tc.name, tc.q)
		}
		pred := "HAS_PERMS_BY_NAME(N'msdb.dbo." + tc.table + "', N'OBJECT', N'SELECT')"
		if !strings.HasPrefix(ifPart, "IF ISNULL(IS_SRVROLEMEMBER(N'sysadmin'), 0) = 0 AND ") || !strings.Contains(ifPart, pred) {
			t.Errorf("%s: predicate = %q; want not sysadmin and %s", tc.name, ifPart, pred)
		}
		if !strings.Contains(ifPart, tc.base) || strings.Contains(elsePart, tc.base) {
			t.Errorf("%s: the base table must be read under the IF only:\n%s", tc.name, tc.q)
		}
		if !strings.Contains(elsePart, tc.view) || strings.Contains(ifPart, tc.view) {
			t.Errorf("%s: the view must be read under the ELSE only:\n%s", tc.name, tc.q)
		}
		for _, part := range []string{ifPart, elsePart} {
			if !strings.Contains(part, "sent_status = @p1 ORDER BY mailitem_id DESC") {
				t.Errorf("%s: a branch lost the filter: %q", tc.name, part)
			}
		}
	}
}
