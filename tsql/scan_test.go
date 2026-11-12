package tsql

import (
	"slices"
	"strings"
	"testing"
)

// spanString renders Spans(s) as kind:text pairs, a ! after an
// unterminated one, for table comparison.
func spanString(s string) string {
	names := [...]string{"code", "line", "block", "lit", "nlit", "bracket", "quoted"}
	var parts []string
	for sp := range Spans(s) {
		p := names[sp.Kind] + ":" + s[sp.Start:sp.End]
		if !sp.Closed {
			p += "!"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " | ")
}

func TestSpans(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"SELECT 1", "code:SELECT 1"},
		{"a 'b' c", "code:a  | lit:'b' | code: c"},
		{"'it''s'", "lit:'it''s'"},
		{"N'x' n'y'", "nlit:N'x' | code:  | nlit:n'y'"},
		{"IN'x'", "code:IN | lit:'x'"},
		{"(N'x')", "code:( | nlit:N'x' | code:)"},
		{"[a]]'b] x", "bracket:[a]]'b] | code: x"},
		{`"a""b" x`, `quoted:"a""b" | code: x`},
		{"a -- c\nb", "code:a  | line:-- c | code:\nb"},
		{"a -- c", "code:a  | line:-- c"},
		{"/* a /* b */ c */x", "block:/* a /* b */ c */ | code:x"},
		{"/* a /* b */", "block:/* a /* b */!"},
		{"'open", "lit:'open!"},
		{"[open]]", "bracket:[open]]!"},
		{"N'", "nlit:N'!"},
		{"-- '\n'--'", "line:-- ' | code:\n | lit:'--'"},
		// SQL Server ends a -- comment at a lone CR as well as at LF.
		{"-- a\rSELECT 1", "line:-- a | code:\rSELECT 1"},
		{"-- a\r\nb", "line:-- a | code:\r\nb"},
		{"SELECT 1 -- c\rDROP TABLE t", "code:SELECT 1  | line:-- c | code:\rDROP TABLE t"},
		{"'/*' */", "lit:'/*' | code: */"},
	} {
		if got := spanString(tc.in); got != tc.want {
			t.Errorf("Spans(%q)\n got %s\nwant %s", tc.in, got, tc.want)
		}
	}
}

func TestSpanUnescape(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"'it''s'", "it's"},
		{"N'it''s'", "it's"},
		{"[a]]b]", "a]b"},
		{`"a""b"`, `a"b`},
		{"'open''", "open'"},
		{"N'", ""},
		{"'", ""},
		{"-- x", "-- x"},
	} {
		sp := Span{}
		for sp = range Spans(tc.in) {
			break
		}
		if got := sp.Unescape(tc.in); got != tc.want {
			t.Errorf("Unescape(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// FuzzSpans checks the scanner against the gosmo walks it replaced: the
// bytes it puts outside code are the bytes scriptCodeSpans skipped (less the
// N of an N'…', which that walk left as code), and its literal values are
// the ones redactSecrets read with scanQuoted. legacySkipped and
// legacyLiterals are those walks as they stood, kept here as the oracle,
// with one rule changed in both since: a -- comment ends at CR as well as LF.
//
//	go test -run XXX -fuzz FuzzSpans -fuzztime 60s -fuzzminimizetime 0
func FuzzSpans(f *testing.F) {
	for _, s := range []string{
		"", "'a''b'", "N'x'", "IN'x'", "éN'x'", "[a]]'b]", `"a""b"`, "-- x", "-- x\ny",
		"-- x\ry", "-- x\r\ny", "/* /* */ */", "/* /*", "'open", "[open", `"open`, "SELECT @p1, '@p2' -- @p2\n/* @p1 */ [@p1] \"@p2\"",
		"([sec].[fn)x]([c], N')'))",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		var skipped []bool
		var lits []string
		at := 0
		for sp := range Spans(s) {
			if sp.Start != at || sp.End <= sp.Start || sp.End > len(s) {
				t.Fatalf("%q: span %+v does not follow %d", s, sp, at)
			}
			if sp.Kind == SpanCode && !sp.Closed {
				t.Fatalf("%q: open code span %+v", s, sp)
			}
			at = sp.End
			for i := sp.Start; i < sp.End; i++ {
				skipped = append(skipped, sp.Kind != SpanCode && !(sp.Kind == SpanNLiteral && i == sp.Start))
			}
			if sp.IsLiteral() {
				lits = append(lits, sp.Unescape(s))
			}
		}
		if at != len(s) {
			t.Fatalf("%q: spans end at %d", s, at)
		}
		if want := legacySkipped(s); !slices.Equal(skipped, want) {
			t.Fatalf("%q: skipped bytes\n got %v\nwant %v", s, skipped, want)
		}
		if want := legacyLiterals(s); !slices.Equal(lits, want) {
			t.Fatalf("%q: literals %q, want %q", s, lits, want)
		}
	})
}

// legacySkipped is scriptCodeSpans as it stood before Spans, reduced to
// which bytes of q it skipped.
func legacySkipped(q string) []bool {
	skipped := make([]bool, len(q))
	for i := 0; i < len(q); {
		var j int
		switch {
		case q[i] == '\'' || q[i] == '"' || q[i] == '[':
			closer := q[i]
			if closer == '[' {
				closer = ']'
			}
			j = i + 1
			for j < len(q) && (q[j] != closer || closer == ']' && j+1 < len(q) && q[j+1] == ']') {
				if q[j] == closer {
					j++
				}
				j++
			}
			if j < len(q) {
				j++
			}
		case strings.HasPrefix(q[i:], "--"):
			j = len(q)
			if k := strings.IndexAny(q[i:], "\r\n"); k >= 0 {
				j = i + k
			}
		case strings.HasPrefix(q[i:], "/*"):
			depth := 1
			for j = i + 2; j < len(q) && depth > 0; {
				switch {
				case strings.HasPrefix(q[j:], "/*"):
					depth, j = depth+1, j+2
				case strings.HasPrefix(q[j:], "*/"):
					depth, j = depth-1, j+2
				default:
					j++
				}
			}
		default:
			i++
			continue
		}
		for k := i; k < j; k++ {
			skipped[k] = true
		}
		i = j
	}
	return skipped
}

// legacyLiterals is redactSecrets' walk as it stood before Spans,
// returning the value of every literal it read with scanQuoted.
func legacyLiterals(stmt string) []string {
	var lits []string
	scanQuoted := func(open int, closer byte) (string, int) {
		var val strings.Builder
		for j := open + 1; j < len(stmt); j++ {
			if stmt[j] == closer {
				if j+1 < len(stmt) && stmt[j+1] == closer {
					val.WriteByte(closer)
					j++
					continue
				}
				return val.String(), j + 1
			}
			val.WriteByte(stmt[j])
		}
		return val.String(), len(stmt)
	}
	literal := func(quote int) int {
		v, end := scanQuoted(quote, '\'')
		lits = append(lits, v)
		return end
	}
	for i := 0; i < len(stmt); {
		switch c := stmt[i]; {
		case c == '\'':
			i = literal(i)
		case isIdentByte(c):
			j := i
			for j < len(stmt) && isIdentByte(stmt[j]) {
				j++
			}
			if j == i+1 && (c == 'N' || c == 'n') && j < len(stmt) && stmt[j] == '\'' {
				j = literal(j)
			}
			i = j
		case c == '[':
			_, i = scanQuoted(i, ']')
		case c == '"':
			_, i = scanQuoted(i, '"')
		case strings.HasPrefix(stmt[i:], "--"):
			if j := strings.IndexAny(stmt[i:], "\r\n"); j >= 0 {
				i += j
			} else {
				i = len(stmt)
			}
		case strings.HasPrefix(stmt[i:], "/*"):
			depth, j := 1, i+2
			for j < len(stmt) && depth > 0 {
				switch {
				case strings.HasPrefix(stmt[j:], "/*"):
					depth, j = depth+1, j+2
				case strings.HasPrefix(stmt[j:], "*/"):
					depth, j = depth-1, j+2
				default:
					j++
				}
			}
			i = j
		default:
			i++
		}
	}
	return lits
}
