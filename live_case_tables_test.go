//go:build livedb

// The generator of collation_case_tables.go, and the check that NameKey
// pairs letters as the server does under every collation it lists.
// Read-only: temporary tables only.
//
//	go test -tags livedb . -run TestLiveCaseTables -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Regenerate against the newest server (its collation list is a superset),
// then run again without the flag to check the new tables:
//
//	go test -tags livedb . -run TestLiveCaseTables -update-case-tables -livedb '<dsn>'
package gosmo

import (
	"bytes"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"go/format"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

var updateCaseTables = flag.Bool("update-case-tables", false, "rewrite collation_case_tables.go from the -livedb server")

type runePair struct{ a, b rune }

func TestLiveCaseTables(t *testing.T) {
	skipSlowInShort(t)
	db, ctx, done := liveDBTimeout(t, 30*time.Minute)
	t.Cleanup(done)
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	colls := liveCaseCollations(t, ctx, conn)
	pairs := liveCasePairs(t, ctx, conn, colls)
	if !*updateCaseTables {
		// Every pair the compiled tables join, so a group the server does
		// not hold together (a non-transitive pairing) shows.
		for _, f := range caseFolds() {
			byKey := map[rune][]rune{}
			for r := range rune(0x80) {
				byKey[f.ascii[r]] = append(byKey[f.ascii[r]], r)
			}
			for r, k := range f.other {
				byKey[k] = append(byKey[k], r)
			}
			for k, rs := range byKey {
				if !slices.Contains(rs, k) {
					rs = append(rs, k)
				}
				for i := range rs {
					for j := i + 1; j < len(rs); j++ {
						pairs[runePair{min(rs[i], rs[j]), max(rs[i], rs[j])}] = true
					}
				}
			}
		}
	}
	list := slices.SortedFunc(maps.Keys(pairs), func(x, y runePair) int {
		if x.a != y.a {
			return int(x.a - y.a)
		}
		return int(x.b - y.b)
	})
	liveLoadCasePairs(t, ctx, conn, list)
	equal := liveCaseEqualities(t, ctx, conn, colls, len(list))
	t.Logf("%d collations, %d letter pairs", len(colls), len(list))

	if *updateCaseTables {
		src := caseTablesSource(t, ctx, conn, colls, list, equal)
		if err := os.WriteFile("collation_case_tables.go", src, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("wrote collation_case_tables.go; run again without -update-case-tables to check it")
		return
	}
	bad := 0
	for _, c := range colls {
		if caseFoldOf(c) == nil {
			t.Errorf("collation %s is not in collation_case_tables.go; regenerate it", c)
			continue
		}
		for i, p := range list {
			if got := SameName(c, string(p.a), string(p.b)); got != equal[c][i] {
				if bad++; bad <= 20 {
					t.Errorf("SameName(%s, %U, %U) = %v, server says %v", c, p.a, p.b, got, equal[c][i])
				}
			}
		}
	}
	if bad > 20 {
		t.Errorf("... %d disagreements in all", bad)
	}
}

// liveCaseCollations is every case-insensitive, accent-sensitive collation
// the server lists. An _AI one keys as its _AS sibling (caseClassKeys drops
// both), whose pairing is the case one.
func liveCaseCollations(t *testing.T, ctx context.Context, conn *sql.Conn) []string {
	t.Helper()
	rows, err := conn.QueryContext(ctx, "SELECT name FROM sys.fn_helpcollations() ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var colls []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		if !bareCollation.MatchString(n) {
			t.Fatalf("collation name %q is not a bare identifier", n)
		}
		if CollationIgnoresCase(n) && !accentInsensitive(n) {
			colls = append(colls, n)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return colls
}

// liveCasePairs is every pair of runes either Unicode simple folding joins or
// the server's UPPER or LOWER maps one onto the other under some collation
// (one per base: Turkish i/İ is no Unicode pair).
func liveCasePairs(t *testing.T, ctx context.Context, conn *sql.Conn, colls []string) map[runePair]bool {
	t.Helper()
	pairs := map[runePair]bool{}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f > r {
				pairs[runePair{r, f}] = true
			}
		}
	}
	// #cp holds every code point as nvarchar: a CASE over NCHAR() alone
	// would type it nchar(2) and pad each BMP letter with a space.
	if _, err := conn.ExecContext(ctx, `
CREATE TABLE #cp (n int PRIMARY KEY, c nvarchar(2) NOT NULL);
WITH d AS (SELECT TOP (1114112) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) - 1 AS n
           FROM sys.all_columns a CROSS JOIN sys.all_columns b)
INSERT #cp SELECT n, CASE WHEN n < 65536 THEN CAST(NCHAR(n) AS nvarchar(2))
                     ELSE CAST(NCHAR(55296 + (n - 65536) / 1024) + NCHAR(56320 + (n - 65536) % 1024) AS nvarchar(2)) END
FROM d WHERE n < 55296 OR n > 57343`); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range colls {
		keys := caseClassKeys(c)
		if base := keys[len(keys)-1]; seen[base] {
			continue
		} else {
			seen[base] = true
		}
		q := fmt.Sprintf(`SELECT n, UPPER(c COLLATE %[1]s), LOWER(c COLLATE %[1]s) FROM #cp
WHERE CAST(UPPER(c COLLATE %[1]s) AS varbinary(8)) <> CAST(c AS varbinary(8))
   OR CAST(LOWER(c COLLATE %[1]s) AS varbinary(8)) <> CAST(c AS varbinary(8))`, c)
		rows, err := conn.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", c, err)
		}
		for rows.Next() {
			var n int
			var up, low sql.NullString
			if err := rows.Scan(&n, &up, &low); err != nil {
				t.Fatal(err)
			}
			for _, s := range []sql.NullString{up, low} {
				if !s.Valid || utf8.RuneCountInString(s.String) != 1 {
					continue
				}
				if r, _ := utf8.DecodeRuneInString(s.String); r != rune(n) && r != utf8.RuneError {
					pairs[runePair{min(r, rune(n)), max(r, rune(n))}] = true
				}
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	return pairs
}

func liveLoadCasePairs(t *testing.T, ctx context.Context, conn *sql.Conn, list []runePair) {
	t.Helper()
	if _, err := conn.ExecContext(ctx, "CREATE TABLE #p (i int PRIMARY KEY, a nvarchar(2) NOT NULL, b nvarchar(2) NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	const batch = 600 // three parameters a row, under the 2100 limit
	for lo := 0; lo < len(list); lo += batch {
		var sb strings.Builder
		var args []any
		sb.WriteString("INSERT #p VALUES ")
		for i := lo; i < min(lo+batch, len(list)); i++ {
			if i > lo {
				sb.WriteString(", ")
			}
			n := len(args)
			fmt.Fprintf(&sb, "(@p%d, @p%d, @p%d)", n+1, n+2, n+3)
			args = append(args, i, string(list[i].a), string(list[i].b))
		}
		if _, err := conn.ExecContext(ctx, sb.String(), args...); err != nil {
			t.Fatal(err)
		}
	}
}

// liveCaseEqualities is, per collation, whether the server's = joins each
// pair of #p.
func liveCaseEqualities(t *testing.T, ctx context.Context, conn *sql.Conn, colls []string, n int) map[string][]bool {
	t.Helper()
	equal := map[string][]bool{}
	for _, c := range colls {
		eq := make([]bool, n)
		rows, err := conn.QueryContext(ctx, "SELECT i FROM #p WHERE a = b COLLATE "+c)
		if err != nil {
			t.Fatalf("%s: %v", c, err)
		}
		for rows.Next() {
			var i int
			if err := rows.Scan(&i); err != nil {
				t.Fatal(err)
			}
			eq[i] = true
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		equal[c] = eq
	}
	return equal
}

// caseTablesSource is collation_case_tables.go: the case classes (distinct
// pairings, the commonest first) as groups of runes, and each collation's
// class under the least specific caseClassKeys key that tells its siblings
// apart.
func caseTablesSource(t *testing.T, ctx context.Context, conn *sql.Conn, colls []string, list []runePair, equal map[string][]bool) []byte {
	t.Helper()
	sig := func(c string) string {
		var b strings.Builder
		for _, e := range equal[c] {
			b.WriteByte("01"[btoi(e)])
		}
		return b.String()
	}
	count := map[string]int{}
	for _, c := range colls {
		count[sig(c)]++
	}
	sigs := slices.SortedFunc(maps.Keys(count), func(x, y string) int {
		if count[x] != count[y] {
			return count[y] - count[x]
		}
		return strings.Compare(x, y)
	})
	class := map[string]int{}
	for i, s := range sigs {
		class[s] = i
	}
	if len(sigs) > 255 {
		t.Fatalf("%d case classes do not fit a uint8", len(sigs))
	}

	// Collations by base; each base at the first level its collations agree.
	byBase := map[string][]string{}
	for _, c := range colls {
		keys := caseClassKeys(c)
		byBase[keys[len(keys)-1]] = append(byBase[keys[len(keys)-1]], c)
	}
	entries := map[string]int{}
	for base, cs := range byBase {
		levels := []func(string) string{
			func(string) string { return base },
			func(c string) string {
				if strings.Contains("_"+strings.ToUpper(c)+"_", "_WS_") {
					return base + "_WS"
				}
				return base
			},
			func(c string) string { return caseClassKeys(c)[0] },
		}
		placed := false
		for _, level := range levels {
			m := map[string]int{}
			ok := true
			for _, c := range cs {
				k := level(c)
				if prev, seen := m[k]; seen && prev != class[sig(c)] {
					ok = false
					break
				}
				m[k] = class[sig(c)]
			}
			if ok {
				maps.Copy(entries, m)
				placed = true
				break
			}
		}
		if !placed {
			t.Fatalf("collations of base %s differ in case pairing beyond their KS/WS/VSS/SC/UTF8 options: %v", base, cs)
		}
	}

	// Each class's groups: union-find over the pairs it joins.
	groupsOf := func(s string) map[rune][]rune {
		parent := map[rune]rune{}
		var find func(r rune) rune
		find = func(r rune) rune {
			if p, ok := parent[r]; ok && p != r {
				root := find(p)
				parent[r] = root
				return root
			}
			return r
		}
		for i, p := range list {
			parent[p.a], parent[p.b] = find(p.a), find(p.b)
			if s[i] == '1' {
				ra, rb := find(p.a), find(p.b)
				if ra != rb {
					parent[max(ra, rb)] = min(ra, rb)
				}
			}
		}
		members := map[rune][]rune{}
		for r := range parent {
			root := find(r)
			members[root] = append(members[root], r)
		}
		g := map[rune][]rune{}
		for _, rs := range members {
			slices.Sort(rs)
			for _, r := range rs {
				g[r] = rs
			}
		}
		return g
	}
	render := func(rs []rune) string { return fmt.Sprintf("%+q", string(rs)) }
	base := groupsOf(sigs[0])
	var classGroups [][]string
	for i, s := range sigs {
		g := groupsOf(s)
		set := map[string]bool{}
		for r, rs := range g {
			if i == 0 {
				if len(rs) > 1 {
					set[render(rs)] = true
				}
			} else if !slices.Equal(rs, base[r]) {
				set[render(rs)] = true
			}
		}
		classGroups = append(classGroups, slices.SortedFunc(maps.Keys(set), func(x, y string) int {
			return cmpRuneLiteral(x, y)
		}))
	}

	var version string
	if err := conn.QueryRowContext(ctx, "SELECT CAST(SERVERPROPERTY('ProductVersion') AS nvarchar(128))").Scan(&version); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by TestLiveCaseTables (-update-case-tables) against SQL Server %s; DO NOT EDIT.\n\n", version)
	b.WriteString("package gosmo\n\n")
	b.WriteString("// caseClassByCollation is each collation's case class (an index into\n// caseClassGroups), under the caseClassKeys key that tells it from its\n// siblings.\n")
	b.WriteString("var caseClassByCollation = map[string]uint8{\n")
	for _, k := range slices.Sorted(maps.Keys(entries)) {
		fmt.Fprintf(&b, "\t%q: %d,\n", k, entries[k])
	}
	b.WriteString("}\n\n")
	b.WriteString("// caseClassGroups is, per case class, the groups of runes the class pairs\n// ignoring case, each written smallest rune first: class 0 (the commonest)\n// in full, every other class only the groups where it differs from class 0.\n")
	b.WriteString("var caseClassGroups = [][]string{\n")
	for i, gs := range classGroups {
		var ex []string
		for _, c := range colls {
			if class[sig(c)] == i && len(ex) < 3 {
				ex = append(ex, c)
			}
		}
		fmt.Fprintf(&b, "\t// %d: %d collations, e.g. %s\n\t{", i, count[sigs[i]], strings.Join(ex, ", "))
		for j, g := range gs {
			if j%8 == 0 {
				b.WriteString("\n\t\t")
			} else {
				b.WriteString(" ")
			}
			b.WriteString(g + ",")
		}
		b.WriteString("\n\t},\n")
	}
	b.WriteString("}\n")
	src, err := format.Source(b.Bytes())
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	return src
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// cmpRuneLiteral orders two rendered groups by their first rune.
func cmpRuneLiteral(x, y string) int {
	a, _ := strconv.Unquote(x)
	b, _ := strconv.Unquote(y)
	ra, _ := utf8.DecodeRuneInString(a)
	rb, _ := utf8.DecodeRuneInString(b)
	if ra != rb {
		return int(ra - rb)
	}
	return strings.Compare(a, b)
}
