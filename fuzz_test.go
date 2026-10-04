package gosmo

import (
	"strings"
	"testing"
)

// FuzzNames drives the name and script-text helpers with arbitrary text:
// every catalog name reaches QuoteName and NameKey, and every statement
// captured under WithScript reaches bindScriptArgs. The seeds — the shapes
// the example tests pin — run in plain go test; fuzzing is on demand:
//
//	go test -run XXX -fuzz FuzzNames -fuzztime 60s -fuzzminimizetime 0
func FuzzNames(f *testing.F) {
	for _, s := range []string{
		"", "dbo", "a]b", "[a]]b]", `"a""b"`, "[a].[b]", "[a]b]", "User", "_x$1",
		"@v", "#t", "ſ", "ς", "σ", "İ", "ａ", "\xff",
		"SELECT @p1, '@p2' -- @p2\n/* @p1 */ [@p1] \"@p2\"",
		"@p0 @p3 @p10 @p", "'it''s @p1'", "/* /* @p2 */ */ @p2",
		"CREATE PROCEDURE p AS SELECT 1",
		"/* a /* b */ c */\n-- x\n  create\tview v as select 1",
		"CREATE OR ALTER VIEW v AS SELECT 1", "'CREATE' VIEW", "CREATEVIEW",
		`host\inst,1433`, "tcp:x.database.windows.net,1433", "[::1]:1433", "::1",
		"admin:1433", "np:pipe", "h,-1", `\`, ",", ":",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if got := UnquoteName(QuoteName(s)); got != s {
			t.Fatalf("UnquoteName(QuoteName(%q)) = %q", s, got)
		}
		if q := QuoteNameIfNeeded(s); q != s && q != QuoteName(s) {
			t.Fatalf("QuoteNameIfNeeded(%q) = %q: neither bare nor QuoteName", s, q)
		}
		UnquoteName(s)

		key := NameKey("", s)
		if NameKey("", key) != key {
			t.Fatalf("NameKey(%q) = %q is not idempotent", s, key)
		}
		if !strings.EqualFold(s, key) {
			t.Fatalf("NameKey(%q) = %q: not EqualFold to its name", s, key)
		}
		for _, other := range []string{strings.ToUpper(s), strings.ToLower(s), "x" + s} {
			if (NameKey("", other) == key) != strings.EqualFold(other, s) {
				t.Fatalf("NameKey and EqualFold disagree on %q and %q", s, other)
			}
		}
		if NameKey("Latin1_General_CS_AS", s) != s {
			t.Fatalf("NameKey on a case-sensitive collation changed %q", s)
		}

		if got, err := bindScriptArgs(s, nil); err != nil || got != s {
			t.Fatalf("bindScriptArgs(%q, nil) = %q, %v", s, got, err)
		}
		bindScriptArgs(s, []any{int64(1), s})

		if got := alterModuleDefinition(s); got != s {
			at := 0
			for at < len(s) && s[at] == got[at] {
				at++
			}
			if !strings.EqualFold(s[at:min(at+6, len(s))], "CREATE") || got != s[:at]+"ALTER"+s[at+6:] {
				t.Fatalf("alterModuleDefinition(%q) = %q: not CREATE → ALTER", s, got)
			}
		}

		host, instance, _ := ParseServerAddress(s)
		if !strings.Contains(s, host) || !strings.Contains(s, instance) {
			t.Fatalf("ParseServerAddress(%q) = %q, %q: not parts of the address", s, host, instance)
		}
	})
}
