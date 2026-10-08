package gosmo

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// G11 (review plan 2026-10-08): a call refused for its own arguments wraps
// ErrInvalidRequest, so a caller can tell "fix the request" from "the server
// said no" with errors.Is rather than by matching text. invalidf builds every
// such error.
//
// This keeps the rule from eroding: every fmt.Errorf whose format has no %w —
// an error that wraps nothing, so one gosmo decided on its own — must be on
// this list, which is the refusals that are about the server's state or
// gosmo's own limits rather than the caller's arguments. A new argument check
// written with fmt.Errorf fails here; write it with invalidf (or unsupportedf,
// when the request is fine and gosmo has no form for it).
func TestEveryArgumentRefusalWrapsErrInvalidRequest(t *testing.T) {
	// File → the start of each allowed format string.
	allowed := map[string][]string{
		"catalog.go":           {"result set %d of %d is missing"},
		"certificate.go":       {"gosmo: encode certificate %q in %q: CERTENCODED returned nothing"},
		"connection_dsn.go":    {"gosmo: connection string: cannot mask an unparseable DSN"},
		"database_mail.go":     {"gosmo: read mail status: unexpected status"},
		"database_snapshot.go": {"gosmo: restore from snapshot %q: its source database is gone"},
		"database_users.go":    {"gosmo: create user %q: a user with a password needs a contained database"},
		"entra.go":             {"gosmo: no Entra credential mapping for fedauth"},
		"errors.go":            {"", ""}, // invalidf and unsupportedf themselves
		"executionplan.go":     {"gosmo: capture execution plan: no plan was returned"},
		"extended_events_read.go": {
			"gosmo: %s: the server reports no error-log directory",
			"gosmo: event session %q has no event_file target",
			"gosmo: event session %q: event_file target has no filename",
		},
		"helpers.go":          {"a result set names database %q, which was not asked for"},
		"index_management.go": {"gosmo: partition %d listed at position %d", "not supported for a %s index"},
		"login.go":            {"gosmo: login %q is not mapped to database %q"},
		"scripter_index.go":   {"partition scheme %s has no partitioning column"},
		"scripter_module.go": {
			"gosmo: script %s %s: definition is not readable (encrypted)",
			"gosmo: script database trigger %q: definition is not readable (encrypted)",
		},
		"scripter_objects.go":  {"gosmo: script statistic %q on %s: it belongs to the index of the same name"},
		"scripter_security.go": {"gosmo: script database audit specification %q: it names no audit"},
		"scripter_server.go": {
			"gosmo: script server trigger %q: definition is not readable (encrypted)",
			"gosmo: script endpoint %q: its database mirroring detail could not be read",
			"gosmo: script endpoint %q: its service broker detail could not be read",
			"gosmo: script endpoint %q: goSMO cannot script a %s endpoint",
			"gosmo: script server audit specification %q: it names no audit",
		},
		// Script mode's argument binding: the statement is fine, the capture
		// has no literal form for it.
		"script.go": {
			"gosmo: script: named argument %q cannot be scripted positionally",
			"gosmo: script: %s has no argument (%d given)",
			"gosmo: script: %s appears only inside a literal or comment",
			"named argument %q cannot be scripted positionally",
			"cannot script a %T argument",
		},
		"server.go": {"a log file needs a data file beside it, and the instance reports no default data path"},
		"symmetric_key.go": {
			"symmetric key %q has no decryptor to open it with",
			"symmetric key %q: decryptor chain deeper than %d",
		},
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Errorf" {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "fmt" || len(call.Args) == 0 {
				return true
			}
			format := literalText(call.Args[0])
			if strings.Contains(format, "%w") {
				return true
			}
			seen++
			for _, prefix := range allowed[name] {
				if strings.HasPrefix(format, prefix) {
					return true
				}
			}
			t.Errorf("%s: fmt.Errorf(%q) wraps nothing: an argument check is invalidf, and anything else belongs on this test's list",
				fset.Position(call.Pos()), format)
			return true
		})
	}
	if seen == 0 {
		t.Fatal("found no fmt.Errorf at all; the scan is broken")
	}
}

// literalText is a format argument's text: a string literal, or a
// concatenation of them. Anything else reads as "", which only the errors.go
// entries match.
func literalText(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.BasicLit:
		s, err := strconv.Unquote(e.Value)
		if err == nil {
			return s
		}
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			return literalText(e.X) + literalText(e.Y)
		}
	}
	return ""
}

// A sample of refusals from different families, through the public API: each
// wraps ErrInvalidRequest and keeps its message.
func TestArgumentRefusalsWrapErrInvalidRequest(t *testing.T) {
	d, ctx, _ := scriptedDB(t)
	s := d.server
	cases := []struct {
		name string
		err  error
		msg  string
	}{
		{"create table", errOnly(d.CreateTable(ctx, CreateTableRequest{})), "gosmo: create table: name is required"},
		{"set recovery model", d.SetRecoveryModel(ctx, "NOPE"), `gosmo: set recovery model of "appdb": unrecognized recovery model "NOPE"`},
		{"create login", errOnly(s.CreateLogin(ctx, CreateLoginRequest{Name: "x", Source: LoginSourceSQL})), "a SQL login requires a password"},
		{"signer", d.StoredProcedureRef("dbo", "p").AddSignature(ctx, Signer{Kind: SignerCertificate}, false), "signature by certificate has no name"},
		{"apply permission", d.ApplyPermission(ctx, VerbGrant, Securable{Class: SecurableTable, Schema: "dbo", Name: "t"}, PermExecute, "x", PermissionOptions{}),
			`gosmo: grant permission: "EXECUTE" cannot be granted on a table`},
	}
	for _, tc := range cases {
		if !errors.Is(tc.err, ErrInvalidRequest) {
			t.Errorf("%s: %v does not wrap ErrInvalidRequest", tc.name, tc.err)
		}
		if tc.err == nil || !strings.Contains(tc.err.Error(), tc.msg) {
			t.Errorf("%s: error %v, want it to read %q", tc.name, tc.err, tc.msg)
		}
		if errors.Is(tc.err, ErrUnsupported) {
			t.Errorf("%s: an argument refusal also reads as ErrUnsupported", tc.name)
		}
	}
}
