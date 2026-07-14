package gosmo

import (
	"strings"
	"testing"
)

func TestBuildChangePasswordStatementQuotesLiteralByDefault(t *testing.T) {
	got := buildChangePasswordStatement("app_login", "hunter2", false, false)
	want := "ALTER LOGIN [app_login] WITH PASSWORD = " + nStringLiteral("hunter2")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, "HASHED") {
		t.Error("statement used HASHED, which tells SQL Server the value is already a password hash, not cleartext")
	}
}

func TestBuildChangePasswordStatementMustChangeAddsCheckExpiration(t *testing.T) {
	// MUST_CHANGE requires CHECK_EXPIRATION = ON or SQL Server rejects it.
	got := buildChangePasswordStatement("app_login", "hunter2", true, false)
	want := "ALTER LOGIN [app_login] WITH PASSWORD = " + nStringLiteral("hunter2") + " MUST_CHANGE, CHECK_EXPIRATION = ON"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestBuildChangePasswordStatementUnlock(t *testing.T) {
	// UNLOCK is a password-clause modifier, space-separated after
	// PASSWORD = '...' — not a comma-separated <set_option> — confirmed
	// against a live server ("ALTER LOGIN ... WITH PASSWORD = '...', UNLOCK"
	// is rejected with "Incorrect syntax near 'UNLOCK'").
	got := buildChangePasswordStatement("app_login", "hunter2", false, true)
	want := "ALTER LOGIN [app_login] WITH PASSWORD = " + nStringLiteral("hunter2") + " UNLOCK"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestBuildChangePasswordStatementMustChangeAndUnlock(t *testing.T) {
	got := buildChangePasswordStatement("app_login", "hunter2", true, true)
	want := "ALTER LOGIN [app_login] WITH PASSWORD = " + nStringLiteral("hunter2") + " MUST_CHANGE UNLOCK, CHECK_EXPIRATION = ON"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestBuildChangePasswordStatementEscapesQuotes(t *testing.T) {
	got := buildChangePasswordStatement("app_login", "it's a secret", true, false)
	want := "ALTER LOGIN [app_login] WITH PASSWORD = " + nStringLiteral("it's a secret") + " MUST_CHANGE, CHECK_EXPIRATION = ON"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
