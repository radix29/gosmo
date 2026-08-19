package gosmo

import (
	"strings"
	"testing"
)

// A date boundary comes back from sys.partition_range_values as text and has
// to be re-quoted, or the generated CREATE doesn't parse; a numeric one must
// not be.
func TestBuildPartitionFunctionScriptQuotesNonNumericBoundaries(t *testing.T) {
	opts := DefaultScriptOptions()

	num := &PartitionFunction{Name: "pfInt", InputType: DataTypeInt, IsRight: true,
		Boundaries: []string{"100", "200"}}
	got := buildPartitionFunctionScript(num, opts)
	if !strings.Contains(got, "AS RANGE RIGHT FOR VALUES (100, 200)") {
		t.Errorf("numeric boundaries wrong:\n%s", got)
	}
	if !strings.Contains(got, "CREATE PARTITION FUNCTION [pfInt] (int)") {
		t.Errorf("input type wrong:\n%s", got)
	}

	dates := &PartitionFunction{Name: "pfDate", InputType: DataTypeDate,
		Boundaries: []string{"2026-01-01", "2026-07-01"}}
	got = buildPartitionFunctionScript(dates, opts)
	if !strings.Contains(got, "AS RANGE LEFT FOR VALUES (N'2026-01-01', N'2026-07-01')") {
		t.Errorf("date boundaries not quoted:\n%s", got)
	}
}

func TestBuildPartitionSchemeScript(t *testing.T) {
	ps := &PartitionScheme{Name: "psTest", FunctionName: "pfInt",
		FileGroups: []string{"PRIMARY", "FG2"}}

	got := buildPartitionSchemeScript(ps, DefaultScriptOptions())
	if !strings.Contains(got, "CREATE PARTITION SCHEME [psTest]") ||
		!strings.Contains(got, "AS PARTITION [pfInt] TO ([PRIMARY], [FG2])") {
		t.Errorf("scheme script wrong:\n%s", got)
	}

	opts := DefaultScriptOptions()
	opts.Verb = ScriptDrop
	if got := buildPartitionSchemeScript(ps, opts); !strings.Contains(got, "DROP PARTITION SCHEME IF EXISTS [psTest]") {
		t.Errorf("scheme drop wrong:\n%s", got)
	}
}

// A disabled policy has to be recreated disabled: STATE = ON would start it
// filtering rows the original wasn't.
func TestBuildSecurityPolicyScriptKeepsItsState(t *testing.T) {
	p := &SecurityPolicy{Name: "pol", Schema: "sec", IsEnabled: false, IsSchemaBound: true,
		Predicates: []*SecurityPredicate{
			{PredicateType: "FILTER", PredicateDefinition: "([sec].[fn]([TenantID]))",
				TargetSchema: "dbo", TargetTable: "Orders"},
			{PredicateType: "BLOCK", PredicateDefinition: "([sec].[fn]([TenantID]))",
				TargetSchema: "dbo", TargetTable: "Orders", Operation: "AFTER INSERT"},
		}}

	got := buildSecurityPolicyScript(p, DefaultScriptOptions())
	for _, want := range []string{
		"CREATE SECURITY POLICY [sec].[pol]",
		// The catalog's outer parentheses are stripped — see unwrapPredicate.
		"ADD FILTER PREDICATE [sec].[fn]([TenantID]) ON [dbo].[Orders]",
		"ADD BLOCK PREDICATE [sec].[fn]([TenantID]) ON [dbo].[Orders] AFTER INSERT",
		"WITH (STATE = OFF, SCHEMABINDING = ON)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	// Two predicates, so exactly one separating comma.
	if n := strings.Count(got, ",\n    ADD "); n != 1 {
		t.Errorf("predicate separators = %d, want 1:\n%s", n, got)
	}
}

// Neither key has an IF EXISTS drop form, so both are guarded by a catalog
// lookup instead.
func TestBuildColumnKeyScriptsGuardTheirDrops(t *testing.T) {
	cmk := &ColumnMasterKey{Name: "CMK1", KeyStoreProviderName: "MSSQL_CERTIFICATE_STORE",
		KeyPath: "CurrentUser/My/ABC"}
	opts := DefaultScriptOptions()
	opts.Verb = ScriptDrop
	got := buildColumnMasterKeyScript(cmk, opts)
	if strings.Contains(got, "DROP COLUMN MASTER KEY IF EXISTS") {
		t.Errorf("DROP COLUMN MASTER KEY has no IF EXISTS form:\n%s", got)
	}
	if !strings.Contains(got, "IF EXISTS (SELECT 1 FROM sys.column_master_keys WHERE name = N'CMK1')") {
		t.Errorf("drop is unguarded:\n%s", got)
	}

	cek := &ColumnEncryptionKey{Name: "CEK1"}
	got = buildColumnEncryptionKeyScript(cek, opts)
	if !strings.Contains(got, "IF EXISTS (SELECT 1 FROM sys.column_encryption_keys WHERE name = N'CEK1')") {
		t.Errorf("drop is unguarded:\n%s", got)
	}
}

// An enclave-enabled master key can only be recreated with its own
// signature, which the server verifies against the rest of the metadata.
func TestBuildColumnMasterKeyScriptCarriesTheEnclaveSignature(t *testing.T) {
	k := &ColumnMasterKey{Name: "CMK1", KeyStoreProviderName: "MSSQL_CERTIFICATE_STORE",
		KeyPath: "CurrentUser/My/ABC", AllowEnclaveComputations: true,
		Signature: []byte{0x0a, 0xff}}
	got := buildColumnMasterKeyScript(k, DefaultScriptOptions())
	if !strings.Contains(got, "ENCLAVE_COMPUTATIONS (SIGNATURE = 0x0AFF)") {
		t.Errorf("signature not scripted:\n%s", got)
	}
	plain := buildColumnMasterKeyScript(&ColumnMasterKey{Name: "CMK2"}, DefaultScriptOptions())
	if strings.Contains(plain, "ENCLAVE_COMPUTATIONS") {
		t.Errorf("non-enclave key got an ENCLAVE_COMPUTATIONS clause:\n%s", plain)
	}
}

// A key mid-rotation is encrypted under two master keys and the CREATE has
// to restate both values.
func TestBuildColumnEncryptionKeyScriptRestatesEveryValue(t *testing.T) {
	k := &ColumnEncryptionKey{Name: "CEK1", Values: []*ColumnEncryptionKeyValue{
		{MasterKeyName: "CMK1", EncryptionAlgorithm: "RSA_OAEP", EncryptedValue: []byte{0x01}},
		{MasterKeyName: "CMK2", EncryptionAlgorithm: "RSA_OAEP", EncryptedValue: []byte{0x02}},
	}}
	got := buildColumnEncryptionKeyScript(k, DefaultScriptOptions())
	if n := strings.Count(got, "COLUMN_MASTER_KEY = "); n != 2 {
		t.Errorf("values scripted = %d, want 2:\n%s", n, got)
	}
	if !strings.Contains(got, "ENCRYPTED_VALUE = 0x01") || !strings.Contains(got, "ENCRYPTED_VALUE = 0x02") {
		t.Errorf("encrypted values wrong:\n%s", got)
	}
}

// sys.security_predicates reports a predicate wrapped in parentheses, which
// ADD FILTER PREDICATE rejects — see unwrapPredicate.
func TestUnwrapPredicate(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"([sec].[fn]([TenantID]))", "[sec].[fn]([TenantID])"},
		{"[sec].[fn]([TenantID])", "[sec].[fn]([TenantID])"},
		// Starts and ends with a paren, but not the same pair.
		{"(a) OR (b)", "(a) OR (b)"},
		{"", ""},
	} {
		if got := unwrapPredicate(tt.in); got != tt.want {
			t.Errorf("unwrapPredicate(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
