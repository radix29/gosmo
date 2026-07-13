package gosmo

import (
	"strings"
	"testing"
)

func TestBuildChangePasswordStatementHashedByDefault(t *testing.T) {
	got := buildChangePasswordStatement("app_login", "hunter2", false, false)
	want := "ALTER LOGIN [app_login] WITH PASSWORD = " + passwordHexLiteral("hunter2") + " HASHED"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestBuildChangePasswordStatementMustChangeUsesCleartextLiteral(t *testing.T) {
	// MUST_CHANGE can't be combined with a HASHED password (SQL Server
	// rejects it), so this path must fall back to a quoted literal instead
	// of the UTF-16LE hex encoding every other password change uses.
	got := buildChangePasswordStatement("app_login", "hunter2", true, false)
	want := "ALTER LOGIN [app_login] WITH PASSWORD = " + QuoteLiteral("hunter2") + " MUST_CHANGE"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, passwordHexLiteral("hunter2")) {
		t.Error("MUST_CHANGE statement used the HASHED encoding, which SQL Server rejects alongside MUST_CHANGE")
	}
}

func TestBuildChangePasswordStatementUnlock(t *testing.T) {
	got := buildChangePasswordStatement("app_login", "hunter2", false, true)
	if !strings.Contains(got, ", UNLOCK") {
		t.Errorf("statement missing UNLOCK clause: %s", got)
	}
}

func TestBuildChangePasswordStatementEscapesQuotesInCleartextPath(t *testing.T) {
	got := buildChangePasswordStatement("app_login", "it's a secret", true, false)
	want := "ALTER LOGIN [app_login] WITH PASSWORD = " + QuoteLiteral("it's a secret") + " MUST_CHANGE"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
