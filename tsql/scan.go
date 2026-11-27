package tsql

import (
	"iter"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SpanKind says what one run of T-SQL text is to the server's lexer.
type SpanKind uint8

const (
	SpanCode         SpanKind = iota // anything not below
	SpanLineComment                  // -- to the end of the line
	SpanBlockComment                 // /* … */, nested
	SpanLiteral                      // '…'
	SpanNLiteral                     // N'…'
	SpanBracketIdent                 // […]
	SpanQuotedIdent                  // "…"
)

// Span is one run of text from Spans: s[Start:End] is the whole of it,
// delimiters and an N prefix included. Closed is false for a quoted span or
// block comment that ran to the end of the text unterminated.
type Span struct {
	Kind       SpanKind
	Start, End int
	Closed     bool
}

// Spans splits s into the runs the server lexes differently: code, the
// two comment forms, the two literal forms and the two quoted-identifier
// forms. The spans tile s in order with no gaps; code spans are never empty.
//
// It is the one lexical walk behind every rewrite of script text in gosmo —
// binding @pN arguments, redacting secrets, reading a module's CREATE header,
// unwrapping a security predicate — so a rule fixed here is fixed for all of
// them. It scans, it does not parse; the rules:
//
//   - A doubled closer escapes itself in each quoted form: two quotes in a
//     literal or quoted identifier, ]] in a bracketed one. Without the last,
//     "[a]]'b]" ends the identifier early and reads "'b]…" as a literal.
//   - Block comments nest, so they are counted, not matched: the first */ of
//     "/* a /* b */ c */" does not end it.
//   - A -- comment ends before its first CR or LF, or runs to the end of
//     the text when there is none; it is closed either way. SQL Server ends
//     the comment at a lone CR too ("SELECT 1 -- c<CR>SELECT 2" returns two
//     result sets), so text after a bare CR is code, not comment. A quoted span or block
//     comment left open runs to the end of the text with Closed false.
//   - An N (either case) opens a Unicode literal only as a word of its own,
//     not as the last letter of one: "N'x'" is a literal, "IN'x'" is the
//     word IN and then one. A word is a run of isIdentByte, so a non-ASCII
//     letter does not join it.
//   - A number's exponent takes a sign even with no digits after it, so the
//     first '-' of "1e--5" is the number's and the second a minus: the
//     server returns -4, and runs whatever follows on the line, where a scan
//     that saw "--" there read a comment. A number starts at an ASCII digit
//     that is not inside a word (numberEnd).
func Spans(s string) iter.Seq[Span] {
	return func(yield func(Span) bool) {
		code := 0 // where the pending code span starts
		for i := 0; i < len(s); {
			var sp Span
			switch c := s[i]; {
			case c == '\'':
				sp = scanDelimited(s, i, i, SpanLiteral, '\'')
			case (c == 'N' || c == 'n') && i+1 < len(s) && s[i+1] == '\'' && (i == 0 || !isIdentByte(s[i-1])):
				sp = scanDelimited(s, i, i+1, SpanNLiteral, '\'')
			case c == '[':
				sp = scanDelimited(s, i, i, SpanBracketIdent, ']')
			case c == '"':
				sp = scanDelimited(s, i, i, SpanQuotedIdent, '"')
			case '0' <= c && c <= '9' && !afterWordRune(s, i):
				i = numberEnd(s, i)
				continue
			case strings.HasPrefix(s[i:], "--"):
				sp = Span{Kind: SpanLineComment, Start: i, End: len(s), Closed: true}
				if j := strings.IndexAny(s[i:], "\r\n"); j >= 0 {
					sp.End = i + j
				}
			case strings.HasPrefix(s[i:], "/*"):
				depth, j := 1, i+2
				for j < len(s) && depth > 0 {
					switch {
					case strings.HasPrefix(s[j:], "/*"):
						depth, j = depth+1, j+2
					case strings.HasPrefix(s[j:], "*/"):
						depth, j = depth-1, j+2
					default:
						j++
					}
				}
				sp = Span{Kind: SpanBlockComment, Start: i, End: j, Closed: depth == 0}
			default:
				i++
				continue
			}
			if code < sp.Start && !yield(Span{Kind: SpanCode, Start: code, End: sp.Start, Closed: true}) {
				return
			}
			if !yield(sp) {
				return
			}
			code, i = sp.End, sp.End
		}
		if code < len(s) {
			yield(Span{Kind: SpanCode, Start: code, End: len(s), Closed: true})
		}
	}
}

// scanDelimited reads the quoted span that starts at s[start] (an N prefix,
// or the delimiter itself) and opens at s[open], up to the closer that ends
// it. A doubled closer is an escaped one.
func scanDelimited(s string, start, open int, kind SpanKind, closer byte) Span {
	for j := open + 1; j < len(s); j++ {
		if s[j] != closer {
			continue
		}
		if j+1 < len(s) && s[j+1] == closer {
			j++
			continue
		}
		return Span{Kind: kind, Start: start, End: j + 1, Closed: true}
	}
	return Span{Kind: kind, Start: start, End: len(s)}
}

// IsQuoted reports whether sp is a literal or a quoted identifier.
func (sp Span) IsQuoted() bool {
	return sp.Kind >= SpanLiteral
}

// IsLiteral reports whether sp is a string literal, with or without N.
func (sp Span) IsLiteral() bool {
	return sp.Kind == SpanLiteral || sp.Kind == SpanNLiteral
}

// IsComment reports whether sp is a comment of either form.
func (sp Span) IsComment() bool {
	return sp.Kind == SpanLineComment || sp.Kind == SpanBlockComment
}

// Unescape returns the value of a quoted span of s — its text without the
// delimiters or N prefix, each doubled closer made single — and any other
// span's text as it stands. An unterminated span's value runs to the end.
func (sp Span) Unescape(s string) string {
	from, closer := sp.Start+1, "'"
	switch sp.Kind {
	case SpanLiteral:
	case SpanNLiteral:
		from++
	case SpanBracketIdent:
		closer = "]"
	case SpanQuotedIdent:
		closer = `"`
	default:
		return s[sp.Start:sp.End]
	}
	to := sp.End
	if sp.Closed {
		to--
	}
	return strings.ReplaceAll(s[from:to], closer+closer, closer)
}

// afterWordRune reports whether the rune before s[i] can continue a T-SQL
// word: a letter or digit, '_', '$', '#' or '@'. A digit there is part of
// that word, not the start of a number; after a leading '$' it is money,
// which takes no exponent, so it needs no skipping either.
func afterWordRune(s string, i int) bool {
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return r == '_' || r == '$' || r == '#' || r == '@' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// numberEnd returns the end of the number starting at the ASCII digit s[i],
// as SQL Server lexes it: 0x and hex digits, or digits, an optional '.' and
// digits, then an optional exponent, e or E, an optional sign and optional
// digits ("1e" is 1.0). It is the same rule as gossms's sqltext lexer, which
// FuzzLexersAgree there holds Spans to.
func numberEnd(s string, i int) int {
	digits := func(j int, hex bool) int {
		for j < len(s) && ('0' <= s[j] && s[j] <= '9' || hex && 'a' <= s[j]|0x20 && s[j]|0x20 <= 'f') {
			j++
		}
		return j
	}
	if s[i] == '0' && i+1 < len(s) && s[i+1]|0x20 == 'x' {
		return digits(i+2, true)
	}
	j := digits(i+1, false)
	if j < len(s) && s[j] == '.' {
		j = digits(j+1, false)
	}
	if j >= len(s) || s[j]|0x20 != 'e' {
		return j
	}
	j++
	if j < len(s) && (s[j] == '+' || s[j] == '-') {
		j++
	}
	return digits(j, false)
}

// isIdentByte reports whether c can be part of an unquoted T-SQL word, as
// far as an ASCII scan can tell.
func isIdentByte(c byte) bool {
	return c == '_' || c == '@' || c == '#' || c == '$' ||
		'0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}
