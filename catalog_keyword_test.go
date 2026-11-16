package gosmo

import (
	"strings"
	"testing"
)

// A catalog value outside its clause's keyword set reaches the script as a
// placeholder the server refuses, never as the text itself: the injected
// value below would otherwise end the statement and run its own.
func TestAnUnrecognizedCatalogValueBecomesAPlaceholder(t *testing.T) {
	const evil = "AUTO); DROP TABLE x; --"
	for _, tc := range []struct {
		name, got, want string
	}{
		{"keyword, other case", scriptKeyword("lock escalation", "auto", "AUTO", "DISABLE"), "AUTO"},
		{"keyword", scriptKeyword("lock escalation", evil, "AUTO", "DISABLE"), "<unrecognized lock escalation>"},
		{"lock escalation", lockEscalationStatement(tableScriptOptions{LockEscalation: evil}, "[dbo].[T]"),
			"ALTER TABLE [dbo].[T] SET (LOCK_ESCALATION = <unrecognized lock escalation>);\nGO\n\n"},
		{"retention unit, no plural", systemVersioningOption(tableScriptOptions{HistoryRetentionPeriod: 3, HistoryRetentionUnit: evil}),
			"SYSTEM_VERSIONING = ON (HISTORY_RETENTION_PERIOD = 3 <unrecognized history retention unit>)"},
		{"auth method", endpointAuthClause("NTLM, "+evil, ""), "WINDOWS NTLM WINDOWS <unrecognized Windows authentication method>"},
		{"algorithm, comma desc", endpointEncryptionClause(true, "AES, RC4"), "REQUIRED ALGORITHM AES RC4"},
		{"algorithm", endpointEncryptionClause(true, evil), "REQUIRED ALGORITHM <unrecognized encryption algorithm>"},
		{"compression", strings.Join(compressionOptions([]PartitionOptions{{DataCompression: evil}}, func(DataCompression) bool { return true }), ","),
			"DATA_COMPRESSION = <unrecognized data compression>"},
		{"integer", scriptInteger("sequence increment", "-5"), "-5"},
		{"integer", scriptInteger("sequence increment", "1\n-- x"), "<unrecognized sequence increment>"},
		{"object name", scriptObjectName("base object name", "[db].[dbo].[t]"), "[db].[dbo].[t]"},
		{"object name", scriptObjectName("base object name", "[t]; DROP TABLE x"), "<unrecognized base object name>"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}
